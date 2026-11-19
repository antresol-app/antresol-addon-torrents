// An Antresol addon (contract 1, docs/addons/contract.md in the core): the
// server starts this program with its settings in ANTRESOL_* variables; it
// serves HTTP on the Unix socket ANTRESOL_ADDON_SOCKET and must answer
// GET /health within 15 s. The app reaches /api/… through the core's proxy
// (/api/addons/<id>/x/…); the program reaches the core through
// addonkit.Client, and the core calls its hooks (/hooks/<name>).
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/admin"
	"github.com/antresol-app/antresol-addon-torrents/internal/boot"
	"github.com/antresol-app/antresol-addon-torrents/internal/channelfetch"
	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/find"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/followapi"
	"github.com/antresol-app/antresol-addon-torrents/internal/holds"
	"github.com/antresol-app/antresol-addon-torrents/internal/importer"
	"github.com/antresol-app/antresol-addon-torrents/internal/libraryhooks"
	"github.com/antresol-app/antresol-addon-torrents/internal/migrateapi"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol-addon-torrents/internal/trackinghooks"
	"github.com/antresol-app/antresol-addon-torrents/internal/watch"
	"github.com/antresol-app/antresol/addonkit"
)

// manifestJSON is the addon's own manifest: its helper services.
//
//go:embed antresol-addon.json
var manifestJSON []byte

// solverHelper is the watch over the FlareSolverr helper service the
// manifest declares (nil: none declared).
func solverHelper(manifest []byte) (*addonkit.HelperWatch, error) {
	hs, err := addonkit.HelpersFromManifest(manifest)
	if err != nil {
		return nil, err
	}
	for _, h := range hs {
		if h.Name == "flaresolverr" {
			return &addonkit.HelperWatch{Helper: h}, nil
		}
	}
	return nil, nil
}

func main() {
	boot.SetProcessStart(time.Now())
	// a bad ANTRESOL_TORRENTS_NO_SEEDING stops the program before
	// anything else: no socket, no /info, no file
	forced, err := boot.ParseNoSeeding(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	env, err := addonkit.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(env.DataDir, 0o700); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	os.Remove(env.Socket) // a stale socket of an earlier run
	ln, err := net.Listen("unix", env.Socket)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	// an earlier run's ready.state goes: until this run is ready there is
	// none (the deploy check reads it)
	if err := boot.ClearReady(env.DataDir); err != nil {
		log.Fatal(err)
	}
	ready := &addonkit.Ready{}
	core := addonkit.NewClient(env)
	status := &bootStatus{}
	adminAPI, profileAPI, mediaRoutes := &lateHandler{}, &lateHandler{}, &lateHandler{}
	root := http.NewServeMux()
	// the admin pages' API (/api/admin/…, admins only: the core keeps
	// x/admin/… to admins), mounted once the services are built and the
	// first holds push is done
	root.Handle("/api/admin/", adminAPI)
	// the profiles' API (/api/…: «Смотреть сейчас», the finding, follows,
	// pinned releases, voice-overs),
	// and the inputs' reads (/media/…: on the socket only, never through
	// the proxy, which forwards /api/… alone); both from the same moment
	root.Handle("/api/", profileAPI)
	root.Handle("/media/", mediaRoutes)
	handler, hooks := newHandler(env, core, ready)
	root.Handle("/", handler)
	srv := &http.Server{Handler: status.wrap(root), ReadHeaderTimeout: 10 * time.Second}

	// the core stops an addon with SIGTERM and kills it 10 s later
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	// The long start (the one-time import, the no-seeding state, the
	// engine, the services) runs in the background while /health already
	// answers {"ok": true, "ready": false}; an error keeps the addon not
	// ready, with the error in its log and in /health, and writes no
	// ready.state. A start error is tried again with a growing pause
	// (boot.Retry) unless it is final (boot.Final: a refusal). Ready (and ready.state, and the admin routes) come from
	// Services.Run once the services are built and the first holds push is
	// done.
	var loops sync.WaitGroup
	var db atomic.Pointer[sql.DB] // closed after the loops (the engine saves into it)
	started := make(chan struct{})
	go func() {
		defer close(started)
		var b boot.Booted
		var conf boot.Conf
		var info addonkit.Info
		if err := boot.Retry(ctx, func() (err error) {
			b, conf, info, err = start(ctx, core, env, forced)
			return err
		}, func(err error) {
			log.Printf("%s: not ready: %v", env.ID, err)
			status.fail(err)
		}, boot.Backoff); err != nil {
			return
		}
		status.clear()
		svc, err := boot.Build(ctx, b, conf, core, info)
		if err != nil {
			log.Printf("%s: not ready: %v", env.ID, err)
			status.fail(err)
			b.Engine.Close()
			b.DB.Close()
			return
		}
		// the holds: pushed before ready (and before the admin routes
		// serve), then kept current; the files.* hooks answer for them
		pusher := &holds.Pusher{
			Src: holds.EngineSource{Engine: svc.Engine}, Busy: svc.Acq.Organizing,
			Put: core.PutHolds, FileRoots: info.FileRoots, MissingRoots: info.MissingRoots,
			// roots come and go on disk (a late mount): read again before
			// each push; holds under a missing root stay in the set
			Roots: func(ctx context.Context) ([]string, []string, error) {
				i, err := core.Info(ctx)
				return i.FileRoots, i.MissingRoots, err
			},
			// the set the core kept from the last run: never cleared by a
			// first push made while a root is missing
			Saved: core.Holds,
		}
		// «Смотреть сейчас»: the inputs registered with the core, dropped
		// with their download (an admin's remove or a core item delete:
		// both go through Acq.Remove)
		inputs := &watch.Inputs{Core: core}
		svc.Acq.SetOnRemoved(func(ih string) {
			inputs.Drop(ih)
			pusher.Changed()
		})
		// the library hooks: pending cards, disk segments, the kids' requests;
		// a segment folder the core would drop is said at the start
		libraryhooks.Register(hooks, libraryhooks.Deps{Acquire: svc.Acq, Sources: svc.Sources,
			DownloadDir: conf.DownloadDir, StreamDir: conf.StreamDir})
		libraryhooks.CheckSegmentDirs(slog.Default(), info, conf.DownloadDir, conf.StreamDir)
		// the tracking hooks: the pins' and the torrent search's scans (a pinned
		// scan of a followed title then downloads its missing episodes in
		// the background on ctx), the followed titles, their followers,
		// a library file's original language
		trackinghooks.Register(hooks, svc.TrackingHooks())
		// «ТВ» channels with fetching: their programmes downloaded before
		// they air (channels.fetch), as any download (no upload where
		// «Не раздавать» is on)
		fetchDeps := channelfetch.Deps{Acquire: svc.Acq, Torrents: svc.Engine.List, DB: svc.DB}
		if svc.Follows != nil {
			fetchDeps.Follows = svc.Follows
		}
		channelfetch.Register(ctx, hooks, fetchDeps)
		(&holds.Hooks{
			Engine:  svc.Engine,
			Remove:  func(ctx context.Context, ih string) error { return svc.Acq.Remove(ctx, ih, false) },
			Changed: pusher.Changed,
		}).Register(hooks)
		// «Переезд»: what the old services have that does not move, for
		// the core's screen (its runs, routes and reports are the addon's)
		hooks.Handle("migrate.leftovers", svc.Migrate.LeftoversHook)
		svc.Engine.OnComplete(func(torrent.Status) { pusher.Changed() }) // the rest shows at the next tick
		// a pinned release swapped in the engine, or its old files swept:
		// the holds go out at once (Services reads it when called)
		svc.HoldsChanged = pusher.Changed
		svc.FirstPush = pusher.PushNow
		// a refused set (400: the core's text names the path) or a core
		// that does not answer shows in /health while not ready
		svc.OnNotReady = func(err error) { status.fail(fmt.Errorf("holds push: %w", err)) }
		svc.Loops = append(svc.Loops, pusher.Run)
		// FlareSolverr on its compose name, while the admin gave no address
		solver, err := solverHelper(manifestJSON)
		if err != nil {
			log.Printf("%s: manifest helpers: %v", env.ID, err)
		}
		if solver != nil {
			solver.Health = &addonkit.HelperHealth{} // its probes, for helpers.status
			sw := boot.NewSolverWatch(solver, svc.Sources, svc.Settings.FlareSolverrURL, conf.FlareSolverr)
			svc.Loops = append(svc.Loops, sw.Run)
		}
		// «Состояние» of the core: FlareSolverr as the sources use it
		solverStatus := &boot.SolverStatus{Watch: solver, Saved: svc.Settings.FlareSolverrURL, Server: conf.FlareSolverr}
		solverStatus.Register(hooks)
		svc.OnReady = func() {
			// the admin routes (paths /api/admin/…, as the proxy delivers
			// them) serve once the holds are with the core
			// one cache of the profiles' «Языки» for find and the settings
			profiles := &coreadapt.Profiles{C: core}
			fd := find.Deps{Acquire: find.Service{Service: svc.Acq}, Languages: profiles.Languages,
				Approve: core.ApproveRequests, Added: pusher.Changed}
			files, stats := find.Admin(fd)
			adminAPI.set(admin.Routes(admin.Deps{
				Acquire: svc.Acq, Sources: svc.Sources, Settings: svc.Settings, NoSeed: svc.NoSeed,
				Upload: svc.Engine, StreamCacheDefaultGB: float64(conf.StreamCacheBytes) / (1 << 30),
				ProwlarrDB: conf.ProwlarrDB, FlareSolverrServer: conf.FlareSolverr, FlareSolverrHelper: solver, Outside: pusher.LastOutside,
				Watched: core.Watched, Languages: profiles.Languages, AcquireFiles: files, Stats: stats,
				Migrate: migrateapi.Routes(migrateapi.Deps{Migrate: svc.Migrate}),
			}))
			wd := watch.Deps{Acquire: svc.Acq, Library: coreadapt.Library{C: core}, Inputs: inputs}
			// «Следить», the pinned releases, «Найти озвучку…»
			ad := followapi.Deps{Follows: svc.Follows, Pins: svc.Pins, Acquire: svc.Acq, Library: coreadapt.Library{C: core},
				Watch: wd.Watch, WatchError: watch.WriteError, Added: pusher.Changed}
			profileAPI.set(profileRoutes(wd, fd, ad))
			mediaRoutes.set(watch.Media(wd))
			status.clear()
			ready.Set()
			log.Printf("%s: ready", env.ID)
		}
		db.Store(b.DB)
		svc.Run(ctx, &loops)
	}()
	log.Printf("%s: serving on %s", env.ID, env.Socket)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
	// stopping: the loops end (ctx), then acquire and the engine close
	// (Run); all of it within the core's 10 s before its SIGKILL
	deadline := time.After(8 * time.Second)
	select {
	case <-started:
	case <-deadline:
		log.Printf("%s: stop: the start did not end in time", env.ID)
		return
	}
	stopped := make(chan struct{})
	go func() { loops.Wait(); close(stopped) }()
	select {
	case <-stopped:
		if d := db.Load(); d != nil {
			d.Close()
		}
	case <-deadline:
		log.Printf("%s: stop: the engine did not close in time", env.ID)
	}
}

// profileRoutes is the profiles' /api/…: one mux, each family adds its
// routes (watch, find, follow/pinned/voice-overs). Never under
// /api/admin/: those routes are for every adult profile.
func profileRoutes(wd watch.Deps, fd find.Deps, ad followapi.Deps) *http.ServeMux {
	mux := http.NewServeMux()
	watch.Register(mux, wd)
	find.Register(mux, fd)
	followapi.Register(mux, ad)
	return mux
}

// msgStarting answers every route asked before the addon is ready.
const msgStarting = "Расширение запускается"

// lateHandler serves a handler set after the start (the routes built on
// the services); until then 503 {"error": msgStarting}, in the asking
// profile's language (Russian without one: the core's own reads of
// /media/).
type lateHandler struct{ h atomic.Pointer[http.Handler] }

func (l *lateHandler) set(h http.Handler) { l.h.Store(&h) }

func (l *lateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := l.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	lang := i18n.RU
	if u, ok := addonkit.UserFrom(r); ok && u.Lang != "" {
		lang = u.Lang
	}
	w.Header().Set("Retry-After", "5")
	addonkit.WriteError(w, http.StatusServiceUnavailable, i18n.TL(lang, msgStarting))
}

// start asks the core for /info, reads the configuration and boots: the
// engine exists only once the no-seeding state is written. It returns the
// configuration and /info for Build. main retries it (boot.Retry): a core
// busy at its own start or a vault.db written meanwhile is tried again, a
// bad configuration is final.
func start(ctx context.Context, core *addonkit.Client, env addonkit.Env, forced bool) (boot.Booted, boot.Conf, addonkit.Info, error) {
	info, err := core.Info(ctx)
	if err != nil {
		return boot.Booted{}, boot.Conf{}, info, fmt.Errorf("core /info: %w", err)
	}
	conf, err := boot.LoadConf(os.Getenv, forced, info, env.DataDir)
	if err != nil {
		return boot.Booted{}, conf, info, boot.Final(err)
	}
	b, err := boot.Boot(ctx, conf, boot.Deps{
		OpenDB:      store.Open,
		Import:      importer.Phase1,
		Fingerprint: importer.Fingerprint,
		MapUsers: func(ctx context.Context, db *sql.DB) error {
			return follow.MapUserUIDs(ctx, db, core.Users)
		},
		NewEngine: func(cfg torrent.EngineConfig, db *sql.DB) (boot.Engine, error) {
			e, err := torrent.New(cfg, db)
			if err != nil {
				return nil, err // never a typed nil in the interface
			}
			return e, nil
		},
	})
	return b, conf, info, err
}

// bootStatus carries a failed start into /health:
// {"ok": true, "ready": false, "error": "…"}.
type bootStatus struct{ err atomic.Pointer[string] }

func (s *bootStatus) fail(err error) { msg := err.Error(); s.err.Store(&msg) }

func (s *bootStatus) clear() { s.err.Store(nil) }

func (s *bootStatus) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			if msg := s.err.Load(); msg != nil {
				addonkit.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "ready": false, "error": *msg})
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}
