package followapi

import (
	"context"
	"net/http"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
)

// guestU is an adult limited to 12+ (X-Antresol-User maxRating).
const guestU = `{"id":5,"name":"Гость","isAdmin":false,"isKid":false,"lang":"ru","maxRating":"12+"}`

// TestPinRoutesHonourRating (ported): a limited profile does not reach
// the pin of a title over its limit: a library series rated 18+, or a
// title off the library with no certification; every route is 404 and
// nothing changes (no title made, no stream). A title off the library
// certified 12+ and a library series rated 12+ pass.
func TestPinRoutesHonourRating(t *testing.T) {
	e := newEnv(t)
	e.lib.Items = append(e.lib.Items,
		library.Item{ID: 50, Kind: "series", Title: "Взрослое", Rating: "18+", TmdbID: 100, MetaJSON: "{}"},
		library.Item{ID: 51, Kind: "series", Title: "Семейное", Rating: "12+", TmdbID: 400, MetaJSON: "{}"})
	e.pin(e.do("POST", "/api/pinned/series/100", adminU, `{"sourceId":3,"link":"https://tracker.test/dl/1"}`))
	for _, id := range []string{"100", "200"} {
		for _, c := range []struct{ method, path, body string }{
			{"GET", "", ""},
			{"POST", "", `{"sourceId":3,"link":"https://tracker.test/dl/1"}`},
			{"POST", "/own", `{"input":"` + e.magnet + `"}`},
			{"DELETE", "", ""},
			{"POST", "/refresh", ""},
			{"POST", "/accept-suggestion", ""},
			{"POST", "/play", `{"season":1,"episode":1}`},
			{"POST", "/download", `{"season":1,"episodes":[1]}`},
		} {
			if w := e.do(c.method, "/api/pinned/series/"+id+c.path, guestU, c.body); w.Code != http.StatusNotFound {
				t.Errorf("limited %s %s%s: %d %s", c.method, id, c.path, w.Code, w.Body)
			}
		}
	}
	if got := e.pin(e.do("GET", "/api/pinned/series/100", adminU, "")); got.Pin == nil {
		t.Fatal("a hidden title's pin was dropped")
	}
	if n := len(e.track.Titles); n != 1 || len(e.player.plays)+len(e.player.dl) != 0 {
		t.Fatalf("tracked titles %d, streams %d, downloads %d", n, len(e.player.plays), len(e.player.dl))
	}
	// under the limit: certified off the library, or rated in it
	if _, err := e.track.EnsureTitle(context.Background(), "movie", 300, "Мультфильм", ""); err != nil {
		t.Fatal(err)
	}
	e.track.Titles[len(e.track.Titles)-1].Cert = "6+"
	for _, p := range []string{"/api/pinned/movie/300", "/api/pinned/series/400"} {
		if w := e.do("GET", p, guestU, ""); w.Code != http.StatusOK {
			t.Errorf("limited GET %s: %d %s", p, w.Code, w.Body)
		}
	}
	// a title off the library rated over the limit stays hidden
	e.track.Titles[len(e.track.Titles)-1].Cert = "16+"
	if w := e.do("GET", "/api/pinned/movie/300", guestU, ""); w.Code != http.StatusNotFound {
		t.Errorf("limited GET over the cert: %d", w.Code)
	}
	// no limit: everything
	if w := e.do("GET", "/api/pinned/series/200", mamaU, ""); w.Code != http.StatusOK {
		t.Errorf("unlimited GET: %d", w.Code)
	}
}

// The voice-over routes of a library item over the profile's limit are
// 404, an episode by its series' rating; one under it passes.
func TestVoiceOversHonourRating(t *testing.T) {
	e := newEnv(t)
	e.lib.Items = append(e.lib.Items,
		library.Item{ID: 60, Kind: "movie", Title: "Ужасы", Rating: "18+", MetaJSON: "{}"},
		library.Item{ID: 61, Kind: "movie", Title: "Мультфильм", Rating: "6+", MetaJSON: "{}"},
		library.Item{ID: 62, Kind: "movie", Title: "Без рейтинга", MetaJSON: "{}"},
		library.Item{ID: 63, Kind: "series", Title: "Взрослое", Rating: "18+", MetaJSON: "{}"},
		library.Item{ID: 64, Kind: "season", ParentID: 63, SeasonNo: 1, MetaJSON: "{}"},
		library.Item{ID: 65, Kind: "episode", ParentID: 64, SeasonNo: 1, EpisodeNo: 1, MetaJSON: "{}"})
	for _, id := range []string{"60", "62", "65"} {
		base := "/api/items/" + id + "/voiceovers"
		for _, c := range []struct{ method, path string }{
			{"GET", base}, {"POST", base}, {"POST", base + "/files"}, {"GET", base + "/downloads"},
		} {
			if w := e.do(c.method, c.path, guestU, `{"id":"c1"}`); w.Code != http.StatusNotFound {
				t.Errorf("limited %s %s: %d %s", c.method, c.path, w.Code, w.Body)
			}
		}
		if w := e.do("GET", base, mamaU, ""); w.Code != http.StatusOK {
			t.Errorf("unlimited GET %s: %d", base, w.Code)
		}
	}
	if len(e.player.voices) != 0 {
		t.Fatalf("hidden downloads %+v", e.player.voices)
	}
	if w := e.do("GET", "/api/items/61/voiceovers", guestU, ""); w.Code != http.StatusOK {
		t.Fatalf("under the limit: %d %s", w.Code, w.Body)
	}
}
