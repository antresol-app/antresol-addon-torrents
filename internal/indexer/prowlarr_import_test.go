package indexer

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// prowlarrFixture creates a small prowlarr.db with the Indexers schema subset
// and a Definitions/Custom directory next to it, as in Prowlarr's /config.
func prowlarrFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prowlarr.db")
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stmts := []string{
		`CREATE TABLE "Indexers" ("Id" INTEGER PRIMARY KEY AUTOINCREMENT, "Name" TEXT NOT NULL, "Implementation" TEXT NOT NULL,
			"Settings" TEXT, "ConfigContract" TEXT, "Enable" INTEGER, "Priority" INTEGER NOT NULL DEFAULT 25,
			"Added" DATETIME, "Redirect" INTEGER, "AppProfileId" INTEGER, "Tags" TEXT)`,
		`INSERT INTO Indexers (Name, Implementation, Settings, ConfigContract, Enable, Priority, Tags) VALUES
			('RuTracker.org (Cardigann)', 'Cardigann', '{"definitionFile":"rutracker","extraFieldData":{"username":"alex","password":"p@ss","sort":1,"type":0},"baseUrl":"https://rutracker.org/","baseSettings":{"limitsUnit":0}}', 'CardigannSettings', 1, 10, '[1]'),
			('RuTracker.org', 'RuTracker', '{"username":"alex2","password":"pw2","russianLetters":false,"baseUrl":"https://rutracker.net/"}', 'RuTrackerSettings', 0, 25, '[]'),
			('Мой трекер', 'Cardigann', '{"definitionFile":"my-custom","extraFieldData":{"cookie":"bb_session=1"}}', 'CardigannSettings', 1, 25, '[]'),
			('Kinozal (old)', 'Cardigann', '{"definitionFile":"kinozal","fields":[{"name":"username","value":"kz"},{"name":"password","value":"kzpw"}]}', 'CardigannSettings', 1, 30, '[]'),
			('Some Usenet', 'Newznab', '{"baseUrl":"https://nzb.example/","apiKey":"k"}', 'NewznabSettings', 1, 25, '[]'),
			('Lost', 'Cardigann', '{"definitionFile":"does-not-exist"}', 'CardigannSettings', 1, 25, '[]')`,
	}
	for _, s := range stmts {
		if _, err := d.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	custom := filepath.Join(dir, "Definitions", "Custom")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	def := fakeDef("my-custom", "Мой трекер", "http://127.0.0.1:1")
	def = replaceSettings(def, "settings:\n  - name: cookie\n    type: text\n    label: Cookie\n")
	if err := os.WriteFile(filepath.Join(custom, "my-custom.yml"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func replaceSettings(def, settings string) string {
	const empty = "settings: []\n"
	for i := 0; i+len(empty) <= len(def); i++ {
		if def[i:i+len(empty)] == empty {
			return def[:i] + settings + def[i+len(empty):]
		}
	}
	return def
}

func TestImportProwlarr(t *testing.T) {
	e := newEnv(t)
	e.reload()
	ctx := context.Background()
	path := prowlarrFixture(t)

	imported, err := e.m.ImportProwlarr(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Imported{}
	for _, im := range imported {
		byName[im.Name] = im
	}
	if len(imported) != 6 {
		t.Fatalf("imported = %+v", imported)
	}

	rt := byName["RuTracker.org (Cardigann)"]
	if rt.Source == nil || rt.Source.DefinitionID != "rutracker" {
		t.Fatalf("rutracker = %+v", rt)
	}
	s := rt.Source.Settings
	if s["username"] != "alex" || s["password"] != "p@ss" {
		t.Fatalf("rutracker settings = %+v", s)
	}
	// Prowlarr stores selects as an index into the sorted option keys
	if s["sort"] != "10" || s["type"] != "1" {
		t.Fatalf("select settings = sort %q type %q", s["sort"], s["type"])
	}
	if _, ok := s["sitelink"]; ok {
		t.Errorf("default base URL stored as sitelink: %q", s["sitelink"])
	}
	if !rt.Source.Enabled || rt.Source.Priority != 10 || rt.Source.Status != StatusUnknown {
		t.Errorf("rutracker source = %+v", rt.Source)
	}

	native := byName["RuTracker.org"]
	if native.Source == nil || native.Source.DefinitionID != "rutracker" || native.Source.Enabled ||
		native.Source.Settings["password"] != "pw2" || native.Source.Settings["sitelink"] != "https://rutracker.net/" {
		t.Fatalf("native rutracker = %+v", native)
	}

	custom := byName["Мой трекер"]
	if custom.Source == nil || custom.Source.DefinitionID != "my-custom" || custom.Source.Settings["cookie"] != "bb_session=1" {
		t.Fatalf("custom = %+v", custom)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "definitions", "my-custom.yml")); err != nil {
		t.Fatalf("custom definition not copied: %v", err)
	}
	if ce, ok := e.cat.Get("my-custom"); !ok || !ce.Custom {
		t.Fatalf("custom definition not in catalog: %+v", ce)
	}

	if kz := byName["Kinozal (old)"]; kz.Source == nil || kz.Source.Settings["username"] != "kz" || kz.Source.Settings["password"] != "kzpw" {
		t.Fatalf("kinozal (fields) = %+v", kz)
	}
	for _, name := range []string{"Some Usenet", "Lost"} {
		if im := byName[name]; im.Source != nil || im.Reason == "" || !hasCyrillic(im.Reason) {
			t.Errorf("%s = %+v, want skipped with a Russian reason", name, im)
		}
	}

	// importing again does not duplicate
	again, err := e.m.ImportProwlarr(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range again {
		if im.Source != nil {
			t.Errorf("re-import created %+v", im.Source)
		}
	}
	srcs, _ := e.m.Sources(ctx)
	if len(srcs) != 4 {
		t.Fatalf("sources = %d, want 4", len(srcs))
	}
}

func TestImportProwlarrMissingFile(t *testing.T) {
	e := newEnv(t)
	e.reload()
	_, err := e.m.ImportProwlarr(context.Background(), filepath.Join(t.TempDir(), "nope.db"))
	if err == nil || !hasCyrillic(err.Error()) {
		t.Fatalf("err = %v, want a Russian message", err)
	}
}

// A stopped Prowlarr leaves a WAL-mode database without -wal/-shm files;
// its /config is often mounted read-only. Import must still work.
func TestImportProwlarrReadOnlyWAL(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := prowlarrFixture(t)
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	imported, err := e.m.ImportProwlarr(context.Background(), path)
	if err != nil {
		t.Fatalf("import from read-only WAL db: %v", err)
	}
	n := 0
	for _, im := range imported {
		if im.Source != nil {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("imported %d sources: %+v", n, imported)
	}
}

func TestImportProwlarrBrokenFileMessage(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := filepath.Join(t.TempDir(), "prowlarr.db")
	os.WriteFile(path, []byte("this is not a database at all, definitely not sqlite"), 0o644)
	_, err := e.m.ImportProwlarr(context.Background(), path)
	if err == nil || !hasCyrillic(err.Error()) || strings.Contains(err.Error(), "sqlite") || strings.Contains(err.Error(), "database") {
		t.Fatalf("err = %v, want a human Russian message", err)
	}
}

// user-facing import failures are a typed error, so the API
// does not have to guess from the text.
func TestImportProwlarrErrorsAreTyped(t *testing.T) {
	e := newEnv(t)
	e.reload()
	broken := filepath.Join(t.TempDir(), "prowlarr.db")
	os.WriteFile(broken, []byte("not sqlite"), 0o644)
	for _, p := range []string{filepath.Join(t.TempDir(), "nope.db"), broken} {
		_, err := e.m.ImportProwlarr(context.Background(), p)
		var ie *ImportError
		if !errors.As(err, &ie) || ie.Msg == "" {
			t.Fatalf("%s: %v is not an ImportError", p, err)
		}
	}
}

// the db+wal snapshot is only taken when neither file
// changed during the copy.
func TestSnapshotDBRetriesWhileWritten(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "p.db")
	os.WriteFile(src, []byte("v1"), 0o644)
	os.WriteFile(src+"-wal", []byte("w1"), 0o644)
	dst := filepath.Join(t.TempDir(), "copy.db")
	if err := snapshotDB(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst + "-wal"); string(b) != "w1" {
		t.Fatalf("wal copy %q", b)
	}
	if fileStampsEqual := stampOf(src) == stampOf(src); !fileStampsEqual {
		t.Fatal("stamp not stable")
	}
	// No -wal: a plain copy.
	os.Remove(src + "-wal")
	if err := snapshotDB(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst + "-wal"); !os.IsNotExist(err) {
		t.Fatal("stale -wal left in the snapshot")
	}
}

// addTags gives the fixture Prowlarr's Tags table and tags the named
// indexer with flaresolverr.
func addTags(t *testing.T, path, tagged string) {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, s := range []string{
		`CREATE TABLE "Tags" ("Id" INTEGER PRIMARY KEY AUTOINCREMENT, "Label" TEXT NOT NULL)`,
		`INSERT INTO Tags (Id, Label) VALUES (1, 'FlareSolverr'), (2, 'ru')`,
		`UPDATE Indexers SET Tags='[2]'`,
	} {
		if _, err := d.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Exec(`UPDATE Indexers SET Tags='[2,1]' WHERE Name=?`, tagged); err != nil {
		t.Fatal(err)
	}
}

func TestProwlarrPreviewNoWrites(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := prowlarrFixture(t)
	list, err := e.m.PreviewProwlarr(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 6 {
		t.Fatalf("preview = %+v", list)
	}
	will := 0
	for _, im := range list {
		if im.Source != nil {
			t.Fatalf("preview created %+v", im.Source)
		}
		if im.Reason == "Будет добавлен" {
			will++
		}
	}
	if will != 4 {
		t.Fatalf("will add %d, want 4: %+v", will, list)
	}
	if srcs, _ := e.m.Sources(context.Background()); len(srcs) != 0 {
		t.Fatalf("sources = %+v", srcs)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "definitions", "my-custom.yml")); !os.IsNotExist(err) {
		t.Fatalf("custom definition copied by preview: %v", err)
	}
	if _, ok := e.cat.Get("my-custom"); ok {
		t.Fatal("custom definition loaded into the catalog by preview")
	}

	// after the import, the preview says «Уже добавлен»
	if _, err := e.m.ImportProwlarr(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	list, _ = e.m.PreviewProwlarr(context.Background(), path)
	already := 0
	for _, im := range list {
		if im.Reason == "Уже добавлен" {
			already++
		}
	}
	if already != 4 {
		t.Fatalf("already %d, want 4: %+v", already, list)
	}
}

func TestProwlarrPreviewFindsCustom(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := prowlarrFixture(t)
	custom := filepath.Join(filepath.Dir(path), "Definitions", "Custom")
	def := replaceSettings(fakeDef("rutracker-cookie", "RuTracker.org (cookie)", "http://127.0.0.1:1"),
		"settings:\n  - name: cookie\n    type: text\n    label: Cookie\n")
	if err := os.WriteFile(filepath.Join(custom, "rutracker-cookie.yaml"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO Indexers (Name, Implementation, Settings, Enable, Priority, Tags) VALUES
		('RuTracker.org (cookie)', 'Cardigann', '{"definitionFile":"rutracker-cookie","extraFieldData":{"cookie":"bb=1"}}', 1, 25, '[]')`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	list, err := e.m.PreviewProwlarr(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range list {
		if im.Name == "RuTracker.org (cookie)" {
			if im.Reason != "Будет добавлен" || im.DefinitionID != "rutracker-cookie" {
				t.Fatalf("custom = %+v", im)
			}
			return
		}
	}
	t.Fatalf("custom indexer missing: %+v", list)
}

func TestProwlarrFlareSolverrTag(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := prowlarrFixture(t)
	addTags(t, path, "Kinozal (old)")
	for name, run := range map[string]func(context.Context, string) ([]Imported, error){
		"preview": e.m.PreviewProwlarr, "import": e.m.ImportProwlarr,
	} {
		list, err := run(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range list {
			if want := im.Name == "Kinozal (old)"; im.NeedsFlareSolverr != want {
				t.Errorf("%s: %s NeedsFlareSolverr = %v", name, im.Name, im.NeedsFlareSolverr)
			}
		}
	}
}

func TestProwlarrNoTagsTable(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := prowlarrFixture(t) // Indexers.Tags = [1], no Tags table
	list, err := e.m.PreviewProwlarr(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range list {
		if im.NeedsFlareSolverr {
			t.Fatalf("%+v needs FlareSolverr without a Tags table", im)
		}
	}
}

// The snapshot (tracker passwords, cookies) goes only under the given
// folder and is gone when the call returns.
func TestProwlarrSnapshotInGivenDir(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := prowlarrFixture(t)
	sys := t.TempDir()
	t.Setenv("TMPDIR", sys)
	run := t.TempDir()
	for _, f := range []func(context.Context, string, string) ([]Imported, error){e.m.PreviewProwlarrIn, e.m.ImportProwlarrIn} {
		if _, err := f(context.Background(), path, run); err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{sys, run} {
			if left, _ := os.ReadDir(dir); len(left) != 0 {
				t.Fatalf("left in %s: %v", dir, left)
			}
		}
	}
	// the folder is really used: a file in its place fails the read
	notDir := filepath.Join(run, "file")
	if err := os.WriteFile(notDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.PreviewProwlarrIn(context.Background(), path, notDir); err == nil {
		t.Fatal("snapshot not taken in the given folder")
	}
}

// An Indexers table without a Tags column is read, with no FlareSolverr.
func TestProwlarrNoTagsColumn(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := prowlarrFixture(t)
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`ALTER TABLE Indexers DROP COLUMN Tags`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	list, err := e.m.PreviewProwlarr(context.Background(), path)
	if err != nil || len(list) != 6 {
		t.Fatalf("preview = %+v, %v", list, err)
	}
}
