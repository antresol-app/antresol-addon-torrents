package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func open(t *testing.T, p string) *sql.DB {
	t.Helper()
	d, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func versions(t *testing.T, d *sql.DB) []int {
	t.Helper()
	rows, err := d.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// TestStore_MigratesFresh: a new torrents.db gets the twelve replayed
// migrations, the addon's own ones (0013, 0014, 0015) and the family's tables; no core
// table is referenced (a follow of user 42 goes in with no users table).
func TestStore_MigratesFresh(t *testing.T) {
	d := open(t, filepath.Join(t.TempDir(), "torrents.db"))
	if got, want := versions(t, d), []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}; !slices.Equal(got, want) {
		t.Fatalf("versions %v, want %v", got, want)
	}
	for _, tbl := range []string{"torrents", "torrent_pieces", "sources", "acquisitions", "follows", "release_attempts", "release_switch_failures", "pinned_releases"} {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s: %d %v", tbl, n, err)
		}
	}
	for _, tbl := range []string{"users", "tracked_titles", "title_marks", "items"} {
		var n int
		d.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, tbl).Scan(&n)
		if n != 0 {
			t.Fatalf("core table %s in torrents.db", tbl)
		}
	}
	if _, err := d.Exec(`INSERT INTO follows (kind, tmdb_id, title, user_id, created_at) VALUES ('movie', 1, 'Дюна', 42, 'x')`); err != nil {
		t.Fatalf("follow of user 42: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO pinned_releases (title_id, infohash, pinned_by, pinned_at) VALUES (7, 'aa', 42, 'x')`); err != nil {
		t.Fatalf("pin of title 7: %v", err)
	}
	// 0015: the rows' profile by uid
	for _, col := range [][2]string{{"follows", "user_uid"}, {"pinned_releases", "pinned_by_uid"}, {"torrents", "added_by_uid"}, {"acquisitions", "added_by_uid"}} {
		var n int
		if err := d.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?`, col[0], col[1]).Scan(&n); err != nil || n != 1 {
			t.Fatalf("column %s.%s: %d %v", col[0], col[1], n, err)
		}
	}
	var fk int
	if err := d.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys %d %v", fk, err)
	}
	var mode string
	if err := d.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode %q %v", mode, err)
	}
}

// TestStore_Reopen: a second open applies nothing again (the rows stay).
func TestStore_Reopen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "torrents.db")
	d, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO sources (id, definition_id, name) VALUES (1, 'kinozal', 'Kinozal'), (2, 'kinozal', 'Kinozal')`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d = open(t, p)
	if got := versions(t, d); len(got) != 15 {
		t.Fatalf("versions %v", got)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM sources`).Scan(&n)
	if n != 2 { // 0008's dedupe ran once, on the empty table
		t.Fatalf("sources %d after reopen", n)
	}
}

// TestStore_RefusesDuplicateVersion: two files of one version apply nothing.
func TestStore_RefusesDuplicateVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"m/0001_a.sql": {Data: []byte(`CREATE TABLE a (x)`)},
		"m/0001_b.sql": {Data: []byte(`CREATE TABLE b (x)`)},
	}
	d, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	err = migrate(d, fsys, "m")
	if err == nil || !strings.Contains(err.Error(), "used twice") {
		t.Fatalf("err %v", err)
	}
	var n int
	d.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d objects after a refused set", n)
	}
}

// TestSourcesDedupe (0008, the core's 0040): of exact copies of a source
// the oldest stays; a source that differs in any of those is kept.
func TestSourcesDedupe(t *testing.T) {
	p := filepath.Join(t.TempDir(), "torrents.db")
	d := open(t, p)
	for _, q := range []string{
		`INSERT INTO sources (id, definition_id, name, settings_json) VALUES
		  (1, 'noname-club', 'NoNaMe Club', '{"cookie":"a=1"}'),
		  (2, 'noname-club', 'NoNaMe Club', '{"cookie":"a=1"}'),
		  (3, 'noname-club', 'NoNaMe Club', '{"cookie":"a=2"}'),
		  (4, 'noname-club', 'NoNaMe Club 2', '{"cookie":"a=1"}'),
		  (5, 'noname-clubl', 'NoNaMe Club', '{"cookie":"a=1"}'),
		  (6, 'noname-club', 'NoNaMe Club', '{"cookie":"a=1"}'),
		  (7, 'kinozal', 'Kinozal', '{}')`,
		`DELETE FROM schema_migrations WHERE version = 8`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close()
	d = open(t, p)
	rows, err := d.Query(`SELECT id FROM sources ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if want := []int{1, 3, 4, 5, 7}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sources %v, want %v", got, want)
	}
}

// TestFollowQuality (0012, the core's 10002 without its marks): every
// follow keeps what it was and gains the household resolution (”).
func TestFollowQuality(t *testing.T) {
	p := filepath.Join(t.TempDir(), "torrents.db")
	d := open(t, p)
	for _, q := range []string{
		`INSERT INTO follows (id, kind, tmdb_id, title, user_id, active, created_at) VALUES
		  (1, 'movie', 100, 'Дюна', 1, 1, 'x'), (2, 'movie', 200, 'Брат', 2, 0, 'x'), (3, 'series', 400, 'Тьма', NULL, 1, 'x')`,
		`ALTER TABLE follows DROP COLUMN resolution`,
		`ALTER TABLE pinned_releases DROP COLUMN stale_json`,
		`DELETE FROM schema_migrations WHERE version = 12`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	d.Close()
	d = open(t, p)
	rows, err := d.Query(`SELECT id, active, resolution FROM follows ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var id, active int
		var res string
		if err := rows.Scan(&id, &active, &res); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%d:%q", id, active, res))
	}
	if want := `[1:1:"" 2:0:"" 3:1:""]`; fmt.Sprint(got) != want {
		t.Fatalf("follows %v, want %s", got, want)
	}
}
