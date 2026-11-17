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
