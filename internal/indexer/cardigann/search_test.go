package cardigann

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
)

func loadDef(t *testing.T, name string) *Definition {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	def, err := Parse(data)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return def
}

func mustParse(t *testing.T, yml string) *Definition {
	t.Helper()
	def, err := Parse([]byte(yml))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return def
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseRealDefinitions(t *testing.T) {
	for _, name := range []string{"rutor.yml", "kinozal.yml", "thepiratebay.yml"} {
		def := loadDef(t, name)
		if def.ID == "" || len(def.Search.Paths) == 0 || len(def.Search.Fields) == 0 {
			t.Fatalf("%s: incomplete definition %+v", name, def)
		}
	}
	k := loadDef(t, "kinozal.yml")
	if k.Encoding != "windows-1251" || k.Login == nil || k.Login.Method != "post" {
		t.Fatalf("kinozal login/encoding not parsed: %+v", k.Login)
	}
	// field order and the case map order must be preserved
	if k.Search.Fields[0].Key != "title" {
		t.Fatalf("first kinozal field = %q", k.Search.Fields[0].Key)
	}
	for _, fl := range k.Search.Fields {
		if fl.Key == "downloadvolumefactor" {
			if len(fl.Case) != 3 || fl.Case[0].Key != "a.r1" || fl.Case[2].Key != "*" {
				t.Fatalf("case order lost: %+v", fl.Case)
			}
		}
	}
}

func TestSearchHTML(t *testing.T) {
	page := readFixture(t, "rutor_search.html")
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(page)
	}))
	defer srv.Close()

	r := &Runner{Def: loadDef(t, "rutor.yml"), Settings: map[string]string{"sitelink": srv.URL + "/"}, HTTP: newClient(t)}
	res, err := r.Search(context.Background(), Query{Keywords: "Дюна"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "/search/0/0/100/0/%D0%94%D1%8E%D0%BD%D0%B0/"; gotURI != want {
		t.Fatalf("request URI = %q, want %q", gotURI, want)
	}
	if len(res) < 3 {
		t.Fatalf("got %d results, want >= 3: %+v", len(res), res)
	}
	first := res[0]
	if want := "Дюна: Часть вторая / Dune: Part Two (2024) WEB-DL 1080p-Дубляж"; first.Title != want {
		t.Fatalf("title = %q, want %q", first.Title, want)
	}
	if first.Size != 11005853696 { // 10.25 GB
		t.Fatalf("size = %d", first.Size)
	}
	if first.Seeders != 1520 || first.Leechers != 87 {
		t.Fatalf("peers = %d/%d", first.Seeders, first.Leechers)
	}
	if first.InfoHash != "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678" {
		t.Fatalf("infohash = %q", first.InfoHash)
	}
	if !strings.HasPrefix(first.Magnet, "magnet:?xt=urn:btih:a1b2c3d4e5f60718293a4b5c6d7e8f9012345678") {
		t.Fatalf("magnet = %q", first.Magnet)
	}
	if first.Link != "http://d.rutor.info/download/970001" {
		t.Fatalf("link = %q", first.Link)
	}
	if first.Details != srv.URL+"/torrent/970001/djuna-chast-vtoraja_dune-part-two-2024-web-dl-1080p" {
		t.Fatalf("details = %q", first.Details)
	}
	wantDate := time.Date(2024, 3, 5, 0, 0, 0, 0, time.FixedZone("", 3*3600))
	if !first.PublishDate.Equal(wantDate) {
		t.Fatalf("date = %v, want %v", first.PublishDate, wantDate)
	}
	if len(first.Category) != 1 || first.Category[0] != 8000 {
		t.Fatalf("category = %v", first.Category)
	}
	if first.DownloadVolumeFactor != 0 || first.UploadVolumeFactor != 1 {
		t.Fatalf("volume factors = %v/%v", first.DownloadVolumeFactor, first.UploadVolumeFactor)
	}
	// row without a comments cell: size found via :contains, not position
	if res[1].Size != 4692251770 || res[1].Seeders != 340 {
		t.Fatalf("second row size/seeders = %d/%d", res[1].Size, res[1].Seeders)
	}
	if !strings.Contains(res[2].Title, "S1E1-6 of 6") {
		t.Fatalf("season normalisation: %q", res[2].Title)
	}
	if res[3].Size != 747005542 { // 712.40 MB
		t.Fatalf("MB size = %d", res[3].Size)
	}
}

func TestSearchKeywordlessAndIMDB(t *testing.T) {
	var uris []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uris = append(uris, r.RequestURI)
		w.Write([]byte("<html><body><table></table></body></html>"))
	}))
	defer srv.Close()
	r := &Runner{Def: loadDef(t, "rutor.yml"), Settings: map[string]string{"sitelink": srv.URL + "/", "sort": "2"}, HTTP: newClient(t)}
	if _, err := r.Search(context.Background(), Query{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Search(context.Background(), Query{IMDBID: "tt0111161"}); err != nil {
		t.Fatal(err)
	}
	if len(uris) != 2 || uris[0] != "/" || uris[1] != "/search/0/0/010/2/tt0111161" {
		t.Fatalf("uris = %v", uris)
	}
}

func TestSearchCP1251(t *testing.T) {
	page, err := charmap.Windows1251.NewEncoder().Bytes(readFixture(t, "kinozal_search.html"))
	if err != nil {
		t.Fatal(err)
	}
	var loginForm url.Values
	var searchQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/takelogin.php", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", 405)
			return
		}
		r.ParseForm()
		loginForm = r.PostForm
		http.SetCookie(w, &http.Cookie{Name: "uid", Value: "77", Path: "/"})
		http.SetCookie(w, &http.Cookie{Name: "pass", Value: "secret", Path: "/"})
		http.Redirect(w, r, "/", http.StatusFound)
	})
	authed := func(r *http.Request) bool {
		c, err := r.Cookie("uid")
		return err == nil && c.Value == "77"
	}
	mux.HandleFunc("/my.php", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Redirect(w, r, "/login.php", http.StatusFound)
			return
		}
		w.Write([]byte(`<a href="/logout.php?hash4u=1">x</a>`))
	})
	mux.HandleFunc("/browse.php", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Redirect(w, r, "/login.php", http.StatusFound)
			return
		}
		searchQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		w.Write(page)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("home")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := &Runner{
		Def: loadDef(t, "kinozal.yml"),
		Settings: map[string]string{
			"sitelink": srv.URL + "/", "username": "Пользователь", "password": "pw",
			"stripcyrillic": "false", "addrussian": "false",
		},
		HTTP: newClient(t),
	}
	res, err := r.Search(context.Background(), Query{Keywords: "Брат 2"})
	if err != nil {
		t.Fatal(err)
	}
	if loginForm.Get("password") != "pw" {
		t.Fatalf("login form = %v", loginForm)
	}
	// the username is sent in windows-1251: decode it back
	if u, _ := charmap.Windows1251.NewDecoder().String(loginForm.Get("username")); u != "Пользователь" {
		t.Fatalf("login username not cp1251: %q", loginForm.Get("username"))
	}
	if !strings.Contains(searchQuery, "s=%C1%F0%E0%F2+2") {
		t.Fatalf("keywords not cp1251-encoded: %q", searchQuery)
	}
	if len(res) != 3 {
		t.Fatalf("got %d results: %+v", len(res), res)
	}
	if res[0].Title != "Брат 2 2000 BDRip 1080p" {
		t.Fatalf("title = %q", res[0].Title)
	}
	if !strings.Contains(res[1].Title, "Мастер и Маргарита") || !strings.Contains(res[2].Title, "Жмурки") {
		t.Fatalf("cyrillic titles broken: %q / %q", res[1].Title, res[2].Title)
	}
	if res[0].Size != 10576356966 || res[0].Seeders != 410 || res[0].Leechers != 7 {
		t.Fatalf("size/peers = %d %d/%d", res[0].Size, res[0].Seeders, res[0].Leechers)
	}
	if res[0].Link != srv.URL+"/download.php?id=2001001" || res[0].Magnet != "" {
		t.Fatalf("link = %q magnet = %q (semi-private: no magnet)", res[0].Link, res[0].Magnet)
	}
	if res[0].DownloadVolumeFactor != 0 || res[1].DownloadVolumeFactor != 1 || res[2].DownloadVolumeFactor != 0.5 {
		t.Fatalf("dvf = %v %v %v", res[0].DownloadVolumeFactor, res[1].DownloadVolumeFactor, res[2].DownloadVolumeFactor)
	}
	if len(res[1].Category) == 0 || res[1].Category[0] != 5000 || res[0].Category[0] != 2000 {
		t.Fatalf("categories = %v %v", res[0].Category, res[1].Category)
	}
	now := time.Now()
	if d := res[0].PublishDate; d.Year() != now.Year() || d.YearDay() != now.YearDay() || d.Hour() != 12 || d.Minute() != 30 {
		t.Fatalf("today date = %v", d)
	}
	if d := res[2].PublishDate; d.Year() != 2017 || d.Month() != 10 || d.Day() != 24 || d.Hour() != 23 {
		t.Fatalf("dated row = %v", d)
	}
	if !strings.Contains(r.Cookies(), "uid=77") {
		t.Fatalf("cookies = %q", r.Cookies())
	}
}

const formLoginDef = `
id: formsite
name: Form Site
type: private
encoding: UTF-8
links: ["http://placeholder/"]
caps:
  categorymappings:
    - {id: 10, cat: Movies/HD, desc: "HD"}
    - {id: 20, cat: TV, desc: "TV"}
  modes: {search: [q]}
settings:
  - {name: username, type: text, label: Username}
  - {name: password, type: password, label: Password}
login:
  path: login.php
  method: form
  form: form#login
  inputs:
    user: "{{ .Config.username }}"
    pass: "{{ .Config.password }}"
  selectorinputs:
    csrf:
      selector: meta[name="csrf"]
      attribute: content
  error:
    - selector: div.error
  test:
    path: index.php
    selector: a.logout
search:
  paths:
    - path: browse.php
  inputs:
    q: "{{ .Keywords }}"
    cats: "{{ join .Categories \",\" }}"
  rows:
    selector: tr.row
  fields:
    title:
      selector: a.t
    details:
      selector: a.t
      attribute: href
    download:
      selector: a.dl
      attribute: href
    category:
      selector: td.cat
    size:
      selector: td.size
    seeders:
      selector: td.s
    leechers:
      selector: td.l
    date:
      selector: td.date
      filters:
        - name: dateparse
          args: "2006-01-02"
download:
  selectors:
    - selector: a[href^="magnet:"]
      attribute: href
`

func formSiteServer(t *testing.T, logins *int32) *httptest.Server {
	const sid = "s3cr3t"
	authed := func(r *http.Request) bool {
		c, err := r.Cookie("sid")
		return err == nil && c.Value == sid
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/login.php", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.SetCookie(w, &http.Cookie{Name: "pre", Value: "1", Path: "/"})
			w.Write([]byte(`<html><head><meta name="csrf" content="tok123"></head><body>
<form id="search" action="/search.php"><input name="q"></form>
<form id="login" action="/dologin.php" method="post">
<input type="text" name="user" value="">
<input type="password" name="pass">
<input type="hidden" name="hidden1" value="h">
<input type="checkbox" name="remember" value="1" checked>
<input type="checkbox" name="newsletter" value="1">
<input type="text" name="disabled_field" value="x" disabled>
<input type="submit" value="Go">
</form></body></html>`))
			return
		}
		http.Error(w, "use dologin", 400)
	})
	mux.HandleFunc("/dologin.php", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f := r.PostForm
		if r.Method != http.MethodPost || f.Get("csrf") != "tok123" || f.Get("hidden1") != "h" || f.Get("remember") != "1" ||
			f.Has("newsletter") || f.Has("disabled_field") {
			http.Error(w, "bad form: "+f.Encode(), 400)
			return
		}
		if c, err := r.Cookie("pre"); err != nil || c.Value != "1" {
			http.Error(w, "landing cookie missing", 400)
			return
		}
		if f.Get("user") != "alice" || f.Get("pass") != "wonder" {
			w.Write([]byte(`<html><body><div class="error">Неверный пароль</div></body></html>`))
			return
		}
		atomic.AddInt32(logins, 1)
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: sid, Path: "/"})
		http.Redirect(w, r, "/index.php", http.StatusFound)
	})
	mux.HandleFunc("/index.php", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.Write([]byte(`<html><body>guest</body></html>`))
			return
		}
		w.Write([]byte(`<html><body><a class="logout" href="/logout">out</a></body></html>`))
	})
	mux.HandleFunc("/browse.php", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Redirect(w, r, "/login.php", http.StatusFound)
			return
		}
		if r.URL.Query().Get("q") != "" && r.URL.Query().Get("cats") != "10" {
			http.Error(w, "cats = "+r.URL.Query().Get("cats"), 400)
			return
		}
		w.Write([]byte(`<html><body><a class="logout" href="/logout">out</a><table>
<tr class="row"><td class="cat">10</td><td><a class="t" href="/details.php?id=1">Movie.2024.1080p.WEB-DL</a> <a class="dl" href="/dl.php?id=1">dl</a></td>
<td class="size">1.5 GB</td><td class="s">10</td><td class="l">2</td><td class="date">2024-05-06</td></tr>
</table></body></html>`))
	})
	mux.HandleFunc("/details.php", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><a href="magnet:?xt=urn:btih:ABCDEFABCDEFABCDEFABCDEFABCDEFABCDEFABCD&dn=x">magnet</a></body></html>`))
	})
	return httptest.NewServer(mux)
}

func TestLoginFormPost(t *testing.T) {
	var logins int32
	srv := formSiteServer(t, &logins)
	defer srv.Close()
	def := mustParse(t, formLoginDef)

	bad := &Runner{Def: def, Settings: map[string]string{"sitelink": srv.URL + "/", "username": "alice", "password": "nope"}, HTTP: newClient(t)}
	err := bad.Login(context.Background())
	if !errors.Is(err, ErrLoginFailed) || !strings.Contains(err.Error(), "Неверный пароль") {
		t.Fatalf("bad password: err = %v", err)
	}

	r := &Runner{Def: def, Settings: map[string]string{"sitelink": srv.URL + "/", "username": "alice", "password": "wonder"}, HTTP: newClient(t)}
	if err := r.Test(context.Background()); err != nil {
		t.Fatalf("Test: %v", err)
	}
	res, err := r.Search(context.Background(), Query{Keywords: "movie", Categories: []int{2000}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Title != "Movie.2024.1080p.WEB-DL" || res[0].Size != 1610612736 || res[0].Category[0] != 2040 {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Magnet != "" {
		t.Fatalf("private site must not get a generated magnet: %q", res[0].Magnet)
	}
	if atomic.LoadInt32(&logins) != 1 {
		t.Fatalf("logins = %d, want 1", logins)
	}
	// a fresh runner (new cookie jar) logs in by itself before searching
	r2 := &Runner{Def: def, Settings: r.Settings, HTTP: newClient(t)}
	if _, err := r2.Search(context.Background(), Query{Keywords: "movie", Categories: []int{2000}}); err != nil {
		t.Fatalf("search without explicit login: %v", err)
	}
	if atomic.LoadInt32(&logins) != 2 {
		t.Fatalf("logins = %d, want 2", logins)
	}
	// download selectors: details page -> magnet
	dl, err := r.Download(context.Background(), srv.URL+"/details.php?id=1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dl.Magnet, "magnet:?xt=urn:btih:ABCDEF") {
		t.Fatalf("download = %+v", dl)
	}
}

const cookieLoginDef = `
id: rutracker-cookie
name: RuTracker (cookie)
type: semi-private
encoding: windows-1251
links: ["https://rutracker.org/"]
caps:
  categorymappings:
    - {id: 313, cat: Movies/HD, desc: "HD Video"}
  modes: {search: [q]}
settings:
  - name: cookie
    type: text
    label: Cookie
login:
  method: cookie
  inputs:
    cookie: "{{ .Config.cookie }}"
  test:
    path: forum/index.php
    selector: "#logged-in-username"
search:
  paths:
    - path: forum/tracker.php
  inputs:
    nm: "{{ .Keywords }}"
  rows:
    selector: 'div#search-results tr:has(a[href^="viewtopic.php?t="])'
  fields:
    category:
      selector: a[href^="tracker.php?f="]
      attribute: href
      filters:
        - name: querystring
          args: f
    title:
      selector: a[href^="viewtopic.php?t="]
    details:
      selector: a[href^="viewtopic.php?t="]
      attribute: href
    download:
      selector: a.tr-dl
      attribute: href
    size:
      selector: a.tr-dl
    seeders:
      selector: b.seedmed
    leechers:
      selector: td.leechmed
    date:
      selector: td p
      filters:
        - name: fuzzytime
`

func TestLoginCookieMethod(t *testing.T) {
	var sawCookie string
	mux := http.NewServeMux()
	ok := func(r *http.Request) bool {
		c, err := r.Cookie("bb_session")
		return err == nil && c.Value == "0-123-abc"
	}
	mux.HandleFunc("/forum/index.php", func(w http.ResponseWriter, r *http.Request) {
		sawCookie = r.Header.Get("Cookie")
		if !ok(r) {
			w.Write([]byte(`<html><body><a href="login.php">Вход</a></body></html>`))
			return
		}
		w.Write([]byte(`<html><body><a id="logged-in-username">bob</a></body></html>`))
	})
	mux.HandleFunc("/forum/tracker.php", func(w http.ResponseWriter, r *http.Request) {
		if !ok(r) {
			http.Redirect(w, r, "/forum/login.php", http.StatusFound)
			return
		}
		html, err := charmap.Windows1251.NewEncoder().String(`<html><body><a id="logged-in-username">bob</a><div id="search-results"><table>
<tr><td><a href="tracker.php?f=313">HD Video</a></td><td><a href="viewtopic.php?t=6543210">Сёгун / Shogun / Сезон: 1 / Серии: 1-10 из 10 [2024, WEB-DL 1080p]</a></td>
<td><a class="tr-dl" href="dl.php?t=6543210">19.6 GB</a></td><td><b class="seedmed">250</b></td><td class="leechmed">31</td><td><p>2024-04-23 12:00</p></td></tr>
</table></div></body></html>`)
		if err != nil {
			t.Error(err)
		}
		w.Write([]byte(html))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	def := mustParse(t, cookieLoginDef)

	r := &Runner{Def: def, Settings: map[string]string{"sitelink": srv.URL + "/", "cookie": "bb_session=0-123-abc; bb_ssl=1"}, HTTP: newClient(t)}
	if err := r.Test(context.Background()); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if !strings.Contains(sawCookie, "bb_session=0-123-abc") || !strings.Contains(sawCookie, "bb_ssl=1") {
		t.Fatalf("cookie header = %q", sawCookie)
	}
	res, err := r.Search(context.Background(), Query{Keywords: "Сёгун"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !strings.HasPrefix(res[0].Title, "Сёгун / Shogun") || res[0].Seeders != 250 || res[0].Category[0] != 2040 {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Link != srv.URL+"/forum/dl.php?t=6543210" {
		t.Fatalf("link = %q", res[0].Link)
	}

	expired := &Runner{Def: def, Settings: map[string]string{"sitelink": srv.URL + "/", "cookie": "bb_session=old"}, HTTP: newClient(t)}
	if err := expired.Test(context.Background()); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("expired cookie: err = %v", err)
	}
	if _, err := expired.Search(context.Background(), Query{Keywords: "x"}); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("expired cookie search: err = %v", err)
	}
}

func TestFlareSolverrFallback(t *testing.T) {
	page := readFixture(t, "rutor_search.html")
	var siteUA, siteCookie atomic.Value
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("cf_clearance")
		if err != nil || c.Value != "cleared" || r.UserAgent() != "FS-Browser/1.0" {
			w.Header().Set("Server", "cloudflare")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`<!DOCTYPE html><html><head><title>Just a moment...</title></head><body><div id="challenge-platform"></div></body></html>`))
			return
		}
		siteUA.Store(r.UserAgent())
		siteCookie.Store(c.Value)
		w.Write(page)
	}))
	defer site.Close()
	siteURL, _ := url.Parse(site.URL)

	var fsCalls int32
	var fsReq map[string]any
	fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1" || r.Method != http.MethodPost {
			http.Error(w, "not found", 404)
			return
		}
		atomic.AddInt32(&fsCalls, 1)
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &fsReq)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "message": "Challenge solved!",
			"solution": map[string]any{
				"url": fsReq["url"], "status": 200,
				"response":  string(page),
				"userAgent": "FS-Browser/1.0",
				"cookies": []map[string]any{
					{"name": "cf_clearance", "value": "cleared", "domain": siteURL.Hostname(), "path": "/"},
				},
			},
		})
	}))
	defer fs.Close()

	r := &Runner{Def: loadDef(t, "rutor.yml"), Settings: map[string]string{"sitelink": site.URL + "/"}, HTTP: newClient(t), FlareSolverr: fs.URL}
	res, err := r.Search(context.Background(), Query{Keywords: "Дюна"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) < 3 {
		t.Fatalf("results via flaresolverr = %d", len(res))
	}
	if fsReq["cmd"] != "request.get" || !strings.HasPrefix(fsReq["url"].(string), site.URL+"/search/") {
		t.Fatalf("flaresolverr request = %v", fsReq)
	}
	// second search goes direct with the solved cookie and user agent
	if _, err := r.Search(context.Background(), Query{Keywords: "Дюна"}); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&fsCalls); n != 1 {
		t.Fatalf("flaresolverr calls = %d, want 1", n)
	}
	if siteUA.Load() != "FS-Browser/1.0" || siteCookie.Load() != "cleared" {
		t.Fatalf("direct request lacked UA/cookie: %v %v", siteUA.Load(), siteCookie.Load())
	}

	// without FlareSolverr the challenge is a clear error
	plain := &Runner{Def: loadDef(t, "rutor.yml"), Settings: map[string]string{"sitelink": site.URL + "/"}, HTTP: newClient(t)}
	if _, err := plain.Search(context.Background(), Query{Keywords: "x"}); !errors.Is(err, ErrCloudflare) {
		t.Fatalf("err = %v, want ErrCloudflare", err)
	}
}

func TestSearchJSON(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[
 {"id":"101","name":"Dune Part Two 2024 1080p WEBRip x265","info_hash":"AAAABBBBCCCCDDDDEEEEFFFF0000111122223333","leechers":"12","seeders":"345","num_files":"3","size":"2147483648","username":"uploader1","added":"1710000000","status":"vip","category":"207","imdb":"tt15239678"},
 {"id":"102","name":"Dune 2021 720p","info_hash":"1111222233334444555566667777888899990000","leechers":"1","seeders":"20","num_files":"1","size":"1073741824","username":"other","added":"1700000000","status":"member","category":"201","imdb":""}
]`))
	}))
	defer srv.Close()
	// the definition hard-codes https://{{ .Config.apiurl }}/: route example.com to the test server
	tr := srv.Client().Transport.(*http.Transport).Clone()
	addr := srv.Listener.Addr().String()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Transport: tr, Jar: jar}
	r := &Runner{Def: loadDef(t, "thepiratebay.yml"), Settings: map[string]string{"apiurl": "example.com"}, HTTP: client}
	res, err := r.Search(context.Background(), Query{Keywords: "Dune", Categories: []int{2000}})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("q") != "dune" || !strings.Contains(gotQuery.Get("cat"), "207") {
		t.Fatalf("query = %v", gotQuery)
	}
	if len(res) != 2 {
		t.Fatalf("results = %+v", res)
	}
	a := res[0]
	if a.Title != "Dune Part Two 2024 1080p WEBRip x265" || a.Size != 2147483648 || a.Seeders != 345 || a.Leechers != 12 {
		t.Fatalf("first = %+v", a)
	}
	if a.InfoHash != "aaaabbbbccccddddeeeeffff0000111122223333" || !strings.HasPrefix(a.Magnet, "magnet:?xt=urn:btih:aaaabbbb") {
		t.Fatalf("hash/magnet = %q %q", a.InfoHash, a.Magnet)
	}
	if a.Details != "https://thepiratebay.org/description.php?id=101" {
		t.Fatalf("details = %q", a.Details)
	}
	if !a.PublishDate.Equal(time.Unix(1710000000, 0)) {
		t.Fatalf("date = %v", a.PublishDate)
	}
	if len(a.Category) == 0 || a.Category[0] != 2040 {
		t.Fatalf("category = %v", a.Category)
	}
}

func TestDownloadTorrentAndMagnet(t *testing.T) {
	torrent := []byte("d8:announce14:http://t/annou4:infod4:name1:xee")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/1":
			w.Header().Set("Content-Type", "application/x-bittorrent")
			w.Write(torrent)
		case "/to-magnet":
			http.Redirect(w, r, "magnet:?xt=urn:btih:abc&dn=x", http.StatusFound)
		default:
			w.Write([]byte("<html>not a torrent</html>"))
		}
	}))
	defer srv.Close()
	r := &Runner{Def: loadDef(t, "rutor.yml"), Settings: map[string]string{"sitelink": srv.URL + "/"}, HTTP: newClient(t)}
	ctx := context.Background()
	got, err := r.Download(ctx, srv.URL+"/download/1")
	if err != nil || string(got.Torrent) != string(torrent) {
		t.Fatalf("torrent download = %+v, %v", got, err)
	}
	got, err = r.Download(ctx, "magnet:?xt=urn:btih:abc")
	if err != nil || got.Magnet != "magnet:?xt=urn:btih:abc" {
		t.Fatalf("magnet passthrough = %+v, %v", got, err)
	}
	got, err = r.Download(ctx, srv.URL+"/to-magnet")
	if err != nil || got.Magnet != "magnet:?xt=urn:btih:abc&dn=x" {
		t.Fatalf("redirect to magnet = %+v, %v", got, err)
	}
	if _, err := r.Download(ctx, srv.URL+"/html"); err == nil {
		t.Fatal("html instead of torrent must be an error")
	}
}

const xmlDef = `
id: xmlsite
name: XML Site
type: public
links: ["http://placeholder/"]
caps:
  categorymappings:
    - {id: "1", cat: TV, desc: "TV"}
  modes: {search: [q]}
settings: []
search:
  paths:
    - path: rss.php
      response: {type: xml}
  inputs:
    q: "{{ .Keywords }}"
  rows:
    selector: rss > channel > item
  fields:
    title: {selector: title}
    details: {selector: link}
    download: {selector: enclosure, attribute: url}
    size: {selector: enclosure, attribute: length}
    date: {selector: pubDate}
    category: {text: "1"}
    seeders: {selector: seeders, optional: true, default: "0"}
`

func TestSearchXML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>feed</title>
<item><title>Show.S01E01.1080p</title><link>http://site/details/1</link><pubDate>Tue, 05 Mar 2024 10:00:00 +0000</pubDate>
<enclosure url="http://site/dl/1.torrent" length="1073741824" type="application/x-bittorrent"/><seeders>7</seeders></item>
<item><title><![CDATA[Show.S01E02 & more]]></title><link>http://site/details/2</link><pubDate>Wed, 06 Mar 2024 10:00:00 +0000</pubDate>
<enclosure url="http://site/dl/2.torrent" length="2048" type="application/x-bittorrent"/></item>
</channel></rss>`))
	}))
	defer srv.Close()
	r := &Runner{Def: mustParse(t, xmlDef), Settings: map[string]string{"sitelink": srv.URL + "/"}, HTTP: newClient(t)}
	res, err := r.Search(context.Background(), Query{Keywords: "show"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %+v", res)
	}
	if res[0].Title != "Show.S01E01.1080p" || res[0].Details != "http://site/details/1" || res[0].Link != "http://site/dl/1.torrent" ||
		res[0].Size != 1073741824 || res[0].Seeders != 7 || res[0].Category[0] != 5000 {
		t.Fatalf("first = %+v", res[0])
	}
	if res[1].Title != "Show.S01E02 & more" || res[1].Seeders != 0 || res[1].PublishDate.Day() != 6 {
		t.Fatalf("second = %+v", res[1])
	}
}

func TestRowsAfterMergesFollowingRows(t *testing.T) {
	def := mustParse(t, `
id: aftersite
name: After
type: public
links: ["http://placeholder/"]
caps: {categorymappings: [{id: "1", cat: Movies}], modes: {search: [q]}}
settings: []
search:
  paths: [{path: "list"}]
  rows:
    selector: tr.r
    after: 1
  fields:
    title: {selector: a.t}
    download: {selector: a.d, attribute: href}
    seeders: {selector: td.s}
    category: {text: "1"}
`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<table>
<tr class="r"><td><a class="t">One</a></td></tr><tr class="r"><td class="s">11</td><td><a class="d" href="/d/1">d</a></td></tr>
<tr class="r"><td><a class="t">Two</a></td></tr><tr class="r"><td class="s">22</td><td><a class="d" href="/d/2">d</a></td></tr>
</table>`))
	}))
	defer srv.Close()
	r := &Runner{Def: def, Settings: map[string]string{"sitelink": srv.URL + "/"}, HTTP: newClient(t)}
	res, err := r.Search(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Title != "One" || res[0].Seeders != 11 || res[1].Title != "Two" || res[1].Link != srv.URL+"/d/2" {
		t.Fatalf("results = %+v", res)
	}
}

// Kinozal strips Cyrillic letters by default (a Sonarr helper); a search
// may turn that off for itself (Query.Config) without touching the source.
func TestSearchConfigOverride(t *testing.T) {
	page, err := charmap.Windows1251.NewEncoder().Bytes(readFixture(t, "kinozal_search.html"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/takelogin.php", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "uid", Value: "77", Path: "/"})
		http.Redirect(w, r, "/", http.StatusFound)
	})
	mux.HandleFunc("/my.php", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<a href="/logout.php?hash4u=1">x</a>`))
	})
	mux.HandleFunc("/browse.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		w.Write(page)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("home")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	r := &Runner{
		Def:      loadDef(t, "kinozal.yml"),
		Settings: map[string]string{"sitelink": srv.URL + "/", "username": "u", "password": "pw"}, // defaults: strip, add RUSSIAN
		HTTP:     newClient(t),
	}
	res, err := r.Search(context.Background(), Query{Keywords: "Мастер"})
	if err != nil || len(res) != 3 {
		t.Fatalf("%v %d", err, len(res))
	}
	if strings.Contains(res[1].Title, "Мастер") || !strings.HasSuffix(res[1].Title, "RUSSIAN") {
		t.Fatalf("defaults not applied: %q", res[1].Title)
	}
	res, err = r.Search(context.Background(), Query{Keywords: "Мастер", Config: map[string]string{"stripcyrillic": "", "addrussian": ""}})
	if err != nil || len(res) != 3 {
		t.Fatalf("%v %d", err, len(res))
	}
	if !strings.Contains(res[1].Title, "Мастер и Маргарита") || strings.Contains(res[1].Title, "RUSSIAN") {
		t.Fatalf("override not applied: %q", res[1].Title)
	}
	if r.Settings["stripcyrillic"] != "" {
		t.Fatal("the source's settings changed")
	}
}

// A tracker with hundreds of forums for one kind (RuTracker: 420 for music)
// answers nothing to a filter that long: the search goes without it and
// keeps only the results of the wanted categories.
func TestSearchLongCategoryFilterDropped(t *testing.T) {
	var maps strings.Builder
	for i := range 250 { // RuTracker: 420 music forums
		fmt.Fprintf(&maps, "    - {id: \"%d\", cat: Audio/Lossless, desc: \"forum %d\"}\n", 100+i, i)
	}
	maps.WriteString("    - {id: \"9\", cat: Movies, desc: \"films\"}\n")
	def := mustParse(t, strings.Replace(`
id: bigforums
name: Big Forums
type: public
links: ["http://placeholder/"]
caps:
  categorymappings:
MAPS  modes: {search: [q]}
settings: []
search:
  paths:
    - path: rss.php
      response: {type: xml}
  inputs:
    f: "{{ join .Categories \",\" }}"
    q: "{{ .Keywords }}"
  rows:
    selector: rss > channel > item
  fields:
    title: {selector: title}
    download: {selector: enclosure, attribute: url}
    category: {selector: cat}
`, "MAPS", maps.String(), 1))
	var gotF []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotF = append(gotF, r.URL.Query().Get("f"))
		w.Header().Set("Content-Type", "application/rss+xml")
		w.Write([]byte(`<?xml version="1.0"?><rss><channel>
<item><title>Album FLAC</title><enclosure url="http://site/1.torrent"/><cat>100</cat></item>
<item><title>Concert film</title><enclosure url="http://site/2.torrent"/><cat>9</cat></item>
</channel></rss>`))
	}))
	defer srv.Close()
	r := &Runner{Def: def, Settings: map[string]string{"sitelink": srv.URL + "/"}, HTTP: newClient(t)}
	res, err := r.Search(context.Background(), Query{Keywords: "album", Categories: []int{3000}})
	if err != nil {
		t.Fatal(err)
	}
	if len(gotF) != 1 || gotF[0] != "" {
		t.Fatalf("category filter sent = %q, want none", gotF)
	}
	if len(res) != 1 || res[0].Title != "Album FLAC" {
		t.Fatalf("results = %+v, want the album only", res)
	}
	// a short filter is still sent as is
	gotF = nil
	if _, err := r.Search(context.Background(), Query{Keywords: "film", Categories: []int{2000}}); err != nil {
		t.Fatal(err)
	}
	if len(gotF) != 1 || gotF[0] != "9" {
		t.Fatalf("short filter = %q, want 9", gotF)
	}
}

func TestCookieName(t *testing.T) {
	for in, want := range map[string]string{"❯   bb_guid": "bb_guid", " sid ": "sid", "«x»": "x", "❯": ""} {
		if got := CookieName(in); got != want {
			t.Errorf("CookieName(%q) = %q, want %q", in, got, want)
		}
	}
}

// MegaPeer answers a search that finds nothing with a 404 page: that is no
// results, not a broken site. A keywordless search (the source check, RSS)
// still fails on a 404 — there a 404 means the search page is gone.
func TestSearch404IsEmptyForKeywords(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("<html><body><p>Ничего не найдено</p></body></html>"))
	}))
	defer srv.Close()
	r := &Runner{Def: loadDef(t, "rutor.yml"), Settings: map[string]string{"sitelink": srv.URL + "/"}, HTTP: newClient(t)}
	res, err := r.Search(context.Background(), Query{Keywords: "Hardkandy Second to None"})
	if err != nil || len(res) != 0 {
		t.Fatalf("keyword search on 404: %v, %d results; want none, no error", err, len(res))
	}
	if _, err := r.Search(context.Background(), Query{}); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("keywordless search on 404: err = %v, want ErrBadResponse", err)
	}
}
