package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/antresol-app/antresol/addonkit"
)

// leftoverFixtures lays out an import folder with Librarr, Seerr and
// qBittorrent RSS / watched-folder files.
func librarrDB(t *testing.T, path string, wishlist, pending int) {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, s := range []string{
		`CREATE TABLE wishlist (id INTEGER PRIMARY KEY, title TEXT, author TEXT)`,
		`CREATE TABLE monitored_authors (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE series_tracking (id INTEGER PRIMARY KEY, name TEXT)`,
		`CREATE TABLE requests (id INTEGER PRIMARY KEY, title TEXT, status TEXT)`,
	} {
		mustExec(t, d, s)
	}
	for i := 0; i < wishlist; i++ {
		mustExec(t, d, `INSERT INTO wishlist (title, author) VALUES ('Мастер и Маргарита', 'Булгаков')`)
	}
	for i := 0; i < pending; i++ {
		mustExec(t, d, `INSERT INTO requests (title, status) VALUES ('Книга', 'pending')`)
	}
	mustExec(t, d, `INSERT INTO requests (title, status) VALUES ('Готово', 'completed')`)
}

func seerrDB(t *testing.T, path string, pending int) {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	mustExec(t, d, `CREATE TABLE media_request (id INTEGER PRIMARY KEY AUTOINCREMENT, status INTEGER NOT NULL, type VARCHAR)`)
	for i := 0; i < pending; i++ {
		mustExec(t, d, `INSERT INTO media_request (status, type) VALUES (1, 'movie')`)
	}
	mustExec(t, d, `INSERT INTO media_request (status, type) VALUES (2, 'tv')`) // approved: Sonarr/Radarr have it
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLeftoversCounts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	_, p := DefaultPaths(dir, filepath.Join(dir, "prowlarr", "prowlarr.db"))
	os.MkdirAll(filepath.Dir(p.LibrarrDB), 0o755)
	os.MkdirAll(filepath.Dir(p.SeerrDB), 0o755)
	librarrDB(t, p.LibrarrDB, 2, 0)
	seerrDB(t, p.SeerrDB, 1)
	writeFile(t, p.QBRSSRules, `{"Сериалы": {"enabled": true, "mustContain": "1080p"}, "Старое": {"enabled": false}}`)
	writeFile(t, p.QBWatched, `{}`)
	before := treeHash(t, dir)

	lines := Leftovers(ctx, p, t.TempDir())
	if len(lines) != 3 {
		t.Fatalf("lines %+v", lines)
	}
	want := []string{
		"Не переносится: книги из списков Librarr (2) — добавьте вручную",
		"Не переносится: неодобренные запросы Seerr (1)",
		"Не переносится: правила RSS qBittorrent (1)",
	}
	for i, l := range lines {
		if l.Status != StatusWarning || l.Kind != "leftover" || l.Reason != want[i] {
			t.Errorf("line %d = %+v, want %q", i, l, want[i])
		}
	}
	if after := treeHash(t, dir); after != before {
		t.Fatal("the import folder changed")
	}

	// watched folders
	writeFile(t, p.QBWatched, `{"/downloads/watch": {"add_torrent_params": {}}, "/other": {}}`)
	lines = Leftovers(ctx, p, t.TempDir())
	var got []string
	for _, l := range lines {
		got = append(got, l.Reason)
	}
	all := strings.Join(got, "\n")
	if !strings.Contains(all, "Не переносится: следимые папки qBittorrent (2)") || len(lines) != 4 {
		t.Fatalf("lines %q", got)
	}
}

func TestLeftoversEmpty(t *testing.T) {
	ctx := context.Background()
	// no files at all
	_, p := DefaultPaths(t.TempDir(), "")
	if lines := Leftovers(ctx, p, t.TempDir()); len(lines) != 0 {
		t.Fatalf("no files: %+v", lines)
	}
	// empty files and databases without the tables
	dir := t.TempDir()
	_, p = DefaultPaths(dir, "")
	writeFile(t, p.LibrarrDB, "")
	writeFile(t, p.SeerrDB, "")
	writeFile(t, p.QBRSSRules, "")
	writeFile(t, p.QBWatched, "  \n")
	if lines := Leftovers(ctx, p, t.TempDir()); len(lines) != 0 {
		t.Fatalf("empty files: %+v", lines)
	}
	librarrDB(t, p.LibrarrDB+".2", 0, 0)
	os.Rename(p.LibrarrDB+".2", p.LibrarrDB)
	seerrDB(t, p.SeerrDB+".2", 0)
	os.Rename(p.SeerrDB+".2", p.SeerrDB)
	writeFile(t, p.QBRSSRules, `{"Старое": {"enabled": false}}`)
	writeFile(t, p.QBWatched, `{}`)
	if lines := Leftovers(ctx, p, t.TempDir()); len(lines) != 0 {
		t.Fatalf("nothing pending: %+v", lines)
	}
}

func TestLeftoversUnreadableWarns(t *testing.T) {
	dir := t.TempDir()
	_, p := DefaultPaths(dir, "")
	writeFile(t, p.QBRSSRules, `{broken`)
	lines := Leftovers(context.Background(), p, t.TempDir())
	if len(lines) != 1 || lines[0].Status != StatusWarning || !strings.Contains(lines[0].Reason, "RSS") {
		t.Fatalf("lines %+v", lines)
	}
}

// TestLeftoversHook: migrate.leftovers ({"lang"}) answers the
// old services' leftovers in the request's language, as LeftoverLines;
// nothing is left in the run folders.
func TestLeftoversHook(t *testing.T) {
	e := newSvcEnv(t, nil)
	p := e.s.Leftover
	os.MkdirAll(filepath.Dir(p.LibrarrDB), 0o755)
	librarrDB(t, p.LibrarrDB, 2, 0)
	writeFile(t, p.QBRSSRules, `{"Сериалы": {"enabled": true}}`)
	ask := func(body string) addonkit.LeftoversAnswer {
		t.Helper()
		out, err := e.s.LeftoversHook(context.Background(), &addonkit.User{ID: 0}, json.RawMessage(body))
		if err != nil {
			t.Fatal(err)
		}
		a, ok := out.(addonkit.LeftoversAnswer)
		if !ok {
			t.Fatalf("answer %T", out)
		}
		return a
	}
	en := ask(`{"lang":"en"}`)
	want := []addonkit.LeftoverLine{
		{Source: "librarr", Name: "Librarr", Status: StatusWarning, Reason: "Not moved: books from Librarr lists (2) — add them yourself"},
		{Source: SourceQBittorrent, Name: "RSS Rules", Status: StatusWarning, Reason: "Not moved: qBittorrent RSS rules (1)"},
	}
	if !reflect.DeepEqual(en.Lines, want) {
		t.Fatalf("en %+v", en.Lines)
	}
	for _, body := range []string{`{"lang":"ru"}`, `{}`, ``} {
		if ru := ask(body); len(ru.Lines) != 2 || ru.Lines[0].Reason != "Не переносится: книги из списков Librarr (2) — добавьте вручную" {
			t.Fatalf("%q: %+v", body, ru.Lines)
		}
	}
	if _, err := e.s.LeftoversHook(context.Background(), nil, json.RawMessage(`{"lang":`)); err == nil {
		t.Fatal("a broken body answered")
	}
	// nothing to say: an empty list, never null
	e2 := newSvcEnv(t, nil)
	out, err := e2.s.LeftoversHook(context.Background(), nil, json.RawMessage(`{"lang":"en"}`))
	if a, ok := out.(addonkit.LeftoversAnswer); err != nil || !ok || a.Lines == nil || len(a.Lines) != 0 {
		t.Fatalf("none %+v %v", out, err)
	}
	if ents, _ := os.ReadDir(e.tmp); len(ents) != 0 {
		t.Fatalf("left in tmp %v", ents)
	}
}
