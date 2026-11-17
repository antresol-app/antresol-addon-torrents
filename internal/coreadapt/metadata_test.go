package coreadapt_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol/addonkit"
)

// fakeMetaCore answers the pass-through like the core: sources in on are
// on (a probe with no path is 400), others off (404); it records the
// other requests.
type fakeMetaCore struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (f *fakeMetaCore) serve(on ...string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		src, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/addon-core/v1/metadata/"), "/")
		known := false
		for _, s := range on {
			known = known || s == src
		}
		switch {
		case !known:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"Источник выключен"}`))
		case rest == "":
			w.WriteHeader(http.StatusBadRequest)
		default:
			f.mu.Lock()
			f.reqs = append(f.reqs, r)
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"results":[{"id":603,"title":"Матрица","release_date":"1999-03-31"}]}`))
		}
	}))
	return srv
}

func TestMetadata_NoKey(t *testing.T) {
	f := &fakeMetaCore{}
	srv := f.serve("tmdb", "wikidata")
	defer srv.Close()
	c := addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})
	tm, mb, ol, wd := coreadapt.Metadata(c, addonkit.Info{})
	if tm == nil || wd == nil || mb != nil || ol != nil {
		t.Fatalf("sources: tmdb %v musicbrainz %v openlibrary %v wikidata %v", tm != nil, mb != nil, ol != nil, wd != nil)
	}
	if tm.Key != "" || tm.BaseURL != c.MetadataBase("tmdb") || wd.BaseURL != c.MetadataBase("wikidata") {
		t.Fatalf("tmdb %q %q, wikidata %q", tm.Key, tm.BaseURL, wd.BaseURL)
	}
	res, err := tm.SearchMovie(ctx, "Матрица", 1999)
	if err != nil || len(res) != 1 || res[0].ID != 603 {
		t.Fatalf("%+v %v", res, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) != 1 {
		t.Fatalf("%d requests", len(f.reqs))
	}
	r := f.reqs[0]
	if r.URL.Path != "/api/addon-core/v1/metadata/tmdb/search/movie" || r.URL.Query().Has("api_key") ||
		r.Header.Get("Authorization") != "Bearer tok" || r.URL.Query().Get("query") != "Матрица" {
		t.Fatalf("%s %s auth %q", r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"))
	}
}

func TestMetadata_AllOn(t *testing.T) {
	f := &fakeMetaCore{}
	srv := f.serve("tmdb", "musicbrainz", "openlibrary", "wikidata")
	defer srv.Close()
	c := addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})
	tm, mb, ol, wd := coreadapt.Metadata(c, addonkit.Info{})
	if tm == nil || mb == nil || ol == nil || wd == nil {
		t.Fatal("a source is missing")
	}
	// covers stay on their own hosts: the image cache fetches them
	if u := mb.CoverURL("x"); !strings.HasPrefix(u, "https://coverartarchive.org/") {
		t.Fatalf("cover %q", u)
	}
	if u := ol.CoverURL(1); !strings.HasPrefix(u, "https://covers.openlibrary.org/") {
		t.Fatalf("cover %q", u)
	}
	if len(f.reqs) != 0 {
		t.Fatalf("the probe asked the provider: %d", len(f.reqs))
	}
}
