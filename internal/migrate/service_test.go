package migrate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	mediaimport "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/mediaimport/runkit"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// fakeIndexer stands in for indexer.Manager; block holds the preview until
// closed (a long run).
type fakeIndexer struct {
	block chan struct{}
}

func (f *fakeIndexer) PreviewProwlarrIn(ctx context.Context, _, _ string) ([]indexer.Imported, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []indexer.Imported{{Name: "RuTor", Reason: indexer.ReasonWillAdd}}, nil
}

func (f *fakeIndexer) ImportProwlarrIn(ctx context.Context, path, tmp string) ([]indexer.Imported, error) {
	return f.PreviewProwlarrIn(ctx, path, tmp)
}

// fakeEngine: an engine with no torrents (rollback finds nothing to remove).
type fakeEngine struct{}

func (fakeEngine) AddTorrentFile(context.Context, []byte, torrent.AddOptions) (torrent.Handle, error) {
	return nil, errors.New("fake")
}
func (fakeEngine) Get(string) (torrent.Handle, bool)    { return nil, false }
func (fakeEngine) Resume(context.Context, string) error { return nil }
func (fakeEngine) Pause(context.Context, string) error  { return nil }
func (fakeEngine) RemoveIfMode(context.Context, string, string, bool) error {
	return torrent.ErrNotFound
}
func (fakeEngine) Offline() string { return "" }
func (fakeEngine) WaitVerified(context.Context, string) (int64, int64, error) {
	return 0, 0, errors.New("fake")
}

// swTMDB: series 10 has one fully aired season of three episodes; movie 20
// is out; anything else fails.
type swTMDB struct{}

func (swTMDB) TV(_ context.Context, id int) (tmdb.Details, error) {
	if id != 10 {
		return tmdb.Details{}, errors.New("tmdb down")
	}
	return tmdb.Details{ID: id, Title: "Сёгун", Seasons: []tmdb.SeasonCount{{Number: 1, Episodes: 3}}}, nil
}
func (swTMDB) Season(_ context.Context, id, n int) (tmdb.SeasonDetails, error) {
	if id != 10 {
		return tmdb.SeasonDetails{}, errors.New("tmdb down")
	}
	return tmdb.SeasonDetails{Number: n, AirDate: "2020-01-01", Episodes: []tmdb.Episode{
		{Number: 1, AirDate: "2020-01-01"}, {Number: 2, AirDate: "2020-01-08"}, {Number: 3, AirDate: "2020-01-15"}}}, nil
}
func (swTMDB) Movie(_ context.Context, id int) (tmdb.Details, error) {
	if id != 20 {
		return tmdb.Details{}, errors.New("tmdb down")
	}
	return tmdb.Details{ID: id, Title: "Дюна", Year: 2024, ReleaseDate: "2024-02-27"}, nil
}
func (swTMDB) ImageURL(_, p string) string { return p }

type swAcq struct {
	mu    sync.Mutex
	calls []acquire.AcquireRequest
}

func (a *swAcq) Acquire(_ context.Context, r acquire.AcquireRequest) (acquire.AcquireResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, r)
	return acquire.AcquireResult{InfoHash: "ih"}, nil
}
func (a *swAcq) Pending(context.Context) ([]acquire.PendingAcquisition, error) { return nil, nil }

type svcEnv struct {
	t       *testing.T
	db      *sql.DB
	admin   int64  // the core's id of the admin (X-Antresol-User)
	dir     string // the import folder
	tmp     string
	reports string
	idx     *fakeIndexer
	acq     *swAcq
	track   *coretest.Tracking
	kicks   *kicker
	r       *Runner
	s       *Service

	mu       sync.Mutex
	now      time.Time
	ready    bool
	progress []mediaimport.Progress
}

func (e *svcEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *svcEnv) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func (e *svcEnv) setReady(ok bool) {
	e.mu.Lock()
	e.ready = ok
	e.mu.Unlock()
}

func (e *svcEnv) events() []mediaimport.Progress {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.progress)
}

// kicker is follow.Service.OnFollowed as boot wires it (followKicks): the
// core is told to track the title (EnsureTitle) and refresh it (Kick); a
// send the fake core refuses is queued for a retry.
type kicker struct {
	core *coretest.Tracking

	mu     sync.Mutex
	queued []follow.Follow
}

func (k *kicker) followed(ctx context.Context, f follow.Follow) {
	t, err := k.core.EnsureTitle(ctx, f.Kind, int64(f.TMDBID), f.Title, f.Poster)
	if err == nil {
		err = k.core.Kick(ctx, t.ID)
	}
	if err != nil {
		k.mu.Lock()
		k.queued = append(k.queued, f)
		k.mu.Unlock()
	}
}

func (k *kicker) waiting() []follow.Follow {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.queued)
}

// newSvcEnv is the addon's «Переезд» over the addon's database, the fake
// core's library and tracking, and an import folder (the data dir laid out
// as boot does: data/migrate/reports, data/tmp/migrate).
func newSvcEnv(t *testing.T, before func(e *svcEnv)) *svcEnv {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	data := t.TempDir()
	track := &coretest.Tracking{}
	e := &svcEnv{t: t, db: d, admin: coreAdmin, dir: t.TempDir(), tmp: filepath.Join(data, "tmp", "migrate"),
		reports: filepath.Join(data, "migrate", "reports"), idx: &fakeIndexer{}, acq: &swAcq{},
		track: track, kicks: &kicker{core: track},
		now: time.Date(2026, 10, 25, 19, 30, 0, 0, time.UTC), ready: true}
	if before != nil {
		before(e)
	}
	src, left := DefaultPaths(e.dir, filepath.Join(e.dir, "prowlarr", "prowlarr.db"))
	ctx, cancel := context.WithCancel(context.Background())
	e.r = NewRunner(&Runner{
		ReportDir: e.reports, TmpDir: e.tmp, BaseCtx: ctx, Now: e.clock,
		Progress: func(_ context.Context, p mediaimport.Progress) {
			e.mu.Lock()
			e.progress = append(e.progress, p)
			e.mu.Unlock()
		},
		LibraryReady: func(context.Context) (bool, string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.ready, "Медиатека ещё сканируется — подождите"
		},
	})
	lib := &coretest.Library{}
	e.s = New(e.r, Service{
		DB: d, Engine: fakeEngine{}, Indexer: e.idx, Library: lib,
		Follow: &follow.Service{DB: d, Acquire: e.acq, TMDB: swTMDB{}, Library: lib, Pause: -1, OnFollowed: e.kicks.followed,
			Now: func() time.Time { return time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) }},
		Paths: src, Leftover: left,
		QB: QBOptions{StateDir: filepath.Join(data, "migrate", "qbt-state"), LinkDir: filepath.Join(data, "links"),
			KeepPaused: func(context.Context) bool { return false }},
	})
	t.Cleanup(func() { cancel(); e.s.Wait() })
	return e
}

// sonarr puts a Sonarr database with n monitored series into the import
// folder.
func (e *svcEnv) sonarr(n int) {
	var rows []sonarrRow
	for i := 1; i <= n; i++ {
		rows = append(rows, sonarrRow{ID: i, Title: "Сериал " + strconv.Itoa(i), Year: 2000 + i, TvdbID: 100 + i, TmdbID: 1000 + i, Monitored: true})
	}
	e.place(sonarrDB(e.t, rows, nil, false), e.s.Paths.Sonarr)
}

func (e *svcEnv) place(src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// start runs req and waits for it to finish.
func (e *svcEnv) start(req mediaimport.RunRequest) *Report {
	e.t.Helper()
	return e.startAs(req, "")
}

// startAs is start with the admin's uid (StartAs).
func (e *svcEnv) startAs(req mediaimport.RunRequest, uid string) *Report {
	e.t.Helper()
	if req.AdminID == 0 {
		req.AdminID = e.admin
	}
	id, err := e.s.StartAs(context.Background(), req, uid)
	if err != nil {
		e.t.Fatal(err)
	}
	e.s.Wait()
	rep, err := LoadReport(e.reports, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return rep
}

func TestDiscoverSources(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.sonarr(15)
	sourceIDs := []string{"prowlarr", "sonarr", "radarr", "qbittorrent"}
	got := e.s.Discover(context.Background())
	if len(got) != len(sourceIDs) {
		t.Fatalf("sources %+v", got)
	}
	names := map[string]string{"prowlarr": "Prowlarr", "sonarr": "Sonarr", "radarr": "Radarr", "qbittorrent": "qBittorrent"}
	for i, st := range got {
		if st.ID != sourceIDs[i] || st.Name != names[st.ID] {
			t.Fatalf("source %d = %+v", i, st)
		}
		if st.ID == "sonarr" {
			if !st.Found || st.Detail != "15 сериалов, 15 отслеживается" || st.Error != "" || st.Path != e.s.Paths.Sonarr {
				t.Fatalf("sonarr %+v", st)
			}
			continue
		}
		if st.Found || !strings.HasPrefix(st.Detail, "Не найден — смонтируйте ") || !strings.HasSuffix(st.Detail, st.Path) {
			t.Fatalf("%s %+v", st.ID, st)
		}
	}
	if ents, _ := os.ReadDir(e.tmp); len(ents) != 0 {
		t.Fatalf("snapshots left in %s: %v", e.tmp, ents)
	}
}

func TestDiscoverDetails(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.place(radarrDB(t, []radarrRow{{Title: "Дюна", Year: 2021, TmdbID: 1, Monitored: true}, {Title: "Брат", Year: 1997, TmdbID: 2, MovieFileID: 3}}, false), e.s.Paths.Radarr)
	writeFile(t, filepath.Join(e.s.Paths.QBBackup, "a.torrent"), "x")
	writeFile(t, filepath.Join(e.s.Paths.QBBackup, "b.torrent"), "x")
	writeFile(t, filepath.Join(e.s.Paths.QBBackup, "a.fastresume"), "x")
	by := map[string]mediaimport.SourceStatus{}
	for _, st := range e.s.Discover(context.Background()) {
		by[st.ID] = st
	}
	if st := by["radarr"]; st.Detail != "2 фильма, 1 отслеживается" {
		t.Errorf("radarr %+v", st)
	}
	if st := by["qbittorrent"]; !st.Found || !strings.HasPrefix(st.Detail, "2 раздачи") || st.Error == "" {
		// no ANTRESOL_TORRENTS_QBITTORRENT_URL: found, but the run cannot reach it
		t.Errorf("qbittorrent %+v", st)
	}
}

func TestRunRequiresDryRun(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.sonarr(1)
	if _, err := e.s.Start(context.Background(), mediaimport.RunRequest{Sources: []string{"sonarr"}, AdminID: e.admin}); !errors.Is(err, mediaimport.ErrNeedsDryRun) {
		t.Fatalf("no plan: %v", err)
	}
	if _, err := e.s.Start(context.Background(), mediaimport.RunRequest{Sources: []string{"sonarr"}, PlanID: "20261025T193000Z-abcdef", AdminID: e.admin}); !errors.Is(err, mediaimport.ErrNeedsDryRun) {
		t.Fatalf("missing plan: %v", err)
	}
	plan := e.start(mediaimport.RunRequest{Sources: []string{"sonarr"}, DryRun: true})
	if !plan.DryRun || plan.FinishedAt.IsZero() || plan.PlanHash == "" {
		t.Fatalf("plan %+v", plan)
	}
	var f int
	e.db.QueryRow(`SELECT count(*) FROM follows`).Scan(&f)
	if f != 0 {
		t.Fatal("the dry run wrote follows")
	}
	// a real run is not a plan
	real := e.start(mediaimport.RunRequest{Sources: []string{"sonarr"}, PlanID: plan.ID})
	if real.DryRun || real.PlanID != plan.ID || real.FinishedAt.IsZero() {
		t.Fatalf("real %+v", real)
	}
	e.db.QueryRow(`SELECT count(*) FROM follows WHERE active=0 AND source='sonarr'`).Scan(&f)
	if f != 1 {
		t.Fatalf("follows %d", f)
	}
	if _, err := e.s.Start(context.Background(), mediaimport.RunRequest{Sources: []string{"sonarr"}, PlanID: real.ID, AdminID: e.admin}); !errors.Is(err, mediaimport.ErrNeedsDryRun) {
		t.Fatalf("a real report as plan: %v", err)
	}
}

func TestRunPlanExpires(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.sonarr(1)
	plan := e.start(mediaimport.RunRequest{Sources: []string{"sonarr"}, DryRun: true})
	e.advance(61 * time.Minute)
	if _, err := e.s.Start(context.Background(), mediaimport.RunRequest{Sources: []string{"sonarr"}, PlanID: plan.ID, AdminID: e.admin}); !errors.Is(err, mediaimport.ErrNeedsDryRun) {
		t.Fatalf("61 min: %v", err)
	}
	plan = e.start(mediaimport.RunRequest{Sources: []string{"sonarr"}, DryRun: true})
	e.advance(59 * time.Minute)
	e.start(mediaimport.RunRequest{Sources: []string{"sonarr"}, PlanID: plan.ID})
}

func TestRunPlanSourcesMismatch(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.sonarr(1)
	plan := e.start(mediaimport.RunRequest{Sources: []string{"sonarr"}, DryRun: true})
	if _, err := e.s.Start(context.Background(), mediaimport.RunRequest{Sources: []string{"sonarr", "radarr"}, PlanID: plan.ID, AdminID: e.admin}); !errors.Is(err, mediaimport.ErrNeedsDryRun) {
		t.Fatalf("other sources: %v", err)
	}
	// the order of the list does not matter
	plan = e.start(mediaimport.RunRequest{Sources: []string{"radarr", "sonarr"}, DryRun: true})
	e.start(mediaimport.RunRequest{Sources: []string{"sonarr", "radarr"}, PlanID: plan.ID})
}

func TestPlanHash(t *testing.T) {
	a := mediaimport.PlanHash([]string{"sonarr", "radarr"}, map[string]int64{"jellyfin:1": 2, "kavita:3": 4})
	if b := mediaimport.PlanHash([]string{"radarr", "sonarr"}, map[string]int64{"kavita:3": 4, "jellyfin:1": 2}); a != b || len(a) != 64 {
		t.Fatalf("%s != %s", a, b)
	}
	if mediaimport.PlanHash([]string{"sonarr"}, nil) != mediaimport.PlanHash([]string{"sonarr"}, map[string]int64{}) {
		t.Fatal("nil and empty map differ")
	}
	if a == mediaimport.PlanHash([]string{"sonarr", "radarr"}, map[string]int64{"jellyfin:1": 3, "kavita:3": 4}) {
		t.Fatal("the user map is not hashed")
	}
}

func TestRunRefusedUntilScanned(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.sonarr(1)
	e.setReady(false)
	for _, dry := range []bool{true, false} {
		if _, err := e.s.Start(context.Background(), mediaimport.RunRequest{Sources: []string{"sonarr"}, DryRun: dry, AdminID: e.admin}); !errors.Is(err, mediaimport.ErrNotReady) {
			t.Fatalf("dry=%v: %v", dry, err)
		}
	}
	if ents, _ := os.ReadDir(e.reports); len(ents) != 0 {
		t.Fatalf("reports %v", ents)
	}
}

func TestRunUnknownSource(t *testing.T) {
	e := newSvcEnv(t, nil)
	for _, src := range [][]string{{"navidrome"}, {"sonarr", "../x"}} {
		if _, err := e.s.Start(context.Background(), mediaimport.RunRequest{Sources: src, DryRun: true, AdminID: e.admin}); !errors.Is(err, mediaimport.ErrUnknownSource) {
			t.Fatalf("%v: %v", src, err)
		}
	}
	if _, err := e.s.Start(context.Background(), mediaimport.RunRequest{DryRun: true, AdminID: e.admin}); !errors.Is(err, mediaimport.ErrNoSources) {
		t.Fatalf("none: %v", err)
	}
}

// One run at a time: a second start, a rollback and a switch are ErrBusy
// while it runs; afterwards the runner is free again (a new run starts).
func TestRunBusy(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.idx.block = make(chan struct{})
	id, err := e.s.Start(context.Background(), mediaimport.RunRequest{Sources: []string{"prowlarr"}, DryRun: true, AdminID: e.admin})
	if err != nil {
		t.Fatal(err)
	}
	if e.s.Running() != id {
		t.Fatalf("running %q, want %q", e.s.Running(), id)
	}
	if _, err := e.s.Start(context.Background(), mediaimport.RunRequest{Sources: []string{"sonarr"}, DryRun: true, AdminID: e.admin}); !errors.Is(err, mediaimport.ErrBusy) {
		t.Fatalf("second: %v", err)
	}
	if _, err := e.s.Rollback(context.Background(), id); !errors.Is(err, mediaimport.ErrBusy) {
		t.Fatalf("rollback while running: %v", err)
	}
	if _, err := e.s.Switch(context.Background(), 0, true); !errors.Is(err, mediaimport.ErrBusy) {
		t.Fatalf("switch while running: %v", err)
	}
	close(e.idx.block)
	e.s.Wait()
	if e.s.Running() != "" {
		t.Fatal("still running")
	}
	e.sonarr(1)
	e.start(mediaimport.RunRequest{Sources: []string{"sonarr"}, DryRun: true})
}

func TestRunSurvivesClientCancel(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.idx.block = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	id, err := e.s.Start(ctx, mediaimport.RunRequest{Sources: []string{"prowlarr"}, DryRun: true, AdminID: e.admin})
	if err != nil {
		t.Fatal(err)
	}
	cancel() // the browser went away
	time.Sleep(50 * time.Millisecond)
	close(e.idx.block)
	e.s.Wait()
	rep, err := LoadReport(e.reports, id)
	if err != nil {
		t.Fatal(err)
	}
	if rep.FinishedAt.IsZero() || len(rep.Lines) != 1 || rep.Lines[0].Status != StatusPlanned {
		t.Fatalf("report %+v", rep)
	}
}

func TestRunOrder(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.sonarr(1)
	os.MkdirAll(filepath.Dir(e.s.Leftover.LibrarrDB), 0o755)
	librarrDB(t, e.s.Leftover.LibrarrDB, 1, 0)
	rep := e.start(mediaimport.RunRequest{Sources: []string{"qbittorrent", "sonarr", "prowlarr"}, DryRun: true})
	if strings.Join(rep.Sources, ",") != "prowlarr,sonarr,qbittorrent" {
		t.Fatalf("sources %v", rep.Sources)
	}
	if l := rep.Lines[0]; l.Kind != "leftover" || !strings.Contains(l.Reason, "Librarr") {
		t.Fatalf("first line %+v", l)
	}
	var order []string
	seen := map[string]bool{}
	for _, l := range rep.Lines[1:] {
		if l.Kind == "leftover" {
			t.Fatalf("leftover after the sources: %+v", l)
		}
		if !seen[l.Source] {
			seen[l.Source] = true
			order = append(order, l.Source)
		}
	}
	if strings.Join(order, ",") != "prowlarr,sonarr,qbittorrent" {
		t.Fatalf("line order %v (%+v)", order, rep.Lines)
	}
	if rep.Counts[StatusWarning] != 1 || rep.Counts[StatusPlanned] != 2 {
		t.Fatalf("counts %v", rep.Counts)
	}
	// one progress per source, the last says done == total and finished
	got := e.events()
	if len(got) != 3 {
		t.Fatalf("progress %+v", got)
	}
	for i, p := range got {
		if p.ReportID != rep.ID || p.Total != 3 || p.Done != i+1 || p.Source != rep.Sources[i] || p.Finished != (i == 2) {
			t.Fatalf("progress %d = %+v", i, p)
		}
	}
}

func TestRunCleansSnapshots(t *testing.T) {
	var old string
	e := newSvcEnv(t, func(e *svcEnv) {
		old = filepath.Join(e.tmp, "run-old")
		writeFile(t, filepath.Join(old, "prowlarr-1a2b3c4d"), "cookie=secret")
		writeFile(t, filepath.Join(e.tmp, "keep.txt"), "not ours")
	})
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("run-old left: %v", err)
	}
	os.Remove(filepath.Join(e.tmp, "keep.txt"))
	e.sonarr(2)
	e.start(mediaimport.RunRequest{Sources: []string{"sonarr", "prowlarr"}, DryRun: true})
	if ents, _ := os.ReadDir(e.tmp); len(ents) != 0 {
		t.Fatalf("left in tmp: %v", ents)
	}
}

// imported adds inactive follows: series 10 (Sonarr wanted all three
// episodes: one season pack) and movie 20.
func (e *svcEnv) imported() {
	ctx := context.Background()
	base := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	for _, in := range []follow.ImportFollow{
		{Kind: follow.KindSeries, TmdbID: 10, Title: "Сёгун", Source: follow.SourceSonarr,
			Import: &follow.ImportState{Baseline: base, Want: [][2]int{{1, 1}, {1, 2}, {1, 3}}}},
		{Kind: follow.KindMovie, TmdbID: 20, Title: "Дюна", Source: follow.SourceRadarr},
	} {
		if _, err := follow.ImportInactive(ctx, e.db, addonkit.User{ID: e.admin, UID: "uid-admin"}, in); err != nil {
			e.t.Fatal(err)
		}
	}
}

func TestPendingFollows(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.imported()
	ctx := context.Background()
	// Vault's own follows (active or not) and an active imported one do not count
	if _, err := e.db.Exec(`INSERT INTO follows (kind, tmdb_id, title, active, source, created_at) VALUES
		('movie', 30, 'Своё', 0, 'vault', '2026-01-01T00:00:00Z'), ('movie', 31, 'Своё 2', 1, 'vault', '2026-01-01T00:00:00Z'),
		('series', 32, 'Уже включён', 1, 'sonarr', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if n, err := e.s.PendingFollows(ctx); err != nil || n != 2 {
		t.Fatalf("pending %d %v", n, err)
	}
}

func TestSwitchConfirmsCount(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.imported()
	ctx := context.Background()
	plan, err := e.s.SwitchPreview(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if plan != (SwitchPlan{Follows: 2, Episodes: 3, Movies: 1}) {
		t.Fatalf("plan %+v", plan)
	}
	if len(e.acq.calls) != 0 {
		t.Fatal("the preview downloaded")
	}
	if _, err := e.s.Switch(ctx, 3, true); !errors.Is(err, ErrCountChanged) {
		t.Fatalf("confirm 3: %v", err)
	}
	if _, err := e.s.Switch(ctx, 4, false); !errors.Is(err, ErrArrNotStopped) {
		t.Fatalf("arr running: %v", err)
	}
	if n, _ := e.s.PendingFollows(ctx); n != 2 {
		t.Fatalf("refused switches activated follows: %d pending", n)
	}
	n, err := e.s.Switch(ctx, 4, true)
	if err != nil || n != 2 {
		t.Fatalf("switch %d %v", n, err)
	}
	var active int
	e.db.QueryRow(`SELECT count(*) FROM follows WHERE active=1`).Scan(&active)
	if active != 2 {
		t.Fatalf("active %d", active)
	}
}

func TestSwitchPreviewTMDBError(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.imported()
	if _, err := follow.ImportInactive(context.Background(), e.db, addonkit.User{ID: e.admin, UID: "uid-admin"}, follow.ImportFollow{Kind: follow.KindMovie, TmdbID: 99, Title: "Неизвестный", Source: follow.SourceRadarr}); err != nil {
		t.Fatal(err)
	}
	plan, err := e.s.SwitchPreview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if plan != (SwitchPlan{Follows: 3, Episodes: 3, Movies: 1, Errors: 1}) {
		t.Fatalf("plan %+v", plan)
	}
}

func TestSwitchActivatesOnce(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.imported()
	ctx := context.Background()
	if st, err := e.s.State(ctx); err != nil || st.SwitchedAt != nil {
		t.Fatalf("state before %+v %v", st, err)
	}
	if n, err := e.s.Switch(ctx, 4, true); err != nil || n != 2 {
		t.Fatalf("first %d %v", n, err)
	}
	st, err := e.s.State(ctx)
	if err != nil || st.SwitchedAt == nil || !st.SwitchedAt.Equal(e.clock()) {
		t.Fatalf("state %+v %v", st, err)
	}
	e.advance(time.Hour)
	if n, err := e.s.Switch(ctx, 0, true); err != nil || n != 0 {
		t.Fatalf("second %d %v", n, err)
	}
	if n, err := e.s.Switch(ctx, 4, true); err != nil || n != 0 {
		t.Fatalf("third %d %v", n, err)
	}
	if st2, _ := e.s.State(ctx); !st2.SwitchedAt.Equal(*st.SwitchedAt) {
		t.Fatalf("switchedAt moved: %v → %v", st.SwitchedAt, st2.SwitchedAt)
	}
}

func TestSwitchNeedsFollowService(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.imported()
	e.s.Follow = nil
	if _, err := e.s.SwitchPreview(context.Background()); !errors.Is(err, ErrNoFollow) {
		t.Fatalf("preview: %v", err)
	}
	if _, err := e.s.Switch(context.Background(), 4, true); !errors.Is(err, ErrNoFollow) {
		t.Fatalf("switch: %v", err)
	}
}

func TestRollbackReport(t *testing.T) {
	e := newSvcEnv(t, nil)
	qb := fakeQB(t)
	e.s.QB.WebURL = qb.URL
	ih := strings.Repeat("ab", 20)
	from := &Report{ID: NewReportID(e.clock()), Sources: []string{"qbittorrent"}, StartedAt: e.clock(), FinishedAt: e.clock(),
		Lines: []Line{{Source: SourceQBittorrent, Kind: "torrent", Name: "Дюна", Status: StatusImported, Ref: ih, Prev: PrevStopped}}}
	if err := SaveReport(e.reports, from); err != nil {
		t.Fatal(err)
	}
	id, err := e.s.Rollback(context.Background(), from.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	rep, err := LoadReport(e.reports, id)
	if err != nil {
		t.Fatal(err)
	}
	if rep.RollbackOf != from.ID || rep.FinishedAt.IsZero() || len(rep.Lines) != 1 || !strings.HasPrefix(rep.Lines[0].Reason, "Возвращена в qBittorrent") {
		t.Fatalf("rollback %+v", rep)
	}
	if len(qb.posts()) != 0 {
		t.Fatalf("a paused torrent was started: %+v", qb.posts())
	}
	// a rollback report, a dry run and a report without torrents cannot be rolled back
	if _, err := e.s.Rollback(context.Background(), id); !errors.Is(err, ErrNothingToRollback) {
		t.Fatalf("rollback of a rollback: %v", err)
	}
	dry := &Report{ID: NewReportID(e.clock()), DryRun: true, StartedAt: e.clock(), FinishedAt: e.clock(),
		Lines: []Line{{Source: SourceQBittorrent, Status: StatusPlanned, Ref: ih}}}
	SaveReport(e.reports, dry)
	if _, err := e.s.Rollback(context.Background(), dry.ID); !errors.Is(err, ErrNothingToRollback) {
		t.Fatalf("dry: %v", err)
	}
	if _, err := e.s.Rollback(context.Background(), "20261025T193000Z-000000"); !errors.Is(err, mediaimport.ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := e.s.Rollback(context.Background(), "../etc"); !errors.Is(err, mediaimport.ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}

func TestReports(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.sonarr(1)
	plan := e.start(mediaimport.RunRequest{Sources: []string{"sonarr"}, DryRun: true})
	ih := strings.Repeat("cd", 20)
	real := &Report{ID: NewReportID(e.clock().Add(time.Minute)), Sources: []string{"qbittorrent"}, StartedAt: e.clock().Add(time.Minute),
		Lines: []Line{{Source: SourceQBittorrent, Status: StatusImported, Ref: ih}, {Source: SourceQBittorrent, Status: StatusSkipped, Ref: ih}}}
	SaveReport(e.reports, real)
	list, err := e.s.Reports()
	if err != nil || len(list) != 2 {
		t.Fatalf("reports %+v %v", list, err)
	}
	if list[0].ID != real.ID || e.s.Torrents(list[0].ID) != 1 || list[0].Lines != nil || list[1].ID != plan.ID || e.s.Torrents(list[1].ID) != 0 {
		t.Fatalf("reports %+v %+v", list[0], list[1])
	}
	if _, err := e.s.Report("..%2f.."); !errors.Is(err, mediaimport.ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
}

// fullAcq: the media disk is full.
type fullAcq struct{ swAcq }

func (*fullAcq) Disk() (acquire.DiskUsage, error) {
	return acquire.DiskUsage{Level: acquire.LevelFull}, nil
}

func TestSwitchBlockedOnFullDisk(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.imported()
	e.s.Follow.Acquire = &fullAcq{}
	ctx := context.Background()
	plan, err := e.s.SwitchPreview(ctx)
	if err != nil || plan.Blocked == "" || plan.Episodes+plan.Movies != 0 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if _, err := e.s.Switch(ctx, 0, true); !errors.Is(err, ErrSwitchBlocked) {
		t.Fatalf("switch on a full disk: %v", err)
	}
	if n, _ := e.s.PendingFollows(ctx); n != 2 {
		t.Fatalf("activated on a full disk: %d pending", n)
	}
}

// The screen's status reads the old databases once while they stay the
// same: Discover comes from a cache keyed by each
// source file (path, size, mtime); a changed file is read again.
func TestDiscoverCached(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.sonarr(15)
	ctx := context.Background()
	scan := func() { e.s.Discover(ctx) }
	scan()
	first := e.s.Scans()
	if first != 1 {
		t.Fatalf("first look: %d scans, want 1", first)
	}
	scan()
	scan()
	if got := e.s.Scans(); got != first {
		t.Fatalf("unchanged sources were read again: %d scans after %d", got, first)
	}
	e.sonarr(16)
	later := time.Now().Add(time.Minute)
	os.Chtimes(e.s.Paths.Sonarr, later, later)
	var sonarr mediaimport.SourceStatus
	for _, st := range e.s.Discover(ctx) {
		if st.ID == SourceSonarr {
			sonarr = st
		}
	}
	if sonarr.Detail != "16 сериалов, 16 отслеживается" || e.s.Scans() != first+1 {
		t.Fatalf("changed Sonarr: %+v, %d scans", sonarr, e.s.Scans())
	}
	// «Проверить» looks afresh
	e.start(mediaimport.RunRequest{Sources: []string{"sonarr"}, DryRun: true})
	before := e.s.Scans()
	e.s.Discover(ctx)
	if e.s.Scans() != before+1 {
		t.Fatalf("a dry run did not refresh the cache: %d → %d", before, e.s.Scans())
	}
}

// A report is given back to qBittorrent once: a second rollback of it is
// refused (a later run may have moved the same torrents again).
func TestRollbackOnce(t *testing.T) {
	e := newSvcEnv(t, nil)
	qb := fakeQB(t)
	e.s.QB.WebURL = qb.URL
	ih := strings.Repeat("ab", 20)
	from := &Report{ID: NewReportID(e.clock()), Sources: []string{"qbittorrent"}, StartedAt: e.clock(), FinishedAt: e.clock(),
		Lines: []Line{{Source: SourceQBittorrent, Kind: "torrent", Name: "Дюна", Status: StatusImported, Ref: ih, Prev: PrevStopped}}}
	if err := SaveReport(e.reports, from); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := e.s.Rollback(ctx, from.ID); err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	e.advance(time.Minute)
	if _, err := e.s.Rollback(ctx, from.ID); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("second rollback: %v", err)
	}
}

// A rollback cut short by a restart may be finished later, unless a real
// run has started since (it may have moved the same torrents again).
func TestRollbackInterruptedResumes(t *testing.T) {
	e := newSvcEnv(t, nil)
	qb := fakeQB(t)
	e.s.QB.WebURL = qb.URL
	ih := strings.Repeat("ab", 20)
	from := &Report{ID: NewReportID(e.clock()), Sources: []string{"qbittorrent"}, StartedAt: e.clock(), FinishedAt: e.clock(),
		Lines: []Line{{Source: SourceQBittorrent, Kind: "torrent", Name: "Дюна", Status: StatusImported, Ref: ih, Prev: PrevStopped}}}
	e.advance(time.Minute)
	cut := &Report{ID: NewReportID(e.clock()), Sources: []string{"qbittorrent"}, RollbackOf: from.ID, StartedAt: e.clock(), Lines: []Line{}}
	for _, r := range []*Report{from, cut} {
		if err := SaveReport(e.reports, r); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	e.advance(time.Minute)
	later := &Report{ID: NewReportID(e.clock()), Sources: []string{"qbittorrent"}, StartedAt: e.clock(), FinishedAt: e.clock(), Lines: []Line{}}
	if err := SaveReport(e.reports, later); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Rollback(ctx, from.ID); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("rollback after a later run: %v", err)
	}
	if err := os.Remove(filepath.Join(e.reports, later.ID+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Rollback(ctx, from.ID); err != nil {
		t.Fatalf("finishing an interrupted rollback: %v", err)
	}
	e.s.Wait()
}
