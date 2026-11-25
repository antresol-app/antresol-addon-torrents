// Package find is «Найти и добавить» for adult profiles: the title
// search, its releases (NDJSON as the sources answer), the add, a title's
// download progress and its cancel, «На сервер» of a stream, and two
// admin routes (a release's files, the seeding figures) that main mounts
// under /api/admin/. Shapes as the core served them before the move.
package find

import (
	"context"
	"net/http"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/httpx"
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
	mux.Handle("GET /api/discover", httpx.Adults(http.HandlerFunc(h.discover)))
	mux.Handle("GET /api/releases", httpx.Adults(http.HandlerFunc(h.releases)))
	mux.Handle("POST /api/acquire", httpx.Adults(http.HandlerFunc(h.acquire)))
	mux.Handle("GET /api/acquire/progress", httpx.Adults(http.HandlerFunc(h.titleProgress)))
	mux.Handle("GET /api/acquire/progress/batch", httpx.Adults(http.HandlerFunc(h.titlesProgress)))
	mux.Handle("POST /api/acquire/cancel", httpx.Adults(http.HandlerFunc(h.cancelTitle)))
	mux.Handle("POST /api/torrents/{ih}/promote", httpx.Adults(http.HandlerFunc(h.promote)))
}

// Admin is the admin routes' handlers, for admin.Deps (main mounts every
// /api/admin/ path there, behind RequireAdmin): POST
// /api/admin/acquire/files and GET /api/admin/stats.
func Admin(d Deps) (files, stats http.Handler) {
	h := &handlers{d}
	return http.HandlerFunc(h.releaseFiles), http.HandlerFunc(h.stats)
}

type handlers struct{ Deps }

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
