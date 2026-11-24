// Package store opens the addon's own database (torrents.db) and applies
// its embedded migrations. migrations/0001…0012 replay the core's private
// family migrations 0005 0006 0008 0014 0016 0022 0030 0040 0041 0042
// 10001 10002 in that order, without the references to core tables
// (users, tracked_titles), which this database does not have; 0013 is the
// addon's own state (settings, import_state, import_refs), 0014 the follows'
// user_gone mark, 0015 the rows' profile uid (follows.user_uid,
// pinned_releases.pinned_by_uid, torrents.added_by_uid,
// acquisitions.added_by_uid), 0016 where a source came from
// (sources.origin, sources.synced_at), 0017 when a source last answered
// the tracking scan of a title (torrent_scan_answers).
package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open opens (creating if needed) the database at file with the core's
// pragmas (WAL, synchronous NORMAL, foreign keys, busy timeout, immediate
// transactions) and applies the pending migrations in version order.
func Open(file string) (*sql.DB, error) {
	dsn := "file:" + file + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// pragmas are per connection (DSN); _txlock=immediate + busy_timeout
	// serialise the writers; all 8 stay open between bursts
	d.SetMaxOpenConns(8)
	d.SetMaxIdleConns(8)
	if err := migrate(d, migrationsFS, "migrations"); err != nil {
		d.Close()
		return nil, err
	}
	return d, nil
}

type migration struct {
	ver  int
	name string
}

// list returns dir's .sql files by version number; a version used twice is
// an error (only the version is recorded: the second file would be taken
// for applied).
func list(fsys fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		ver, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0])
		if err != nil {
			return nil, fmt.Errorf("migration %s: bad version prefix", e.Name())
		}
		if prev, dup := seen[ver]; dup {
			return nil, fmt.Errorf("migration version %d used twice: %s and %s", ver, prev, e.Name())
		}
		seen[ver] = e.Name()
		out = append(out, migration{ver, e.Name()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ver < out[j].ver })
	return out, nil
}

// migrate applies the files of dir not yet in schema_migrations, each in a
// transaction of its own. The set is checked before the database is
// touched: a refused set applies nothing.
func migrate(d *sql.DB, fsys fs.FS, dir string) error {
	all, err := list(fsys, dir)
	if err != nil {
		return err
	}
	if _, err := d.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY)`); err != nil {
		return err
	}
	for _, m := range all {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`, m.ver).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := fs.ReadFile(fsys, path.Join(dir, m.name))
		if err != nil {
			return err
		}
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version) VALUES(?)`, m.ver); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
