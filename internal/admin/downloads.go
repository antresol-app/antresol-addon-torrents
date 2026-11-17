package admin

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol/addonkit"
)

// downloads: «Загрузки» (the torrents list and its actions) and the
// engine's connectivity («Входящие подключения»).
type downloads struct{ Deps }

// acquireErr writes a service error: its Russian message in the request's
// language, with the old build's status codes.
func acquireErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		return
	}
	msg, ok := acquire.UserMessage(err)
	if !ok {
		internalErr(w, r, what, err)
		return
	}
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, acquire.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, acquire.ErrNotFound), errors.Is(err, acquire.ErrNoRelease):
		code = http.StatusNotFound
	case errors.Is(err, acquire.ErrUnavailable):
		code = http.StatusServiceUnavailable
	case errors.Is(err, acquire.ErrConflict), errors.Is(err, acquire.ErrInProgress):
		code = http.StatusConflict
	case errors.Is(err, acquire.ErrNoFiles):
		code = http.StatusUnprocessableEntity
	case errors.Is(err, acquire.ErrSource):
		code = http.StatusBadGateway
	}
	if code >= 500 {
		log.Printf("admin: %s: %v", what, err)
	}
	addonkit.WriteError(w, code, i18n.Message(r.Context(), msgErr{msg, err}))
}

// msgErr is a service's Russian message with the error it came with
// (i18n.Message renders an *i18n.Error in the chain, else localizes msg).
type msgErr struct {
	msg string
	err error
}

func (e msgErr) Error() string { return e.msg }
func (e msgErr) Unwrap() error { return e.err }

// GET /api/admin/torrents
func (d *downloads) list(w http.ResponseWriter, r *http.Request) {
	vs := acquire.LocalizeTorrents(r.Context(), d.Acquire.Torrents())
	if vs == nil {
		vs = []acquire.TorrentView{}
	}
	d.markWatched(r.Context(), userID(r), vs)
	addonkit.WriteJSON(w, http.StatusOK, vs)
}

// markWatched sets Watched of the movie and series torrents for the
// profile; without Deps.Watched, or on a failed lookup, it stays unset
// (only a filter hint).
func (d *downloads) markWatched(ctx context.Context, uid int64, vs []acquire.TorrentView) {
	if d.Watched == nil {
		return
	}
	ids := d.Acquire.VideoItems(ctx)
	var items, series []int64
	for i := range vs {
		if id, ok := ids[vs[i].InfoHash]; ok {
			if vs[i].Kind == acquire.KindSeries {
				series = append(series, id)
			} else {
				items = append(items, id)
			}
		}
	}
	if len(items)+len(series) == 0 {
		return
	}
	watched, err := d.Watched(ctx, uid, items, series)
	if err != nil {
		log.Printf("admin: torrents watched: %v", err)
		return
	}
	for i := range vs {
		if id, ok := ids[vs[i].InfoHash]; ok {
			w := watched[id]
			vs[i].Watched = &w
		}
	}
}

// POST /api/admin/torrents/{ih}/{pause|resume|organize|organize-force|dismiss-note|promote}
func (d *downloads) action(w http.ResponseWriter, r *http.Request) {
	ih := r.PathValue("ih")
	var f func(context.Context, string) error
	switch r.PathValue("action") {
	case "pause":
		f = d.Acquire.Pause
	case "resume":
		f = d.Acquire.Resume
	case "organize":
		f = d.Acquire.RetryOrganize
	case "organize-force":
		f = d.Acquire.ForceOrganize
	case "dismiss-note":
		f = d.Acquire.DismissAutoNote
	case "promote":
		d.promote(w, r, ih)
		return
	default:
		http.NotFound(w, r)
		return
	}
	if err := f(r.Context(), ih); err != nil {
		acquireErr(w, r, "torrent action", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// promote: {"force": bool} (optional body) → {"status": …}.
func (d *downloads) promote(w http.ResponseWriter, r *http.Request, ih string) {
	var in struct {
		Force bool `json:"force"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	st, err := d.Acquire.Promote(r.Context(), ih, in.Force)
	if err != nil {
		acquireErr(w, r, "promote", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]string{"status": st})
}

// DELETE /api/admin/torrents/{ih}?data=1 deletes the data too (the old
// build's deleteData=true is still read); otherwise the data stays.
func (d *downloads) remove(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	del := q.Get("data") == "1" || q.Get("deleteData") == "true"
	if err := d.Acquire.Remove(r.Context(), r.PathValue("ih"), del); err != nil {
		acquireErr(w, r, "remove torrent", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /api/admin/torrents/{ih}/status?file=N
func (d *downloads) status(w http.ResponseWriter, r *http.Request) {
	file := -1
	if v := r.URL.Query().Get("file"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			file = n
		}
	}
	st, err := d.Acquire.TorrentStatus(r.PathValue("ih"), file)
	if err != nil {
		acquireErr(w, r, "torrent status", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, st)
}

// GET /api/admin/engine → torrent.Connectivity; 204 when the engine cannot
// tell.
func (d *downloads) engine(w http.ResponseWriter, r *http.Request) {
	c, ok := d.Acquire.Connectivity()
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, c)
}
