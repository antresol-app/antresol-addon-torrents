package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// fakeDef is a minimal HTML definition; rows are <tr> with t/h/s/se cells.
func fakeDef(id, name, link string) string {
	return fmt.Sprintf(`---
id: %s
name: %s
description: test tracker
language: ru-RU
type: public
links: [%q]
caps:
  categorymappings:
    - {id: 1, cat: Movies, desc: Movies}
    - {id: 2, cat: TV, desc: TV}
  modes: {search: [q]}
settings: []
search:
  paths:
    - path: search
  inputs:
    q: "{{ .Keywords }}"
  rows:
    selector: tr
  fields:
    title: {selector: td.t}
    download: {selector: a, attribute: href, optional: true}
    infohash: {selector: td.h, optional: true}
    size: {selector: td.s}
    seeders: {selector: td.se}
`, id, name, link+"/")
}

type row struct {
	title, hash, link string
	size              string
	seeders           int
}

func page(rows ...row) string {
	var b strings.Builder
	b.WriteString("<html><body><table>")
	for _, r := range rows {
		b.WriteString("<tr><td class=t>" + r.title + "</td>")
		if r.link != "" {
			b.WriteString(`<td><a href="` + r.link + `">dl</a></td>`)
		}
		if r.hash != "" {
			b.WriteString("<td class=h>" + r.hash + "</td>")
		}
		fmt.Fprintf(&b, "<td class=s>%s</td><td class=se>%d</td></tr>", r.size, r.seeders)
	}
	b.WriteString("</table></body></html>")
	return b.String()
}

type env struct {
	t   *testing.T
	db  *sql.DB
	dir string
	cat *Catalog
	m   *Manager
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := os.MkdirAll(filepath.Join(dir, "definitions"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &env{t: t, db: d, dir: dir}
}

// def writes a user definition and (re)creates catalog and manager.
func (e *env) def(id, name string, h http.Handler) {
	e.t.Helper()
	srv := httptest.NewServer(h)
	e.t.Cleanup(srv.Close)
	if err := os.WriteFile(filepath.Join(e.dir, "definitions", id+".yml"), []byte(fakeDef(id, name, srv.URL)), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.reload()
}

func (e *env) reload() {
	e.t.Helper()
	e.cat = NewCatalog(filepath.Join(e.dir, "definitions"))
	e.m = NewManager(e.db, e.cat, Options{SearchTimeout: 2 * time.Second, TestTimeout: 2 * time.Second})
}

func (e *env) add(defID string) Source {
	e.t.Helper()
	s, err := e.m.Add(context.Background(), defID, nil)
	if err != nil {
		e.t.Fatalf("Add(%s): %v", defID, err)
	}
	return s
}

func serve(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(body))
	})
}

func TestSearchSkipsBrokenSource(t *testing.T) {
	e := newEnv(t)
	e.def("broken", "Сломанный", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	e.def("slow", "Медленный", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	e.def("good", "Рабочий", serve(page(
		row{title: "Дюна (2024) WEB-DL 1080p | Дубляж", hash: strings.Repeat("a", 40), size: "10 GB", seeders: 50},
	)))
	e.m.searchTimeout = 300 * time.Millisecond

	broken := e.add("broken")
	if broken.Status != StatusError || broken.LastError == "" {
		t.Fatalf("broken source after Add: status=%q err=%q, want error with text", broken.Status, broken.LastError)
	}
	slow := e.add("slow")
	good := e.add("good")
	if good.Status != StatusOK {
		t.Fatalf("good source status = %q (%s)", good.Status, good.LastError)
	}

	resp, err := e.m.Search(context.Background(), SearchRequest{Text: "Дюна", Kind: "movie", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Releases) != 1 || resp.Releases[0].SourceID != good.ID {
		t.Fatalf("releases = %+v", resp.Releases)
	}
	failed := map[int64]SourceError{}
	for _, f := range resp.Failed {
		failed[f.SourceID] = f
	}
	if f, ok := failed[broken.ID]; !ok || f.Name != "Сломанный" || f.Message == "" {
		t.Fatalf("failed = %+v, want broken source", resp.Failed)
	}
	if f, ok := failed[slow.ID]; !ok || !strings.Contains(f.Message, "не ответил") {
		t.Fatalf("failed = %+v, want slow source with timeout message", resp.Failed)
	}
	if _, ok := failed[good.ID]; ok {
		t.Fatalf("good source reported as failed: %+v", resp.Failed)
	}
	for _, f := range resp.Failed {
		if strings.Contains(f.Message, "cardigann") || strings.Contains(f.Message, "context") {
			t.Errorf("message is not human: %q", f.Message)
		}
	}
	// the broken source is marked in the list
	srcs, err := e.m.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if s.ID == slow.ID && s.Status != StatusError {
			t.Errorf("slow source status = %q, want error", s.Status)
		}
		if s.ID == good.ID && s.Status != StatusOK {
			t.Errorf("good source status = %q, want ok", s.Status)
		}
	}
}

// A search with WithProgress reports each source as it answers: the slow
// source is pending while the fast one's releases are already there.
func TestSearchProgress(t *testing.T) {
	e := newEnv(t)
	gate := make(chan struct{})
	var hold sync.Once
	var holding bool
	var mu sync.Mutex
	slowPage := page(row{title: "Дюна (2024) WEB-DL 2160p | Дубляж", hash: strings.Repeat("b", 40), size: "30 GB", seeders: 5})
	e.def("slow", "Медленный", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		h := holding
		mu.Unlock()
		if h { // the search, not Add's test request
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		serve(slowPage).ServeHTTP(w, r)
	}))
	e.def("good", "Рабочий", serve(page(
		row{title: "Дюна (2024) WEB-DL 1080p | Дубляж", hash: strings.Repeat("a", 40), size: "10 GB", seeders: 50},
	)))
	slow := e.add("slow")
	e.add("good")
	mu.Lock()
	holding = true
	mu.Unlock()

	var got []Progress
	ctx := WithProgress(context.Background(), func(p Progress) {
		got = append(got, p)
		if len(p.Pending) == 1 {
			hold.Do(func() { close(gate) }) // the fast source answered: let the slow one go
		}
	})
	resp, err := e.m.Search(ctx, SearchRequest{Text: "Дюна", Kind: "movie", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("progress calls = %d, want 2 (start, the fast source)", len(got))
	}
	if len(got[0].Pending) != 2 || len(got[0].Releases) != 0 {
		t.Errorf("first progress = %+v, want both pending and no releases", got[0])
	}
	if p := got[1]; len(p.Pending) != 1 || p.Pending[0].SourceID != slow.ID || p.Pending[0].Name != "Медленный" || len(p.Releases) != 1 {
		t.Errorf("second progress = pending %+v releases %d, want the slow source pending and one release", p.Pending, len(p.Releases))
	}
	if len(resp.Releases) != 2 {
		t.Fatalf("final releases = %d, want 2", len(resp.Releases))
	}
}

// A slow progress callback does not hold up the sources: while it is busy
// with the first answer, the other three answer, the newer report supersedes
// the older and the search ends without waiting on the callback in between.
func TestSearchProgressSlowCallback(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	holding := false
	gate := make(chan struct{})
	var served sync.WaitGroup
	gated := func(hash string) http.Handler {
		body := page(row{title: "Дюна (2024) WEB-DL 1080p | Дубляж", hash: strings.Repeat(hash, 40), size: "10 GB", seeders: 5})
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			h := holding
			mu.Unlock()
			if h {
				defer served.Done()
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			serve(body).ServeHTTP(w, r)
		})
	}
	e.def("first", "Первый", serve(page(row{title: "Дюна (2024) WEB-DL 1080p | Дубляж", hash: strings.Repeat("a", 40), size: "10 GB", seeders: 50})))
	for _, d := range []string{"b", "c", "d"} {
		e.def(d, d, gated(d))
	}
	for _, d := range []string{"first", "b", "c", "d"} {
		e.add(d)
	}
	mu.Lock()
	holding = true
	mu.Unlock()
	served.Add(3)

	var got []Progress
	ctx := WithProgress(context.Background(), func(p Progress) {
		got = append(got, p)
		if len(got) == 2 { // the first source answered: the others answer while this call is busy
			close(gate)
			served.Wait()
			time.Sleep(200 * time.Millisecond) // their reports are in by now
		}
	})
	resp, err := e.m.Search(ctx, SearchRequest{Text: "Дюна", Kind: "movie", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("progress calls = %d, want 3 (start, the first source, the latest of the rest)", len(got))
	}
	if len(got[2].Pending) != 1 || len(got[2].Releases) != 3 {
		t.Errorf("last progress = pending %+v releases %d, want one pending and three releases", got[2].Pending, len(got[2].Releases))
	}
	if len(resp.Releases) != 4 {
		t.Fatalf("final releases = %d, want 4", len(resp.Releases))
	}
}

func TestDedupInfohash(t *testing.T) {
	e := newEnv(t)
	h := strings.Repeat("b", 40)
	e.def("one", "Один", serve(page(
		row{title: "Дюна (2024) WEB-DL 1080p | Дубляж", hash: h, size: "10 GB", seeders: 5},
		row{title: "Дюна (2024) BDRip 720p", size: "4 GB", seeders: 9}, // no link, no magnet: dropped
	)))
	e.def("two", "Два", serve(page(
		row{title: "Dune (2024) WEB-DL 1080p Dub", hash: strings.ToUpper(h), size: "10 GB", seeders: 40},
		row{title: "Дюна (2024) WEB-DL 2160p", link: "/dl/2", size: "20 GB", seeders: 3},
	)))
	e.add("one")
	two := e.add("two")

	resp, err := e.m.Search(context.Background(), SearchRequest{Text: "Дюна", AltTitles: []string{"Dune"}, Kind: "movie", Year: 2024})
	if err != nil {
		t.Fatal(err)
	}
	var withHash int
	for _, r := range resp.Releases {
		if strings.EqualFold(r.InfoHash, h) {
			withHash++
			if r.Seeders != 40 || r.SourceID != two.ID {
				t.Errorf("kept duplicate = %+v, want the one with more seeders", r)
			}
		}
		if r.Link == "" && r.Magnet == "" {
			t.Errorf("release without link and magnet kept: %+v", r)
		}
	}
	if withHash != 1 {
		t.Fatalf("releases with hash = %d, want 1: %+v", withHash, resp.Releases)
	}
	if len(resp.Releases) != 2 {
		t.Fatalf("releases = %d, want 2: %+v", len(resp.Releases), resp.Releases)
	}
}

func TestAddValidatesAndUpdateKeepsSecrets(t *testing.T) {
	e := newEnv(t)
	e.reload()
	ctx := context.Background()
	if _, err := e.m.Add(ctx, "no-such-tracker", nil); err == nil {
		t.Fatal("Add of unknown definition: no error")
	}
	// rutracker needs a login and a password: nothing is saved without them
	if _, err := e.m.Add(ctx, "rutracker", map[string]string{"username": "u"}); err == nil || !strings.Contains(err.Error(), "Пароль") {
		t.Fatalf("Add without password: err = %v, want a message naming the field", err)
	}
	srcs, _ := e.m.Sources(ctx)
	if len(srcs) != 0 {
		t.Fatalf("sources = %+v, want none", srcs)
	}

	// direct insert to avoid touching the network
	s, err := e.m.insert(ctx, "rutracker", "RuTracker.org", map[string]string{"username": "u", "password": "secret"}, true, 25)
	if err != nil {
		t.Fatal(err)
	}
	red := s.Redacted()
	if red.Settings["password"] == "secret" || red.Settings["username"] != "u" {
		t.Fatalf("redacted = %+v", red.Settings)
	}
	off := false
	name := "Мой RuTracker"
	got, err := e.m.Update(ctx, s.ID, SourcePatch{Name: &name, Enabled: &off, Settings: map[string]string{"password": red.Settings["password"]}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings["password"] != "secret" || got.Name != name || got.Enabled {
		t.Fatalf("after update = %+v", got)
	}
	if err := e.m.Remove(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := e.m.Sources(ctx); len(srcs) != 0 {
		t.Fatalf("after remove: %+v", srcs)
	}
}

func TestPrefsDefaultsAndRoundTrip(t *testing.T) {
	e := newEnv(t)
	e.reload()
	ctx := context.Background()
	p, err := e.m.Prefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, DefaultPrefs()) || p.MaxHeight != 1080 || !p.PreferDub || p.MinSources != 1 {
		t.Fatalf("default prefs = %+v", p)
	}
	p.MaxHeight = 2160
	p.MaxSizeGB = 30
	if err := e.m.SavePrefs(ctx, p); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := e.db.QueryRow(`SELECT value FROM settings WHERE key='release_prefs'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	got, _ := e.m.Prefs(ctx)
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("prefs = %+v, want %+v (raw %s)", got, p, raw)
	}
}

func TestSourceJSONRedactsSecrets(t *testing.T) {
	s := Source{ID: 1, DefinitionID: "rutracker", Settings: map[string]string{"username": "alex", "password": "secret", "cookie": "bb_session=1"}}
	raw, err := json.Marshal(Imported{Name: "x", Source: &s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "bb_session") || !strings.Contains(string(raw), "alex") {
		t.Fatalf("json = %s", raw)
	}
	if s.Settings["password"] != "secret" {
		t.Fatal("marshalling changed the source")
	}
}

func TestCancelledAddKeepsStatusUnknown(t *testing.T) {
	e := newEnv(t)
	e.def("hang", "Висит", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	var statuses []string
	e.m.onStatus = func(id int64, status, msg string) { statuses = append(statuses, status) }
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	s, err := e.m.Add(ctx, "hang", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if s.ID == 0 || s.Status != StatusUnknown {
		t.Fatalf("source = %+v, want saved with status unknown", s)
	}
	if len(statuses) != 1 || statuses[0] != StatusUnknown {
		t.Fatalf("OnStatus calls = %v", statuses)
	}
}

func TestLooseSearchAndSeasonQueryForForeignTrackers(t *testing.T) {
	e := newEnv(t)
	e.def("ru", "Русский", serve(page(row{title: "Дюна: Часть вторая (2024) WEB-DL 1080p", hash: strings.Repeat("c", 40), size: "8 GB", seeders: 10})))
	var mu sync.Mutex
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("q"))
		mu.Unlock()
		w.Write([]byte(page()))
	}))
	t.Cleanup(srv.Close)
	def := strings.Replace(fakeDef("intl", "International", srv.URL), "language: ru-RU", "language: en-US", 1)
	os.WriteFile(filepath.Join(e.dir, "definitions", "intl.yml"), []byte(def), 0o644)
	e.reload()
	e.add("ru")
	e.add("intl")
	ctx := context.Background()

	strict, _ := e.m.Search(ctx, SearchRequest{Text: "Дюна", Kind: "movie"})
	loose, _ := e.m.Search(ctx, SearchRequest{Text: "Дюна", Kind: "movie", Loose: true})
	if len(strict.Releases) != 0 || len(strict.Rejected) != 1 || len(loose.Releases) != 1 {
		t.Fatalf("strict %d/%d, loose %d", len(strict.Releases), len(strict.Rejected), len(loose.Releases))
	}

	mu.Lock()
	queries = nil
	mu.Unlock()
	e.m.Search(ctx, SearchRequest{Text: "Сёгун", AltTitles: []string{"Shogun"}, Kind: "series", Season: 1})
	mu.Lock()
	defer mu.Unlock()
	if len(queries) != 2 || queries[0] != "Сёгун S01" || queries[1] != "Shogun S01" {
		t.Fatalf("foreign tracker queries = %q", queries)
	}
}

// A release link a profile sends must belong to its source: a magnet, or an
// http(s) link on a host of the source (or a subdomain of it).
func TestCheckReleaseLink(t *testing.T) {
	e := newEnv(t)
	e.def("tr", "Трекер", serve(page()))
	src := e.add("tr")
	def, err := e.cat.Definition("tr")
	if err != nil || len(def.Links) == 0 {
		t.Fatalf("definition: %v %v", def, err)
	}
	site := strings.TrimSuffix(def.Links[0], "/")
	ctx := context.Background()
	for _, link := range []string{site + "/dl/1.torrent", "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)} {
		if err := e.m.CheckReleaseLink(ctx, src.ID, link); err != nil {
			t.Errorf("%s: %v", link, err)
		}
	}
	for _, link := range []string{
		"http://evil.example.org/dl/1.torrent",
		strings.Replace(site, "127.0.0.1", "localhost", 1) + "/dl/1.torrent",
		"http://169.254.169.254/latest/meta-data",
		"http://sub." + strings.TrimPrefix(site, "http://") + "/x", // an IP has no subdomains
		"file:///etc/passwd",
		"/dl/1.torrent",
		"//evil.example.org/x",
		"",
	} {
		if err := e.m.CheckReleaseLink(ctx, src.ID, link); err == nil {
			t.Errorf("%s accepted", link)
		}
	}
	if err := e.m.CheckReleaseLink(ctx, 9999, site+"/x"); err == nil {
		t.Error("unknown source accepted")
	}
}

func TestHostOnSite(t *testing.T) {
	sites := []string{"https://rutracker.org/", "http://tracker.example", "api.movies.example", "files.example:8080"}
	for host, want := range map[string]bool{
		"rutracker.org": true, "RUTRACKER.org": true, "dl.rutracker.org": true, "tracker.example": true,
		"api.movies.example": true, "cdn.api.movies.example": true, "files.example": true, "movies.example": false,
		"evilrutracker.org": false, "rutracker.org.evil.com": false, "org": false, "": false,
	} {
		if got := hostOnSite(sites, host); got != want {
			t.Errorf("%q: %v", host, got)
		}
	}
}

// An API definition downloads from its api host: the admin's apiurl setting,
// or the definition's default one.
func TestCheckReleaseLinkAPIHost(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(serve(page()))
	t.Cleanup(srv.Close)
	def := strings.Replace(fakeDef("api", "API", srv.URL), "settings: []", "settings:\n  - {name: apiurl, type: text, label: API, default: api.default.example}", 1)
	if err := os.WriteFile(filepath.Join(e.dir, "definitions", "api.yml"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	e.reload()
	ctx := context.Background()
	plain := e.add("api")
	custom, err := e.m.Add(ctx, "api", map[string]string{"apiurl": "movies.custom.example"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		src  Source
		link string
	}{{plain, "https://api.default.example/dl/1.torrent"}, {custom, "https://movies.custom.example/dl/1.torrent"}} {
		if err := e.m.CheckReleaseLink(ctx, c.src.ID, c.link); err != nil {
			t.Errorf("%s: %v", c.link, err)
		}
	}
	if err := e.m.CheckReleaseLink(ctx, custom.ID, "https://evil.example.org/dl/1.torrent"); err == nil {
		t.Error("foreign host accepted")
	}
}

// RuTracker by login and by cookie list the same topic under two names:
// one release (its page), while different topics stay apart.
func TestDedupKeyByDetailsPage(t *testing.T) {
	a := ScoredRelease{Result: Result{Title: "(Thrash Metal) Metallica - Kill 'Em All", Details: "https://rutracker.org/forum/viewtopic.php?t=123", Link: "https://rutracker.org/forum/dl.php?t=123"}, SourceID: 3}
	b := ScoredRelease{Result: Result{Title: "Metallica - Kill 'Em All", Details: "https://www.rutracker.org/forum/viewtopic.php?t=123", Link: "https://rutracker.org/forum/dl.php?t=123&cookie"}, SourceID: 15}
	c := ScoredRelease{Result: Result{Title: "Metallica - Kill 'Em All", Details: "https://rutracker.org/forum/viewtopic.php?t=124"}, SourceID: 15}
	if dedupKey(a, nil) != dedupKey(b, nil) {
		t.Errorf("same topic: %q != %q", dedupKey(a, nil), dedupKey(b, nil))
	}
	if dedupKey(a, nil) == dedupKey(c, nil) {
		t.Errorf("other topic merged: %q", dedupKey(c, nil))
	}
	// no page: the link, per source, as before
	if k := dedupKey(ScoredRelease{Result: Result{Link: "/dl/2"}, SourceID: 7}, nil); k != "src:7:/dl/2" {
		t.Errorf("link key = %q", k)
	}
	// a source whose rows all point at one page: that page is no key
	shared := sharedPages([]found{{done: true, results: []Result{
		{Details: "https://site/index.php", Link: "/dl/1"}, {Details: "https://site/index.php", Link: "/dl/2"},
		// one topic found by two queries
		{Details: "https://rutracker.org/forum/viewtopic.php?t=123", Link: "/dl/123"}, {Details: "https://rutracker.org/forum/viewtopic.php?t=123", Link: "/dl/123"},
	}}})
	if shared[pageKey(a)] {
		t.Error("a topic found twice by one source counts as a shared page")
	}
	x := ScoredRelease{Result: Result{Details: "https://site/index.php", Link: "/dl/1"}, SourceID: 2}
	y := ScoredRelease{Result: Result{Details: "https://site/index.php", Link: "/dl/2"}, SourceID: 2}
	if dedupKey(x, shared) == dedupKey(y, shared) {
		t.Errorf("rows of a shared page merged: %q", dedupKey(x, shared))
	}
}

// A search names the tracker of each source that answered or failed by the
// source the user set up, and lists the ids that stand for it: the backup
// way in answers in the primary's place and its releases carry its own id,
// so the tracking scan can tell which of its earlier rows a failed tracker
// owns.
func TestSearchNamesTheTrackerOfAnswersAndFailures(t *testing.T) {
	e := newEnv(t)
	var primaryDown, backupDown atomic.Bool
	handler := func(down *atomic.Bool, link string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if down.Load() && r.URL.Query().Get("q") != "" {
				http.Error(w, "blocked", http.StatusForbidden)
				return
			}
			serve(page(row{title: "Дюна (2024) WEB-DL 1080p", link: link, size: "10 GB", seeders: 5})).ServeHTTP(w, r)
		})
	}
	e.def("rutracker", "RuTracker.org", handler(&primaryDown, "/dl/1"))
	e.def("rutracker-org-cookie", "RuTracker.org (cookie)", handler(&backupDown, "/dl/2"))
	primary := e.add("rutracker")
	backup := e.add("rutracker-org-cookie")
	q := SearchRequest{Text: "Дюна", Kind: "movie", Year: 2024}
	search := func() SearchResponse {
		t.Helper()
		resp, err := e.m.Search(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// the primary answers
	if resp := search(); !slices.Equal(resp.AnsweredIDs, []int64{primary.ID}) || resp.Answered != 1 || len(resp.Failed) != 0 {
		t.Fatalf("answered %v (%d), failed %+v; want the primary", resp.AnsweredIDs, resp.Answered, resp.Failed)
	}
	// the backup answers in its place: still the primary's tracker
	primaryDown.Store(true)
	resp := search()
	if !slices.Equal(resp.AnsweredIDs, []int64{primary.ID}) || len(resp.Failed) != 0 || resp.Releases[0].SourceID != backup.ID {
		t.Fatalf("answered %v, failed %+v, release of %d; want the primary's tracker, the backup's release", resp.AnsweredIDs, resp.Failed, resp.Releases[0].SourceID)
	}
	// neither answers: the tracker failed, both ways in stand for it
	backupDown.Store(true)
	resp = search()
	if len(resp.AnsweredIDs) != 0 || resp.Answered != 0 || len(resp.Failed) != 1 {
		t.Fatalf("answered %v, failed %+v; want one failure", resp.AnsweredIDs, resp.Failed)
	}
	if f := resp.Failed[0]; f.Tracker() != primary.ID || !slices.Equal(f.Ids(), []int64{primary.ID, backup.ID}) {
		t.Fatalf("failed %+v: tracker %d, ids %v; want the primary's tracker standing for %d and %d", f, f.Tracker(), f.Ids(), primary.ID, backup.ID)
	}
	// the primary resting after a refused sign-in: the backup is asked in
	// its place, and answers or fails as the primary's tracker
	e.m.rest(primary.ID)
	resp = search()
	if len(resp.Failed) != 1 || resp.Failed[0].SourceID != backup.ID || resp.Failed[0].Tracker() != primary.ID {
		t.Fatalf("primary resting, backup down: failed %+v, want the backup's id for the primary's tracker", resp.Failed)
	}
	backupDown.Store(false)
	if resp = search(); !slices.Equal(resp.AnsweredIDs, []int64{primary.ID}) || len(resp.Failed) != 0 {
		t.Fatalf("primary resting, backup up: answered %v, failed %+v; want the primary's tracker", resp.AnsweredIDs, resp.Failed)
	}
	// a source with no backup stands for itself
	if f := (SourceError{SourceID: 9}); f.Tracker() != 9 || !slices.Equal(f.Ids(), []int64{9}) {
		t.Fatalf("a failure naming one source: tracker %d, ids %v", f.Tracker(), f.Ids())
	}
}

// RuTracker by cookie backs RuTracker by login: one source to search. The
// backup is asked only when the primary fails (the tracker is not asked
// twice), and its releases come under the primary's name.
func TestSearchBackupSourceOnlyWhenPrimaryFails(t *testing.T) {
	e := newEnv(t)
	var primaryDown atomic.Bool
	var backupSearches atomic.Int32
	e.def("rutracker", "RuTracker.org", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if primaryDown.Load() && r.URL.Query().Get("q") != "" {
			http.Error(w, "blocked", http.StatusForbidden)
			return
		}
		serve(page(row{title: "Дюна (2024) WEB-DL 1080p", link: "/dl/1", size: "10 GB", seeders: 5})).ServeHTTP(w, r)
	}))
	e.def("rutracker-org-cookie", "RuTracker.org (cookie)", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "" {
			backupSearches.Add(1)
		}
		serve(page(row{title: "Дюна (2024) WEB-DL 2160p", link: "/dl/2", size: "20 GB", seeders: 9})).ServeHTTP(w, r)
	}))
	primary := e.add("rutracker")
	backup := e.add("rutracker-org-cookie")
	if backup.FallbackFor != primary.ID {
		t.Fatalf("backup.FallbackFor = %d, want %d", backup.FallbackFor, primary.ID)
	}
	ctx := context.Background()
	q := SearchRequest{Text: "Дюна", Kind: "movie", Year: 2024}

	resp, err := e.m.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if n := backupSearches.Load(); n != 0 {
		t.Errorf("backup searched %d times while the primary answered", n)
	}
	if len(resp.Releases) != 1 || resp.Releases[0].SourceID != primary.ID {
		t.Fatalf("releases = %+v, want the primary's one", resp.Releases)
	}

	primaryDown.Store(true)
	resp, err = e.m.Search(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if backupSearches.Load() == 0 {
		t.Fatal("backup not asked when the primary failed")
	}
	if len(resp.Failed) != 0 {
		t.Errorf("failed = %+v, want none: the backup answered", resp.Failed)
	}
	if len(resp.Releases) != 1 || resp.Releases[0].SourceID != backup.ID || resp.Releases[0].SourceName != "RuTracker.org" {
		t.Fatalf("releases = %+v, want the backup's under the primary's name", resp.Releases)
	}

	// removing the primary removes its backup too
	if err := e.m.Remove(ctx, primary.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.m.Get(ctx, backup.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("backup after removing the primary: %v", err)
	}
}
