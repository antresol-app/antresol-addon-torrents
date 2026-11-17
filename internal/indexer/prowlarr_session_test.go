package indexer

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	_ "modernc.org/sqlite"
)

// cookieClub is a fake cookie-login tracker in the manner of NoNaMe Club:
// the index page shows a logout link to a signed-in visitor, and the site
// replaces the session id with Set-Cookie (once per entry of next, on a
// search), after which the old id no longer works. Values are fake.
type cookieClub struct {
	mu    sync.Mutex
	valid string
	next  []string
	srv   *httptest.Server
}

func newCookieClub(t *testing.T, valid string, next ...string) *cookieClub {
	c := &cookieClub{valid: valid, next: next}
	mux := http.NewServeMux()
	mux.HandleFunc("/index.php", func(w http.ResponseWriter, r *http.Request) {
		if !c.ok(r) {
			w.Write([]byte(`<html><body><a href="login.php">Вход</a></body></html>`))
			return
		}
		w.Write([]byte(`<html><body><a class="logout" href="login.php?logout=1">Выход</a></body></html>`))
	})
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if !c.ok(r) {
			http.Redirect(w, r, "/login.php", http.StatusFound)
			return
		}
		c.mu.Lock()
		if len(c.next) > 0 {
			c.valid, c.next = c.next[0], c.next[1:]
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: c.valid, Path: "/"})
		}
		c.mu.Unlock()
		w.Write([]byte(`<html><body><a class="logout">Выход</a>` + page(row{title: "Фильм 2024 1080p", hash: strings.Repeat("ab", 20), size: "1 GB", seeders: 5}) + `</body></html>`))
	})
	c.srv = httptest.NewServer(mux)
	t.Cleanup(c.srv.Close)
	return c
}

func (c *cookieClub) ok(r *http.Request) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, err := r.Cookie("sid")
	return err == nil && k.Value == c.valid
}

// cookieClubDef signs in with a cookie and checks it on index.php.
func cookieClubDef(id, link string) string {
	return replaceSettings(fakeDef(id, "Fake ClubL", link), `settings:
  - name: cookie
    type: text
    label: Cookie
login:
  method: cookie
  inputs:
    cookie: "{{ .Config.cookie }}"
  test:
    path: index.php
    selector: a.logout
`)
}

// prowlarrWith makes a prowlarr.db with the given Indexers rows (Id, Name,
// Implementation, Settings) and IndexerStatus rows (ProviderId, Cookies,
// CookiesExpirationDate), as Prowlarr's schema has them.
func prowlarrWith(t *testing.T, stmts ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "prowlarr.db")
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	all := append([]string{
		`CREATE TABLE "Indexers" ("Id" INTEGER PRIMARY KEY AUTOINCREMENT, "Name" TEXT NOT NULL, "Implementation" TEXT NOT NULL,
			"Settings" TEXT, "ConfigContract" TEXT, "Enable" INTEGER, "Priority" INTEGER NOT NULL DEFAULT 25, "Tags" TEXT)`,
		`CREATE TABLE "IndexerStatus" ("Id" INTEGER PRIMARY KEY AUTOINCREMENT, "ProviderId" INTEGER NOT NULL, "InitialFailure" DATETIME,
			"MostRecentFailure" DATETIME, "EscalationLevel" INTEGER NOT NULL DEFAULT 0, "DisabledTill" DATETIME, "LastRssSyncReleaseInfo" TEXT,
			"Cookies" TEXT, "CookiesExpirationDate" DATETIME)`,
	}, stmts...)
	for _, s := range all {
		if _, err := d.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return path
}

func setStatusCookies(t *testing.T, path string, provider int, cookies, expires string) {
	t.Helper()
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var exp any
	if expires != "" {
		exp = expires
	}
	if _, err := d.Exec(`DELETE FROM IndexerStatus WHERE ProviderId=?`, provider); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO IndexerStatus (ProviderId, EscalationLevel, Cookies, CookiesExpirationDate) VALUES (?, 0, ?, ?)`, provider, cookies, exp); err != nil {
		t.Fatal(err)
	}
}

func byName(list []Imported) map[string]Imported {
	out := map[string]Imported{}
	for _, im := range list {
		out[im.Name] = im
	}
	return out
}

// A definition the catalog lacks comes from Prowlarr's own downloaded
// Definitions/<id>.yml: read in a preview, copied on import (Prowlarr's
// «RuTracker.org (cookie)» is rutracker-cookie there).
func TestProwlarrDefinitionFallback(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := prowlarrWith(t, `INSERT INTO Indexers (Id, Name, Implementation, Settings, Enable) VALUES
		(1, 'RuTracker.org (cookie)', 'Cardigann', '{"definitionFile":"rutracker-cookie","extraFieldData":{"cookie":"bb_session=fake"}}', 1),
		(2, 'Broken', 'Cardigann', '{"definitionFile":"broken"}', 1),
		(3, 'Escape', 'Cardigann', '{"definitionFile":"../rutracker-cookie"}', 1)`)
	defs := filepath.Join(filepath.Dir(path), "Definitions")
	if err := os.MkdirAll(defs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defs, "rutracker-cookie.yml"), []byte(cookieClubDef("rutracker-cookie", "http://127.0.0.1:1")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defs, "broken.yml"), []byte("id: [not a definition"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	pre, err := e.m.PreviewProwlarr(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if im := byName(pre)["RuTracker.org (cookie)"]; im.Reason != ReasonWillAdd || im.DefinitionID != "rutracker-cookie" {
		t.Fatalf("preview = %+v", im)
	}
	if _, ok := e.cat.Get("rutracker-cookie"); ok {
		t.Fatal("preview loaded the definition into the catalog")
	}

	list, err := e.m.ImportProwlarr(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	got := byName(list)
	if im := got["RuTracker.org (cookie)"]; im.Source == nil || im.Source.DefinitionID != "rutracker-cookie" || im.Source.Settings["cookie"] != "bb_session=fake" {
		t.Fatalf("import = %+v", im)
	}
	for _, name := range []string{"Broken", "Escape"} {
		if im := got[name]; im.Source != nil || !strings.Contains(im.Reason, "не найдено") {
			t.Errorf("%s = %+v", name, im)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, "definitions", "rutracker-cookie.yml")); err != nil {
		t.Fatalf("definition not copied: %v", err)
	}
	// the «Add source» search finds both RuTracker variants of the catalog
	// and the imported one
	ids := map[string]bool{}
	for _, ce := range e.cat.List("rutracker") {
		ids[ce.ID] = true
	}
	for _, id := range []string{"rutracker", "rutracker-org-cookie", "rutracker-cookie"} {
		if !ids[id] {
			t.Errorf("catalog search misses %s: %v", id, ids)
		}
	}
}

// Prowlarr names a definition by its file: Custom/rutracker-cookie.yml
// holds id rutracker-org-cookie, which Antresol ships. The import finds it
// by the file name and keeps the shipped definition (not overridden by
// Prowlarr's copy).
func TestProwlarrCustomDefinitionByFileName(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := prowlarrWith(t, `INSERT INTO Indexers (Id, Name, Implementation, Settings, Enable) VALUES
		(1, 'RuTracker.org (cookie)', 'Cardigann', '{"definitionFile":"rutracker-cookie","extraFieldData":{"cookie":"bb_session=fake"}}', 1)`)
	custom := filepath.Join(filepath.Dir(path), "Definitions", "Custom")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "rutracker-cookie.yml"), []byte(cookieClubDef("rutracker-org-cookie", "http://127.0.0.1:1")), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pre, err := e.m.PreviewProwlarr(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if im := byName(pre)["RuTracker.org (cookie)"]; im.Reason != ReasonWillAdd {
		t.Fatalf("preview = %+v", im)
	}
	list, err := e.m.ImportProwlarr(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if im := byName(list)["RuTracker.org (cookie)"]; im.Source == nil || im.Source.DefinitionID != "rutracker-org-cookie" || im.Source.Settings["cookie"] != "bb_session=fake" {
		t.Fatalf("import = %+v", im)
	}
	if _, err := os.Stat(filepath.Join(e.dir, "definitions", "rutracker-cookie.yml")); err == nil {
		t.Fatal("Prowlarr's copy overrides the shipped definition")
	}
	if ce, ok := e.cat.Get("rutracker-org-cookie"); !ok || ce.Custom {
		t.Fatalf("rutracker-org-cookie = %+v, %v; want the shipped one", ce, ok)
	}
}

// A cookie-login indexer gets Prowlarr's live session (IndexerStatus)
// over its stale configured cookie, so the imported source signs in.
func TestProwlarrImportUsesLiveSession(t *testing.T) {
	club := newCookieClub(t, "live-2")
	settings := `{"definitionFile":"fake-clubl","extraFieldData":{"cookie":"sid=stale-1; lang=ru"}}`
	for _, tc := range []struct {
		name, expires, want string
	}{
		{"future", time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02 15:04:05.0000000Z"), "sid=live-2; lang=ru; uid=7"},
		{"no expiry", "", "sid=live-2; lang=ru; uid=7"},
		{"expired", time.Now().Add(-time.Hour).UTC().Format("2006-01-02 15:04:05Z"), "sid=stale-1; lang=ru"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if err := os.WriteFile(filepath.Join(e.dir, "definitions", "fake-clubl.yml"), []byte(cookieClubDef("fake-clubl", club.srv.URL)), 0o644); err != nil {
				t.Fatal(err)
			}
			e.reload()
			path := prowlarrWith(t, `INSERT INTO Indexers (Id, Name, Implementation, Settings, Enable) VALUES (5, 'NoNaMe ClubL', 'Cardigann', '`+settings+`', 1)`)
			setStatusCookies(t, path, 5, `{"sid":"live-2","uid":"7"}`, tc.expires)
			list, err := e.m.ImportProwlarr(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			im := byName(list)["NoNaMe ClubL"]
			if im.Source == nil || im.Source.Settings["cookie"] != tc.want {
				t.Fatalf("import = %+v, want cookie %q", im, tc.want)
			}
			err = e.m.Test(context.Background(), im.Source.ID)
			if s, _ := e.m.Get(context.Background(), im.Source.ID); (err == nil) != (tc.name != "expired") || (s.Status == StatusOK) != (err == nil) {
				t.Fatalf("test: %v, status %q %q", err, s.Status, s.LastError)
			}
		})
	}
}

// Importing again after Prowlarr refreshed the session updates the cookie
// of the source imported before (no second source); without a live
// session the source's own cookie is kept.
func TestProwlarrReimportRefreshesCookie(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.dir, "definitions", "fake-clubl.yml"), []byte(cookieClubDef("fake-clubl", "http://127.0.0.1:1")), 0o644); err != nil {
		t.Fatal(err)
	}
	e.reload()
	ctx := context.Background()
	path := prowlarrWith(t, `INSERT INTO Indexers (Id, Name, Implementation, Settings, Enable) VALUES
		(5, 'NoNaMe ClubL', 'Cardigann', '{"definitionFile":"fake-clubl","extraFieldData":{"cookie":"sid=stale-1"}}', 1)`)
	first, err := e.m.ImportProwlarr(ctx, path)
	if err != nil || len(first) != 1 || first[0].Source == nil {
		t.Fatalf("first import = %+v, %v", first, err)
	}
	id := first[0].Source.ID
	if again, _ := e.m.ImportProwlarr(ctx, path); again[0].Reason != ReasonAlready || again[0].Source != nil {
		t.Fatalf("re-import without a session = %+v", again)
	}

	setStatusCookies(t, path, 5, `{"sid":"live-9"}`, "")
	pre, err := e.m.PreviewProwlarr(ctx, path)
	if err != nil || pre[0].Reason != ReasonWillUpdateCookie {
		t.Fatalf("preview = %+v, %v", pre, err)
	}
	list, err := e.m.ImportProwlarr(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if im := list[0]; im.Reason != ReasonCookieUpdated || im.Source == nil || im.Source.ID != id || im.Source.Settings["cookie"] != "sid=live-9" || im.Source.Status != StatusUnknown {
		t.Fatalf("refresh = %+v", im)
	}
	if srcs, _ := e.m.Sources(ctx); len(srcs) != 1 {
		t.Fatalf("sources = %+v", srcs)
	}
	if again, _ := e.m.ImportProwlarr(ctx, path); again[0].Reason != ReasonAlready {
		t.Fatalf("third import = %+v", again)
	}
}

// Two imports at once (two clicks, two tabs) add each tracker once.
func TestProwlarrConcurrentImports(t *testing.T) {
	e := newEnv(t)
	e.reload()
	path := prowlarrFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.m.ImportProwlarr(context.Background(), path)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if srcs, _ := e.m.Sources(context.Background()); len(srcs) != 4 {
		t.Fatalf("sources = %d, want 4", len(srcs))
	}
}

// The site's refreshed session is saved into the source's cookie: after
// a restart (a new Manager) the source still signs in although the site
// no longer takes the old cookie; the running source keeps its session.
func TestSourceSavesRefreshedCookie(t *testing.T) {
	club := newCookieClub(t, "fake-1", "fake-2")
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.dir, "definitions", "fake-clubl.yml"), []byte(cookieClubDef("fake-clubl", club.srv.URL)), 0o644); err != nil {
		t.Fatal(err)
	}
	e.reload()
	ctx := context.Background()
	s, err := e.m.Add(ctx, "fake-clubl", map[string]string{"cookie": "sid=fake-1; lang=ru"})
	if err != nil {
		t.Fatal(err)
	}
	// Add tested it: sign-in, then a search that rotated the session id
	if s.Status != StatusOK {
		t.Fatalf("status %q %q", s.Status, s.LastError)
	}
	if got := cookieSet(s.Settings["cookie"]); got != "lang=ru; sid=fake-2" {
		t.Fatalf("stored cookie %q", s.Settings["cookie"])
	}
	cached := e.m.runners[s.ID].runner
	if r, err := e.m.runner(s); err != nil || r != cached {
		t.Fatal("the runner was rebuilt after its cookie was saved")
	}
	e.reload() // a restart
	if err := e.m.Test(ctx, s.ID); err != nil {
		t.Fatalf("after restart: %v", err)
	}

	// a cookie the user entered meanwhile is not overwritten
	other, _ := e.m.Get(ctx, s.ID)
	r, _ := e.m.runner(other)
	if _, err := e.db.Exec(`UPDATE sources SET settings_json='{"cookie":"sid=typed"}' WHERE id=?`, s.ID); err != nil {
		t.Fatal(err)
	}
	e.m.saveSessionCookie(s.ID, r, "sid=late")
	if got, _ := e.m.Get(ctx, s.ID); got.Settings["cookie"] != "sid=typed" {
		t.Fatalf("user's cookie overwritten: %q", got.Settings["cookie"])
	}
}

// cookieSet sorts the pairs of a cookie header.
func cookieSet(header string) string {
	parts := strings.Split(header, ";")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	slices.Sort(parts)
	return strings.Join(parts, "; ")
}

// Prowlarr's native RuTracker imports as the rutracker source, and a
// failing sign-in shows up as its status (whatever Prowlarr's own state).
func TestImportedRutrackerStatusShowsErrors(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/forum/login.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		warn := ""
		if r.Method == http.MethodPost {
			warn = `<h4 class="warnColor1 tCenter mrg_16">Неверный пароль</h4>`
		}
		w.Write(cp1251(t, `<html><body><div id="login-form-full"><form action="login.php" method="post">`+warn+
			`<input name="login_username"><input type="password" name="login_password"><input type="submit" name="login" value="Вход"></form></div></body></html>`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	e := newEnv(t)
	e.reload()
	path := prowlarrWith(t, `INSERT INTO Indexers (Id, Name, Implementation, Settings, Enable) VALUES
		(1, 'RuTracker.org', 'RuTracker', '{"username":"alex","password":"fake-pw","baseUrl":"`+srv.URL+`/"}', 1)`)
	setStatusCookies(t, path, 1, `{"bb_session":"fake"}`, "") // not a cookie login: ignored
	list, err := e.m.ImportProwlarr(context.Background(), path)
	if err != nil || len(list) != 1 || list[0].Source == nil {
		t.Fatalf("import = %+v, %v", list, err)
	}
	s := list[0].Source
	if s.DefinitionID != "rutracker" || s.Status != StatusUnknown || s.Settings["cookie"] != "" {
		t.Fatalf("source = %+v", s)
	}
	if err := e.m.Test(context.Background(), s.ID); !errors.Is(err, cardigann.ErrLoginFailed) {
		t.Fatalf("test against a rejecting site: %v", err)
	}
	got, _ := e.m.Get(context.Background(), s.ID)
	if got.Status != StatusError || !strings.Contains(got.LastError, "Неверный пароль") {
		t.Fatalf("status %q, error %q", got.Status, got.LastError)
	}
}

// A cookie pasted from a terminal starts with its prompt; the name is still
// bb_guid, so the live session replaces it instead of adding a second one.
func TestMergeCookiesPastedPrompt(t *testing.T) {
	got := mergeCookies("❯   bb_guid=old; bb_ssl=1; bb_guid=dup", map[string]string{"bb_guid": "new", "bb_session": "s"})
	if want := "bb_guid=new; bb_ssl=1; bb_session=s"; got != want {
		t.Fatalf("mergeCookies = %q, want %q", got, want)
	}
}
