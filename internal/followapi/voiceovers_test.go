package followapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// «Найти озвучку…»: adults and the admin reach the routes of a library
// item (kids: TestFollowAPI_KidForbidden); «force» is the admin's only;
// an unknown item is 404.
func TestVoiceOversAPI(t *testing.T) {
	e := newEnv(t)
	film := e.item("movie", "Дюна")
	path := "/api/items/" + itoa(film) + "/voiceovers"
	for _, u := range []string{mamaU, adminU} {
		w := e.do("GET", path, u, "")
		var out struct {
			Candidates    []map[string]any `json:"candidates"`
			VideoReleases []map[string]any `json:"videoReleases"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Candidates == nil || out.VideoReleases == nil {
			t.Fatalf("GET: %d %s", w.Code, w.Body)
		}
	}
	if w := e.do("POST", path+"/files", mamaU, `{"id":"c1"}`); w.Code != http.StatusOK {
		t.Fatalf("files: %d %s", w.Code, w.Body)
	}
	for _, u := range []string{mamaU, adminU} {
		if w := e.do("POST", path, u, `{"id":"c1","force":true}`); w.Code != http.StatusOK {
			t.Fatalf("download: %d %s", w.Code, w.Body)
		}
	}
	if v := e.player.voices; len(v) != 2 || v[0].Force || v[0].UserID != 2 || v[0].ItemID != film || !v[1].Force || v[1].UserID != 1 {
		t.Fatalf("downloads %+v", v)
	}
	if w := e.do("GET", path+"/downloads", mamaU, ""); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("downloads: %d %q", w.Code, w.Body)
	}
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/items/999/voiceovers"}, {"POST", "/api/items/999/voiceovers"},
		{"POST", "/api/items/999/voiceovers/files"}, {"GET", "/api/items/999/voiceovers/downloads"},
	} {
		if w := e.do(c.method, c.path, mamaU, `{"id":"c1"}`); w.Code != http.StatusNotFound {
			t.Errorf("unknown item %s %s: %d", c.method, c.path, w.Code)
		}
	}
	if w := e.do("POST", path, mamaU, `{`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad body: %d", w.Code)
	}
}
