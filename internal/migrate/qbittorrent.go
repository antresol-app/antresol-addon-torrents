package migrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// SourceQBittorrent is the Line.Source of the qBittorrent importer.
const SourceQBittorrent = "qbittorrent"

// TorrentEngine is what the qBittorrent importer needs from torrent.Engine.
type TorrentEngine interface {
	AddTorrentFile(ctx context.Context, data []byte, opt torrent.AddOptions) (torrent.Handle, error)
	Get(infohash string) (torrent.Handle, bool)
	Resume(ctx context.Context, infohash string) error
	Pause(ctx context.Context, infohash string) error
	RemoveIfMode(ctx context.Context, infohash, mode string, deleteData bool) error
	Offline() string
	WaitVerified(ctx context.Context, ih string) (done, size int64, err error)
}

// LibraryFiles is what the importer needs of the core's library
// (coreadapt.Library; coretest.Library in tests): the files under a root,
// an item, and a download's ref put on exactly its library files.
type LibraryFiles interface {
	FilesUnder(ctx context.Context, dir string) ([]library.File, error)
	Item(ctx context.Context, id int64) (library.Item, error)
	BindRef(ctx context.Context, ref string, paths []string) (missing, taken []string, err error)
}

var _ TorrentEngine = (*torrent.Engine)(nil)

// QBOptions configure ImportQBittorrent and RollbackQB.
type QBOptions struct {
	BTBackupDir string // qBittorrent's BT_backup (read only)
	WebURL      string // qBittorrent Web UI, e.g. http://192.168.1.50:8080
	Username    string // "" = no login (the address is whitelisted)
	Password    string
	Paths       PathMap // qBittorrent paths → Vault paths
	// LinkDir holds hard-link trees for torrents with files renamed in
	// qBittorrent: Abs(TorrentsDir)/.qbt-import.
	LinkDir string
	// StateDir keeps one small file per torrent being moved
	// (DataDir/migrate/qbt-state): its state in qBittorrent before the move
	// and whether the move finished. Required for a real run.
	StateDir string
	// LibraryRoots are searched for library files that are hard links of
	// the torrent's data: Abs(Movies), Abs(Series).
	LibraryRoots  []string
	AddedBy       int64
	AddedByUID    string // AddedBy's uid ("" = the torrents name nobody)
	DryRun        bool
	VerifyTimeout time.Duration // per torrent; 0 → 6 h
	StopTimeout   time.Duration // wait for qBittorrent to confirm a stop; 0 → 10 s
	// Checkpoint runs after every line (the caller saves the report).
	Checkpoint func()
	// KeepPaused (boot sets it from no-seeding) says whether the torrents
	// a rollback, or a move that failed after qBittorrent stopped them,
	// gives back must stay paused in qBittorrent, whatever their state
	// there before the move. false: the ones that were running there
	// are started again. nil keeps them paused (fail closed: a caller that
	// forgot it never resumes sharing).
	KeepPaused func(ctx context.Context) bool
	HTTP       *http.Client
}

const defaultVerifyTimeout = 6 * time.Hour

var infohashRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ImportQBittorrent moves qBittorrent's torrents into Vault, one at a time
// in the order of the .torrent names: the torrent is added paused in seed
// mode on the data qBittorrent already has, Vault checks every piece, and
// only then qBittorrent is told to stop it and Vault starts seeding. A
// torrent that cannot be moved stays in qBittorrent untouched (a failed
// stop is undone with a start). Nothing in BT_backup or in qBittorrent's
// folders is written; renamed files get a hard-link tree under LinkDir.
// DryRun only reads and reports planned lines. Running it again skips what
// Vault already seeds and finishes torrents a restart left paused in Vault.
// The error is only ctx's.
func ImportQBittorrent(ctx context.Context, o QBOptions, eng TorrentEngine, lib LibraryFiles, rep *Report) error {
	if o.VerifyTimeout <= 0 {
		o.VerifyTimeout = defaultVerifyTimeout
	}
	im := &qbImport{o: o, eng: eng, lib: lib, rep: rep, qb: newQBClient(o), log: slog.Default().With("component", "migrate")}
	return im.run(ctx)
}

type qbImport struct {
	o   QBOptions
	eng TorrentEngine
	lib LibraryFiles
	rep *Report
	qb  *qbClient
	log *slog.Logger

	index map[fileKey][]library.File // library files by inode, built on first use
}

func (im *qbImport) add(l Line) {
	l.Source = SourceQBittorrent
	if l.Kind == "" {
		l.Kind = "torrent"
	}
	im.rep.Add(l)
	if im.o.Checkpoint != nil {
		im.o.Checkpoint()
	}
}

func (im *qbImport) fail(l Line, reason string) {
	l.Status, l.Reason = StatusFailed, reason
	im.add(l)
}

// connect logs in (when a user is set) and asks for the version: the dry
// run does it too, so a missing API shows up before the real run.
func connectQB(ctx context.Context, o QBOptions, qb *qbClient) error {
	if o.WebURL == "" {
		return errors.New("адрес qBittorrent не задан")
	}
	if err := qb.login(ctx); err != nil {
		return err
	}
	_, err := qb.version(ctx)
	return err
}

func noAPIReason(o QBOptions) string {
	addr := o.WebURL
	if addr == "" {
		addr = "адрес не задан"
	}
	return fmt.Sprintf("Нет связи с qBittorrent (%s) — раздачи не перенесены", addr)
}

func (im *qbImport) run(ctx context.Context) error {
	head := Line{Name: "qBittorrent"}
	if im.eng.Offline() != "" {
		im.fail(head, "Медиадиск недоступен — раздачи не перенесены")
		return nil
	}
	if !im.o.DryRun && im.o.StateDir == "" {
		im.fail(head, "Не задана папка состояния переноса — раздачи не перенесены")
		return nil
	}
	if err := connectQB(ctx, im.o, im.qb); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		im.log.Warn("qbittorrent api", "url", im.o.WebURL, "err", err)
		im.fail(head, noAPIReason(im.o))
		return nil
	}
	entries, err := os.ReadDir(im.o.BTBackupDir)
	if err != nil {
		im.log.Warn("qbittorrent BT_backup", "dir", im.o.BTBackupDir, "err", err)
		im.fail(head, fmt.Sprintf("Не удалось прочитать папку BT_backup (%s) — раздачи не перенесены", im.o.BTBackupDir))
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".torrent") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := im.one(ctx, n); err != nil {
			return err
		}
	}
	return nil
}

// fastresume holds the keys of a qBittorrent .fastresume the importer uses.
type fastresume struct {
	savePath string
	name     string
	paused   bool
	pieces   []byte // one byte per piece, bit 0 = have
	mapped   []string
}

func readFastresume(p string) (fastresume, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return fastresume{}, err
	}
	var m map[string]any
	if err := bencode.Unmarshal(b, &m); err != nil {
		return fastresume{}, err
	}
	str := func(k string) string { s, _ := m[k].(string); return s }
	var fr fastresume
	fr.savePath = str("qBt-savePath")
	if fr.savePath == "" {
		fr.savePath = str("save_path")
	}
	fr.name = str("qBt-name")
	if n, ok := m["paused"].(int64); ok && n != 0 {
		fr.paused = true
	}
	fr.pieces = []byte(str("pieces"))
	if list, ok := m["mapped_files"].([]any); ok {
		for _, v := range list {
			s, _ := v.(string)
			fr.mapped = append(fr.mapped, s)
		}
	}
	return fr, nil
}

// layout is where the torrent's data is on disk.
type layout struct {
	dataDir string
	files   []linkFile // in Info.UpvertedFiles order
	renamed bool       // some file is not at its torrent path: needs a link tree
}

func layoutOf(paths PathMap, info *metainfo.Info, fr fastresume) layout {
	l := layout{dataDir: filepath.Clean(paths.Map(path.Clean(fr.savePath)))}
	name := info.BestName()
	for i, fi := range info.UpvertedFiles() {
		def := path.Join(append([]string{name}, fi.BestPath()...)...)
		actual := def
		if i < len(fr.mapped) && fr.mapped[i] != "" {
			actual = path.Clean(fr.mapped[i])
			if actual != def {
				l.renamed = true
			}
		}
		target := filepath.Join(l.dataDir, filepath.FromSlash(actual))
		if path.IsAbs(actual) {
			target = filepath.Clean(paths.Map(actual))
		}
		l.files = append(l.files, linkFile{Rel: def, Target: target})
	}
	return l
}

func percent(done, total int64) int64 {
	if total <= 0 {
		return 0
	}
	return done * 100 / total
}

func (im *qbImport) one(ctx context.Context, fileName string) error {
	stem := strings.TrimSuffix(fileName, ".torrent")
	l := Line{Name: fileName}
	meta, err := os.ReadFile(filepath.Join(im.o.BTBackupDir, fileName))
	if err != nil {
		im.log.Warn("qbittorrent: read torrent", "file", fileName, "err", err)
		im.fail(l, "Не удалось прочитать файл торрента")
		return nil
	}
	mi, err := metainfo.Load(bytes.NewReader(meta))
	var info metainfo.Info
	if err == nil {
		info, err = mi.UnmarshalInfo()
	}
	if err != nil {
		im.log.Warn("qbittorrent: parse torrent", "file", fileName, "err", err)
		im.fail(l, "Файл торрента повреждён")
		return nil
	}
	ih := mi.HashInfoBytes().HexString()
	l.Name, l.Ref, l.Size = info.BestName(), ih, info.TotalLength()
	// qBittorrent names a v1 (or hybrid) torrent by its v1 infohash; any
	// other name is a v2-only id that Vault cannot stop or start.
	if !strings.EqualFold(stem, ih) {
		im.fail(l, "Торрент v2 — перенос не поддерживается")
		return nil
	}

	// 1. qBittorrent's state of the torrent.
	fr, err := readFastresume(filepath.Join(im.o.BTBackupDir, stem+".fastresume"))
	if errors.Is(err, os.ErrNotExist) {
		im.fail(l, "Нет файла состояния qBittorrent")
		return nil
	}
	if err != nil {
		im.log.Warn("qbittorrent: parse fastresume", "file", stem+".fastresume", "err", err)
		im.fail(l, "Файл состояния qBittorrent не читается")
		return nil
	}
	if fr.name != "" {
		l.Name = fr.name
	}
	lay := layoutOf(im.o.Paths, &info, fr)
	mark, marked, err := loadState(im.o.StateDir, ih)
	if err != nil {
		im.log.Warn("qbittorrent: move state", "infohash", ih, "err", err)
		im.fail(l, fmt.Sprintf("Файл состояния переноса не читается: %s", statePath(im.o.StateDir, ih)))
		return nil
	}
	inProgress := marked && !mark.Done

	// 2. Already in Vault.
	if h, ok := im.eng.Get(ih); ok {
		st := h.Status()
		switch {
		case st.Mode == torrent.ModeSeed && st.Status == torrent.StatusSeeding && inProgress:
			// Crashed after Resume, before the line was saved.
			l.Prev = mark.Prev
			if im.o.DryRun {
				l.Status, l.Reason = StatusPlanned, "Перенос был прерван — будет завершён"
				im.add(l)
				return nil
			}
			// Or crashed before the stop and the copy was resumed in Vault
			// since: make sure qBittorrent has stopped it before calling the
			// move done, so the two never seed at once.
			stopSent, err := im.ensureStopped(ctx, &l, true)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				im.giveBack(ctx, l, fmt.Sprintf("Не удалось остановить раздачу в qBittorrent: %s", err), stopSent, errors.Is(err, errNotInQB))
				return nil
			}
			im.bind(ctx, ih, im.matches(ctx, lay.files))
			l.Status, l.Reason = StatusImported, "Перенос завершён после перезапуска"
			im.add(l)
			im.markDone(ih, qbState{Prev: l.Prev, Report: im.rep.ID})
		case st.Mode == torrent.ModeSeed && st.Status == torrent.StatusSeeding:
			if !im.o.DryRun {
				im.bind(ctx, ih, im.matches(ctx, lay.files))
			}
			l.Status, l.Reason = StatusSkipped, "Уже в Antresol"
			im.add(l)
		case st.Mode == torrent.ModeSeed && st.Status == torrent.StatusPaused && inProgress:
			// An earlier run was interrupted after the add.
			l.Prev = mark.Prev
			if im.o.DryRun {
				l.Status, l.Reason = StatusPlanned, "Перенос был прерван — будет завершён"
				im.add(l)
				return nil
			}
			return im.finish(ctx, l, im.matches(ctx, lay.files), mark.Prev, true)
		case st.Mode == torrent.ModeSeed && st.Status == torrent.StatusPaused:
			// Paused by an admin in Vault, not by this migration.
			l.Status, l.Reason = StatusSkipped, "Уже в Antresol (на паузе)"
			im.add(l)
		case st.Mode != torrent.ModeSeed:
			l.Status, l.Reason = StatusSkipped, fmt.Sprintf("Уже в Antresol (%s)", st.Mode)
			im.add(l)
		default:
			l.Status, l.Reason = StatusSkipped, fmt.Sprintf("Уже в Antresol (%s)", st.Status)
			im.add(l)
		}
		return nil
	}

	// 3. Incomplete in qBittorrent: not moved.
	if n := info.NumPieces(); len(fr.pieces) > 0 && n > 0 {
		have := 0
		for i := 0; i < n && i < len(fr.pieces); i++ {
			if fr.pieces[i]&1 == 1 {
				have++
			}
		}
		if have < n {
			l.Status = StatusWarning
			l.Reason = fmt.Sprintf("Недокачано (%d %%) — докачайте в qBittorrent и повторите перенос", percent(int64(have), int64(n)))
			im.add(l)
			return nil
		}
	}

	// What qBittorrent is doing with it now (the fastresume is saved only
	// now and then): remembered, so a rollback leaves paused ones paused.
	qst, found, err := im.qb.state(ctx, ih)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		im.log.Warn("qbittorrent: torrent state", "infohash", ih, "err", err)
		im.fail(l, fmt.Sprintf("Не удалось узнать состояние раздачи в qBittorrent: %s", err))
		return nil
	}
	if found && qbBusyStates[qst] {
		l.Status = StatusWarning
		l.Reason = fmt.Sprintf("В qBittorrent раздача сейчас в состоянии «%s» — дождитесь, пока она раздаётся или остановлена, и повторите перенос", qst)
		im.add(l)
		return nil
	}
	l.Prev = PrevStopped
	if found && !qbStoppedStates[qst] {
		l.Prev = PrevRunning
	}
	if inProgress && mark.Prev == PrevRunning {
		// An earlier attempt stopped it and could not give it back (or
		// crashed before it could): qBittorrent now says "stopped", but the
		// user had it running.
		l.Prev = PrevRunning
	}

	// 4. The data folder.
	if fr.savePath == "" {
		im.fail(l, "В файле состояния qBittorrent не указана папка раздачи")
		return nil
	}
	if st, err := os.Stat(lay.dataDir); err != nil || !st.IsDir() {
		im.fail(l, fmt.Sprintf("Нет папки %s", lay.dataDir))
		return nil
	}

	// 5. Renamed files: the torrent's layout as hard links.
	saveDir := lay.dataDir
	if lay.renamed {
		if im.o.LinkDir == "" {
			im.fail(l, "Файлы переименованы в qBittorrent, а папка для ссылок не задана")
			return nil
		}
		saveDir = filepath.Join(im.o.LinkDir, ih)
		var err error
		if im.o.DryRun {
			var ok bool
			if ok, err = canLink(saveDir, lay.files); err == nil && !ok {
				err = errCrossDevice
			}
		} else {
			err = buildLinkTree(saveDir, lay.files)
		}
		if errors.Is(err, errCrossDevice) {
			im.fail(l, "Файлы переименованы и лежат на другом диске")
			return nil
		}
		if err != nil {
			im.log.Warn("qbittorrent: link tree", "infohash", ih, "err", err)
			if !im.o.DryRun {
				im.removeLinkTree(ih)
			}
			im.fail(l, fmt.Sprintf("Не удалось подготовить переименованные файлы: %s", err))
			return nil
		}
	}

	// 6. Dry run.
	if im.o.DryRun {
		l.Status = StatusPlanned
		im.add(l)
		return nil
	}

	// 7. Remember the qBittorrent state on disk, add paused, check the data.
	mark = qbState{Prev: l.Prev, Report: im.rep.ID}
	if err := saveState(im.o.StateDir, ih, mark); err != nil {
		im.log.Error("qbittorrent: save move state", "infohash", ih, "err", err)
		im.removeLinkTree(ih)
		im.fail(l, leftInQB(fmt.Sprintf("Не удалось записать состояние переноса: %s", err)))
		return nil
	}
	matched := im.matches(ctx, lay.files)
	opt := torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: saveDir, Paused: true, AddedBy: im.o.AddedBy, AddedByUID: im.o.AddedByUID, ItemID: im.topItem(ctx, matched)}
	if _, err := im.eng.AddTorrentFile(ctx, meta, opt); err != nil {
		im.log.Warn("qbittorrent: add to vault", "infohash", ih, "err", err)
		im.removeLinkTree(ih)
		im.dropState(ih)
		im.fail(l, leftInQB(fmt.Sprintf("Antresol не принял торрент: %s", err)))
		return nil
	}
	return im.finish(ctx, l, matched, l.Prev, false)
}

// finish runs steps 7–9 for a torrent that is in Vault, paused: wait for
// the check, stop it in qBittorrent (only when it was running there), seed
// it in Vault. prev is its state in qBittorrent before the move.
func (im *qbImport) finish(ctx context.Context, l Line, matched []library.File, prev string, resumed bool) error {
	ih := l.Ref
	l.Prev = prev
	vctx, cancel := context.WithTimeout(ctx, im.o.VerifyTimeout)
	done, size, err := im.eng.WaitVerified(vctx, ih)
	cancel()
	if err != nil && ctx.Err() != nil {
		// Shutting down: the torrent stays paused in Vault (with its state
		// file) and the next run finishes it; qBittorrent was not touched.
		return ctx.Err()
	}
	if err != nil {
		im.log.Warn("qbittorrent: verify", "infohash", ih, "err", err)
		reason := fmt.Sprintf("Не удалось проверить данные: %s", err)
		if errors.Is(err, context.DeadlineExceeded) {
			reason = fmt.Sprintf("Проверка данных не закончилась за %s", im.o.VerifyTimeout)
		}
		im.fail(l, im.dropFromVault(ctx, ih, leftInQB(reason)))
		return nil
	}
	if done < size {
		im.fail(l, im.dropFromVault(ctx, ih, leftInQB(fmt.Sprintf("Данные на диске не совпадают с торрентом (%d %%)", percent(done, size)))))
		return nil
	}

	// 8. Stop it in qBittorrent and make sure it stopped.
	stopSent, err := im.ensureStopped(ctx, &l, resumed)
	if err != nil {
		im.giveBack(ctx, l, fmt.Sprintf("Не удалось остановить раздачу в qBittorrent: %s", err), stopSent, errors.Is(err, errNotInQB))
		return nil
	}

	// 9. Seed in Vault.
	if err := im.eng.Resume(ctx, ih); err != nil {
		im.log.Warn("qbittorrent: resume in vault", "infohash", ih, "err", err)
		im.giveBack(ctx, l, fmt.Sprintf("Не удалось запустить раздачу в Antresol: %s", err), stopSent, false)
		return nil
	}
	im.bind(ctx, ih, matched)
	l.Status = StatusImported
	if l.Prev == PrevStopped {
		l.Reason = "В qBittorrent была на паузе — теперь раздаёт Antresol"
	}
	im.add(l)
	im.markDone(ih, qbState{Prev: l.Prev, Report: im.rep.ID})
	return nil
}

// ensureStopped makes sure qBittorrent does not run the torrent once Vault
// seeds it. A torrent that was paused there is looked at again right
// before Vault starts it: if the user started it meanwhile (during a long
// check, or between a crash and this run), it now counts as running —
// recorded in the line and the state file — and is stopped first.
func (im *qbImport) ensureStopped(ctx context.Context, l *Line, resumed bool) (stopSent bool, err error) {
	if l.Prev != PrevRunning {
		st, found, err := im.qb.state(ctx, l.Ref)
		if err != nil {
			return false, err
		}
		if !found || qbStoppedStates[st] {
			return false, nil
		}
		l.Prev = PrevRunning
		if err := saveState(im.o.StateDir, l.Ref, qbState{Prev: PrevRunning, Report: im.rep.ID}); err != nil {
			return false, err
		}
		resumed = false
	}
	return im.stopInQB(ctx, l.Ref, true, resumed)
}

// stopInQB stops a torrent that was running in qBittorrent and waits for
// the stop to show; a torrent that was not running gets nothing. A resumed
// torrent may have been stopped by the interrupted run already (or be gone
// from qBittorrent): then nothing is sent either. stopSent says whether
// this run sent a stop request.
func (im *qbImport) stopInQB(ctx context.Context, ih string, wasRunning, resumed bool) (stopSent bool, err error) {
	if !wasRunning {
		return false, nil
	}
	if resumed {
		st, found, err := im.qb.state(ctx, ih)
		if err != nil {
			return false, err
		}
		if !found || qbStoppedStates[st] {
			return false, nil
		}
	}
	if err := im.qb.stop(ctx, ih); err != nil {
		return true, err
	}
	ok, err := im.qb.stopped(ctx, ih)
	if err != nil {
		return true, err
	}
	if !ok {
		return true, fmt.Errorf("qBittorrent не подтвердил остановку за %s", im.qb.stopTimeout)
	}
	return true, nil
}

// giveBack undoes a move that failed after the check: Vault drops its copy
// first, then qBittorrent is started again — only when it was running
// there, this run sent it the stop, Vault's copy is really gone and
// KeepPaused does not hold (nil holds) — so the two never seed at once, a
// torrent paused in qBittorrent stays paused, and none is started there
// while sharing must stay off.
// When Vault cannot remove its copy, the copy is paused instead and
// qBittorrent is not started. The state file stays unless nothing more is
// to be done: while Vault kept a copy, or the torrent was running in
// qBittorrent and was not started there again, the next run must still
// know that it was running.
func (im *qbImport) giveBack(ctx context.Context, l Line, reason string, stopSent, notInQB bool) {
	ih := l.Ref
	if err := im.removeFromVault(ctx, ih); err != nil {
		note := "она поставлена на паузу"
		if perr := im.eng.Pause(context.WithoutCancel(ctx), ih); perr != nil && !errors.Is(perr, torrent.ErrNotFound) {
			im.log.Error("qbittorrent: pause the copy vault kept", "infohash", ih, "err", perr)
			note = "и поставить её на паузу тоже не удалось"
		}
		im.fail(l, fmt.Sprintf("%s — Antresol не смог убрать свою копию раздачи (%s), поэтому в qBittorrent её не запускаю: проверьте раздачу в Antresol и qBittorrent вручную", reason, note))
		return
	}
	started := false
	switch {
	case notInQB:
		im.fail(l, fmt.Sprintf("%s — в qBittorrent этой раздачи больше нет", reason))
	case l.Prev != PrevRunning:
		im.fail(l, fmt.Sprintf("%s — раздача осталась в qBittorrent на паузе, как была", reason))
	case !stopSent:
		im.fail(l, fmt.Sprintf("%s — проверьте раздачу в qBittorrent и при необходимости запустите её там", reason))
	case im.o.KeepPaused == nil:
		// The same rule as a rollback: a caller that set no rule keeps the
		// torrent stopped in qBittorrent (fail closed), and the reason does
		// not claim no-seeding is on; the state file stays, so a later run
		// still knows it was running.
		im.fail(l, fmt.Sprintf("%s — раздача осталась в qBittorrent на паузе: запустите её там вручную, если нужно", reason))
	case im.o.KeepPaused(ctx):
		// While sharing must stay off, the same: stopped, state file kept.
		im.fail(l, fmt.Sprintf("%s — раздача осталась в qBittorrent на паузе: включён запрет раздачи", reason))
	default:
		if err := im.qb.start(context.WithoutCancel(ctx), ih); err != nil {
			im.log.Warn("qbittorrent: start after a failed stop", "infohash", ih, "err", err)
			im.fail(l, fmt.Sprintf("%s — запустить раздачу в qBittorrent снова не удалось (%s): запустите её там вручную", reason, err))
		} else {
			started = true
			im.fail(l, fmt.Sprintf("%s — раздача возвращена в qBittorrent", reason))
		}
	}
	if started || l.Prev != PrevRunning {
		im.dropState(ih)
	}
}

// dropFromVault removes the torrent from Vault (never its data), its link
// tree and its state file, for failures before any stop, when the copy is
// paused. It returns reason, with a note when Vault kept its copy.
func (im *qbImport) dropFromVault(ctx context.Context, ih, reason string) string {
	if err := im.removeFromVault(ctx, ih); err != nil {
		return keptNote(reason)
	}
	im.dropState(ih)
	return reason
}

// keptNote is reason with the note that Vault kept its paused copy.
func keptNote(reason string) string {
	return fmt.Sprintf("%s (в Antresol она осталась на паузе)", reason)
}

// leftInQB is reason with the note that qBittorrent still has the torrent.
func leftInQB(reason string) string {
	return fmt.Sprintf("%s — раздача осталась в qBittorrent", reason)
}

// removeFromVault removes the torrent from Vault (never its data) and its
// link tree; a torrent Vault no longer has is fine.
func (im *qbImport) removeFromVault(ctx context.Context, ih string) error {
	err := im.eng.RemoveIfMode(context.WithoutCancel(ctx), ih, torrent.ModeSeed, false)
	if err != nil && !errors.Is(err, torrent.ErrNotFound) {
		im.log.Error("qbittorrent: remove from vault", "infohash", ih, "err", err)
		return err
	}
	im.removeLinkTree(ih)
	return nil
}

func (im *qbImport) dropState(ih string) {
	if err := removeState(im.o.StateDir, ih); err != nil {
		im.log.Warn("qbittorrent: remove move state", "infohash", ih, "err", err)
	}
}

// markDone records that the imported line is in the report (saved by the
// Checkpoint that ran in add).
func (im *qbImport) markDone(ih string, s qbState) {
	s.Done = true
	if err := saveState(im.o.StateDir, ih, s); err != nil {
		im.log.Warn("qbittorrent: save move state", "infohash", ih, "err", err)
	}
}

// removeLinkTree deletes LinkDir/<ih>: hard links only, the data keeps its
// other names.
func (im *qbImport) removeLinkTree(ih string) {
	removeLinkTree(im.o.LinkDir, ih, im.log)
}

func removeLinkTree(linkDir, ih string, log *slog.Logger) {
	if linkDir == "" || !infohashRe.MatchString(ih) {
		return
	}
	if err := os.RemoveAll(filepath.Join(linkDir, ih)); err != nil {
		log.Warn("qbittorrent: remove link tree", "infohash", ih, "err", err)
	}
}

// matches returns the library files that are hard links of the torrent's
// files (same device and inode), in torrent file order.
func (im *qbImport) matches(ctx context.Context, files []linkFile) []library.File {
	if im.index == nil {
		im.index = map[fileKey][]library.File{}
		for _, root := range im.o.LibraryRoots {
			list, err := im.lib.FilesUnder(ctx, root)
			if err != nil {
				im.log.Warn("qbittorrent: library files", "root", root, "err", err)
				continue
			}
			for _, f := range list {
				st, err := os.Stat(f.Path)
				if err != nil {
					continue
				}
				if k, ok := keyOf(st); ok {
					im.index[k] = append(im.index[k], f)
				}
			}
		}
	}
	var out []library.File
	seen := map[string]bool{}
	for _, f := range files {
		st, err := os.Stat(f.Target)
		if err != nil {
			continue
		}
		k, ok := keyOf(st)
		if !ok {
			continue
		}
		for _, lf := range im.index[k] {
			if !seen[lf.Path] {
				seen[lf.Path] = true
				out = append(out, lf)
			}
		}
	}
	return out
}

// topItem is the movie or series of the first matched file.
func (im *qbImport) topItem(ctx context.Context, matched []library.File) *int64 {
	if len(matched) == 0 {
		return nil
	}
	id := matched[0].ItemID
	for range 16 {
		it, err := im.lib.Item(ctx, id)
		if err != nil {
			im.log.Warn("qbittorrent: library item", "id", id, "err", err)
			return nil
		}
		if it.ParentID == 0 {
			return &it.ID
		}
		id = it.ParentID
	}
	return nil
}

// bind puts the torrent's ref (its lower-case infohash) on the library
// files that are its data, in one call; none matched binds nothing (an
// empty list would take the ref off files an earlier run bound).
func (im *qbImport) bind(ctx context.Context, ih string, matched []library.File) {
	if len(matched) == 0 {
		return
	}
	paths := make([]string, len(matched))
	for i, f := range matched {
		paths[i] = f.Path
	}
	missing, taken, err := im.lib.BindRef(ctx, strings.ToLower(ih), paths)
	if err != nil {
		im.log.Warn("qbittorrent: bind library files", "infohash", ih, "files", len(paths), "err", err)
		return
	}
	if len(missing)+len(taken) > 0 {
		im.log.Warn("qbittorrent: library files not bound", "infohash", ih, "missing", missing, "taken", taken)
	}
}
