package boot

import (
	"context"
	"path/filepath"
	"sync/atomic"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/migrate"
	"github.com/antresol-app/antresol/addonkit"
)

// buildMigrate makes «Переезд» (Services.Migrate) on the addon's own
// parts: the booted engine (so a moved download is attached as acquire's
// are, under the no-seeding boot decided), the sources, the follows (nil
// without TMDB: no «Перейти на Antresol»), the core's library. The old
// services are under Conf.ImportDir; qBittorrent's link trees go under
// the downloads folder and its move state into the data dir, as the
// old build had them. A rollback, or a move that failed after
// qBittorrent stopped a torrent, keeps the torrents paused in qBittorrent
// while no-seeding is on. Progress goes to the app as
// migrate.EventProgress.
func (s *Services) buildMigrate(ctx context.Context, tm *tmdb.Client, client *addonkit.Client) *migrate.Service {
	c := s.Conf
	lib := coreadapt.Library{C: client}
	r := migrate.NewRunner(&migrate.Runner{
		ReportDir:    filepath.Join(c.DataDir, "migrate", "reports"),
		TmpDir:       filepath.Join(c.DataDir, "tmp", "migrate"),
		BaseCtx:      ctx,
		Progress:     migrate.PublishTo(client, s.log()),
		LibraryReady: libraryReady(lib.LibraryPaths),
		Log:          s.log().With("component", "migrate"),
	})
	var roots []string
	for _, p := range []string{s.Info.LibraryRoots.Movies, s.Info.LibraryRoots.Series} {
		if p != "" {
			roots = append(roots, p)
		}
	}
	paths, left := migrate.DefaultPaths(c.ImportDir, c.ProwlarrDB)
	m := migrate.Service{
		DB: s.DB, Engine: s.Engine, Indexer: s.Sources, Follow: s.Follows, Library: lib,
		Paths: paths, Leftover: left, FlareSolverrSet: c.FlareSolverr != "",
		QB: migrate.QBOptions{
			WebURL: c.QBURL, Username: c.QBUser, Password: c.QBPassword,
			LinkDir:      filepath.Join(c.DownloadDir, ".qbt-import"),
			StateDir:     filepath.Join(c.DataDir, "migrate", "qbt-state"),
			LibraryRoots: roots,
			KeepPaused:   s.keepPaused,
		},
		HoldsChanged: s.holdsChanged,
	}
	if tm != nil {
		m.TMDB = tm // never a typed nil in the interface
	}
	return migrate.New(r, m)
}

// keepPaused: a rollback (and a failed move's give-back) leaves the
// torrents paused in qBittorrent while no-seeding is in effect; no reader, or an unreadable setting, counts as
// on (fail closed).
func (s *Services) keepPaused(ctx context.Context) bool {
	if s.NoSeed == nil {
		return true
	}
	on, _ := s.NoSeed.Effective(ctx)
	return on
}

// libraryReady is Runner.LibraryReady: the library counts as scanned once
// it has a file (paths: GET /library/paths), and then for good (the page
// polls it).
func libraryReady(paths func(ctx context.Context) ([]string, error)) func(ctx context.Context) (bool, string) {
	var ready atomic.Bool
	return func(ctx context.Context) (bool, string) {
		if ready.Load() {
			return true, ""
		}
		ps, err := paths(ctx)
		if err != nil || len(ps) == 0 {
			return false, ""
		}
		ready.Store(true)
		return true, ""
	}
}
