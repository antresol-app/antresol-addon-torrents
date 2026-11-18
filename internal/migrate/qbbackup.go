package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// qbBackup is where the importer reads qBittorrent's torrents from: its
// BT_backup folder (fileBackup) or, when that is not mounted, its Web API
// (apiBackup) — the same .torrent files and the same state. A method's
// reason is the failed line's text ("" = read); err is for the log.
type qbBackup interface {
	// list: the torrents' ids (BT_backup's file stems, qBittorrent's
	// hashes), sorted.
	list(ctx context.Context) (ids []string, reason string, err error)
	// torrent: the .torrent file of id.
	torrent(ctx context.Context, id string) (meta []byte, reason string, err error)
	// resume: what qBittorrent keeps of id (its .fastresume).
	resume(ctx context.Context, id string) (fr fastresume, reason string, err error)
}

func newQBBackup(o QBOptions, qb *qbClient) qbBackup {
	if o.FromAPI {
		return &apiBackup{qb: qb}
	}
	return fileBackup{dir: o.BTBackupDir}
}

// fileBackup is a BT_backup folder (read only).
type fileBackup struct{ dir string }

func (f fileBackup) list(context.Context) ([]string, string, error) {
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, fmt.Sprintf("Не удалось прочитать папку BT_backup (%s) — раздачи не перенесены", f.dir), err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".torrent") {
			ids = append(ids, strings.TrimSuffix(e.Name(), ".torrent"))
		}
	}
	sort.Strings(ids)
	return ids, "", nil
}

func (f fileBackup) torrent(_ context.Context, id string) ([]byte, string, error) {
	b, err := os.ReadFile(filepath.Join(f.dir, id+".torrent"))
	if err != nil {
		return nil, "Не удалось прочитать файл торрента", err
	}
	return b, "", nil
}

func (f fileBackup) resume(_ context.Context, id string) (fastresume, string, error) {
	fr, err := readFastresume(filepath.Join(f.dir, id+".fastresume"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fr, "Нет файла состояния qBittorrent", err
	case err != nil:
		return fr, "Файл состояния qBittorrent не читается", err
	}
	return fr, "", nil
}

// apiBackup reads qBittorrent's Web API: /torrents/info, /torrents/export
// (qBittorrent 4.5 and newer), /torrents/files and /torrents/pieceStates.
type apiBackup struct{ qb *qbClient }

// reasonNoExport: the qBittorrent is too old to give its .torrent files.
const reasonNoExport = "qBittorrent не отдаёт файлы .torrent по API (нужен qBittorrent 4.5 или новее) — обновите его или смонтируйте папку BT_backup"

// qbTorrentBody bounds a .torrent file or a piece list.
const qbTorrentBody = 64 << 20

type qbInfo struct {
	Hash     string `json:"hash"`
	Name     string `json:"name"`
	SavePath string `json:"save_path"`
	State    string `json:"state"`
}

func (a *apiBackup) infos(ctx context.Context, hash string) ([]qbInfo, error) {
	const path = "/api/v2/torrents/info"
	var q url.Values
	if hash != "" {
		q = url.Values{"hashes": {hash}}
	}
	code, b, _, err := a.qb.doN(ctx, http.MethodGet, path, q, nil, qbTorrentBody)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr(path, code)
	}
	var list []qbInfo
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("qBittorrent: непонятный ответ на %s: %w", path, err)
	}
	return list, nil
}

func (a *apiBackup) list(ctx context.Context) ([]string, string, error) {
	list, err := a.infos(ctx, "")
	if err != nil {
		return nil, fmt.Sprintf("Не удалось получить список раздач из qBittorrent (%s) — раздачи не перенесены", err), err
	}
	ids := make([]string, 0, len(list))
	for _, t := range list {
		if t.Hash != "" {
			ids = append(ids, strings.ToLower(t.Hash))
		}
	}
	sort.Strings(ids)
	return ids, "", nil
}

// errNoExport: /torrents/export is not there (qBittorrent older than 4.5).
var errNoExport = errors.New("qBittorrent: no /api/v2/torrents/export")

func (a *apiBackup) export(ctx context.Context, id string) ([]byte, error) {
	const path = "/api/v2/torrents/export"
	code, b, _, err := a.qb.doN(ctx, http.MethodGet, path, url.Values{"hash": {id}}, nil, qbTorrentBody)
	switch {
	case err != nil:
		return nil, err
	case code == http.StatusNotFound || code == http.StatusMethodNotAllowed:
		return nil, errNoExport
	case code != http.StatusOK:
		return nil, statusErr(path, code)
	case len(b) == 0:
		return nil, fmt.Errorf("qBittorrent: пустой ответ на %s", path)
	}
	return b, nil
}

func (a *apiBackup) torrent(ctx context.Context, id string) ([]byte, string, error) {
	b, err := a.export(ctx, id)
	switch {
	case errors.Is(err, errNoExport):
		return nil, reasonNoExport, err
	case err != nil:
		return nil, fmt.Sprintf("Не удалось получить файл торрента из qBittorrent: %s", err), err
	}
	return b, "", nil
}

func (a *apiBackup) resume(ctx context.Context, id string) (fastresume, string, error) {
	fail := func(err error) (fastresume, string, error) {
		return fastresume{}, fmt.Sprintf("Не удалось получить состояние раздачи из qBittorrent: %s", err), err
	}
	list, err := a.infos(ctx, id)
	if err != nil {
		return fail(err)
	}
	var info *qbInfo
	for i := range list {
		if strings.EqualFold(list[i].Hash, id) {
			info = &list[i]
		}
	}
	if info == nil {
		return fastresume{}, "Раздачи больше нет в qBittorrent", os.ErrNotExist
	}
	fr := fastresume{savePath: info.SavePath, name: info.Name, paused: qbStoppedStates[info.State]}

	// the files where qBittorrent has them (renamed ones under their new name)
	const filesPath = "/api/v2/torrents/files"
	code, b, _, err := a.qb.doN(ctx, http.MethodGet, filesPath, url.Values{"hash": {id}}, nil, qbTorrentBody)
	if err == nil && code != http.StatusOK {
		err = statusErr(filesPath, code)
	}
	if err != nil {
		return fail(err)
	}
	var files []struct {
		Index *int   `json:"index"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(b, &files); err != nil {
		return fail(fmt.Errorf("qBittorrent: непонятный ответ на %s: %w", filesPath, err))
	}
	fr.mapped = make([]string, len(files))
	for i, f := range files {
		at := i
		if f.Index != nil {
			at = *f.Index
		}
		if at < 0 || at >= len(files) {
			return fail(fmt.Errorf("qBittorrent: файл с номером %d из %d", at, len(files)))
		}
		fr.mapped[at] = f.Name
	}

	// the pieces it has: 2 = downloaded (a fastresume's bit 0)
	const piecesPath = "/api/v2/torrents/pieceStates"
	code, b, _, err = a.qb.doN(ctx, http.MethodGet, piecesPath, url.Values{"hash": {id}}, nil, qbTorrentBody)
	if err == nil && code != http.StatusOK {
		err = statusErr(piecesPath, code)
	}
	if err != nil {
		return fail(err)
	}
	var states []int
	if err := json.Unmarshal(b, &states); err != nil {
		return fail(fmt.Errorf("qBittorrent: непонятный ответ на %s: %w", piecesPath, err))
	}
	fr.pieces = make([]byte, len(states))
	for i, st := range states {
		if st == 2 {
			fr.pieces[i] = 1
		}
	}
	return fr, "", nil
}

// qbAPICount checks a qBittorrent connection for «Проверить»: it logs in,
// asks for the version and the torrents, and that it gives the .torrent
// files (the move needs them). The count is of its torrents; the error is
// a *RemoteError to show, or ctx's.
func qbAPICount(ctx context.Context, o QBOptions) (int, error) {
	base := strings.TrimRight(strings.TrimSpace(o.WebURL), "/")
	if base == "" {
		return 0, remoteErr(nil, "Укажите адрес qBittorrent")
	}
	if err := checkRemoteURL("qBittorrent", base); err != nil {
		return 0, err
	}
	qb := newQBClient(o)
	wrap := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, errCrossHostRedirect):
			return remoteErr(err, "qBittorrent перенаправляет на другой адрес — укажите адрес, который открывается без перенаправления")
		case errors.Is(err, errQBTooLarge):
			return remoteErr(err, "Ответ qBittorrent слишком большой")
		case errors.Is(err, errQBLoginBlocked):
			return remoteErr(err, "qBittorrent отказал во входе — возможно, адрес временно заблокирован после неудачных попыток; подождите и не пробуйте снова сразу")
		case errors.Is(err, errQBLogin):
			return remoteErr(err, "qBittorrent не принял логин или пароль")
		case errors.Is(err, errNoExport):
			return remoteErr(err, reasonNoExport)
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			return remoteErr(err, "qBittorrent не отвечает по адресу %s", redactURL(base))
		}
		var se *qbStatusError
		if errors.As(err, &se) && se.code == http.StatusForbidden {
			return remoteErr(err, "qBittorrent не пустил: укажите логин и пароль его веб-интерфейса")
		}
		return remoteErr(err, "%s", err.Error())
	}
	if err := connectQB(ctx, o, qb); err != nil {
		return 0, wrap(err)
	}
	a := &apiBackup{qb: qb}
	ids, _, err := a.list(ctx)
	if err != nil {
		return 0, wrap(err)
	}
	if len(ids) > 0 {
		if _, err := a.export(ctx, ids[0]); err != nil {
			return 0, wrap(err)
		}
	}
	return len(ids), nil
}
