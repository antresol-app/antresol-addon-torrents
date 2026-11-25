package opds

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/antresol-app/antresol/addonkit"
)

// flibusta is a fake Flibusta-style catalog: the root links an OpenSearch
// description whose template searches /opds/search?searchTerm=; results
// carry fb2, epub and mobi links (relative, as Flibusta sends them).
func flibusta(t *testing.T, user, pass string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	check := func(w http.ResponseWriter, r *http.Request) bool {
		if user == "" {
			return true
		}
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
			w.WriteHeader(401)
			return false
		}
		return true
	}
	mux.HandleFunc("/opds", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/atom+xml;charset=utf-8")
		fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:opds="http://opds-spec.org/2010/catalog">
<id>tag:root</id><title>Флибуста</title>
<link href="/opds/opensearch.xml" rel="search" type="application/opensearchdescription+xml"/>
<link href="/opds/search?searchTerm={searchTerms}" rel="search" type="application/atom+xml"/>
<entry><title>Новинки</title><link href="/opds/new" type="application/atom+xml;profile=opds-catalog"/></entry>
</feed>`)
	})
	mux.HandleFunc("/opds/opensearch.xml", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		fmt.Fprint(w, `<?xml version="1.0" encoding="utf-8"?>
<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">
<ShortName>Флибуста</ShortName>
<Url type="application/atom+xml" template="/opds/search?searchTerm={searchTerms}"/>
</OpenSearchDescription>`)
	})
	mux.HandleFunc("/opds/search", func(w http.ResponseWriter, r *http.Request) {
		if !check(w, r) {
			return
		}
		if r.URL.Query().Get("searchTerm") == "" {
			http.Error(w, "no term", 400)
			return
		}
		page := r.URL.Query().Get("pageNumber")
		next := ""
		title, id := "Мастер и Маргарита", "tag:book:1"
		if page == "" {
			next = `<link href="/opds/search?searchTerm=x&amp;pageNumber=1" rel="next" type="application/atom+xml;profile=opds-catalog"/>`
		} else {
			title, id = "Собачье сердце", "tag:book:2"
		}
		fmt.Fprintf(w, `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom" xmlns:dc="http://purl.org/dc/terms/">
<id>tag:search</id><title>Поиск</title>%s
<entry><title>%s</title><id>%s</id>
<author><name>Михаил Булгаков</name><uri>/a/1</uri></author>
<dc:language>ru</dc:language>
<link href="/b/1" rel="alternate" type="text/html"/>
<link href="/b/1/fb2" rel="http://opds-spec.org/acquisition/open-access" type="application/fb2+zip"/>
<link href="/b/1/epub" rel="http://opds-spec.org/acquisition/open-access" type="application/epub+zip" length="1234"/>
<link href="/b/1/mobi" rel="http://opds-spec.org/acquisition/open-access" type="application/x-mobipocket-ebook"/>
</entry>
</feed>`, next, title, id)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOPDSClientSearchFlibustaLike(t *testing.T) {
	srv := flibusta(t, "", "")
	c := &Client{URL: srv.URL + "/opds", HTTP: NewHTTPClient(true)}
	got, err := c.Search(context.Background(), "Булгаков Мастер")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("entries %+v", got)
	}
	e := got[0]
	if e.Title != "Мастер и Маргарита" || e.Author != "Михаил Булгаков" || e.ID != "tag:book:1" || e.Lang != "ru" {
		t.Fatalf("entry %+v", e)
	}
	if e.Details != srv.URL+"/b/1" {
		t.Fatalf("details %q", e.Details)
	}
	if len(e.Links) != 3 {
		t.Fatalf("links %+v", e.Links)
	}
	best, ok := Best(e.Links, BookFormats)
	if !ok || best.Href != srv.URL+"/b/1/epub" || best.Size != 1234 || best.Type != "application/epub+zip" {
		t.Fatalf("best %+v", best)
	}
	if got[1].Title != "Собачье сердце" {
		t.Fatalf("next page not followed: %+v", got[1])
	}
}

func TestOPDSClientBasicAuth(t *testing.T) {
	srv := flibusta(t, "reader", "secret")
	c := &Client{URL: srv.URL + "/opds", Username: "reader", Password: "secret", HTTP: NewHTTPClient(true)}
	if err := c.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Search(context.Background(), "x"); err != nil || len(got) != 2 {
		t.Fatalf("%v %+v", err, got)
	}
	c.Password = "wrong"
	err := c.Test(context.Background())
	var ue *Error
	if !errors.As(err, &ue) || !errors.Is(err, ErrAuth) {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestOPDSClientNotACatalog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>hello</body></html>")
	}))
	defer srv.Close()
	err := (&Client{URL: srv.URL, HTTP: NewHTTPClient(true)}).Test(context.Background())
	if !errors.Is(err, ErrNotCatalog) {
		t.Fatalf("html page: %v", err)
	}
	if err := (&Client{URL: "ftp://example.org/opds"}).Test(context.Background()); !errors.Is(err, ErrBadURL) {
		t.Fatalf("ftp: %v", err)
	}
}

// TestOPDSClientRefusesPrivate: without the admin's permission the client
// never connects to loopback, private or link-local addresses (SSRF).
func TestOPDSClientRefusesPrivate(t *testing.T) {
	srv := flibusta(t, "", "")
	err := (&Client{URL: srv.URL + "/opds", HTTP: NewHTTPClient(false)}).Test(context.Background())
	if !errors.Is(err, ErrPrivate) {
		t.Fatalf("loopback allowed: %v", err)
	}
	for _, a := range []string{"127.0.0.1", "10.1.2.3", "192.168.1.1", "172.16.0.1", "169.254.169.254", "100.64.0.1", "0.0.0.0", "::1", "fd00::1", "fe80::1", "::ffff:127.0.0.1",
		"::127.0.0.1", "64:ff9b::7f00:1", "64:ff9b::a00:1", "64:ff9b:1::c0a8:101", "2002:7f00:1::1", "2002:c0a8:101::1",
		"198.18.0.1", "192.0.0.8", "240.0.0.1", "255.255.255.255", "0.1.2.3", "fec0::1", "224.0.0.1", "ff02::1"} {
		if !addonkit.BlockedAddr(a) {
			t.Errorf("%s is not blocked", a)
		}
	}
	for _, a := range []string{"93.184.216.34", "2606:2800:220:1::1", "64:ff9b::5db8:d822", "2002:5db8:d822::1"} {
		if addonkit.BlockedAddr(a) {
			t.Errorf("%s is blocked", a)
		}
	}
}

// With the home network allowed the cloud metadata address stays out of
// reach (the dialer refuses it before connecting).
func TestOPDSClientAllowedHomeNetworkNeverReachesMetadata(t *testing.T) {
	err := (&Client{URL: "http://169.254.169.254/opds", HTTP: NewHTTPClient(true)}).Test(context.Background())
	if !errors.Is(err, ErrPrivate) {
		t.Fatalf("link-local with allowPrivate: %v", err)
	}
}

func TestOPDSClientResponseLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>`)
		fmt.Fprint(w, strings.Repeat("x", maxFeedBytes+10))
		fmt.Fprint(w, `</title></feed>`)
	}))
	defer srv.Close()
	if err := (&Client{URL: srv.URL, HTTP: NewHTTPClient(true)}).Test(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("huge feed: %v", err)
	}
}

func TestBestLinkOrder(t *testing.T) {
	links := []AcqLink{{Href: "p", Type: "application/pdf"}, {Href: "m", Type: "application/x-mobipocket-ebook"}, {Href: "f", Type: "application/x-fictionbook+xml"}}
	if b, _ := Best(links, BookFormats); b.Href != "f" {
		t.Fatalf("best %+v", b)
	}
	if _, ok := Best([]AcqLink{{Href: "d", Type: "image/vnd.djvu"}}, BookFormats); ok {
		t.Fatal("djvu picked")
	}
	if b, ok := Best([]AcqLink{{Href: "c", Type: "application/vnd.comicbook+zip"}}, ComicFormats); !ok || b.Href != "c" || FormatOf(b.Type) != "CBZ" {
		t.Fatalf("comic %+v", b)
	}
}

// hostClient dials every host named in hosts to the test server (as if
// DNS pointed there) and any other address through the address guard; the
// redirect rules are NewHTTPClient's.
func hostClient(srv *httptest.Server, hosts ...string) *http.Client {
	guarded := &net.Dialer{Control: addonkit.DialControl(false)}
	plain := &net.Dialer{}
	tr := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		h, _, _ := net.SplitHostPort(addr)
		for _, x := range hosts {
			if h == x {
				return plain.DialContext(ctx, network, srv.Listener.Addr().String())
			}
		}
		return guarded.DialContext(ctx, network, addr)
	}}
	return &http.Client{Transport: privateErr{tr}, CheckRedirect: NewHTTPClient(false).CheckRedirect}
}

// TestOPDSClientFollowsCatalogRedirects: a catalog whose url redirects to
// another host (a mirror, example.org → www.example.org) works; the login
// goes to the configured host only; a redirect into the home network is
// still refused; documents linked to a third host are not read.
func TestOPDSClientFollowsCatalogRedirects(t *testing.T) {
	cat := flibusta(t, "", "")
	var mu sync.Mutex
	authSeen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if _, _, ok := r.BasicAuth(); ok {
			authSeen[r.Host] = true
		}
		mu.Unlock()
		switch r.Host {
		case "example.org", "old-mirror.net":
			target := "http://www.example.org"
			if r.Host == "old-mirror.net" {
				target = "http://new-mirror.net"
			}
			http.Redirect(w, r, target+r.URL.RequestURI(), http.StatusMovedPermanently)
		case "www.example.org", "new-mirror.net":
			r.URL.Host = ""
			cat.Config.Handler.ServeHTTP(w, r) // the Flibusta-like catalog (relative links)
		case "private.example":
			http.Redirect(w, r, cat.URL+"/opds", http.StatusFound) // 127.0.0.1
		case "foreign.example":
			fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>x</title>
<link rel="search" type="application/opensearchdescription+xml" href="http://third.example/opds/opensearch.xml"/></feed>`)
		}
	}))
	defer srv.Close()
	hc := hostClient(srv, "example.org", "www.example.org", "old-mirror.net", "new-mirror.net", "private.example", "foreign.example", "third.example")

	for _, root := range []string{"http://example.org/opds", "http://old-mirror.net/opds"} {
		c := &Client{URL: root, Username: "reader", Password: "secret", HTTP: hc}
		got, err := c.Search(context.Background(), "x")
		if err != nil || len(got) != 2 {
			t.Fatalf("%s: %v %+v", root, err, got)
		}
		u, _ := url.Parse(got[0].Links[0].Href)
		want := map[string]string{"http://example.org/opds": "www.example.org", "http://old-mirror.net/opds": "new-mirror.net"}[root]
		if u.Hostname() != want {
			t.Fatalf("%s: links on %s", root, u.Host)
		}
		tm, err := c.Template(context.Background())
		if err != nil || tm.Host() != want {
			t.Fatalf("catalog host %q %v", tm.Host(), err)
		}
	}
	mu.Lock()
	if !authSeen["example.org"] || !authSeen["old-mirror.net"] || authSeen["www.example.org"] || authSeen["new-mirror.net"] {
		t.Fatalf("credentials went to %v", authSeen)
	}
	mu.Unlock()

	if err := (&Client{URL: "http://private.example/opds", HTTP: hc}).Test(context.Background()); !errors.Is(err, ErrPrivate) {
		t.Fatalf("redirect into the home network: %v", err)
	}
	if _, err := (&Client{URL: "http://foreign.example/opds", HTTP: hc}).Search(context.Background(), "x"); !errors.Is(err, ErrForeignHost) {
		t.Fatalf("description on a third host: %v", err)
	}
	c := SameSite(NewHTTPClient(true), "example.org", false)
	from, _ := http.NewRequest("GET", "https://example.org/a", nil)
	to, _ := http.NewRequest("GET", "http://example.org/b", nil)
	if err := c.CheckRedirect(to, []*http.Request{from}); !errors.Is(err, errDowngrade) {
		t.Fatalf("downgrade: %v", err)
	}
	sub, _ := http.NewRequest("GET", "https://www.example.org/b", nil)
	sub.Header.Set("Authorization", "Basic x")
	if err := c.CheckRedirect(sub, []*http.Request{from}); err != nil || sub.Header.Get("Authorization") != "" {
		t.Fatalf("login kept on a subdomain: %v %q", err, sub.Header.Get("Authorization"))
	}
}
