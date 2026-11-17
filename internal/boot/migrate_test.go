package boot

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/migrate"
	"github.com/antresol-app/antresol/addonkit"
)

// TestBuild_WiresMigrate: Build makes «Переезд» over the addon's own
// parts: the booted engine (the one no-seeding decided), the sources,
// the follows, the core's library; the old services under ImportDir;
// qBittorrent from the conf with the link tree under the downloads and
// the move state in the data dir; reports and snapshots in the data dir;
// the holds told through HoldsChanged as it is when called; a rollback
// keeps the torrents paused exactly while no-seeding is on; the library
// counts as ready once it has files; progress goes out as
// admin.migrate.progress.
func TestBuild_WiresMigrate(t *testing.T) {
	b, c := realBoot(t, "")
	c.ImportDir, c.ProwlarrDB = "/old", "/p/prowlarr.db"
	c.QBURL, c.QBUser, c.QBPassword, c.FlareSolverr = "http://qb", "u", "p", "http://fs"
	tc := &trackCore{}
	client := tc.client(t)
	info := addonkit.Info{LibraryRoots: addonkit.LibraryRoots{Movies: "/m/movies", Series: "/m/series"}}
	s, err := Build(context.Background(), b, c, client, info)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Acq.Close()
	m := s.Migrate
	if m == nil {
		t.Fatal("no migrate service")
	}
	if m.Engine != s.Engine || m.DB != s.DB || m.Indexer != s.Sources || m.Follow != s.Follows || m.Follow == nil || m.TMDB == nil || !m.FlareSolverrSet {
		t.Fatalf("migrate %+v", m)
	}
	if l, ok := m.Library.(coreadapt.Library); !ok || l.C != client {
		t.Fatalf("library %#v", m.Library)
	}
	wantPaths, wantLeft := migrate.DefaultPaths("/old", "/p/prowlarr.db")
	if m.Paths != wantPaths || m.Leftover != wantLeft {
		t.Fatalf("paths %+v %+v", m.Paths, m.Leftover)
	}
	q := m.QB
	if q.WebURL != "http://qb" || q.Username != "u" || q.Password != "p" ||
		q.LinkDir != filepath.Join(c.DownloadDir, ".qbt-import") || q.StateDir != filepath.Join(c.DataDir, "migrate", "qbt-state") ||
		!slices.Equal(q.LibraryRoots, []string{"/m/movies", "/m/series"}) {
		t.Fatalf("qb %+v", q)
	}
	if m.ReportDir != filepath.Join(c.DataDir, "migrate", "reports") || m.TmpDir != filepath.Join(c.DataDir, "tmp", "migrate") {
		t.Fatalf("dirs %s %s", m.ReportDir, m.TmpDir)
	}
	// HoldsChanged is read when called (main sets it after Build)
	n := 0
	s.HoldsChanged = func() { n++ }
	m.HoldsChanged()
	if n != 1 {
		t.Fatalf("holds told %d", n)
	}
	ctx := context.Background()
	if q.KeepPaused == nil || q.KeepPaused(ctx) {
		t.Fatal("kept paused with no-seeding off")
	}
	if err := s.NoSeed.Set(ctx, true); err != nil {
		t.Fatal(err)
	}
	if !q.KeepPaused(ctx) {
		t.Fatal("not kept paused with no-seeding on")
	}
	if ok, _ := m.Ready(ctx); !ok {
		t.Fatal("the golden library has files: ready")
	}
	m.Progress(ctx, migrate.Progress{ReportID: "r"})
	if got := tc.seen(); !slices.Contains(got, "event admin.migrate.progress") {
		t.Fatalf("core calls %v", got)
	}
}

// The library counts as ready only once it has files, and stays so
// without asking again.
func TestLibraryReady(t *testing.T) {
	var asked int
	paths := []string{}
	ready := libraryReady(func(context.Context) ([]string, error) { asked++; return paths, nil })
	if ok, _ := ready(context.Background()); ok {
		t.Fatal("ready while empty")
	}
	paths = []string{"/m/movies/a.mkv"}
	if ok, _ := ready(context.Background()); !ok {
		t.Fatal("not ready with a file")
	}
	ready(context.Background())
	if asked != 2 {
		t.Fatalf("asked %d", asked)
	}
}

// «Перейти на Antresol» through boot: the switched-on follows reach the
// core's tracking (ensure + kick) by the follows' kick loop.
func TestMigrateSwitch_KicksThroughBoot(t *testing.T) {
	tc := &trackCore{}
	s := buildTracked(t, tc)
	ctx := context.Background()
	if _, err := follow.ImportInactive(ctx, s.DB, addonkit.User{ID: 7, UID: "uid-7"}, follow.ImportFollow{Kind: follow.KindSeries, TmdbID: 42, Title: "Сёгун", Source: follow.SourceSonarr,
		Import: &follow.ImportState{Baseline: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Follows.ActivateImported(ctx); err != nil || n != 1 {
		t.Fatalf("activate %d %v", n, err)
	}
	tc.waitCalls(t, "ensure series 42", "kick 7")
}

// No no-seeding reader: a rollback keeps the torrents paused (fail
// closed), never resuming sharing in qBittorrent.
func TestKeepPaused_NoReaderFailsClosed(t *testing.T) {
	if !(&Services{}).keepPaused(context.Background()) {
		t.Fatal("no reader resumed sharing")
	}
}
