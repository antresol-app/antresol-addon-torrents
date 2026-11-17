package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/releaseprefs"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	_ "modernc.org/sqlite"
)

// fixture is a vault.db built from testdata/vault-main.sql (WAL mode) with
// the fileDirs files beside it; edit (optional) runs on it before it closes.
type fixture struct {
	from, fromDataDir, dataDir string
	db                         *sql.DB // torrents.db
}

func newFixture(t *testing.T, edit ...string) fixture {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	from := filepath.Join(src, "vault.db")
	sqlText, err := os.ReadFile("testdata/vault-main.sql")
	if err != nil {
		t.Fatal(err)
	}
	v, err := sql.Open("sqlite", "file:"+from+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Exec(string(sqlText)); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, e := range edit {
		if _, err := v.Exec(e); err != nil {
			t.Fatalf("fixture edit %q: %v", e, err)
		}
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}
	for rel, body := range map[string]string{
		"torrents/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.torrent": "d4:infoe",
		"definitions/x.yml":   "id: x\n",
		"import-tokens/a":     "token",
		"migrate/qbt-state/s": "state",
	} {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data := filepath.Join(root, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := store.Open(filepath.Join(data, "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return fixture{from: from, fromDataDir: src, dataDir: data, db: d}
}

func (f fixture) opts() Options {
	return Options{From: f.from, DataDir: f.dataDir, DB: f.db, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func count(t *testing.T, d *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestPhase1_CountsEqualFixture(t *testing.T) {
	f := newFixture(t)
	c, err := Phase1(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"torrents": 3, "torrent_pieces": 3, "sources": 2, "acquisitions": 4, "follows": 2,
		"release_attempts": 1, "release_switch_failures": 1, "pinned_releases": 1, "refs": 4}
	for k, n := range want {
		if c[k] != n {
			t.Errorf("Counts[%s] = %d, want %d (all %v)", k, c[k], n, c)
		}
	}
	for _, tbl := range familyTables {
		if got := count(t, f.db, tbl); got != want[tbl] {
			t.Errorf("torrents.db %s has %d rows, want %d", tbl, got, want[tbl])
		}
	}
	if got := count(t, f.db, "import_refs"); got != 4 {
		t.Errorf("import_refs %d, want 4", got)
	}
	st, ok, err := LoadState(context.Background(), f.db)
	if err != nil || !ok {
		t.Fatalf("LoadState: %v %v", ok, err)
	}
	if st.Phase != 1 || st.Source != f.from || st.Fingerprint == "" || st.Stamp == "" || st.Counts["torrents"] != 3 {
		t.Fatalf("state %+v", st)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "import")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data/import left behind: %v", err)
	}
}

func TestPhase1_Settings(t *testing.T) {
	f := newFixture(t)
	if _, err := Phase1(context.Background(), f.opts()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s := store.Settings{DB: f.db}
	if on, err := s.NoSeeding(ctx); err != nil || !on {
		t.Fatalf("NoSeeding = %v, %v", on, err)
	}
	if limited, kbs, err := s.Upload(ctx); err != nil || !limited || kbs != 512 {
		t.Fatalf("Upload = %v, %d, %v", limited, kbs, err)
	}
	if gb, err := s.StreamCacheGB(ctx, 1); err != nil || gb != 30 {
		t.Fatalf("StreamCacheGB = %v, %v", gb, err)
	}
	// copied as they are
	src, err := sql.Open("sqlite", "file:"+f.from+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, k := range []string{releaseprefs.SettingsKey, "migration", "import_token_key"} {
		var a, b string
		if err := src.QueryRow(`SELECT value FROM settings WHERE key=?`, k).Scan(&a); err != nil {
			t.Fatal(err)
		}
		if err := f.db.QueryRow(`SELECT value FROM settings WHERE key=?`, k).Scan(&b); err != nil || a != b {
			t.Fatalf("settings[%s] = %q, %v; want %q", k, b, err, a)
		}
	}
	if _, err := (releaseprefs.Store{DB: f.db}).Load(ctx); err != nil {
		t.Fatalf("release prefs do not load: %v", err)
	}
	var app int
	f.db.QueryRow(`SELECT count(*) FROM settings WHERE key='app_settings'`).Scan(&app)
	if app != 0 {
		t.Fatal("app_settings copied as a row")
	}
}

func TestPhase1_RefsLowerCase(t *testing.T) {
	f := newFixture(t)
	if _, err := Phase1(context.Background(), f.opts()); err != nil {
		t.Fatal(err)
	}
	rows, err := f.db.Query(`SELECT path, ref, state FROM import_refs ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var p, r, st string
		if err := rows.Scan(&p, &r, &st); err != nil {
			t.Fatal(err)
		}
		if st != "pending" {
			t.Errorf("%s state %s", p, st)
		}
		got[p] = r
	}
	b := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, p := range []string{"/media/series/Sample Series/S01E01.mkv", "/media/series/Sample Series/S01E02.mkv"} {
		if got[p] != b {
			t.Errorf("ref of %s = %q, want lower-case %s", p, got[p], b)
		}
	}
	if _, ok := got["/media/movies/Sample Movie (2001)/extra.mkv"]; ok {
		t.Error("a file without an infohash got a ref")
	}
}

func TestPhase1_ExtraAndMissingColumns(t *testing.T) {
	f := newFixture(t, `ALTER TABLE acquisitions ADD COLUMN future TEXT`, `UPDATE acquisitions SET future='x'`)
	if _, err := f.db.Exec(`ALTER TABLE acquisitions DROP COLUMN meta_json`); err != nil {
		t.Fatal(err)
	}
	if _, err := Phase1(context.Background(), f.opts()); err != nil {
		t.Fatal(err)
	}
	var title string
	var year int
	if err := f.db.QueryRow(`SELECT title, year FROM acquisitions WHERE infohash='cccccccccccccccccccccccccccccccccccccccc'`).Scan(&title, &year); err != nil || title != "Sample Album" || year != 2010 {
		t.Fatalf("acquisition columns: %q %d %v", title, year, err)
	}
	if got := count(t, f.db, "acquisitions"); got != 4 {
		t.Fatalf("acquisitions %d", got)
	}
}

func TestPhase1_RefusesNonEmpty(t *testing.T) {
	for _, ins := range []string{
		`INSERT INTO sources (id, definition_id, name, enabled, settings_json, status, priority, created_at) VALUES (9, 'd', 'n', 1, '{}', 'ok', 1, 'now')`,
		`INSERT INTO torrents (infohash, name, mode, save_dir, added_at, status) VALUES ('eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee', 'n', 'download', '/x', 'now', 'paused')`,
	} {
		f := newFixture(t)
		if _, err := f.db.Exec(ins); err != nil {
			t.Fatal(err)
		}
		if _, err := Phase1(context.Background(), f.opts()); !errors.Is(err, ErrNotEmpty) {
			t.Fatalf("err = %v, want ErrNotEmpty", err)
		}
		if _, ok, _ := LoadState(context.Background(), f.db); ok {
			t.Fatal("import_state written")
		}
		if count(t, f.db, "follows") != 0 || count(t, f.db, "settings") != 0 {
			t.Fatal("rows written")
		}
		if _, err := os.Stat(filepath.Join(f.dataDir, "definitions")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("files copied before the refusal")
		}
	}
}

func sum(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return s[:]
}

func TestPhase1_SourceUntouched(t *testing.T) {
	f := newFixture(t)
	// a row only in the WAL: written by a connection that stays open (no checkpoint)
	w, err := sql.Open("sqlite", "file:"+f.from+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	if _, err := w.Exec(`INSERT INTO sources (id, definition_id, name, enabled, settings_json, status, priority, created_at) VALUES (3, 'wal-def', 'WAL only', 1, '{}', 'ok', 40, 'now')`); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(f.from + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("no WAL content: %v", err)
	}
	before := [][]byte{sum(t, f.from), sum(t, f.from+"-wal")}
	c, err := Phase1(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	after := [][]byte{sum(t, f.from), sum(t, f.from+"-wal")}
	if !bytes.Equal(before[0], after[0]) || !bytes.Equal(before[1], after[1]) {
		t.Fatal("vault.db or its WAL changed")
	}
	if c["sources"] != 3 {
		t.Fatalf("sources %d: the WAL-only row was not imported", c["sources"])
	}
	var name string
	if err := f.db.QueryRow(`SELECT name FROM sources WHERE id=3`).Scan(&name); err != nil || name != "WAL only" {
		t.Fatalf("WAL row: %q %v", name, err)
	}
}

func TestPhase1_CopiesFiles(t *testing.T) {
	f := newFixture(t)
	c, err := Phase1(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"torrents/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.torrent", "definitions/x.yml", "import-tokens/a", "migrate/qbt-state/s"} {
		a, err := os.ReadFile(filepath.Join(f.fromDataDir, rel))
		if err != nil {
			t.Fatalf("source %s gone: %v", rel, err)
		}
		b, err := os.ReadFile(filepath.Join(f.dataDir, rel))
		if err != nil || !bytes.Equal(a, b) {
			t.Fatalf("copy of %s: %v", rel, err)
		}
	}
	if c["files"] != 4 {
		t.Fatalf("Counts[files] = %d", c["files"])
	}
}

func TestPhase1_FromDataDirDefaultAndExplicit(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(filepath.Join(other, "definitions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "definitions", "y.yml"), []byte("id: y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	o := f.opts()
	o.FromDataDir = other
	c, err := Phase1(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "definitions", "y.yml")); err != nil {
		t.Fatal("FromDataDir ignored")
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "definitions", "x.yml")); err == nil {
		t.Fatal("copied from the db's dir although FromDataDir was set")
	}
	if c["files"] != 1 {
		t.Fatalf("files %d", c["files"])
	}
}

func TestPhase1_FailureLeavesNoState(t *testing.T) {
	// a torrents row the addon schema refuses (name NULL; the source table lost its NOT NULL)
	f := newFixture(t,
		`CREATE TABLE torrents_loose AS SELECT * FROM torrents`,
		`DELETE FROM torrent_pieces`,
		`DROP TABLE torrents`,
		`ALTER TABLE torrents_loose RENAME TO torrents`,
		`INSERT INTO torrents (infohash, name, mode, save_dir, added_at, status) VALUES ('ffffffffffffffffffffffffffffffffffffffff', NULL, 'download', '/x', 'now', 'paused')`)
	if _, err := Phase1(context.Background(), f.opts()); err == nil {
		t.Fatal("imported a row the schema refuses")
	} else if !strings.Contains(err.Error(), "torrents row") {
		t.Fatalf("failed for another reason: %v", err)
	}
	if _, ok, _ := LoadState(context.Background(), f.db); ok {
		t.Fatal("import_state written after a failure")
	}
	for _, tbl := range append(familyTables, "settings", "import_refs") {
		if n := count(t, f.db, tbl); n != 0 {
			t.Fatalf("%s has %d rows after a failure", tbl, n)
		}
	}
}

func TestPhase1_FileCopyFailureRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads unreadable dirs")
	}
	f := newFixture(t)
	dir := filepath.Join(f.fromDataDir, "torrents")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if _, err := Phase1(context.Background(), f.opts()); err == nil {
		t.Fatal("imported with an unreadable metafile dir")
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("failed for another reason: %v", err)
	}
	if _, ok, _ := LoadState(context.Background(), f.db); ok {
		t.Fatal("import_state after a failed file copy")
	}
	if n := count(t, f.db, "torrents"); n != 0 {
		t.Fatalf("%d torrents rows after a failed file copy", n)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Phase1(context.Background(), f.opts()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "torrents", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.torrent")); err != nil {
		t.Fatal("metafile missing after the retry")
	}
	if _, ok, _ := LoadState(context.Background(), f.db); !ok {
		t.Fatal("no import_state after the retry")
	}
}

func TestPhase1_BadAppSettingsFails(t *testing.T) {
	for _, bad := range []string{`{not json`, `{"noSeeding":"yes"}`, `{"uploadLimitKBs":"x"}`} {
		f := newFixture(t, `UPDATE settings SET value='`+bad+`' WHERE key='app_settings'`)
		if _, err := Phase1(context.Background(), f.opts()); err == nil {
			t.Fatalf("app_settings %s imported", bad)
		} else if !strings.Contains(err.Error(), "app_settings") || !errors.Is(err, ErrBadSettings) {
			t.Fatalf("app_settings %s failed for another reason (or not as ErrBadSettings): %v", bad, err)
		}
		var n int
		f.db.QueryRow(`SELECT count(*) FROM settings WHERE key='no_seeding'`).Scan(&n)
		if n != 0 {
			t.Fatalf("no_seeding row after a bad app_settings %s", bad)
		}
		if _, ok, _ := LoadState(context.Background(), f.db); ok {
			t.Fatal("import_state written")
		}
	}
}

func TestPhase1_MissingSaveDirCounted(t *testing.T) {
	f := newFixture(t)
	c, err := Phase1(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	// the fixture's save dirs (/media/.torrents/…) do not exist on the test machine
	if c["missingSaveDirs"] != 3 {
		t.Fatalf("missingSaveDirs %d, want 3", c["missingSaveDirs"])
	}
}

func TestFingerprint_ChangesWithRows(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := Fingerprint(ctx, f.from, f.dataDir)
	if err != nil || a == "" {
		t.Fatalf("Fingerprint: %q %v", a, err)
	}
	if again, _ := Fingerprint(ctx, f.from, f.dataDir); again != a {
		t.Fatal("not stable")
	}
	v, err := sql.Open("sqlite", "file:"+f.from)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err := v.Exec(`INSERT INTO items (id, kind, title, year) VALUES (9, 'movie', 'Core change', 2020)`); err != nil {
		t.Fatal(err)
	}
	if got, _ := Fingerprint(ctx, f.from, f.dataDir); got != a {
		t.Fatal("a core table change moved the fingerprint")
	}
	if _, err := v.Exec(`UPDATE torrents SET save_dir='/elsewhere' WHERE infohash='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'`); err != nil {
		t.Fatal(err)
	}
	if got, _ := Fingerprint(ctx, f.from, f.dataDir); got == a {
		t.Fatal("a torrents change did not move the fingerprint")
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "import")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the fingerprint's snapshot was left behind")
	}
}

func TestPhase1_FingerprintRecordedEqualsFingerprint(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := Phase1(ctx, f.opts()); err != nil {
		t.Fatal(err)
	}
	st, _, _ := LoadState(ctx, f.db)
	fp, err := Fingerprint(ctx, f.from, f.dataDir)
	if err != nil || fp != st.Fingerprint {
		t.Fatalf("recorded %q, now %q (%v)", st.Fingerprint, fp, err)
	}
}

// --- review round 1 ---

// TestPhase1_SymlinkedDirAndFile: a fileDirs dir that is a symlink is followed,
// and a symlink to a regular file inside is copied by its content.
func TestPhase1_SymlinkedDirAndFile(t *testing.T) {
	f := newFixture(t)
	real := filepath.Join(t.TempDir(), "real-torrents")
	if err := os.Rename(filepath.Join(f.fromDataDir, "torrents"), real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(f.fromDataDir, "torrents")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "def-target.yml")
	if err := os.WriteFile(target, []byte("id: linked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(f.fromDataDir, "definitions", "linked.yml")); err != nil {
		t.Fatal(err)
	}
	c, err := Phase1(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "torrents", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.torrent")); err != nil {
		t.Fatal("metafile behind a symlinked dir not imported")
	}
	fi, err := os.Lstat(filepath.Join(f.dataDir, "definitions", "linked.yml"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("linked file not copied as a regular file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dataDir, "definitions", "linked.yml")); string(b) != "id: linked\n" {
		t.Fatalf("content %q", b)
	}
	if c["files"] != 5 {
		t.Fatalf("files %d, want 5", c["files"])
	}
}

// TestPhase1_BadEntriesFail: a dangling link, a link to a dir, a dangling
// root link and a FIFO each fail the import, leaving no state.
func TestPhase1_BadEntriesFail(t *testing.T) {
	cases := map[string]func(t *testing.T, src string){
		"dangling link": func(t *testing.T, src string) {
			os.Symlink(filepath.Join(src, "nowhere"), filepath.Join(src, "definitions", "gone.yml"))
		},
		"link to a dir": func(t *testing.T, src string) {
			os.Symlink(t.TempDir(), filepath.Join(src, "import-tokens", "dir"))
		},
		"dangling root": func(t *testing.T, src string) {
			os.RemoveAll(filepath.Join(src, "migrate", "qbt-state"))
			os.Symlink(filepath.Join(src, "nowhere"), filepath.Join(src, "migrate", "qbt-state"))
		},
		"fifo": func(t *testing.T, src string) {
			if err := mkfifo(filepath.Join(src, "torrents", "pipe")); err != nil {
				t.Skip("no mkfifo:", err)
			}
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mk(t, f.fromDataDir)
			if _, err := Phase1(context.Background(), f.opts()); err == nil {
				t.Fatal("imported")
			}
			if _, ok, _ := LoadState(context.Background(), f.db); ok {
				t.Fatal("import_state written")
			}
			if n := count(t, f.db, "torrents"); n != 0 {
				t.Fatalf("%d torrents rows", n)
			}
		})
	}
}

// TestFingerprint_ReadErrorFails: a read error while hashing rows is an
// error, never a fingerprint of part of them.
func TestFingerprint_ReadErrorFails(t *testing.T) {
	dir := t.TempDir()
	from := filepath.Join(dir, "vault.db")
	v, err := sql.Open("sqlite", "file:"+from)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Exec(`PRAGMA page_size=4096; CREATE TABLE torrents (infohash TEXT PRIMARY KEY, name TEXT)`); err != nil {
		t.Fatal(err)
	}
	pad := string(bytes.Repeat([]byte("x"), 900))
	for i := 0; i < 400; i++ {
		if _, err := v.Exec(`INSERT INTO torrents VALUES (?, ?)`, fmt.Sprintf("%040d", i), pad); err != nil {
			t.Fatal(err)
		}
	}
	v.Close()
	// overwrite the middle of the file (table pages, not the header or the schema page)
	fh, err := os.OpenFile(from, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := fh.Stat()
	junk := bytes.Repeat([]byte{0xff}, 4096*8)
	if _, err := fh.WriteAt(junk, fi.Size()/2/4096*4096); err != nil {
		t.Fatal(err)
	}
	fh.Close()
	if fp, err := Fingerprint(context.Background(), from, dir); err == nil {
		t.Fatalf("fingerprint %q over a corrupt table", fp)
	}
}

func TestFingerprint_NoSnapshotLeft(t *testing.T) {
	f := newFixture(t)
	if _, err := Fingerprint(context.Background(), f.from, f.dataDir); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(f.dataDir, "fingerprint-*")); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

func TestPhase1_AppSettingsTrailingText(t *testing.T) {
	f := newFixture(t, `UPDATE settings SET value='{"noSeeding":true} x' WHERE key='app_settings'`)
	if _, err := Phase1(context.Background(), f.opts()); err == nil || !strings.Contains(err.Error(), "app_settings") {
		t.Fatalf("err = %v", err)
	}
}

// TestPhase1_ExistingSettingsRows: rows written before the import (a forced
// start without IMPORT_FROM) do not block it; no_seeding never turns off.
func TestPhase1_ExistingSettingsRows(t *testing.T) {
	f := newFixture(t, `UPDATE settings SET value='{"uploadLimited":false,"uploadLimitKBs":0,"streamCacheGB":30,"noSeeding":false}' WHERE key='app_settings'`)
	if _, err := f.db.Exec(`INSERT INTO settings (key, value) VALUES ('no_seeding', '1'), ('upload_limited', '1')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Phase1(context.Background(), f.opts()); err != nil {
		t.Fatal(err)
	}
	s := store.Settings{DB: f.db}
	if on, err := s.NoSeeding(context.Background()); err != nil || !on {
		t.Fatalf("no_seeding turned off by the import: %v %v", on, err)
	}
	if limited, _, _ := s.Upload(context.Background()); limited {
		t.Fatal("upload_limited not overwritten by the import")
	}
}

// Deleting a profile in the core nulls follows.user_id and
// pinned_releases.pinned_by through their foreign keys: that is not a change
// of the source, so the fingerprint must not move.
func TestFingerprint_ProfileDeleteKeepsIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	a, err := Fingerprint(ctx, f.from, f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := sql.Open("sqlite", "file:"+f.from+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err := v.Exec(`DELETE FROM users WHERE id=2`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := v.QueryRow(`SELECT count(*) FROM follows WHERE user_id IS NULL`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("setup: the delete did not null follows.user_id (%d, %v)", n, err)
	}
	if err := v.QueryRow(`SELECT count(*) FROM pinned_releases WHERE pinned_by IS NULL`).Scan(&n); err != nil || n == 0 {
		t.Fatalf("setup: the delete did not null pinned_releases.pinned_by (%d, %v)", n, err)
	}
	if got, err := Fingerprint(ctx, f.from, f.dataDir); err != nil || got != a {
		t.Fatalf("a profile delete moved the fingerprint (%v)", err)
	}
	if _, err := v.Exec(`UPDATE follows SET title='Renamed' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if got, _ := Fingerprint(ctx, f.from, f.dataDir); got == a {
		t.Fatal("a follows change did not move the fingerprint")
	}
}

// TestPhase1_CopiesMigrateReports: the old build's «Переезд»
// reports, the regular migrate/*.json of the import data dir, are copied
// into data/migrate/reports; qbt-state keeps its place; the source stays.
func TestPhase1_CopiesMigrateReports(t *testing.T) {
	reports := map[string]string{
		"20261020T100000Z-aaaaaa.json": `{"id":"20261020T100000Z-aaaaaa"}`,
		"20261021T100000Z-bbbbbb.json": `{"id":"20261021T100000Z-bbbbbb"}`,
	}
	put := func(f fixture) {
		for name, body := range reports {
			if err := os.WriteFile(filepath.Join(f.fromDataDir, "migrate", name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	f := newFixture(t)
	put(f)
	before := sum(t, filepath.Join(f.fromDataDir, "migrate", "20261020T100000Z-aaaaaa.json"))
	c, err := Phase1(context.Background(), f.opts())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range reports {
		if b, err := os.ReadFile(filepath.Join(f.dataDir, "migrate", "reports", name)); err != nil || string(b) != body {
			t.Fatalf("report %s: %q %v", name, b, err)
		}
		if _, err := os.Stat(filepath.Join(f.fromDataDir, "migrate", name)); err != nil {
			t.Fatalf("source %s gone: %v", name, err)
		}
	}
	if !bytes.Equal(before, sum(t, filepath.Join(f.fromDataDir, "migrate", "20261020T100000Z-aaaaaa.json"))) {
		t.Fatal("source report changed")
	}
	if b, err := os.ReadFile(filepath.Join(f.dataDir, "migrate", "qbt-state", "s")); err != nil || string(b) != "state" {
		t.Fatalf("qbt-state moved: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "migrate", "reports", "qbt-state")); err == nil {
		t.Fatal("qbt-state copied among the reports")
	}
	if c["files"] != 6 {
		t.Fatalf("files %d, want 6", c["files"])
	}
}

// A run that fails before the rows leaves no import_state; the next one
// copies the reports again over what the failed one renamed into place.
func TestPhase1_MigrateReportsRedoneAfterFailure(t *testing.T) {
	f := newFixture(t,
		`CREATE TABLE torrents_loose AS SELECT * FROM torrents`,
		`DELETE FROM torrent_pieces`,
		`DROP TABLE torrents`,
		`ALTER TABLE torrents_loose RENAME TO torrents`,
		`INSERT INTO torrents (infohash, name, mode, save_dir, added_at, status) VALUES ('ffffffffffffffffffffffffffffffffffffffff', NULL, 'download', '/x', 'now', 'paused')`)
	rep := filepath.Join(f.fromDataDir, "migrate", "20261020T100000Z-aaaaaa.json")
	if err := os.WriteFile(rep, []byte(`{"v":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Phase1(context.Background(), f.opts()); err == nil {
		t.Fatal("imported a row the schema refuses")
	}
	if _, ok, _ := LoadState(context.Background(), f.db); ok {
		t.Fatal("import_state after a failure")
	}
	// the source is fixed and the report changed meanwhile: the rerun redoes it
	v, err := sql.Open("sqlite", "file:"+f.from)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Exec(`DELETE FROM torrents WHERE name IS NULL`); err != nil {
		t.Fatal(err)
	}
	v.Close()
	if err := os.WriteFile(rep, []byte(`{"v":2}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Phase1(context.Background(), f.opts()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.dataDir, "migrate", "reports", "20261020T100000Z-aaaaaa.json")); string(b) != `{"v":2}` {
		t.Fatalf("report after the rerun %q", b)
	}
	if _, ok, _ := LoadState(context.Background(), f.db); !ok {
		t.Fatal("no import_state after the rerun")
	}
}

// A report that is a link is copied by its content; a dir or a device
// named *.json fails the import.
func TestPhase1_MigrateReportEntries(t *testing.T) {
	f := newFixture(t)
	target := filepath.Join(t.TempDir(), "r.json")
	if err := os.WriteFile(target, []byte(`{"linked":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(f.fromDataDir, "migrate", "20261022T100000Z-cccccc.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Phase1(context.Background(), f.opts()); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(f.dataDir, "migrate", "reports", "20261022T100000Z-cccccc.json")
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("linked report not a regular file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(p); string(b) != `{"linked":true}` {
		t.Fatalf("content %q", b)
	}
	for name, mk := range map[string]func(p string) error{
		"dir":  func(p string) error { return os.Mkdir(p, 0o755) },
		"fifo": mkfifo,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if err := mk(filepath.Join(f.fromDataDir, "migrate", "x.json")); err != nil {
				t.Skip(err)
			}
			if _, err := Phase1(context.Background(), f.opts()); err == nil {
				t.Fatal("imported")
			}
			if _, ok, _ := LoadState(context.Background(), f.db); ok {
				t.Fatal("import_state written")
			}
		})
	}
}
