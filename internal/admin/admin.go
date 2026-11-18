// Package admin serves the addon's admin routes under /api/admin/: the
// downloads, the engine, the sources and the settings of the web part's
// «Загрузки», «Источники» and «Настройки загрузок» pages. The core proxies
// x/admin/… here for admins only; every route checks it again
// (addonkit.RequireAdmin). The answers have the old build's shapes, so
// the moved pages changed least. Messages are Russian, given in the
// profile's language (X-Antresol-User lang).
package admin

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// Acquire is what the downloads routes need of *acquire.Service.
type Acquire interface {
	Torrents() []acquire.TorrentView
	VideoItems(ctx context.Context) map[string]int64
	Pause(ctx context.Context, ih string) error
	Resume(ctx context.Context, ih string) error
	RetryOrganize(ctx context.Context, ih string) error
	ForceOrganize(ctx context.Context, ih string) error
	DismissAutoNote(ctx context.Context, ih string) error
	Remove(ctx context.Context, ih string, deleteData bool) error
	Promote(ctx context.Context, ih string, force bool) (string, error)
	TorrentStatus(ih string, file int) (acquire.StreamStatus, error)
	Connectivity() (torrent.Connectivity, bool)
	SetStreamCacheBytes(n int64)
}

// NoSeeding is what the settings need of *boot.NoSeed, the only owner of
// the effective value: the routes read it and change it only
// through Set.
type NoSeeding interface {
	Forced() bool
	Effective(ctx context.Context) (bool, error)
	Set(ctx context.Context, on bool) error
}

// UploadLimiter is the engine's upload limit (*torrent.Engine). Nothing
// here reaches the engine's no-seeding.
type UploadLimiter interface {
	SetUploadLimit(bytesPerSec int64)
}

// Deps are the services the routes use.
type Deps struct {
	Acquire  Acquire
	Sources  *indexer.Manager
	Settings store.Settings
	NoSeed   NoSeeding
	Upload   UploadLimiter
	// StreamCacheDefaultGB: «Кеш просмотра» before it was ever saved
	// (Conf.StreamCacheBytes in GB).
	StreamCacheDefaultGB float64
	// ProwlarrDB: the default path of «Импорт из Prowlarr».
	ProwlarrDB string
	// FlareSolverrServer: the server's ANTRESOL_TORRENTS_FLARESOLVERR_URL
	// ("" = none), used while no address is saved in the settings.
	FlareSolverrServer string
	// FlareSolverrHelper (optional) watches the FlareSolverr helper
	// service the manifest declares, used while neither address is set.
	FlareSolverrHelper *addonkit.HelperWatch
	// SolverHTTP (optional) checks a FlareSolverr address (default
	// http.DefaultClient; a field for tests).
	SolverHTTP *http.Client
	// Outside (optional): the holds left out of the core's push, outside
	// the file browser's roots; GET settings carries the count.
	Outside func() int
	// Watched (optional): which of the items / series the profile has
	// watched (the «Просмотренные» filter of «Загрузки»; Client.Watched);
	// without it the field stays unset.
	Watched func(ctx context.Context, userID int64, items, series []int64) (map[int64]bool, error)
	// Languages (optional): the profile's «Языки» (coreadapt.Profiles),
	// the settings' language order; without them, or empty, the
	// profile's interface language alone.
	Languages func(ctx context.Context, userID int64) []string
	// AcquireFiles and Stats (optional): «Найти и добавить»'s admin routes
	// (find.Admin): POST /api/admin/acquire/files, GET /api/admin/stats.
	AcquireFiles, Stats http.Handler
	// Migrate (optional): «Переезд» (migrateapi.Routes), served at
	// /api/admin/migrate and below inside this mux, so the admin check
	// and the language apply to it too.
	Migrate http.Handler
}

// Routes is the handler of every admin route (paths start with
// /api/admin/).
func Routes(d Deps) http.Handler {
	mux := http.NewServeMux()
	dl := &downloads{d}
	mux.HandleFunc("GET /api/admin/torrents", dl.list)
	mux.HandleFunc("POST /api/admin/torrents/{ih}/{action}", dl.action)
	mux.HandleFunc("DELETE /api/admin/torrents/{ih}", dl.remove)
	mux.HandleFunc("GET /api/admin/torrents/{ih}/status", dl.status)
	mux.HandleFunc("GET /api/admin/engine", dl.engine)
	src := &sources{d}
	mux.HandleFunc("GET /api/admin/sources/catalog", src.catalog)
	mux.HandleFunc("GET /api/admin/sources/catalog/{id}", src.catalogEntry)
	mux.HandleFunc("GET /api/admin/sources", src.list)
	mux.HandleFunc("POST /api/admin/sources", src.add)
	mux.HandleFunc("POST /api/admin/sources/import-prowlarr", src.importProwlarr)
	mux.HandleFunc("PATCH /api/admin/sources/{id}", src.update)
	mux.HandleFunc("DELETE /api/admin/sources/{id}", src.remove)
	mux.HandleFunc("POST /api/admin/sources/{id}/test", src.test)
	st := &settings{d}
	mux.HandleFunc("GET /api/admin/settings", st.get)
	mux.HandleFunc("PUT /api/admin/settings", st.put)
	mux.HandleFunc("PUT /api/admin/settings/no-seeding", st.putNoSeeding)
	mux.HandleFunc("PUT /api/admin/settings/flaresolverr", st.putFlareSolverr)
	if d.AcquireFiles != nil {
		mux.Handle("POST /api/admin/acquire/files", d.AcquireFiles)
	}
	if d.Stats != nil {
		mux.Handle("GET /api/admin/stats", d.Stats)
	}
	if d.Migrate != nil {
		mux.Handle("/api/admin/migrate", d.Migrate)
		mux.Handle("/api/admin/migrate/", d.Migrate)
	}
	return addonkit.RequireAdmin(withLanguage(mux))
}

// withLanguage puts the profile's language into the request's context.
func withLanguage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := addonkit.UserFrom(r); ok && u.Lang != "" {
			r = r.WithContext(i18n.WithLanguage(r.Context(), u.Lang))
		}
		next.ServeHTTP(w, r)
	})
}

func userID(r *http.Request) int64 {
	if u, ok := addonkit.UserFrom(r); ok {
		return u.ID
	}
	return 0
}

// writeErr writes {"error": msg} with msg (Russian) in the request's language.
func writeErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	addonkit.WriteError(w, code, i18n.Localize(r.Context(), msg))
}

func internalErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	log.Printf("admin: %s: %v", what, err)
	writeErr(w, r, http.StatusInternalServerError, "Внутренняя ошибка сервера")
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

// idParam is the path's {name} as a positive id; false: a 400 is written.
func idParam(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, r, http.StatusBadRequest, "Неверный запрос")
		return 0, false
	}
	return id, true
}
