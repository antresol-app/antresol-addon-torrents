package migrate

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

func status(t *testing.T, e *env, ih string) torrent.Status {
	t.Helper()
	h, ok := e.eng.Get(ih)
	if !ok {
		t.Fatalf("%s is not in Vault", ih)
	}
	return h.Status()
}

func notInVault(t *testing.T, e *env, ih string) {
	t.Helper()
	if _, ok := e.eng.Get(ih); ok {
		t.Fatalf("%s was left in Vault", ih)
	}
}

func TestQBImportSeedsWithoutRedownload(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(300 << 10), "Show/ep2.mkv": randBytes(200 << 10)}, nil)
	btBefore, saveBefore := treeHash(t, e.bt), treeHash(t, e.save)
	rep := e.run(e.opts())
	if treeHash(t, e.bt) != btBefore || treeHash(t, e.save) != saveBefore {
		t.Fatal("qBittorrent's files changed")
	}
	l := onlyLine(t, rep)
	if l.Status != StatusImported || l.Ref != ih || l.Source != SourceQBittorrent || l.Kind != "torrent" || l.Name != "Show" || l.Size != 500<<10 {
		t.Fatalf("line %+v", l)
	}
	st := status(t, e, ih)
	if st.Mode != torrent.ModeSeed || st.Status != torrent.StatusSeeding || st.Done != st.Size || st.Downloaded != 0 || st.SaveDir != e.save {
		t.Fatalf("status %+v", st)
	}
	posts := e.qb.posts()
	if len(posts) != 1 || posts[0].Path != "/api/v2/torrents/stop" || posts[0].Hashes != ih {
		t.Fatalf("posts %+v", posts)
	}
	at, ok := inEngine(posts[0].Engine, ih)
	if !ok || at.Status != torrent.StatusPaused || at.Size == 0 || at.Done != at.Size {
		t.Fatalf("engine at stop: %+v", at)
	}
	if _, err := os.Stat(e.link); !os.IsNotExist(err) {
		t.Fatal("link tree made for a torrent without renames")
	}
}

func TestQBImportMappedFiles(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(300 << 10), "Show/ep2.mkv": randBytes(100 << 10)},
		map[string]any{"mapped_files": []string{"Show/Серия 1.mkv", ""}})
	renamed := filepath.Join(e.save, "Show", "Серия 1.mkv")
	if err := os.Rename(filepath.Join(e.save, "Show", "ep1.mkv"), renamed); err != nil {
		t.Fatal(err)
	}
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusImported {
		t.Fatalf("line %+v", l)
	}
	a, err := os.Stat(filepath.Join(e.link, ih, "Show", "ep1.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.Stat(renamed)
	if !os.SameFile(a, b) {
		t.Fatal("ep1.mkv is not a hard link of Серия 1.mkv")
	}
	st := status(t, e, ih)
	if st.Status != torrent.StatusSeeding || st.Downloaded != 0 || st.SaveDir != filepath.Join(e.link, ih) {
		t.Fatalf("status %+v", st)
	}
	if _, err := os.Stat(filepath.Join(e.save, "Show", "ep1.mkv")); !os.IsNotExist(err) {
		t.Fatal("qBittorrent's folder was changed")
	}
}

func TestQBImportCyrillicName(t *testing.T) {
	e := newEnv(t)
	name := "День выборов WEB-DL (1080p).mkv"
	ih, _ := e.addTorrent(map[string][]byte{name: randBytes(200 << 10)}, nil)
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusImported || l.Name != name || l.Ref != ih {
		t.Fatalf("line %+v", l)
	}
	if st := status(t, e, ih); st.Status != torrent.StatusSeeding || st.Downloaded != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestQBImportCorruptDataKeepsQB(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Movie (2000)/movie.mkv": randBytes(300 << 10)}, nil)
	p := filepath.Join(e.save, "Movie (2000)", "movie.mkv")
	b, _ := os.ReadFile(p)
	b[1000] ^= 0xff
	os.WriteFile(p, b, 0o644)
	before := fileHash(t, p)
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusFailed || !strings.Contains(l.Reason, "не совпадают") || !strings.Contains(l.Reason, "осталась в qBittorrent") {
		t.Fatalf("line %+v", l)
	}
	if posts := e.qb.posts(); len(posts) != 0 {
		t.Fatalf("posts %+v", posts)
	}
	notInVault(t, e, ih)
	if fileHash(t, p) != before {
		t.Fatal("data changed")
	}
}

func TestQBImportIncompleteWarns(t *testing.T) {
	e := newEnv(t)
	files := map[string][]byte{"Show/ep1.mkv": randBytes(256 << 10)}
	meta, ih := makeTorrent(t, t.TempDir(), files) // only to count the pieces
	pieces := []byte(allPieces(t, meta))
	pieces[3] = 0
	ih2, _ := e.addTorrent(files, map[string]any{"pieces": string(pieces)})
	if ih2 != ih {
		t.Fatal("same data, other infohash")
	}
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusWarning || !strings.HasPrefix(l.Reason, "Недокачано") || !strings.Contains(l.Reason, "87 %") {
		t.Fatalf("line %+v", l)
	}
	notInVault(t, e, ih)
	if posts := e.qb.posts(); len(posts) != 0 {
		t.Fatalf("posts %+v", posts)
	}
}

func TestQBImportStopFailsRollsBack(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(200 << 10)}, nil)
	p := filepath.Join(e.save, "Show", "ep1.mkv")
	before := fileHash(t, p)
	e.qb.stopCode = 500
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusFailed || !strings.HasPrefix(l.Reason, "Не удалось остановить") || !strings.Contains(l.Reason, "возвращена в qBittorrent") {
		t.Fatalf("line %+v", l)
	}
	posts := e.qb.posts()
	if len(posts) != 2 || posts[0].Path != "/api/v2/torrents/stop" || posts[1].Path != "/api/v2/torrents/start" || posts[1].Hashes != ih {
		t.Fatalf("posts %+v", posts)
	}
	if _, ok := inEngine(posts[1].Engine, ih); ok {
		t.Fatal("qBittorrent was started while Vault still had the torrent")
	}
	notInVault(t, e, ih)
	if fileHash(t, p) != before {
		t.Fatal("data changed")
	}
	if _, ok, _ := loadState(e.state, ih); ok {
		t.Fatal("state file left behind")
	}
}

func TestQBImportStopTimeoutRestarts(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(200 << 10)}, nil)
	e.qb.ignoreStop = true
	o := e.opts()
	o.StopTimeout = time.Second
	l := onlyLine(t, e.run(o))
	if l.Status != StatusFailed || !strings.Contains(l.Reason, "возвращена в qBittorrent") {
		t.Fatalf("line %+v", l)
	}
	posts := e.qb.posts()
	if len(posts) != 2 || posts[1].Path != "/api/v2/torrents/start" {
		t.Fatalf("posts %+v", posts)
	}
	if _, ok := inEngine(posts[1].Engine, ih); ok {
		t.Fatal("qBittorrent was started while Vault still had the torrent")
	}
	notInVault(t, e, ih)
}

func TestQBImportV4PauseFallback(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(200 << 10)}, nil)
	e.qb.stopCode = 404
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusImported {
		t.Fatalf("line %+v", l)
	}
	posts := e.qb.posts()
	if len(posts) != 2 || posts[0].Path != "/api/v2/torrents/stop" || posts[1].Path != "/api/v2/torrents/pause" || posts[1].Hashes != ih {
		t.Fatalf("posts %+v", posts)
	}
}

func TestQBImportIdempotent(t *testing.T) {
	e := newEnv(t)
	e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(200 << 10)}, nil)
	if l := onlyLine(t, e.run(e.opts())); l.Status != StatusImported {
		t.Fatalf("first run %+v", l)
	}
	n := len(e.qb.posts())
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusSkipped || l.Reason != "Уже в Antresol" {
		t.Fatalf("second run %+v", l)
	}
	if len(e.qb.posts()) != n {
		t.Fatalf("posts %+v", e.qb.posts())
	}
}

// libraryEpisode hard-links data into the series library and puts it in
// the fake core's library as S01E01 of «X» (series 1, season 2, episode
// 3); returns the library path and the series id.
func libraryEpisode(t *testing.T, e *env, data string) (string, int64) {
	t.Helper()
	p := filepath.Join(e.series, "X", "Season 1", "X.S01E01.mkv")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.Link(data, p); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	e.lib.Items = append(e.lib.Items,
		library.Item{ID: 1, Kind: library.KindSeries, Title: "X"},
		library.Item{ID: 2, Kind: library.KindSeason, ParentID: 1, SeasonNo: 1},
		library.Item{ID: 3, Kind: library.KindEpisode, ParentID: 2, EpisodeNo: 1})
	e.lib.Files = append(e.lib.Files, library.File{ID: 10, ItemID: 3, Path: p, Size: st.Size(), Mtime: st.ModTime().Unix()})
	return p, 1
}

func TestQBImportRebindsOnRerun(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"X.S01.1080p/X.S01E01.mkv": randBytes(200 << 10)}, nil)
	if l := onlyLine(t, e.run(e.opts())); l.Status != StatusImported {
		t.Fatalf("first run %+v", l)
	}
	p, _ := libraryEpisode(t, e, filepath.Join(e.save, "X.S01.1080p", "X.S01E01.mkv"))
	if l := onlyLine(t, e.run(e.opts())); l.Status != StatusSkipped {
		t.Fatalf("second run %+v", l)
	}
	if got := e.lib.Bound(ih); !slices.Equal(got, []string{p}) {
		t.Fatalf("bound %v, want %s", got, p)
	}
}

func TestQBImportResumesInterrupted(t *testing.T) {
	for _, qbNow := range []string{"stoppedUP", "uploading"} {
		t.Run(qbNow, func(t *testing.T) {
			e := newEnv(t)
			ih, meta := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(200 << 10)}, nil)
			// An earlier run added it (paused) and the server restarted
			// before qBittorrent was stopped.
			if err := saveState(e.state, ih, qbState{Prev: PrevRunning, Report: "x"}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.eng.AddTorrentFile(context.Background(), meta, torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: e.save, Paused: true}); err != nil {
				t.Fatal(err)
			}
			e.qb.setState(ih, qbNow)
			l := onlyLine(t, e.run(e.opts()))
			if l.Status != StatusImported || l.Ref != ih || l.Prev != PrevRunning {
				t.Fatalf("line %+v", l)
			}
			if st := status(t, e, ih); st.Status != torrent.StatusSeeding {
				t.Fatalf("not resumed: %+v", st)
			}
			posts := e.qb.posts()
			want := 0
			if qbNow == "uploading" {
				want = 1
			}
			if len(posts) != want || (want == 1 && (posts[0].Path != "/api/v2/torrents/stop" || posts[0].Hashes != ih)) {
				t.Fatalf("posts %+v", posts)
			}
		})
	}
}

func TestQBImportCheckpointsEachTorrent(t *testing.T) {
	e := newEnv(t)
	for _, n := range []string{"A", "B", "C"} {
		e.addTorrent(map[string][]byte{n + "/f.mkv": randBytes(100 << 10)}, nil)
	}
	o := e.opts()
	var rep *Report
	var seen []int
	o.Checkpoint = func() { seen = append(seen, len(rep.Lines)) }
	rep = &Report{ID: NewReportID(time.Now())}
	if err := ImportQBittorrent(context.Background(), o, e.eng, e.lib, rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Lines) != 3 || len(seen) < 3 {
		t.Fatalf("lines %d, checkpoints %v", len(rep.Lines), seen)
	}
	for _, want := range []int{1, 2, 3} {
		found := false
		for _, n := range seen {
			found = found || n == want
		}
		if !found {
			t.Fatalf("no checkpoint after line %d: %v", want, seen)
		}
	}
}

func TestQBImportDryRunTouchesNothing(t *testing.T) {
	e := newEnv(t)
	e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(200 << 10)}, nil)
	e.addTorrent(map[string][]byte{"Other/a.mkv": randBytes(100 << 10), "Other/b.mkv": randBytes(100 << 10)},
		map[string]any{"mapped_files": []string{"Other/renamed.mkv", ""}})
	os.Rename(filepath.Join(e.save, "Other", "a.mkv"), filepath.Join(e.save, "Other", "renamed.mkv"))
	btBefore, saveBefore := treeHash(t, e.bt), treeHash(t, e.save)
	o := e.opts()
	o.DryRun = true
	rep := e.run(o)
	if len(rep.Lines) != 2 {
		t.Fatalf("lines %+v", rep.Lines)
	}
	for _, l := range rep.Lines {
		if l.Status != StatusPlanned || l.Size == 0 {
			t.Fatalf("line %+v", l)
		}
	}
	if list := e.eng.List(); len(list) != 0 {
		t.Fatalf("engine %+v", list)
	}
	calls := e.qb.all()
	if len(calls) == 0 || calls[0].Path != "/api/v2/app/version" {
		t.Fatalf("calls %+v", calls)
	}
	reads := map[string]bool{"/api/v2/app/version": true, "/api/v2/torrents/info": true}
	if e.api { // BT_backup over the Web API: its reads too
		reads["/api/v2/torrents/export"], reads["/api/v2/torrents/files"], reads["/api/v2/torrents/pieceStates"] = true, true, true
	}
	for _, c := range calls {
		if c.Method != "GET" || !reads[c.Path] {
			t.Fatalf("dry run sent %+v", c)
		}
	}
	if _, err := os.Stat(e.state); !os.IsNotExist(err) {
		t.Fatal("dry run wrote move state")
	}
	if treeHash(t, e.bt) != btBefore || treeHash(t, e.save) != saveBefore {
		t.Fatal("source files changed")
	}
	if _, err := os.Stat(e.link); !os.IsNotExist(err) {
		t.Fatal("dry run made the link tree")
	}
}

func TestQBImportNoAPIRefuses(t *testing.T) {
	e := newEnv(t)
	e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	o := e.opts()
	o.WebURL = ""
	l := onlyLine(t, e.run(o))
	if l.Status != StatusFailed || !strings.HasPrefix(l.Reason, "Нет связи с qBittorrent") {
		t.Fatalf("line %+v", l)
	}
	if list := e.eng.List(); len(list) != 0 {
		t.Fatalf("engine %+v", list)
	}
	// A qBittorrent that refuses the login is the same.
	e.qb.user, e.qb.pass = "admin", "secret"
	o = e.opts()
	o.Username, o.Password = "admin", "wrong"
	l = onlyLine(t, e.run(o))
	if l.Status != StatusFailed || !strings.Contains(l.Reason, e.qb.URL) {
		t.Fatalf("line %+v", l)
	}
	if len(e.qb.posts()) != 0 || len(e.eng.List()) != 0 {
		t.Fatal("something was done without a working API")
	}
}

type offlineEngine struct{ TorrentEngine }

func (offlineEngine) Offline() string {
	return "Медиадиск недоступен: нет папки /srv/media"
}

func TestQBImportStorageOffline(t *testing.T) {
	e := newEnv(t)
	e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	rep := &Report{}
	if err := ImportQBittorrent(context.Background(), e.opts(), offlineEngine{e.eng}, e.lib, rep); err != nil {
		t.Fatal(err)
	}
	l := onlyLine(t, rep)
	if l.Status != StatusFailed || !strings.HasPrefix(l.Reason, "Медиадиск недоступен") {
		t.Fatalf("line %+v", l)
	}
	if len(e.eng.List()) != 0 || len(e.qb.all()) != 0 {
		t.Fatal("something was done with the disk offline")
	}
}

func TestQBImportBindsHardlinkedLibraryFile(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"X.S01.1080p/X.S01E01.mkv": randBytes(200 << 10), "X.S01.1080p/X.S01E02.mkv": randBytes(100 << 10)}, nil)
	p, seriesID := libraryEpisode(t, e, filepath.Join(e.save, "X.S01.1080p", "X.S01E01.mkv"))
	if l := onlyLine(t, e.run(e.opts())); l.Status != StatusImported {
		t.Fatalf("line %+v", l)
	}
	if got := e.lib.Bound(ih); !slices.Equal(got, []string{p}) {
		t.Fatalf("bound %v, want %s", got, p)
	}
	st := status(t, e, ih)
	if st.ItemID == nil || *st.ItemID != seriesID {
		t.Fatalf("item %v, want series %d", st.ItemID, seriesID)
	}
	if st.AddedBy == nil || *st.AddedBy != 1 || st.AddedByUID != "uid-1" {
		t.Fatalf("added by %v %q", st.AddedBy, st.AddedByUID)
	}
}

func TestQBImportLoginWhenUserSet(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	e.qb.user, e.qb.pass = "admin", "secret"
	o := e.opts()
	o.Username, o.Password = "admin", "secret"
	if l := onlyLine(t, e.run(o)); l.Status != StatusImported {
		t.Fatalf("line %+v", l)
	}
	calls := e.qb.all()
	if len(calls) == 0 || calls[0].Path != "/api/v2/auth/login" {
		t.Fatalf("calls %+v", calls)
	}
	stops := 0
	for _, c := range calls {
		if c.Path == "/api/v2/torrents/stop" {
			stops++
			if c.Cookie != fakeSID || c.Hashes != ih {
				t.Fatalf("stop %+v", c)
			}
		}
	}
	if stops != 1 {
		t.Fatalf("calls %+v", calls)
	}
}

func TestQBImportMissingStateAndFolder(t *testing.T) {
	e := newEnv(t)
	ih1, _ := e.addTorrent(map[string][]byte{"A/f.mkv": randBytes(100 << 10)}, nil)
	os.Remove(filepath.Join(e.bt, ih1+".fastresume"))
	gone := filepath.Join(t.TempDir(), "gone")
	ih2, _ := e.addTorrent(map[string][]byte{"B/f.mkv": randBytes(100 << 10)}, map[string]any{"qBt-savePath": gone})
	o := e.opts()
	rep := e.run(o)
	if len(rep.Lines) != 2 {
		t.Fatalf("lines %+v", rep.Lines)
	}
	byRef := map[string]Line{}
	for _, l := range rep.Lines {
		byRef[l.Ref] = l
	}
	if l := byRef[ih1]; l.Status != StatusFailed || l.Reason != "Нет файла состояния qBittorrent" {
		t.Fatalf("no fastresume: %+v", l)
	}
	if l := byRef[ih2]; l.Status != StatusFailed || l.Reason != "Нет папки "+gone {
		t.Fatalf("no folder: %+v", l)
	}
	if len(e.eng.List()) != 0 || len(e.qb.posts()) != 0 {
		t.Fatal("something was done")
	}
}

func TestQBImportPausedInQB(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, map[string]any{"paused": 1})
	e.qb.setState(ih, "stoppedUP")
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusImported || l.Reason != "В qBittorrent была на паузе — теперь раздаёт Antresol" {
		t.Fatalf("line %+v", l)
	}
	if st := status(t, e, ih); st.Status != torrent.StatusSeeding {
		t.Fatalf("status %+v", st)
	}
}

// resumeFails is an engine whose Resume fails (the disk went away).
type resumeFails struct{ *torrent.Engine }

func (resumeFails) Resume(context.Context, string) error { return torrent.ErrStorageOffline }

func runWith(t *testing.T, e *env, eng TorrentEngine, o QBOptions) *Report {
	t.Helper()
	rep := &Report{ID: NewReportID(time.Now())}
	if err := ImportQBittorrent(context.Background(), o, eng, e.lib, rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

// a torrent paused in qBittorrent gets no stop, is still moved, and
// its state is remembered on disk.
func TestQBImportStoppedInQBSendsNoStop(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil) // fastresume says running
	e.qb.setState(ih, "stoppedUP")                                                      // qBittorrent says stopped
	e.qb.stopCode = 500
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusImported || l.Prev != PrevStopped || l.Reason != "В qBittorrent была на паузе — теперь раздаёт Antresol" {
		t.Fatalf("line %+v", l)
	}
	if posts := e.qb.posts(); len(posts) != 0 {
		t.Fatalf("posts %+v", posts)
	}
	if st := status(t, e, ih); st.Status != torrent.StatusSeeding {
		t.Fatalf("status %+v", st)
	}
	s, ok, err := loadState(e.state, ih)
	if err != nil || !ok || s.Prev != PrevStopped || !s.Done {
		t.Fatalf("state %+v %v %v", s, ok, err)
	}
}

// a running torrent's state is on disk before the stop is sent.
func TestQBImportStateSavedBeforeStop(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	var at qbState
	var had bool
	e.qb.engine = func() []torrent.Status {
		at, had, _ = loadState(e.state, ih)
		return e.eng.List()
	}
	if l := onlyLine(t, e.run(e.opts())); l.Status != StatusImported || l.Prev != PrevRunning || l.Reason != "" {
		t.Fatalf("line %+v", l)
	}
	if !had || at.Prev != PrevRunning || at.Done {
		t.Fatalf("state at stop %+v %v", at, had)
	}
}

// a failure after the check never starts a torrent that was paused in
// qBittorrent.
func TestQBImportFailureKeepsQBPaused(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, map[string]any{"paused": 1})
	e.qb.setState(ih, "stoppedUP")
	l := onlyLine(t, runWith(t, e, resumeFails{e.eng}, e.opts()))
	if l.Status != StatusFailed || !strings.Contains(l.Reason, "на паузе, как была") {
		t.Fatalf("line %+v", l)
	}
	if posts := e.qb.posts(); len(posts) != 0 {
		t.Fatalf("posts %+v", posts)
	}
	notInVault(t, e, ih)
}

// torrents qBittorrent is still downloading or checking are not moved.
func TestQBImportBusyInQBWarns(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	e.qb.setState(ih, "checkingUP")
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusWarning || !strings.Contains(l.Reason, "checkingUP") {
		t.Fatalf("line %+v", l)
	}
	notInVault(t, e, ih)
	if len(e.qb.posts()) != 0 {
		t.Fatal("posted")
	}
}

// on the resumed path, a Resume failure after qBittorrent was already
// stopped (by the interrupted run) sends no start from this run.
func TestQBImportResumeFailsNoStartWithoutStop(t *testing.T) {
	e := newEnv(t)
	ih, meta := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	saveState(e.state, ih, qbState{Prev: PrevRunning, Report: "x"})
	if _, err := e.eng.AddTorrentFile(context.Background(), meta, torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: e.save, Paused: true}); err != nil {
		t.Fatal(err)
	}
	e.qb.setState(ih, "stoppedUP")
	l := onlyLine(t, runWith(t, e, resumeFails{e.eng}, e.opts()))
	if l.Status != StatusFailed || !strings.HasPrefix(l.Reason, "Не удалось запустить раздачу в Antresol") || strings.Contains(l.Reason, "возвращена") {
		t.Fatalf("line %+v", l)
	}
	if posts := e.qb.posts(); len(posts) != 0 {
		t.Fatalf("posts %+v", posts)
	}
	notInVault(t, e, ih)
}

// the process died after Resume and before the line was saved: the
// next run records the move as imported, and it can be rolled back.
func TestQBImportCrashAfterResumeRecorded(t *testing.T) {
	e := newEnv(t)
	ih, meta := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	saveState(e.state, ih, qbState{Prev: PrevRunning, Report: "x"})
	if _, err := e.eng.AddTorrentFile(context.Background(), meta, torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: e.save}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.eng.WaitVerified(context.Background(), ih); err != nil {
		t.Fatal(err)
	}
	e.qb.setState(ih, "stoppedUP")
	rep := e.run(e.opts())
	l := onlyLine(t, rep)
	if l.Status != StatusImported || l.Reason != "Перенос завершён после перезапуска" || l.Prev != PrevRunning {
		t.Fatalf("line %+v", l)
	}
	if len(e.qb.posts()) != 0 {
		t.Fatal("posted")
	}
	if s, _, _ := loadState(e.state, ih); !s.Done {
		t.Fatal("state not marked done")
	}
	if l := onlyLine(t, e.run(e.opts())); l.Status != StatusSkipped || l.Reason != "Уже в Antresol" {
		t.Fatalf("third run %+v", l)
	}
	back := &Report{ID: NewReportID(time.Now())}
	if err := RollbackQB(context.Background(), e.opts(), e.eng, rep, back); err != nil {
		t.Fatal(err)
	}
	if bl := onlyLine(t, back); bl.Status != StatusImported || bl.Reason != "Возвращена в qBittorrent" {
		t.Fatalf("rollback %+v", bl)
	}
	notInVault(t, e, ih)
	if s, _ := e.qb.stateOf(ih); s != "uploading" {
		t.Fatalf("qBittorrent state %q", s)
	}
}

// a seed an admin paused in Vault is not a move to finish.
func TestQBImportPausedByAdminSkipped(t *testing.T) {
	e := newEnv(t)
	ih, meta := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	if _, err := e.eng.AddTorrentFile(context.Background(), meta, torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: e.save, Paused: true}); err != nil {
		t.Fatal(err)
	}
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusSkipped || l.Reason != "Уже в Antresol (на паузе)" {
		t.Fatalf("line %+v", l)
	}
	if st := status(t, e, ih); st.Status != torrent.StatusPaused {
		t.Fatalf("resumed: %+v", st)
	}
	if len(e.qb.posts()) != 0 {
		t.Fatal("posted")
	}
}

// a v2-only torrent (BT_backup name is not the v1 infohash).
func TestQBImportV2Refused(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	v2 := strings.Repeat("ab", 20)
	os.Rename(filepath.Join(e.bt, ih+".torrent"), filepath.Join(e.bt, v2+".torrent"))
	os.Rename(filepath.Join(e.bt, ih+".fastresume"), filepath.Join(e.bt, v2+".fastresume"))
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusFailed || l.Reason != "Торрент v2 — перенос не поддерживается" {
		t.Fatalf("line %+v", l)
	}
	if len(e.eng.List()) != 0 {
		t.Fatal("added")
	}
}

// the torrent vanished from qBittorrent during the stop: no start,
// and the reason says so.
func TestQBImportVanishedFromQB(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	e.qb.vanish = true
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusFailed || !strings.Contains(l.Reason, "больше нет") {
		t.Fatalf("line %+v", l)
	}
	posts := e.qb.posts()
	if len(posts) != 1 || posts[0].Path != "/api/v2/torrents/stop" {
		t.Fatalf("posts %+v", posts)
	}
	notInVault(t, e, ih)
}

// an expired session is renewed once.
func TestQBClientRelogsIn(t *testing.T) {
	f := fakeQB(t)
	f.user, f.pass = "admin", "secret"
	c := newQBClient(QBOptions{WebURL: f.URL, Username: "admin", Password: "secret"})
	ctx := context.Background()
	if err := c.login(ctx); err != nil {
		t.Fatal(err)
	}
	f.expire()
	if _, err := c.version(ctx); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Fatalf("logins %d", f.logins)
	}
}

// a move marked in progress whose copy is seeding in Vault
// while qBittorrent still runs it (crash before the stop, copy resumed in
// Vault since) is stopped in qBittorrent before it is called done.
func TestQBImportRecoveryStopsQBFirst(t *testing.T) {
	seeding := func(t *testing.T) (*env, string) {
		e := newEnv(t)
		ih, meta := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
		saveState(e.state, ih, qbState{Prev: PrevRunning, Report: "x"})
		if _, err := e.eng.AddTorrentFile(context.Background(), meta, torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: e.save}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := e.eng.WaitVerified(context.Background(), ih); err != nil {
			t.Fatal(err)
		}
		return e, ih // qBittorrent: uploading
	}
	t.Run("stopped", func(t *testing.T) {
		e, ih := seeding(t)
		l := onlyLine(t, e.run(e.opts()))
		if l.Status != StatusImported || l.Reason != "Перенос завершён после перезапуска" || l.Prev != PrevRunning {
			t.Fatalf("line %+v", l)
		}
		posts := e.qb.posts()
		if len(posts) != 1 || posts[0].Path != "/api/v2/torrents/stop" || posts[0].Hashes != ih {
			t.Fatalf("posts %+v", posts)
		}
		if s, _ := e.qb.stateOf(ih); !qbStoppedStates[s] {
			t.Fatalf("qBittorrent %q", s)
		}
		if st := status(t, e, ih); st.Status != torrent.StatusSeeding {
			t.Fatalf("status %+v", st)
		}
	})
	t.Run("stop fails", func(t *testing.T) {
		e, ih := seeding(t)
		e.qb.stopCode = 500
		l := onlyLine(t, e.run(e.opts()))
		if l.Status != StatusFailed || !strings.HasPrefix(l.Reason, "Не удалось остановить") {
			t.Fatalf("line %+v", l)
		}
		notInVault(t, e, ih)
		if s, _ := e.qb.stateOf(ih); s != "uploading" {
			t.Fatalf("qBittorrent %q", s)
		}
	})
}

// a state file without a known prev fails closed: nothing
// is stopped, started, resumed or recorded as moved.
func TestQBImportInvalidStateFailsClosed(t *testing.T) {
	for _, body := range []string{`{}`, `{"prev":"maybe"}`, `not json`} {
		t.Run(body, func(t *testing.T) {
			e := newEnv(t)
			ih, meta := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
			os.MkdirAll(e.state, 0o755)
			os.WriteFile(statePath(e.state, ih), []byte(body), 0o644)
			if _, err := e.eng.AddTorrentFile(context.Background(), meta, torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: e.save, Paused: true}); err != nil {
				t.Fatal(err)
			}
			l := onlyLine(t, e.run(e.opts()))
			if l.Status != StatusFailed || !strings.HasPrefix(l.Reason, "Файл состояния переноса не читается") {
				t.Fatalf("line %+v", l)
			}
			if posts := e.qb.posts(); len(posts) != 0 {
				t.Fatalf("posts %+v", posts)
			}
			if st := status(t, e, ih); st.Status != torrent.StatusPaused {
				t.Fatalf("status %+v", st)
			}
		})
	}
}

// when the start after a failed stop fails too, the state file stays,
// so the next run still knows the torrent was running in qBittorrent.
func TestQBImportStartFailureKeepsState(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	e.qb.stopCode, e.qb.startCode = 500, 500
	e.qb.mu.Lock()
	e.qb.ignoreStop = true
	e.qb.mu.Unlock()
	if l := onlyLine(t, e.run(e.opts())); l.Status != StatusFailed || !strings.Contains(l.Reason, "вручную") {
		t.Fatalf("line %+v", l)
	}
	s, ok, err := loadState(e.state, ih)
	if err != nil || !ok || s.Prev != PrevRunning || s.Done {
		t.Fatalf("state %+v %v %v", s, ok, err)
	}
	// The stop had landed after all; the user retries.
	e.qb.setState(ih, "stoppedUP")
	e.qb.stopCode, e.qb.startCode, e.qb.ignoreStop = 0, 0, false
	n := len(e.qb.posts())
	l := onlyLine(t, e.run(e.opts()))
	if l.Status != StatusImported || l.Prev != PrevRunning {
		t.Fatalf("retry %+v", l)
	}
	if len(e.qb.posts()) != n+1 {
		t.Fatalf("posts %+v", e.qb.posts()[n:])
	}
}

// seedingInVault adds the torrent to Vault as an interrupted move left it
// (state file prev, verified; paused or seeding).
func seedingInVault(t *testing.T, e *env, ih string, meta []byte, prev string, paused bool) {
	t.Helper()
	if err := saveState(e.state, ih, qbState{Prev: prev, Report: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.eng.AddTorrentFile(context.Background(), meta, torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: e.save, Paused: paused}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.eng.WaitVerified(context.Background(), ih); err != nil {
		t.Fatal(err)
	}
}

type removeFails struct{ *torrent.Engine }

func (removeFails) RemoveIfMode(context.Context, string, string, bool) error { return torrent.ErrBusy }

// the recovery stop is not confirmed and Vault cannot remove
// its seeding copy: qBittorrent is not started, Vault's copy is paused, the
// state file stays.
func TestQBImportRemoveFailsNeverStartsQB(t *testing.T) {
	e := newEnv(t)
	ih, meta := e.addTorrent(map[string][]byte{"S/e.mkv": randBytes(64 << 10)}, nil)
	seedingInVault(t, e, ih, meta, PrevRunning, false)
	e.qb.mu.Lock()
	e.qb.ignoreStop = true
	e.qb.mu.Unlock()
	o := e.opts()
	o.StopTimeout = 500 * time.Millisecond
	l := onlyLine(t, runWith(t, e, removeFails{e.eng}, o))
	if l.Status != StatusFailed || !strings.Contains(l.Reason, "не смог убрать свою копию") || strings.Contains(l.Reason, "возвращена") {
		t.Fatalf("line %+v", l)
	}
	for _, c := range e.qb.posts() {
		if c.Path != "/api/v2/torrents/stop" {
			t.Fatalf("post %+v", c)
		}
	}
	if st := status(t, e, ih); st.Status != torrent.StatusPaused {
		t.Fatalf("vault %+v", st)
	}
	if s, ok, _ := loadState(e.state, ih); !ok || s.Prev != PrevRunning || s.Done {
		t.Fatalf("state %+v %v", s, ok)
	}
}

// a transient API error during the recovery stop (the
// crashed run had already stopped qBittorrent) keeps prev=running, so the
// next run records the move as running and a rollback starts it.
func TestQBImportRecoveryAPIErrorKeepsRunning(t *testing.T) {
	e := newEnv(t)
	ih, meta := e.addTorrent(map[string][]byte{"S/e.mkv": randBytes(64 << 10)}, nil)
	seedingInVault(t, e, ih, meta, PrevRunning, false)
	e.qb.setState(ih, "stoppedUP")
	var fail atomic.Bool
	fail.Store(true)
	u, _ := url.Parse(e.qb.URL)
	rp := httputil.NewSingleHostReverseProxy(u)
	px := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/torrents/info" && fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		rp.ServeHTTP(w, r)
	}))
	defer px.Close()
	o := e.opts()
	o.WebURL = px.URL
	if l := onlyLine(t, e.run(o)); l.Status != StatusFailed {
		t.Fatalf("first run %+v", l)
	}
	if len(e.qb.posts()) != 0 {
		t.Fatal("posted without a working API")
	}
	if s, ok, _ := loadState(e.state, ih); !ok || s.Prev != PrevRunning {
		t.Fatalf("state %+v %v", s, ok)
	}
	fail.Store(false)
	rep := e.run(o)
	if l := onlyLine(t, rep); l.Status != StatusImported || l.Prev != PrevRunning {
		t.Fatalf("next run %+v", l)
	}
	back := &Report{ID: NewReportID(time.Now())}
	if err := RollbackQB(context.Background(), o, e.eng, rep, back); err != nil {
		t.Fatal(err)
	}
	if l := onlyLine(t, back); l.Reason != "Возвращена в qBittorrent" {
		t.Fatalf("rollback %+v", l)
	}
	if s, _ := e.qb.stateOf(ih); s != "uploading" {
		t.Fatalf("qBittorrent %q", s)
	}
}

// A torrent paused in qBittorrent that the user started meanwhile is
// stopped before Vault seeds it and recorded as running (and the same
// on the paused-copy path).
func TestQBImportStartedMeanwhileIsStopped(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("vault paused %v", paused), func(t *testing.T) {
			e := newEnv(t)
			ih, meta := e.addTorrent(map[string][]byte{"S/e.mkv": randBytes(64 << 10)}, nil)
			seedingInVault(t, e, ih, meta, PrevStopped, paused) // qBittorrent: uploading
			l := onlyLine(t, e.run(e.opts()))
			if l.Status != StatusImported || l.Prev != PrevRunning {
				t.Fatalf("line %+v", l)
			}
			posts := e.qb.posts()
			if len(posts) != 1 || posts[0].Path != "/api/v2/torrents/stop" {
				t.Fatalf("posts %+v", posts)
			}
			if s, _ := e.qb.stateOf(ih); !qbStoppedStates[s] {
				t.Fatalf("qBittorrent %q", s)
			}
			if s, _, _ := loadState(e.state, ih); s.Prev != PrevRunning || !s.Done {
				t.Fatalf("state %+v", s)
			}
		})
	}
}

// A failed move gives a torrent back to qBittorrent like a rollback does:
// while KeepPaused holds (boot: no-seeding) — or the caller set none — the
// torrent qBittorrent stopped stays stopped there, whatever failed after
// the stop; its state file stays, so the next run knows it was running.
func TestQBImportGiveBackKeepsPaused(t *testing.T) {
	on := func(context.Context) bool { return true }
	off := func(context.Context) bool { return false }
	cases := []struct {
		name   string
		keep   func(context.Context) bool
		resume bool // Resume fails (else: qBittorrent never confirms the stop)
		start  bool
	}{
		{"stop timeout, keep paused", on, false, false},
		{"stop timeout, nil keeps paused", nil, false, false},
		{"stop timeout, sharing allowed", off, false, true},
		{"resume fails, keep paused", on, true, false},
		{"resume fails, nil keeps paused", nil, true, false},
		{"resume fails, sharing allowed", off, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
			o := e.opts()
			o.KeepPaused = c.keep
			var eng TorrentEngine = e.eng
			if c.resume {
				eng = resumeFails{e.eng}
			} else {
				e.qb.ignoreStop = true
				o.StopTimeout = time.Second
			}
			l := onlyLine(t, runWith(t, e, eng, o))
			if l.Status != StatusFailed {
				t.Fatalf("line %+v", l)
			}
			posts := e.qb.posts()
			if c.start {
				if len(posts) != 2 || posts[1].Path != "/api/v2/torrents/start" || !strings.Contains(l.Reason, "возвращена в qBittorrent") {
					t.Fatalf("line %+v posts %+v", l, posts)
				}
				if _, ok, _ := loadState(e.state, ih); ok {
					t.Fatal("state file left after the start")
				}
			} else {
				if len(posts) != 1 || posts[0].Path != "/api/v2/torrents/stop" {
					t.Fatalf("qBittorrent was sent %+v", posts)
				}
				// the reason says why: no-seeding on, or no rule given
				want := " — раздача осталась в qBittorrent на паузе: включён запрет раздачи"
				if c.keep == nil {
					want = " — раздача осталась в qBittorrent на паузе: запустите её там вручную, если нужно"
				}
				if !strings.HasSuffix(l.Reason, want) {
					t.Fatalf("line %+v, want the suffix %q", l, want)
				}
				if s, ok, err := loadState(e.state, ih); err != nil || !ok || s.Prev != PrevRunning || s.Done {
					t.Fatalf("state %+v %v %v", s, ok, err)
				}
			}
			notInVault(t, e, ih)
		})
	}
}
