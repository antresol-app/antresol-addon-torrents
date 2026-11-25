package cardigann

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const directDef = `
id: booksite
name: Book Site
type: public
links: ["http://placeholder/"]
caps: {categorymappings: [{id: "1", cat: Books}], modes: {search: [q]}}
settings: []
search:
  paths: [{path: "search"}]
  rows: {selector: li}
  fields:
    title: {selector: a}
    details: {selector: a, attribute: href}
    download: {selector: a, attribute: href}
    category: {text: "1"}
download:
  direct: true
  selectors:
    - selector: form:has(input[name=filename][value$=".epub"])
      submit: true
      follow:
        - selector: meta[http-equiv="Refresh"]
          attribute: content
          filters: [{name: regexp, args: "url=(.+)"}]
    - selector: form:has(input[name=filename][value$=".pdf"])
      submit: true
      follow:
        - selector: meta[http-equiv="Refresh"]
          attribute: content
          filters: [{name: regexp, args: "url=(.+)"}]
`

// bookSite serves a details page with a form per format; the form posts to
// /fetch, which answers a page refreshing to the file. formats lists the
// forms on /book/1; /files/*.html is a web page, not a file.
type bookSite struct {
	formats []string
	mu      sync.Mutex
	posted  []string // "id=…&filename=…" of each /fetch
}

func (b *bookSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/book/1":
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html><body>")
		for _, f := range b.formats {
			io.WriteString(w, `<form action="/fetch" method="post"><input name="id" type="hidden" value="srv3">`+
				`<input name="filename" type="hidden" value="Book.`+f+`"><input type="image" name="go" src="x.jpg"></form>`)
		}
		io.WriteString(w, "</body></html>")
	case r.URL.Path == "/fetch" && r.Method == http.MethodPost:
		r.ParseForm()
		b.mu.Lock()
		b.posted = append(b.posted, r.PostForm.Encode())
		b.mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, `<html><head></head><body>wait<meta http-equiv="Refresh" content="5;url=/files/`+r.PostForm.Get("filename")+`?k=1" /></body></html>`)
	case strings.HasPrefix(r.URL.Path, "/files/") && strings.HasSuffix(r.URL.Path, ".html"):
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>ads</html>")
	case strings.HasPrefix(r.URL.Path, "/files/"):
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="/Site/`+strings.TrimPrefix(r.URL.Path, "/files/")+`"`)
		io.WriteString(w, "BOOK:"+r.URL.Path)
	default:
		http.NotFound(w, r)
	}
}

func openDirect(t *testing.T, site http.Handler, def string) (string, error) {
	t.Helper()
	srv := httptest.NewServer(site)
	t.Cleanup(srv.Close)
	r := &Runner{Def: mustParse(t, def), Settings: map[string]string{"sitelink": srv.URL + "/"}, HTTP: newClient(t)}
	resp, err := r.OpenDirect(context.Background(), srv.URL+"/book/1")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func TestOpenDirectSubmitsFormAndFollowsRefresh(t *testing.T) {
	site := &bookSite{formats: []string{"pdf", "epub"}}
	got, err := openDirect(t, site, directDef)
	if err != nil {
		t.Fatal(err)
	}
	if got != "BOOK:/files/Book.epub" {
		t.Fatalf("file = %q, want the epub (first selector)", got)
	}
	if len(site.posted) != 1 || site.posted[0] != "filename=Book.epub&id=srv3" {
		t.Fatalf("posted = %v (hidden fields only, no image button)", site.posted)
	}
}

func TestOpenDirectFallsBackToNextSelector(t *testing.T) {
	got, err := openDirect(t, &bookSite{formats: []string{"pdf"}}, directDef)
	if err != nil || got != "BOOK:/files/Book.pdf" {
		t.Fatalf("file = %q, %v; want the pdf", got, err)
	}
}

func TestOpenDirectRefusesWebPage(t *testing.T) {
	_, err := openDirect(t, &bookSite{formats: []string{"html"}}, strings.ReplaceAll(directDef, `$=".epub"`, `$=".html"`))
	if !errors.Is(err, ErrNotFile) {
		t.Fatalf("err = %v, want ErrNotFile", err)
	}
}

func TestDownloadRefusesDirectDefinition(t *testing.T) {
	r := &Runner{Def: mustParse(t, directDef), HTTP: newClient(t)}
	if _, err := r.Download(context.Background(), "http://placeholder/book/1"); !errors.Is(err, ErrNotTorrent) {
		t.Fatalf("err = %v, want ErrNotTorrent", err)
	}
	torrentDef := strings.Replace(directDef, "direct: true", "direct: false", 1)
	r = &Runner{Def: mustParse(t, torrentDef), HTTP: newClient(t)}
	if _, err := r.OpenDirect(context.Background(), "http://placeholder/book/1"); err == nil {
		t.Fatal("OpenDirect of a torrent definition must fail")
	}
}

// Submit and follow work for torrents too: a GET form leading to a page
// that links the .torrent.
func TestDownloadSubmitGetFormThenFollow(t *testing.T) {
	torrent := []byte("d8:announce14:http://t/annou4:infod4:name1:xee")
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/details/7":
			io.WriteString(w, `<form id="get" action="/confirm?drop=1#x"><input name="id" value="7">`+
				`<input type="checkbox" name="agree" checked><input type="checkbox" name="spam" value="1">`+
				`<select name="fmt"><option value="a">A</option><option value="b" selected>B</option></select>`+
				`<textarea name="note">hi</textarea><input name="off" value="x" disabled><button name="b">go</button></form>`)
		case "/confirm":
			query = r.URL.RawQuery
			io.WriteString(w, `<a class="dl" href="/t/7.torrent">torrent</a>`)
		case "/t/7.torrent":
			w.Write(torrent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	def := mustParse(t, `
id: formsite
name: Form Site
links: ["http://placeholder/"]
caps: {categorymappings: [{id: "1", cat: Movies}], modes: {search: [q]}}
settings: []
search:
  paths: [{path: "s"}]
  rows: {selector: tr}
  fields: {title: {selector: td}, category: {text: "1"}}
download:
  selectors:
    - selector: form#get
      submit: true
      follow:
        - {selector: a.dl, attribute: href}
`)
	r := &Runner{Def: def, Settings: map[string]string{"sitelink": srv.URL + "/"}, HTTP: newClient(t)}
	got, err := r.Download(context.Background(), srv.URL+"/details/7")
	if err != nil || string(got.Torrent) != string(torrent) {
		t.Fatalf("download = %+v, %v", got, err)
	}
	if query != "id=7&agree=on&fmt=b&note=hi" {
		t.Fatalf("form query = %q", query)
	}
}

// slowDef: the details page links the file directly.
const slowDef = `
id: slowsite
name: Slow Site
links: ["http://placeholder/"]
caps: {categorymappings: [{id: "1", cat: Books}], modes: {search: [q]}}
settings: []
search:
  paths: [{path: "s"}]
  rows: {selector: li}
  fields: {title: {selector: a}, category: {text: "1"}}
download:
  direct: true
  headers:
    X-Api-Key: ["k1"]
  selectors:
    - selector: a.file
      attribute: href
`

// slowSite serves /book/1 linking /f.epub, whose body comes in parts
// pause apart; it records the X-Api-Key of the file request.
func slowSite(t *testing.T, parts int, pause time.Duration, key *atomic.Value) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/book/1":
			io.WriteString(w, `<a class="file" href="/f.epub">f</a>`)
		case "/f.epub":
			key.Store(r.Header.Get("X-Api-Key"))
			w.Header().Set("Content-Type", "application/epub+zip")
			for i := 0; i < parts; i++ {
				io.WriteString(w, "part;")
				w.(http.Flusher).Flush()
				select {
				case <-time.After(pause):
				case <-r.Context().Done():
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The body of a direct download may take longer than the client's
// Timeout (90 s in Vault): only the caller's context and a stalled body
// end it. The definition's headers reach the file request.
func TestOpenDirectOutlivesClientTimeout(t *testing.T) {
	var key atomic.Value
	srv := slowSite(t, 6, 100*time.Millisecond, &key)
	client := newClient(t)
	client.Timeout = 250 * time.Millisecond
	r := &Runner{Def: mustParse(t, slowDef), Settings: map[string]string{"sitelink": srv.URL + "/"}, HTTP: client}
	resp, err := r.OpenDirect(context.Background(), srv.URL+"/book/1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || string(b) != strings.Repeat("part;", 6) {
		t.Fatalf("body %q, %v", b, err)
	}
	if key.Load() != "k1" {
		t.Fatalf("X-Api-Key on the file request = %v", key.Load())
	}
}

func TestOpenDirectStalledBodyFails(t *testing.T) {
	old := DirectIdleTimeout
	DirectIdleTimeout = 150 * time.Millisecond
	t.Cleanup(func() { DirectIdleTimeout = old })
	var key atomic.Value
	srv := slowSite(t, 3, 2*time.Second, &key)
	r := &Runner{Def: mustParse(t, slowDef), Settings: map[string]string{"sitelink": srv.URL + "/"}, HTTP: newClient(t)}
	resp, err := r.OpenDirect(context.Background(), srv.URL+"/book/1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	start := time.Now()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("a stalled body must fail")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("stalled body ended after %v", d)
	}
}

// Strict runners do not follow a redirect from https to http. A request that
// carries the definition's headers (an API key) never does, strict or not
// (TestRedirectWithSecretsNeverDowngrades): this site sends none.
func TestStrictRefusesDowngrade(t *testing.T) {
	noKey := strings.Replace(slowDef, "  headers:\n    X-Api-Key: [\"k1\"]\n", "", 1)
	if noKey == slowDef {
		t.Fatal("the definition keeps its header")
	}
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/epub+zip")
		io.WriteString(w, "BOOK")
	}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/book/1":
			io.WriteString(w, `<a class="file" href="/go">f</a>`)
		case "/go":
			http.Redirect(w, r, plain.URL+"/f.epub", http.StatusFound)
		}
	}))
	defer tls.Close()
	for _, strict := range []bool{false, true} {
		r := &Runner{Def: mustParse(t, noKey), Settings: map[string]string{"sitelink": tls.URL + "/"}, HTTP: tls.Client(), Strict: strict}
		resp, err := r.OpenDirect(context.Background(), tls.URL+"/book/1")
		if resp != nil {
			resp.Body.Close()
		}
		if (err != nil) != strict {
			t.Errorf("strict=%v: err = %v", strict, err)
		}
	}
}
