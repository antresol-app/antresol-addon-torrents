//go:build ignore

package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/antresol-app/antresol/internal/i18n"
	"github.com/antresol-app/antresol/internal/musicimport"
	"github.com/antresol-app/antresol/internal/playlistimport"
	"github.com/go-chi/chi/v5"
)

// playlistFull is the full build's part of «Импорт медиатеки»: the
// services' sign-ins (musicimport.OAuth) and the downloads.
type playlistFull struct{ oauth *musicimport.OAuth }

func newPlaylistFull(deps Deps) playlistFull { return playlistFull{oauth: deps.ImportOAuth} }

// reader is the background reader of an upload's videos; a nil interface
// without sign-ins (never a typed nil).
func (f playlistFull) reader() backgroundReader {
	if f.oauth == nil {
		return nil
	}
	return f.oauth
}

// err answers the sign-in failures (false: not one of them).
func (f playlistFull) err(w http.ResponseWriter, r *http.Request, err error) bool {
	if !errors.Is(err, musicimport.ErrNotConfigured) && !errors.Is(err, musicimport.ErrConnect) && !errors.Is(err, musicimport.ErrLinkExpired) {
		return false
	}
	e, _ := musicimport.KnownError(err)
	writeErr(w, r, http.StatusBadRequest, e.Error())
	return true
}

// catalogErr answers a failed search of the catalog.
func (playlistFull) catalogErr(w http.ResponseWriter, r *http.Request, err error) {
	acquireErr(w, r, "import search", err)
}

func (h *importAPI) mountFull(r chi.Router) {
	r.Post("/api/import/groups/{id}/retry", h.retry)
	r.Post("/api/import/connect/{service}", h.connect)
	r.Post("/api/import/connect/{service}/poll", h.poll)
	r.Post("/api/import/connect/{service}/token", h.token)
	r.Get("/api/import/oauth/{service}/callback", h.callback)
	r.Post("/api/import/oauth/{service}/paste", h.paste)
	r.Post("/api/import/deezer/public", h.deezerPublic)
}

// POST /api/import/groups/{id}/retry: «Повторить» of an album not found,
// failed, waiting for space or waiting for another download (its own
// search then); ?format=any is «Скачать в другом формате» of an album
// with no release of «Формат загрузки».
func (h *importAPI) retry(w http.ResponseWriter, r *http.Request) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return
	}
	g, err := h.svc.Store.Group(r.Context(), id)
	if err == nil {
		var j playlistimport.Job
		if j, err = h.svc.Store.Job(r.Context(), g.JobID); err == nil && j.UserID != userOf(r).ID {
			err = playlistimport.ErrNotFound
		}
	}
	if err == nil {
		if r.URL.Query().Get("format") == "any" {
			err = h.svc.RetryAnyFormat(r.Context(), id)
		} else {
			err = h.svc.Retry(r.Context(), id)
		}
	}
	if err != nil {
		h.err(w, r, "import retry", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/import/connect/{service}: «Подключить» → the service's
// sign-in page and how the browser comes back (redirect or paste), or for
// a device sign-in (Yandex) the code to enter and where (DeviceDTO).
func (h *importAPI) connect(w http.ResponseWriter, r *http.Request) {
	service := chi.URLParam(r, "service")
	if h.full.oauth == nil {
		if !slices.Contains(playlistimport.Services, service) {
			writeErr(w, r, http.StatusBadRequest, playlistimport.ErrUnknownService.Error())
		} else {
			writeErr(w, r, http.StatusBadRequest, musicimport.ErrNotConfigured.Error())
		}
		return
	}
	// an open job of the service would refuse what the sign-in reads: say
	// so before the admin signs in
	if !h.noOpenJob(w, r, service) {
		return
	}
	if h.full.oauth.IsDevice(service) {
		d, err := h.full.oauth.BeginDevice(r.Context(), userOf(r).ID, service)
		if err != nil {
			h.err(w, r, "import connect", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, d)
		return
	}
	b, err := h.full.oauth.Begin(userOf(r).ID, service)
	if err != nil {
		h.err(w, r, "import connect", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, b)
}

// GET /api/import/oauth/{service}/callback?code&state (or ?error&state):
// the service's sign-in sends the browser back here. Always a redirect to
// «Импорт» on this server: ?job=<id> (the job reading the library) or
// ?error=<text>.
func (h *importAPI) callback(w http.ResponseWriter, r *http.Request) {
	// the address carries a one-time code: not cached, not passed on
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	back := func(q url.Values) { http.Redirect(w, r, "/manage/import?"+q.Encode(), http.StatusFound) }
	fail := func(err error) { // a known, Russian text
		e, _ := musicimport.KnownError(err)
		back(url.Values{"error": {i18n.Message(r.Context(), e)}})
	}
	o := h.full.oauth
	if o == nil {
		fail(musicimport.ErrNotConfigured)
		return
	}
	q := r.URL.Query()
	uid := userOf(r).ID
	service := chi.URLParam(r, "service")
	if musicimport.Refused(q) {
		fail(o.DeniedFor(uid, service, q.Get("state")))
		return
	}
	id, err := o.FinishFor(r.Context(), uid, service, q.Get("state"), q.Get("code"))
	if err != nil {
		if _, known := musicimport.KnownError(err); !known && r.Context().Err() == nil {
			log.Printf("api: import sign-in: %v", err) // a vault or database failure (never a token)
		}
		fail(err)
		return
	}
	back(url.Values{"job": {strconv.FormatInt(id, 10)}})
}

// POST /api/import/oauth/{service}/paste {"url"}: the address the browser
// showed after the sign-in (paste mode) → 201 the job reading the library.
func (h *importAPI) paste(w http.ResponseWriter, r *http.Request) {
	o := h.full.oauth
	if o == nil || !o.Configured(chi.URLParam(r, "service")) {
		writeErr(w, r, http.StatusBadRequest, musicimport.ErrNotConfigured.Error())
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &req) {
		return
	}
	id, err := o.FinishPasted(r.Context(), userOf(r).ID, req.URL)
	if err != nil {
		h.err(w, r, "import sign-in", err)
		return
	}
	h.writeJob(w, r, http.StatusCreated, id)
}

// POST /api/import/connect/{service}/poll {"pollId"}: has the listener
// entered the device code? 202 {"pending":true} (ask again after the
// interval), 201 the job reading the library (the sign-in is used up),
// 400 «Ссылка устарела. Подключитесь заново.», or 409 while a job of the
// service is open.
func (h *importAPI) poll(w http.ResponseWriter, r *http.Request) {
	o := h.full.oauth
	service := chi.URLParam(r, "service")
	if !o.IsDevice(service) {
		writeErr(w, r, http.StatusBadRequest, musicimport.ErrNotConfigured.Error())
		return
	}
	var req struct {
		PollID string `json:"pollId"`
	}
	if !decode(w, r, &req) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	// a job of the service opened meanwhile (a pasted token in another
	// tab) would refuse what the sign-in reads
	if !h.noOpenJob(w, r, service) {
		return
	}
	id, err := o.Poll(r.Context(), userOf(r).ID, service, req.PollID)
	if errors.Is(err, musicimport.ErrPending) {
		writeJSON(w, http.StatusAccepted, map[string]bool{"pending": true})
		return
	}
	if err != nil {
		h.err(w, r, "import sign-in", err)
		return
	}
	h.writeJob(w, r, http.StatusCreated, id)
}

// POST /api/import/connect/{service}/token {"token"}: «Вставить токен» —
// a token the listener already has → 201 the job reading the library
// (refused by the service: the job fails with the text). The token is
// never logged.
func (h *importAPI) token(w http.ResponseWriter, r *http.Request) {
	o := h.full.oauth
	service := chi.URLParam(r, "service")
	if !o.IsDevice(service) {
		writeErr(w, r, http.StatusBadRequest, musicimport.ErrNotConfigured.Error())
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !h.noOpenJob(w, r, service) {
		return
	}
	id, err := o.FinishToken(r.Context(), userOf(r).ID, service, req.Token)
	if err != nil {
		h.err(w, r, "import sign-in", err)
		return
	}
	h.writeJob(w, r, http.StatusCreated, id)
}

// publicCheckTimeout bounds the link check the admin waits for (a short
// link's hops, then the playlist itself): past it, «try again».
var publicCheckTimeout = 15 * time.Second

// POST /api/import/deezer/public {"url"}: «Публичный плейлист по ссылке» —
// a public Deezer playlist (www.deezer.com/<lang>/playlist/<id>, or a
// link.deezer.com short link) with no sign-in → 201 the job reading its
// songs; 400 «Не найдено» for a link that is not one (or over 2048
// characters), 404 for a playlist that is gone or private, 400 «Не удалось
// подключиться…» when Deezer does not answer within 15 s, 409 while a
// Deezer job is open.
func (h *importAPI) deezerPublic(w http.ResponseWriter, r *http.Request) {
	pub, o := h.svc.Public, h.full.oauth
	if pub == nil || o == nil {
		writeErr(w, r, http.StatusBadRequest, musicimport.ErrNotConfigured.Error())
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !h.noOpenJob(w, r, playlistimport.ServiceDeezer) {
		return
	}
	// the link is checked now, within a bound: a wrong one is said at once
	cctx, cancel := context.WithTimeout(r.Context(), publicCheckTimeout)
	p, err := pub.FindPublic(cctx, req.URL)
	cancel()
	switch {
	case errors.Is(err, playlistimport.ErrBadLink):
		writeErr(w, r, http.StatusBadRequest, "Не найдено")
		return
	case r.Context().Err() != nil:
		return
	case errors.Is(err, context.DeadlineExceeded):
		log.Printf("api: import deezer link: no answer in %s", publicCheckTimeout)
		writeErr(w, r, http.StatusBadRequest, musicimport.ErrConnect.Error())
		return
	case errors.Is(err, musicimport.ErrConnect):
		log.Printf("api: import deezer link: %v", err) // a path and a status, never the link's query
		writeErr(w, r, http.StatusBadRequest, musicimport.ErrConnect.Error())
		return
	case err != nil:
		h.err(w, r, "import deezer link", err) // «Не найдено», or logged once as a 500
		return
	}
	// the songs in the background: a cancel or delete of the job stops it
	id, err := o.ReadInBackground(r.Context(), userOf(r).ID, playlistimport.ServiceDeezer, func(ctx context.Context, _ int64) (playlistimport.Snapshot, error) {
		return pub.ReadPublic(ctx, p)
	})
	if err != nil {
		h.err(w, r, "import deezer link", err)
		return
	}
	h.writeJob(w, r, http.StatusCreated, id)
}
