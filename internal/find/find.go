// Package find is «Найти и добавить» for adult profiles: the title
// search, its releases (NDJSON as the sources answer), the add, a title's
// download progress and its cancel, «На сервер» of a stream, and two
// admin routes (a release's files, the seeding figures) that main mounts
// under /api/admin/. Shapes as the core served them before the move.
package find

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol/addonkit"
)

// Acquire is what the routes use of acquire.Service (Service adapts it).
type Acquire interface {
	Discover(ctx context.Context, q string) ([]acquire.Candidate, error)
	DiscoverMedia(ctx context.Context, kind, q string) ([]acquire.Candidate, error)
	// Releases calls progress (when set) as the sources answer.
	Releases(ctx context.Context, req acquire.ReleaseRequest, progress func(indexer.Progress)) (indexer.SearchResponse, error)
	Acquire(ctx context.Context, req acquire.AcquireRequest) (acquire.AcquireResult, error)
	TitleProgress(ctx context.Context, kind string, tmdbID, season int) (acquire.TitleProgress, bool)
	TitlesProgress(ctx context.Context, keys []acquire.TitleKey) map[acquire.TitleKey]acquire.TitleProgress
	CancelTitle(ctx context.Context, kind string, tmdbID, season int, keep string) (int, error)
	Promote(ctx context.Context, ih string, force bool) (string, error)
	ReleaseFiles(ctx context.Context, req acquire.ReleaseFilesRequest) (acquire.ReleaseFiles, error)
	Torrents() []acquire.TorrentView
}

// Service is Acquire over the addon's acquire.Service.
type Service struct{ *acquire.Service }

func (s Service) Releases(ctx context.Context, req acquire.ReleaseRequest, progress func(indexer.Progress)) (indexer.SearchResponse, error) {
	if progress != nil {
		ctx = indexer.WithProgress(ctx, progress)
	}
	return s.Service.Releases(ctx, req)
}

// Deps are what the routes need.
type Deps struct {
	Acquire Acquire
	// Languages is the profile's «Языки» (coreadapt.Profiles): the
	// subtitles a release lists are cut to them. nil or empty: the
	// profile's interface language alone.
	Languages func(ctx context.Context, userID int64) []string
	// Approve (optional): after an admin's download, the kids' requests of
	// it are approved (Client.ApproveRequests); a failure is logged.
	Approve func(ctx context.Context, a addonkit.ApproveRequest) (int, error)
	// Added (optional): a download was added (the holds go out at once).
	Added func()
}

// Register adds the adults' routes to mux (the /api/ routes served once
// the addon is ready).
func Register(mux *http.ServeMux, d Deps) {
	h := &handlers{d}
	mux.Handle("GET /api/discover", adults(http.HandlerFunc(h.discover)))
	mux.Handle("GET /api/releases", adults(http.HandlerFunc(h.releases)))
	mux.Handle("POST /api/acquire", adults(http.HandlerFunc(h.acquire)))
	mux.Handle("GET /api/acquire/progress", adults(http.HandlerFunc(h.titleProgress)))
	mux.Handle("GET /api/acquire/progress/batch", adults(http.HandlerFunc(h.titlesProgress)))
	mux.Handle("POST /api/acquire/cancel", adults(http.HandlerFunc(h.cancelTitle)))
	mux.Handle("POST /api/torrents/{ih}/promote", adults(http.HandlerFunc(h.promote)))
}

// Admin is the admin routes' handlers, for admin.Deps (main mounts every
// /api/admin/ path there, behind RequireAdmin): POST
// /api/admin/acquire/files and GET /api/admin/stats.
func Admin(d Deps) (files, stats http.Handler) {
	h := &handlers{d}
	return http.HandlerFunc(h.releaseFiles), http.HandlerFunc(h.stats)
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

// user is the request's profile (adults let only those through).
func user(r *http.Request) addonkit.User {
	if u, ok := addonkit.UserFrom(r); ok {
		return *u
	}
	return addonkit.User{}
}

// myLanguages is the profile's «Языки», its interface language when
// the core gives none.
func (h *handlers) myLanguages(r *http.Request) []string {
	u := user(r)
	if h.Languages != nil {
		if l := h.Languages(r.Context(), u.ID); len(l) > 0 {
			return l
		}
	}
	if u.Lang != "" {
		return []string{u.Lang}
	}
	return nil
}

// writeErr writes {"error": msg} (Russian) in the request's language.
func writeErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	addonkit.WriteError(w, code, i18n.Localize(r.Context(), msg))
}

// decode reads a JSON body (at most 1 MiB); false: a 400 is written.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err == nil {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "Неверный запрос")
		return false
	}
	return true
}

// msgErr is a service's Russian message with the error it came with.
type msgErr struct {
	msg string
	err error
}

func (e msgErr) Error() string { return e.msg }
func (e msgErr) Unwrap() error { return e.err }

// acquireErr writes a service error: its message in the request's
// language (an *i18n.Error in the chain, "Источник: …", rendered whole),
// with the status of its kind; no_format carries {"code": "no_format"}.
func acquireErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	msg, ok := acquire.UserMessage(err)
	if !ok {
		slog.Warn("find: "+what, "err", err)
		writeErr(w, r, http.StatusInternalServerError, "Внутренняя ошибка сервера")
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
		slog.Warn("find: "+what, "err", err)
	}
	msg = i18n.Message(r.Context(), msgErr{msg, err})
	if errors.Is(err, acquire.ErrNoFormat) {
		// «Скачать в другом формате» resends the request with anyFormat
		addonkit.WriteJSON(w, code, map[string]string{"error": msg, "code": "no_format"})
		return
	}
	addonkit.WriteError(w, code, msg)
}
