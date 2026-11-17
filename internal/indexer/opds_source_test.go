package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeOPDS is a small catalog: root → OpenSearch → results with an fb2,
// an epub and a mobi link; /b/1/epub downloads with a Content-Disposition
// name. Basic auth reader:secret when auth is set.
func fakeOPDS(t *testing.T, auth bool, hits ...*atomic.Int32) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			for _, n := range hits {
				n.Add(1)
			}
			if u, p, ok := r.BasicAuth(); auth && (!ok || u != "reader" || p != "secret") {
				w.WriteHeader(401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/opds", guard(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>Каталог</title>
<link rel="search" type="application/opensearchdescription+xml" href="/opds/opensearch.xml"/></feed>`)
	}))
	mux.HandleFunc("/opds/opensearch.xml", guard(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
<Url type="application/atom+xml" template="/opds/search?searchTerm={searchTerms}"/></OpenSearchDescription>`)
	}))
	mux.HandleFunc("/opds/search", guard(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom" xmlns:dc="http://purl.org/dc/terms/"><title>Поиск</title>
<entry><title>Мастер и Маргарита</title><id>tag:1</id><author><name>Михаил Булгаков</name></author><dc:language>ru</dc:language>
<link rel="alternate" type="text/html" href="/b/1"/>
<link rel="http://opds-spec.org/acquisition/open-access" type="application/fb2+zip" href="/b/1/fb2"/>
<link rel="http://opds-spec.org/acquisition/open-access" type="application/epub+zip" href="/b/1/epub"/>
<link rel="http://opds-spec.org/acquisition/open-access" type="application/x-mobipocket-ebook" href="/b/1/mobi"/>
</entry></feed>`)
	}))
	mux.HandleFunc("/b/1/epub", guard(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/epub+zip")
		w.Header().Set("Content-Disposition", `attachment; filename="Bulgakov_Master.epub"`)
		fmt.Fprint(w, "EPUBDATA")
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (e *env) addOPDS(url string, extra map[string]string) Source {
	e.t.Helper()
	if e.m == nil {
		e.reload()
	}
	settings := map[string]string{"url": url, "allow_local": "true"}
	for k, v := range extra {
		settings[k] = v
	}
	s, err := e.m.Add(context.Background(), OPDSDefinitionID, settings)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func TestOPDSCatalogRequiredURL(t *testing.T) {
	e := newEnv(t)
	e.reload()
	entry, ok := e.cat.Get("opds")
	if !ok || entry.Name != "OPDS-каталог" || entry.Language != "ru-RU" || entry.Type != "public" {
		t.Fatalf("entry %+v", entry)
	}
	if entry.Settings[0].Name != "url" || !entry.Settings[0].Required || entry.Settings[0].Label != "Адрес каталога" {
		t.Fatalf("url setting %+v", entry.Settings[0])
	}
	found := false
	for _, c := range e.cat.List("opds") {
		found = found || c.ID == "opds"
	}
	if !found {
		t.Fatal("opds is not in the catalog list")
	}
	_, err := e.m.Add(context.Background(), "opds", map[string]string{"username": "x"})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "url" || ve.Error() != "Заполните поле «Адрес каталога»" {
		t.Fatalf("no url: %v", err)
	}
}

func TestOPDSSourceTest(t *testing.T) {
	e := newEnv(t)
	srv := fakeOPDS(t, true)
	good := e.addOPDS(srv.URL+"/opds", map[string]string{"username": "reader", "password": "secret"})
	if good.Status != StatusOK {
		t.Fatalf("good: %s %q", good.Status, good.LastError)
	}
	if good.Redacted().Settings["password"] != RedactedValue {
		t.Fatal("password not redacted")
	}
	badAuth := e.addOPDS(srv.URL+"/opds", map[string]string{"username": "reader", "password": "nope"})
	if badAuth.Status != StatusError || !strings.Contains(badAuth.LastError, "логин и пароль") {
		t.Fatalf("bad auth: %s %q", badAuth.Status, badAuth.LastError)
	}
	html := httptest.NewServer(serve("<html>not a catalog</html>"))
	defer html.Close()
	bad := e.addOPDS(html.URL+"/", nil)
	if bad.Status != StatusError || !strings.Contains(bad.LastError, "нет OPDS-каталога") {
		t.Fatalf("html: %s %q", bad.Status, bad.LastError)
	}
	wrong := e.addOPDS("ftp://example.org/opds", nil)
	if wrong.Status != StatusError || !strings.Contains(wrong.LastError, "Неверный адрес") {
		t.Fatalf("ftp: %s %q", wrong.Status, wrong.LastError)
	}
	// without the admin's permission the home network is refused (SSRF)
	s, err := e.m.Add(context.Background(), OPDSDefinitionID, map[string]string{"url": srv.URL + "/opds", "username": "reader", "password": "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusError || !strings.Contains(s.LastError, "домашнюю сеть") {
		t.Fatalf("loopback: %s %q", s.Status, s.LastError)
	}
}

func TestOPDSSearchAndFetch(t *testing.T) {
	e := newEnv(t)
	srv := fakeOPDS(t, true)
	src := e.addOPDS(srv.URL+"/opds", map[string]string{"username": "reader", "password": "secret"})
	resp, err := e.m.Search(context.Background(), SearchRequest{Kind: "book", Text: "Михаил Булгаков Мастер и Маргарита", AltTitles: []string{"Мастер и Маргарита"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Releases) != 1 || len(resp.Failed) != 0 {
		t.Fatalf("resp %+v", resp)
	}
	r := resp.Releases[0]
	if !r.Direct || r.Title != "Михаил Булгаков — Мастер и Маргарита" || r.Link != srv.URL+"/b/1/epub" || r.Details != srv.URL+"/b/1" || r.SourceID != src.ID {
		t.Fatalf("release %+v", r)
	}
	body, info, err := e.m.Fetch(context.Background(), src.ID, r.Link)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	body.Close()
	if string(data) != "EPUBDATA" || info.Name != "Bulgakov_Master.epub" || info.ContentType != "application/epub+zip" || info.Size != 8 {
		t.Fatalf("fetch %q %+v", data, info)
	}
}

func TestFetchRejectsForeignHost(t *testing.T) {
	e := newEnv(t)
	srv := fakeOPDS(t, false)
	src := e.addOPDS(srv.URL+"/opds", nil)
	other := httptest.NewServer(serve("x"))
	defer other.Close()
	for _, link := range []string{
		"http://evil.example.org/b/1/epub",
		strings.Replace(other.URL, "127.0.0.1", "localhost", 1) + "/b/1/epub",
		"file:///etc/passwd",
	} {
		if _, _, err := e.m.Fetch(context.Background(), src.ID, link); err == nil || !strings.Contains(err.Error(), "Ссылка ведёт на другой сайт") {
			t.Errorf("%s: %v", link, err)
		}
	}
	// a torrent source has no direct downloads
	e.def("tr", "Трекер", serve(page()))
	tr := e.add("tr")
	if _, _, err := e.m.Fetch(context.Background(), tr.ID, "http://x/1"); err == nil {
		t.Fatal("fetch from a torrent source")
	}
}

// A direct source is no business of the release-link check: Download refuses
// it and Fetch holds its links to the catalog host, which may be the mirror
// its url redirects to.
func TestCheckReleaseLinkOPDSMirror(t *testing.T) {
	e := newEnv(t)
	srv := fakeOPDS(t, false)
	src := e.addOPDS(srv.URL+"/opds", nil)
	rememberCatalogHost(src, "mirror.example")
	t.Cleanup(func() { catalogHosts.Delete(catalogKey(src)) })
	if err := e.m.CheckReleaseLink(context.Background(), src.ID, "http://mirror.example/b/1/epub"); err != nil {
		t.Fatalf("catalog host: %v", err)
	}
	// and Fetch still refuses a foreign host
	if _, _, err := e.m.Fetch(context.Background(), src.ID, "http://evil.example.org/b/1/epub"); err == nil {
		t.Fatal("fetch of a foreign host")
	}
}

func TestFetchSizeLimit(t *testing.T) {
	e := newEnv(t)
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/opds" {
			fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>x</title></feed>`)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(int64(maxDirectBytes)+1))
		w.WriteHeader(200)
	}))
	defer big.Close()
	src := e.addOPDS(big.URL+"/opds", nil)
	if _, _, err := e.m.Fetch(context.Background(), src.ID, big.URL+"/b/1"); err == nil || !strings.Contains(err.Error(), "500 МБ") {
		t.Fatalf("big file: %v", err)
	}
}

// TestOPDSMirrorSearchAndFetch: a source whose url redirects to a mirror on
// another host finds releases there, and Fetch accepts the mirror's links
// (also with nothing remembered, after a restart).
func TestOPDSMirrorSearchAndFetch(t *testing.T) {
	e := newEnv(t)
	mirror := fakeOPDS(t, false)
	mirrorURL := strings.Replace(mirror.URL, "127.0.0.1", "localhost", 1)
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, mirrorURL+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	defer old.Close()
	src := e.addOPDS(old.URL+"/opds", map[string]string{"username": "reader", "password": "secret"})
	if src.Status != StatusOK {
		t.Fatalf("source %s %q", src.Status, src.LastError)
	}
	resp, err := e.m.Search(context.Background(), SearchRequest{Kind: "book", Text: "Михаил Булгаков Мастер и Маргарита", AltTitles: []string{"Мастер и Маргарита"}})
	if err != nil || len(resp.Releases) != 1 || !strings.HasPrefix(resp.Releases[0].Link, mirrorURL) {
		t.Fatalf("%v %+v", err, resp)
	}
	catalogHosts.Delete(catalogKey(src)) // as after a restart
	body, _, err := e.m.Fetch(context.Background(), src.ID, resp.Releases[0].Link)
	if err != nil {
		t.Fatal(err)
	}
	body.Close()
}
