// Package watch is «Смотреть сейчас» through the core's inputs: POST
// /api/watch/{ih}/{idx} registers the torrent file as an input of the
// asking profile and answers its id; the core's player then reads it from
// the addon's socket (/media/…, media.go) with the release's audio and
// subtitle files beside it as sidecars (sidecars.go).
package watch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// Acquire is what watching needs of acquire.Service.
type Acquire interface {
	OpenStream(ctx context.Context, ih string, idx int) (torrent.FileInfo, func(context.Context) (io.ReadSeekCloser, error), func(), error)
	StreamFiles(ih string) []torrent.FileInfo
	TorrentStatus(ih string, file int) (acquire.StreamStatus, error)
	WatchTitle(ctx context.Context, ih, file string) (kind string, tmdbID, season, episode int, ok bool)
}

// Core is the core's input registry (addonkit.Client).
type Core interface {
	RegisterInput(ctx context.Context, in addonkit.InputRequest) (addonkit.InputCreated, error)
	DropInput(ctx context.Context, id string) error
}

// Deps are the parts the routes use. Library finds the library file a
// torrent file already is (FileByPath, RefFiles).
type Deps struct {
	Acquire Acquire
	Library coreadapt.Core
	Inputs  *Inputs
}

// Register adds the profiles' watch routes to mux (the /api/ routes
// served once the addon is ready): POST /api/watch/{ih}/{idx} and GET
// /api/torrents/{ih}/status. Kids are refused (the core keeps them from
// the proxy already).
func Register(mux *http.ServeMux, d Deps) {
	h := &handlers{d}
	mux.Handle("POST /api/watch/{ih}/{idx}", adults(http.HandlerFunc(h.watch)))
	mux.Handle("GET /api/torrents/{ih}/status", adults(http.HandlerFunc(h.status)))
}

type handlers struct{ Deps }

// adults lets signed-in adult profiles through, in their language.
func adults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := addonkit.UserFrom(r)
		if !ok || u.IsKid {
			writeErr(w, r, http.StatusForbidden, "Недоступно в детском профиле")
			return
		}
		if u.Lang != "" {
			r = r.WithContext(i18n.WithLanguage(r.Context(), u.Lang))
		}
		next.ServeHTTP(w, r)
	})
}

// validIH: ih is an infohash (40 hex characters, lowered).
func validIH(ih string) bool { return len(ih) == 40 && strings.Trim(ih, "0123456789abcdef") == "" }

// params are the path's {ih} (validIH, lowered) and {idx}.
func params(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	ih := strings.ToLower(r.PathValue("ih"))
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if !validIH(ih) || err != nil || idx < 0 {
		writeErr(w, r, http.StatusNotFound, "Файл не найден")
		return "", 0, false
	}
	return ih, idx, true
}

// watch registers file idx of torrent ih as an input of the profile and
// answers {inputId}; the profile's earlier input of the same file is
// dropped (one live input per profile and file).
func (h *handlers) watch(w http.ResponseWriter, r *http.Request) {
	ih, idx, ok := params(w, r)
	if !ok {
		return
	}
	u, _ := addonkit.UserFrom(r)
	id, err := h.Deps.Watch(r.Context(), *u, ih, idx)
	if err != nil {
		WriteError(w, r, "watch", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]string{"inputId": id})
}

// errInput: the core did not take the input (refused or unreachable).
type errInput struct {
	err error
	ih  string
	idx int
}

func (e errInput) Error() string { return "watch: register input: " + e.err.Error() }
func (e errInput) Unwrap() error { return e.err }

// Watch registers file idx of torrent ih (lowered) as an input of u with
// the core and returns its id: POST /api/watch/{ih}/{idx}, and «Смотреть»
// of a pinned release (followapi) once its stream is started. Its
// errors are for WriteError.
func (d Deps) Watch(ctx context.Context, u addonkit.User, ih string, idx int) (string, error) {
	h := &handlers{d}
	video, _, release, err := d.Acquire.OpenStream(ctx, ih, idx)
	if err != nil {
		return "", err
	}
	release() // only checks the file is there; the reads mark their own use
	files := d.Acquire.StreamFiles(ih)
	req := addonkit.InputRequest{UserID: u.ID, Path: mediaPath(ih, idx), Name: video.Path, Size: video.Size,
		Sidecars: sidecarsOf(ih, idx, video, files), Slow: true}
	if kind, tmdb, season, episode, ok := d.Acquire.WatchTitle(ctx, ih, video.Path); ok {
		req.Title = &addonkit.InputTitle{Kind: kind, TMDBID: int64(tmdb), Season: season, Episode: episode}
	}
	req.ItemID = h.itemOf(ctx, ih, video)
	mode := ""
	if st, err := d.Acquire.TorrentStatus(ih, -1); err == nil {
		mode = st.Mode
	}
	req.Meta, _ = json.Marshal(map[string]any{"ih": ih, "idx": idx, "mode": mode})
	id, err := d.Inputs.Register(ctx, key{u.ID, ih, idx}, req)
	if err != nil && !errors.Is(err, errRemoved) {
		return "", errInput{err, ih, idx}
	}
	return id, err
}

// WriteError answers an error of Watch: the stream's (acquire's, with the
// status of its kind), 404 «Раздача не найдена» for a download removed
// meanwhile, 502 «Не удалось открыть видео» when the core did not take
// the input.
func WriteError(w http.ResponseWriter, r *http.Request, what string, err error) {
	var in errInput
	switch {
	case errors.Is(err, errRemoved):
		writeErr(w, r, http.StatusNotFound, "Раздача не найдена")
	case errors.As(err, &in):
		if r.Context().Err() == nil {
			slog.Warn(what+": register input", "infohash", in.ih, "idx", in.idx, "err", in.err)
			writeErr(w, r, http.StatusBadGateway, "Не удалось открыть видео")
		}
	default:
		acquireErr(w, r, what, err)
	}
}

// itemOf is the library item the torrent file already is (0: none): the
// library's file at its own path, else a library file of the download
// (its ref, the infohash) that is the same file on disk (a hardlink).
func (h *handlers) itemOf(ctx context.Context, ih string, video torrent.FileInfo) int64 {
	if h.Library == nil || video.FullPath == "" {
		return 0
	}
	if f, ok, err := h.Library.FileByPath(ctx, video.FullPath); err == nil && ok {
		return f.ItemID
	}
	refs, err := h.Library.RefFiles(ctx, ih)
	if err != nil || len(refs) == 0 {
		return 0
	}
	vst, err := statFile(video.FullPath)
	if err != nil {
		return 0
	}
	for _, rf := range refs {
		if st, err := statFile(rf.Path); err == nil && sameFile(vst, st) {
			return rf.ItemID
		}
	}
	return 0
}

// key is one profile's input of one torrent file.
type key struct {
	user int64
	ih   string
	idx  int
}

// inputLife is how long the core keeps an input (addons.InputTTL, 6 h):
// an entry older than that names an input that is gone.
const inputLife = 6 * time.Hour

// Inputs keeps the input id of each (profile, torrent, file) the core has
// from this addon, so a new one replaces it and a removed download drops
// its own.
type Inputs struct {
	Core Core
	// DropTimeout bounds dropping an input at the core (0: 10 s): the
	// replaced one, or a removed download's.
	DropTimeout time.Duration
	// now is the clock (tests); nil: time.Now.
	now func() time.Time

	mu   sync.Mutex
	byID map[key]entry
	// drops counts each download's Drop calls: a register that saw another
	// count before its core call raced a remove
	drops map[string]uint64
}

// errRemoved: the download went while its input was being registered.
var errRemoved = errors.New("watch: the download was removed")

type entry struct {
	id string
	at time.Time
}

func (in *Inputs) clock() time.Time {
	if in.now != nil {
		return in.now()
	}
	return time.Now()
}

func (in *Inputs) dropTimeout() time.Duration {
	if in.DropTimeout > 0 {
		return in.DropTimeout
	}
	return 10 * time.Second
}

// Register registers req for k and drops the input k had before (the
// core may have forgotten it, after 6 h or a restart; the new one always
// plays). The drop runs in the background and does not depend on the
// request (the answer never waits on it; a client gone right after still
// gets its old input dropped). A download removed during the core call
// gets the new input dropped too (errRemoved). Entries past inputLife go.
func (in *Inputs) Register(ctx context.Context, k key, req addonkit.InputRequest) (string, error) {
	in.mu.Lock()
	gen := in.drops[k.ih]
	in.mu.Unlock()
	c, err := in.Core.RegisterInput(ctx, req)
	if err != nil {
		return "", err
	}
	now := in.clock()
	in.mu.Lock()
	if in.drops[k.ih] != gen {
		in.mu.Unlock()
		in.dropLater("watch: drop the input of a download removed meanwhile", c.ID)
		return "", errRemoved
	}
	if in.byID == nil {
		in.byID = map[key]entry{}
	}
	for kk, e := range in.byID {
		if now.Sub(e.at) > inputLife {
			delete(in.byID, kk)
		}
	}
	old := in.byID[k].id
	in.byID[k] = entry{c.ID, now}
	in.mu.Unlock()
	if old != "" && old != c.ID {
		in.dropLater("watch: drop the replaced input", old)
	}
	return c.ID, nil
}

// dropLater drops ids at the core in the background (within dropTimeout).
func (in *Inputs) dropLater(warn string, ids ...string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), in.dropTimeout())
		defer cancel()
		for _, id := range ids {
			if err := in.Core.DropInput(ctx, id); err != nil {
				slog.Warn(warn, "id", id, "err", err)
			}
		}
	}()
}

// size is the number of inputs kept (tests).
func (in *Inputs) size() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.byID)
}

// Drop forgets the inputs of download ih and drops them at the core, in
// the background (the removal never waits on the core).
func (in *Inputs) Drop(ih string) {
	ih = strings.ToLower(ih)
	in.mu.Lock()
	if in.drops == nil {
		in.drops = map[string]uint64{}
	}
	in.drops[ih]++
	var ids []string
	for k, e := range in.byID {
		if k.ih == ih {
			ids = append(ids, e.id)
			delete(in.byID, k)
		}
	}
	in.mu.Unlock()
	if len(ids) > 0 {
		in.dropLater("watch: drop the input of a removed download", ids...)
	}
}

// writeErr writes {"error": msg} (Russian) in the request's language.
func writeErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	addonkit.WriteError(w, code, i18n.Localize(r.Context(), msg))
}

// acquireErr answers a service error: its message with the status of its
// kind (not found 404, unavailable 503, no files 422), anything else 500.
func acquireErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		return
	}
	msg, ok := acquire.UserMessage(err)
	if !ok && errors.Is(err, acquire.ErrNotFound) {
		msg, ok = "Файл не найден", true
	}
	if !ok {
		slog.Warn("watch: "+what, "err", err)
		writeErr(w, r, http.StatusInternalServerError, "Внутренняя ошибка сервера")
		return
	}
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, acquire.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, acquire.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, acquire.ErrUnavailable):
		code = http.StatusServiceUnavailable
	case errors.Is(err, acquire.ErrNoFiles):
		code = http.StatusUnprocessableEntity
	}
	if code >= 500 {
		slog.Warn("watch: "+what, "err", err)
	}
	writeErr(w, r, code, msg)
}
