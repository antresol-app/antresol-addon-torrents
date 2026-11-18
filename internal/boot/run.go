package boot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/events"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/importer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/migrate"
	"github.com/antresol-app/antresol-addon-torrents/internal/pinned"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/studios"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol-addon-torrents/internal/trackinghooks"
	"github.com/antresol-app/antresol/addonkit"
)

// ReadyFile is written in the data dir once the addon is fully ready (the
// services built and the first holds push done): one line, the start
// time of this process in RFC 3339. The deploy check reads its mtime (it
// must be newer than the container's start); it is never written on a
// boot or build error, and ClearReady removes an earlier run's at start.
const ReadyFile = "ready.state"

// Defaults of the loops' retries.
const (
	phase2Every = 60 * time.Second // importer.Phase2 after an error
	retryEvery  = 5 * time.Second  // the first holds push, the ready file
)

// Services are the addon's running parts, built on a Booted engine.
// NoSeed is the only way to change no-seeding: nothing here hands
// the engine's own setter on.
type Services struct {
	Engine   *torrent.Engine
	Sources  *indexer.Manager
	Acq      *acquire.Service
	Bus      *events.Bus
	Settings store.Settings
	Client   *addonkit.Client
	Conf     Conf
	Info     addonkit.Info
	NoSeed   *NoSeed
	DB       *sql.DB
	// StreamCacheBytes is the «Смотреть сейчас» cache limit Build gave
	// acquire: the saved setting, else the env's.
	StreamCacheBytes int64
	// Follows is «Следить» (nil when the core has no TMDB: no follows,
	// as before the move); Pins the pinned releases; PinActions what a
	// pinned scan of a followed title downloads (nil without Follows).
	Follows    *follow.Service
	Pins       *pinned.Service
	PinActions *pinned.Actions
	// Torrents is the torrent search's tracking scanner (one per process:
	// it keeps when each title was searched).
	Torrents pinned.Scanner
	// Studios is the core's favourite studios, read by the searches.
	Studios *studios.Core
	// Migrate is «Переезд» from Prowlarr, Sonarr, Radarr and qBittorrent
	// (buildMigrate): its routes (migrateapi) and the migrate.leftovers
	// hook are main's; its runs end with the run context.
	Migrate *migrate.Service
	// HoldsChanged (optional; main: the holds pusher's Changed, made after
	// Build) is told when the files held may have changed outside acquire:
	// a pinned release swapped in the engine, the old files of one swept.
	// It is read when called.
	HoldsChanged func()
	// KickRetryEvery (default 30 s) is how long a followed title the core
	// did not take waits before it is sent again (tests shorten it).
	KickRetryEvery time.Duration

	// FirstPush is the holds pusher's first push (Pusher.PushNow).
	// Ready waits until it succeeds (retried every RetryEvery); nil: ready
	// waits on nothing more than Build.
	FirstPush func(ctx context.Context) error
	// Loops are more loops Run starts and waits for (Pusher.Run).
	Loops []func(ctx context.Context)
	// OnNotReady is called with every failed FirstPush (main: /health shows
	// it, e.g. the core's 400 naming the path it refused); nil: only logged.
	OnNotReady func(error)
	// OnReady is called once, right after ReadyFile is written (main:
	// addonkit.Ready.Set).
	OnReady func()
	// RetryEvery (default 5 s) and Phase2Every (default 60 s) are the
	// loops' waits after an error (tests shorten them).
	RetryEvery  time.Duration
	Phase2Every time.Duration
	Log         *slog.Logger

	binder importer.RefBinder // Client (tests replace it)
	// runCtx is Build's context (main's run context): the finish
	// callback's background work runs on it.
	runCtx context.Context
	// sweep is Pins.SweepStale (tests replace it)
	sweep func(ctx context.Context, ih string) error
	kicks *followKicks
	// finished is the finish callback Build gave acquire (tests call it)
	finished func(acquire.Finished)
}

// Build makes the sources manager, the event bus and the acquire service
// on b's engine and database, with the core reached through client.
func Build(ctx context.Context, b Booted, c Conf, client *addonkit.Client, info addonkit.Info) (*Services, error) {
	eng, ok := b.Engine.(*torrent.Engine)
	if !ok || eng == nil {
		return nil, errors.New("boot: build: the engine is not a *torrent.Engine")
	}
	if b.DB == nil || client == nil {
		return nil, errors.New("boot: build: no database or core client")
	}
	// the addon's own studios, before anything reads a release name
	studios.Register()
	settings := store.Settings{DB: b.DB}
	bus := &events.Bus{}
	// the searches score with the core's favourite studios first
	coreStudios := &studios.Core{Studios: client.Studios}
	sources := indexer.NewManager(b.DB, indexer.NewCatalog(filepath.Join(c.DataDir, "definitions")), indexer.Options{
		FlareSolverr: c.FlareSolverr, Studios: coreStudios.List,
		OnStatus: func(id int64, status, message string) {
			bus.Publish(events.Event{Type: indexer.EventSourceStatus, Data: map[string]any{"id": id, "status": status, "lastError": message}})
		},
	})
	// the FlareSolverr address saved in «Настройки загрузок» wins over the server's
	if u, err := settings.FlareSolverrURL(ctx); err != nil {
		return nil, fmt.Errorf("boot: build: %w", err)
	} else if u != "" {
		sources.SetFlareSolverr(u)
	}
	gb, err := settings.StreamCacheGB(ctx, float64(c.StreamCacheBytes)/(1<<30))
	if err != nil {
		return nil, fmt.Errorf("boot: build: %w", err)
	}
	cache := int64(gb * (1 << 30))

	// the metadata sources the core has on (probed once, here: a key
	// enabled later takes a restart); nil interfaces, never typed nils
	tm, mb, ol, wd := coreadapt.Metadata(client, info)
	var meta acquire.Metadata
	if tm != nil {
		meta = tm
	}
	var mbCat acquire.MusicBrainz
	if mb != nil {
		mbCat = mb
	}
	var olCat acquire.OpenLibrary
	if ol != nil {
		olCat = ol
	}
	var wdCat acquire.Wikidata
	if wd != nil {
		wdCat = wd
	}
	roots := info.LibraryRoots
	acq := acquire.New(acquire.Options{
		DB: b.DB, Engine: eng, Indexer: sources, Metadata: meta,
		Library: coreadapt.Library{C: client},
		Users:   coreadapt.Users{C: client}, // «Добавил»
		Disk:    &acquire.Disk{Root: c.DownloadDir},
		FFprobe: info.FFprobe,
		Bus:     bus,

		TorrentsDir: c.DownloadDir, MoviesDir: roots.Movies, SeriesDir: roots.Series, MusicDir: roots.Music,
		EbooksDir: roots.Ebooks, AudiobooksDir: roots.Audiobooks, ComicsDir: roots.Comics,
		MusicBrainz: mbCat, OpenLibrary: olCat, Wikidata: wdCat, Images: coreadapt.Images{C: client},
		StreamCacheBytes: cache,
	})
	s := &Services{
		Engine: eng, Sources: sources, Acq: acq, Bus: bus, Settings: settings, Client: client,
		Conf: c, Info: info, NoSeed: b.NoSeed, DB: b.DB, StreamCacheBytes: cache, binder: client,
		runCtx: ctx, Studios: coreStudios,
	}
	// the voice-over tracks «Найти озвучку…» places are queued for their
	// fit at the core just before they are linked, and the sheet reads
	// their fits there
	vo := coreadapt.VoiceOver{C: client}
	acq.SetVoiceOverFit(func(ctx context.Context, p acquire.PlacedVoiceOver) error {
		return vo.Placed(ctx, p.Path, p.FileID, p.OrigLang)
	}, vo.State)
	var followTMDB follow.TMDB // a nil interface without TMDB, never a typed nil
	if tm != nil {
		followTMDB = tm
	}
	s.buildTracking(followTMDB, client)
	s.Migrate = s.buildMigrate(ctx, tm, client)
	// the one finish callback: the admins' letter through the
	// core, then the stale files of a renamed pinned re-upload
	notifier := coreadapt.Notifier{C: client, T: i18n.TL, ItemPath: coreadapt.AcquisitionItemPath(b.DB, client)}
	s.finished = finishedCallback(s.log, func(ctx context.Context, f acquire.Finished) error {
		return notifier.Finished(ctx, coreadapt.Finished(f))
	}, s.sweepStale)
	acq.SetFinished(s.finished)
	return s, nil
}

// buildTracking makes the pinned releases and, with TMDB, the follows
// over the core (as the old build's trackingHooks did): a swapped
// release keeps its download record (Rekey) and its new hold goes out at
// once; the old files of a renamed re-upload go once the library let go
// of them (Library, Imported). After each pinned scan of a followed title
// its missing episodes are downloaded from the pin (Actions.Sync, from
// the scan's hook: TrackingHooks); a
// follow turned on has the core track and refresh its title (followKicks).
// The loops of the follows (their checks, the kicks) join Loops.
func (s *Services) buildTracking(tm follow.TMDB, client *addonkit.Client) {
	lib := coreadapt.Library{C: client}
	track := coreadapt.Tracking{C: client}
	s.Pins = &pinned.Service{DB: s.DB, Tracking: track, Indexer: pinned.ManagerIndexer(s.Sources), Engine: s.Engine,
		Library: lib, Imported: s.Acq.Imported, Log: s.log().With("component", "pinned"),
		Replaced: func(ctx context.Context, oldIH, newIH string) error {
			err := s.Acq.Rekey(ctx, oldIH, newIH)
			s.holdsChanged()
			return err
		}}
	s.sweep = s.Pins.SweepStale
	s.Torrents = pinned.NewTorrentScanner(s.Sources, s.Pins, 0)
	if tm == nil {
		return
	}
	s.Follows = &follow.Service{DB: s.DB, Acquire: s.Acq, TMDB: tm, Library: lib, Bus: s.Bus,
		Users: coreadapt.Users{C: client}, Titles: lib, Log: s.log().With("component", "follow")}
	// a pinned scan of a followed title downloads its missing episodes:
	// the tracking.scan:pinned hook runs Sync after its answer
	// (TrackingHooks), not the scan itself (Pins.AfterScan stays nil)
	s.PinActions = &pinned.Actions{Pins: s.Pins, Follows: s.Follows, Acquire: s.Acq}
	s.Follows.Covered, s.Follows.FilmPinned = s.Pins.Covered, s.Pins.FilmPinned
	s.kicks = newFollowKicks(track, func() time.Duration { return orDefault(s.KickRetryEvery, kickRetryEvery) }, s.log)
	s.Follows.OnFollowed = s.kicks.Followed
	s.Loops = append(s.Loops, s.Follows.Run, func(ctx context.Context) { s.kicks.Run(ctx) })
}

// TrackingHooks are the parts of the tracking hooks (trackinghooks.Register: main, after
// Build, before Run): the pins' and the torrent search's scanners, the
// pin actions' Sync on the run context (none without follows), the
// follows' database, the core's profiles and acquire's original language.
func (s *Services) TrackingHooks() trackinghooks.Deps {
	d := trackinghooks.Deps{Pinned: s.Pins.Scanner(), Torrent: s.Torrents, Run: s.runCtx, DB: s.DB,
		Users: coreadapt.Users{C: s.Client}, OrigLang: s.Acq.OriginalLanguageOf, Log: s.log().With("component", "trackinghooks")}
	if s.PinActions != nil {
		d.Sync = s.PinActions.Sync
	}
	return d
}

// notifyTimeout bounds a finished download's letter to the core.
var notifyTimeout = 10 * time.Second

// finishedCallback is acquire's finish callback: notify runs in the
// background under notifyTimeout (acquire never waits on the core; a
// failure is logged), then each of then runs in order, at once.
func finishedCallback(log func() *slog.Logger, notify func(context.Context, acquire.Finished) error, then ...func(acquire.Finished)) func(acquire.Finished) {
	return func(f acquire.Finished) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
			defer cancel()
			if err := notify(ctx, f); err != nil {
				log().Warn("torrents: download letter not sent", "key", f.Key, "failed", f.Failed, "err", err)
			}
		}()
		for _, h := range then {
			h(f)
		}
	}
}

func (s *Services) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// Run starts the loops in the background, each counted in wg: acquire's
// Run, the bus forwarded to the app (admin.progress, admin.source-status),
// the import's phase 2 (one loop, retried every Phase2Every), the Loops,
// and readiness (FirstPush, then ReadyFile, then OnReady). When ctx ends
// the loops stop, then acquire, the bus and the engine are closed (the
// engine saves its rows), and wg is done.
func (s *Services) Run(ctx context.Context, wg *sync.WaitGroup) {
	var loops sync.WaitGroup
	start := func(f func(context.Context)) {
		loops.Add(1)
		go func() { defer loops.Done(); f(ctx) }()
	}
	start(s.Acq.Run)
	start(func(ctx context.Context) { coreadapt.Forward(ctx, s.Bus, s.Client) })
	start(s.phase2)
	for _, l := range s.Loops {
		start(l)
	}
	start(s.ready)
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-ctx.Done()
		loops.Wait()
		if s.Migrate != nil {
			s.Migrate.Wait() // a run or rollback saves its report and stops
		}
		s.Acq.Close()
		s.Bus.Close()
		if err := s.Engine.Close(); err != nil {
			s.log().Error("torrents: close the engine", "err", err)
		}
	}()
}

// phase2 binds the import's pending refs (importer.Phase2) until done,
// again every Phase2Every after an error. Only this loop runs it.
func (s *Services) phase2(ctx context.Context) {
	b := s.binder
	if b == nil {
		b = s.Client
	}
	every := orDefault(s.Phase2Every, phase2Every)
	for {
		done, err := importer.Phase2(ctx, s.DB, b, 0)
		if done {
			return
		}
		if ctx.Err() != nil {
			return
		}
		s.log().Error("torrents: import phase 2 (retried)", "err", err, "in", every)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// ready waits for the first holds push, writes ReadyFile and calls
// OnReady; each step is retried every RetryEvery until it succeeds or ctx
// ends.
func (s *Services) ready(ctx context.Context) {
	every := orDefault(s.RetryEvery, retryEvery)
	wait := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(every):
			return true
		}
	}
	if s.FirstPush != nil {
		for {
			err := s.FirstPush(ctx)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			s.log().Error("torrents: first holds push (retried; not ready until it succeeds)", "err", err)
			if s.OnNotReady != nil {
				s.OnNotReady(err)
			}
			if !wait() {
				return
			}
		}
	}
	for {
		// a stop (SIGTERM) after the push leaves no fresh ready.state
		if ctx.Err() != nil {
			return
		}
		err := WriteReady(s.Conf.DataDir)
		if err == nil {
			break
		}
		s.log().Error("torrents: ready state (retried)", "err", err)
		if !wait() {
			return
		}
	}
	if ctx.Err() != nil {
		return
	}
	if s.OnReady != nil {
		s.OnReady()
	}
}

// WriteReady writes dataDir/ready.state: the process's start time (as
// SetProcessStart recorded it), RFC 3339, one line; atomic and never in the
// start's whole second, as WriteState.
func WriteReady(dataDir string) error {
	stateMu.Lock()
	t := processStart
	if t.IsZero() {
		t = clockNow()
	}
	stateMu.Unlock()
	if err := writeAfterStart(dataDir, ReadyFile, t.UTC().Format(time.RFC3339)+"\n"); err != nil {
		return fmt.Errorf("boot: ready state: %w", err)
	}
	return nil
}

// ClearReady removes an earlier run's ready.state (main, at start): until
// this run is ready there is none.
func ClearReady(dataDir string) error {
	if err := os.Remove(filepath.Join(dataDir, ReadyFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("boot: clear ready state: %w", err)
	}
	return nil
}
