package cardigann

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A response over the size limit must fail instead of being silently
// truncated (a cut .torrent would otherwise be returned as valid).
func TestOversizedResponseIsError(t *testing.T) {
	old := maxBody
	maxBody = 64
	t.Cleanup(func() { maxBody = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("d8:announce" + strings.Repeat("x", 200) + "e"))
	}))
	defer srv.Close()
	def, err := Parse([]byte("id: big\nname: Big\nlinks: [\"" + srv.URL + "/\"]\nsettings: []\nsearch:\n  path: s\n  rows: {selector: tr}\n  fields: {title: {selector: td}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Def: def}
	_, err = r.Download(context.Background(), srv.URL+"/file.torrent")
	if !errors.Is(err, ErrBadResponse) {
		t.Fatalf("err = %v, want ErrBadResponse", err)
	}
}

// A page to be parsed has a smaller limit than a .torrent file: it is
// copied to a string and built into a tree of about ten times its size.
func TestPageBodyLimitIsSmallerThanTorrentLimit(t *testing.T) {
	old := maxPageBody
	maxPageBody = 1 << 10
	t.Cleanup(func() { maxPageBody = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/page" {
			w.Write([]byte("<html>" + strings.Repeat("x", 4<<10) + "</html>"))
			return
		}
		w.Write([]byte("d8:announce" + strings.Repeat("x", 4<<10) + "e")) // a torrent file of the same size
	}))
	defer srv.Close()
	r := redirectRunner(t, srv.URL)
	if _, err := r.do(context.Background(), &request{method: http.MethodGet, url: srv.URL + "/page"}); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("page: err = %v, want ErrBadResponse", err)
	}
	res, err := r.Download(context.Background(), srv.URL+"/file.torrent")
	if err != nil || len(res.Torrent) < 4<<10 {
		t.Fatalf("torrent: %d bytes, %v", len(res.Torrent), err)
	}
}

// A page with more rows than maxRows gives maxRows results; XML nested
// deeper than maxXMLDepth is refused.
func TestParseCapsRowsAndXMLDepth(t *testing.T) {
	r := &Runner{Def: mustParse(t, "id: rows\nname: Rows\nlinks: [\"http://example.test/\"]\nsettings: []\nsearch:\n  path: s\n  rows: {selector: tr}\n  fields: {title: {selector: td}}\n")}
	if err := r.setup(); err != nil {
		t.Fatal(err)
	}
	page := "<table>" + strings.Repeat("<tr><td>t</td></tr>", maxRows+500) + "</table>"
	got, err := r.parseDOM(page, false, r.base, map[string]any{})
	if err != nil || len(got) != maxRows {
		t.Fatalf("html rows: %d, %v; want %d", len(got), err, maxRows)
	}
	got, err = r.parseDOM(page, true, r.base, map[string]any{})
	if err != nil || len(got) != maxRows {
		t.Fatalf("xml rows: %d, %v; want %d", len(got), err, maxRows)
	}
	deep := strings.Repeat("<a>", maxXMLDepth+10) + strings.Repeat("</a>", maxXMLDepth+10)
	if _, err := r.parseDOM(deep, true, r.base, map[string]any{}); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("deep xml: err = %v, want ErrBadResponse", err)
	}
}

// A response is decoded and parsed as HTML once, whoever looks at it (the
// sign-in check, the error check, the row parser).
func TestResponseIsDecodedAndParsedOnce(t *testing.T) {
	r := &Runner{Def: mustParse(t, "id: once\nname: Once\nlinks: [\"http://example.test/\"]\nsettings: []\nsearch:\n  path: s\n  rows: {selector: tr}\n  fields: {title: {selector: td}}\n")}
	if err := r.setup(); err != nil {
		t.Fatal(err)
	}
	resp := &response{body: []byte("<table><tr><td>a</td></tr></table>")}
	d1, err1 := r.htmlDoc(resp)
	d2, err2 := r.htmlDoc(resp)
	if err1 != nil || err2 != nil || d1 == nil || d1 != d2 {
		t.Fatalf("two looks gave %p and %p (%v, %v)", d1, d2, err1, err2)
	}
	first := r.text(resp)
	if r.text(resp) != first || resp.str == nil {
		t.Fatal("the text is decoded again")
	}
	got, err := r.parseResults(resp, &r.Def.Search.Paths[0], map[string]any{})
	if err != nil || len(got) != 1 || got[0].Title != "a" {
		t.Fatalf("results %+v, %v", got, err)
	}
}
