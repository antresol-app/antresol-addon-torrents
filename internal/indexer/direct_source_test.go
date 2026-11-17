package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// directSiteDef is a direct-download definition (download.direct): search
// rows link details pages, a details page links the file.
func directSiteDef(link string) string {
	return fmt.Sprintf(`---
id: booksite
name: Книжный сайт
language: en-US
type: public
links: [%q]
caps:
  categorymappings:
    - {id: 1, cat: Books, desc: Books}
  modes: {search: [q], book-search: [q]}
settings: []
search:
  paths: [{path: search}]
  inputs: {q: "{{ .Keywords }}"}
  rows: {selector: li}
  fields:
    title: {selector: a}
    details: {selector: a, attribute: href}
    download: {selector: a, attribute: href}
    category: {text: "1"}
download:
  direct: true
  selectors:
    - {selector: a.file, attribute: href}
`, link+"/")
}

func newDirectSite(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			hits.Add(1)
			io.WriteString(w, `<ul><li><a href="/book/1">Mikhail Bulgakov — The Master and Margarita</a></li></ul>`)
		case "/book/1":
			io.WriteString(w, `<a class="file" href="/f/master.epub">EPUB</a>`)
		case "/f/master.epub":
			w.Header().Set("Content-Type", "application/epub+zip")
			io.WriteString(w, "EPUBDATA")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDirectDefinitionSearchAndFetch(t *testing.T) {
	e := newEnv(t)
	var hits atomic.Int32
	srv := newDirectSite(t, &hits)
	if err := os.WriteFile(filepath.Join(e.dir, "definitions", "booksite.yml"), []byte(directSiteDef(srv.URL)), 0o644); err != nil {
		t.Fatal(err)
	}
	e.reload()
	// the test site is on 127.0.0.1: the home network must be allowed
	src, err := e.m.Add(context.Background(), "booksite", map[string]string{"allow_local": "true"})
	if err != nil || src.Status != StatusOK {
		t.Fatalf("Add: %+v %v", src, err)
	}
	hits.Store(0) // adding tests the source
	ctx := context.Background()

	for _, kind := range []string{"movie", "music", "audiobook"} {
		if _, err := e.m.Search(ctx, SearchRequest{Kind: kind, Text: "The Master and Margarita"}); err != nil {
			t.Fatal(err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("a direct-download source was searched for other kinds (%d)", hits.Load())
	}
	resp, err := e.m.Search(ctx, SearchRequest{Kind: "book", Text: "The Master and Margarita"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Releases) != 1 || !resp.Releases[0].Direct || resp.Releases[0].Link != srv.URL+"/book/1" {
		t.Fatalf("resp %+v", resp)
	}
	body, info, err := e.m.Fetch(ctx, src.ID, resp.Releases[0].Link)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "EPUBDATA" || info.Name != "master.epub" || info.ContentType != "application/epub+zip" {
		t.Fatalf("fetch %q %+v", data, info)
	}
	if _, err := e.m.Download(ctx, src.ID, resp.Releases[0].Link); err == nil {
		t.Fatal("a direct-download source gave a torrent")
	}
	for _, link := range []string{"http://evil.example.org/book/1", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/book/1", "file:///etc/passwd"} {
		if _, _, err := e.m.Fetch(ctx, src.ID, link); err == nil || !strings.Contains(err.Error(), "Ссылка ведёт на другой сайт") {
			t.Errorf("%s: %v", link, err)
		}
	}
}

// A direct-download source must not reach the home network unless the
// admin allows it: by default every hop (the page, a follow step, a form
// action, a redirect, the file) goes through the guarded client.
func TestDirectSourceRefusesHomeNetwork(t *testing.T) {
	e := newEnv(t)
	var hits atomic.Int32
	srv := newDirectSite(t, &hits)
	if err := os.WriteFile(filepath.Join(e.dir, "definitions", "booksite.yml"), []byte(directSiteDef(srv.URL)), 0o644); err != nil {
		t.Fatal(err)
	}
	e.reload()
	src := e.add("booksite") // no allow_local: 127.0.0.1 is the home network
	if src.Status != StatusError || !strings.Contains(src.LastError, "домашнюю сеть") {
		t.Fatalf("source %+v", src)
	}
	if _, _, err := e.m.Fetch(context.Background(), src.ID, srv.URL+"/book/1"); err == nil || !strings.Contains(err.Error(), "домашнюю сеть") {
		t.Fatalf("fetch: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the site was reached %d times", hits.Load())
	}
}

const stepsDef = `---
id: stepsite
name: Шаги
type: public
links: [%q]
caps: {categorymappings: [{id: 1, cat: Books}], modes: {search: [q]}}
settings: []
search:
  paths: [{path: search}]
  rows: {selector: li}
  fields: {title: {selector: a}, download: {selector: a, attribute: href}, category: {text: "1"}}
download:
  direct: true
  selectors:
    - {selector: a.file, attribute: href}
    - {selector: "form#f", submit: true}
`

// TestDirectStepsCannotLeaveForHomeNetwork: the site is allowed, a host
// standing for the home network is not (the guard works on the dialled
// address). A follow link, a form action and a redirect to it are refused
// before a connection is made.
func TestDirectStepsCannotLeaveForHomeNetwork(t *testing.T) {
	var internal atomic.Int32
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internal.Add(1)
		w.Header().Set("Content-Type", "application/epub+zip")
		io.WriteString(w, "SECRET")
	}))
	defer home.Close()
	homeAddr := strings.TrimPrefix(home.URL, "http://")
	var page atomic.Value
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/book/1":
			io.WriteString(w, page.Load().(string))
		case "/redir":
			http.Redirect(w, r, home.URL+"/f.epub", http.StatusFound)
		default:
			io.WriteString(w, "<ul></ul>")
		}
	}))
	defer site.Close()
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.dir, "definitions", "stepsite.yml"), []byte(fmt.Sprintf(stepsDef, site.URL+"/")), 0o644); err != nil {
		t.Fatal(err)
	}
	e.reload()
	dialer := &net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
		if address == homeAddr {
			return errors.New("home network refused")
		}
		return nil
	}}
	e.m.directHTTP = func(Source) *http.Client {
		return &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true}}
	}
	src := e.add("stepsite")
	for name, body := range map[string]string{
		"follow link": `<a class="file" href="` + home.URL + `/f.epub">f</a>`,
		"form action": `<form id="f" method="post" action="` + home.URL + `/apply.cgi"><input name="dns1" value="6.6.6.6"></form>`,
		"redirect":    `<a class="file" href="/redir">f</a>`,
	} {
		page.Store(body)
		if _, _, err := e.m.Fetch(context.Background(), src.ID, site.URL+"/book/1"); err == nil {
			t.Errorf("%s: fetched from the home network", name)
		}
	}
	if internal.Load() != 0 {
		t.Fatalf("the home network was reached %d times", internal.Load())
	}
}
