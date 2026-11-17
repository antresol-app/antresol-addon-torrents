package followapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
)

// Kids are refused every route here (defence in depth: the core keeps
// them from the proxy), and so is a request with no profile.
func TestFollowAPI_KidForbidden(t *testing.T) {
	e := newEnv(t)
	f := e.follow(adminU, "series", 42)
	film := e.item("movie", "Дюна")
	id := itoa(f.ID)
	vo := "/api/items/" + itoa(film) + "/voiceovers"
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/follow", ""},
		{"POST", "/api/follow", `{"kind":"series","tmdbId":42}`},
		{"GET", "/api/follow/by-tmdb?kind=series&tmdbId=42", ""},
		{"DELETE", "/api/follow/" + id, ""},
		{"PATCH", "/api/follow/" + id, `{"active":false}`},
		{"POST", "/api/follow/" + id + "/check", ""},
		{"GET", "/api/pinned/series/100", ""},
		{"POST", "/api/pinned/series/100", `{"sourceId":3,"link":"https://tracker.test/dl/1"}`},
		{"POST", "/api/pinned/series/100/own", `{"input":"magnet:?xt=urn:btih:00"}`},
		{"DELETE", "/api/pinned/series/100", ""},
		{"POST", "/api/pinned/series/100/refresh", ""},
		{"POST", "/api/pinned/series/100/accept-suggestion", ""},
		{"POST", "/api/pinned/series/100/play", `{"season":1,"episode":1}`},
		{"POST", "/api/pinned/series/100/download", `{"season":1,"episodes":[1]}`},
		{"GET", vo, ""},
		{"POST", vo, `{"id":"x"}`},
		{"POST", vo + "/files", `{"id":"x"}`},
		{"GET", vo + "/downloads", ""},
	} {
		for _, u := range []string{kidU, ""} {
			w := e.do(c.method, c.path, u, c.body)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Недоступно в детском профиле") {
				t.Errorf("%q %s %s: %d %s", u, c.method, c.path, w.Code, w.Body)
			}
		}
	}
	if n := len(e.player.plays) + len(e.player.dl) + len(e.player.voices) + len(e.track.Titles); n != 0 {
		t.Fatalf("a kid's request did something: %d", n)
	}
}

// TestFollowAPI_Ownership: any adult follows (no
// approval; one follow per title); adults see and change only the
// follows they added, the admin all of them.
func TestFollowAPI_Ownership(t *testing.T) {
	e := newEnv(t)
	adminF := e.follow(adminU, "series", 42)
	if g := e.follow(mamaU, "series", 42); g.ID != adminF.ID {
		t.Fatalf("second follow %+v vs %+v", g, adminF)
	}
	own := e.follow(mamaU, "movie", 7)
	if own.AddedBy != "Мама" {
		t.Fatalf("author: %+v", own)
	}
	if l := e.list(mamaU); len(l) != 1 || l[0].ID != own.ID {
		t.Fatalf("adult list: %+v", l)
	}
	if l := e.list(sonU); len(l) != 0 {
		t.Fatalf("another adult's list: %+v", l)
	}
	if l := e.list(adminU); len(l) != 2 {
		t.Fatalf("admin list: %+v", l)
	}
	if w := e.do("POST", "/api/follow", mamaU, `{"kind":"album","tmdbId":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("album: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/follow/by-tmdb?kind=series&tmdbId=42", mamaU, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":`+itoa(adminF.ID)) {
		t.Fatalf("by-tmdb: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/follow/by-tmdb?kind=movie&tmdbId=42", mamaU, ""); w.Code != http.StatusNotFound {
		t.Fatalf("by-tmdb missing: %d %s", w.Code, w.Body)
	}
	other := itoa(adminF.ID)
	for _, c := range []struct{ method, path, body string }{
		{"DELETE", "/api/follow/" + other, ""},
		{"PATCH", "/api/follow/" + other, `{"active":false}`},
		{"POST", "/api/follow/" + other + "/check", ""},
	} {
		w := e.do(c.method, c.path, mamaU, c.body)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "Это слежение добавил другой профиль") {
			t.Errorf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
	}
	if g, _ := e.follows.Get(context.Background(), adminF.ID); !g.Active {
		t.Fatal("someone else's follow was switched off")
	}
	mine := itoa(own.ID)
	if w := e.do("PATCH", "/api/follow/"+mine, mamaU, `{"active":false}`); w.Code != 200 {
		t.Fatalf("patch own: %d %s", w.Code, w.Body)
	}
	if w := e.do("PATCH", "/api/follow/"+mine, mamaU, `{"active":true}`); w.Code != 200 {
		t.Fatalf("patch own: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/follow/"+mine+"/check", mamaU, ""); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"started":true`) {
		t.Fatalf("check own: %d %s", w.Code, w.Body)
	}
	if w := e.do("PATCH", "/api/follow/"+mine, adminU, `{"active":false}`); w.Code != 200 {
		t.Fatalf("admin patch: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/follow/"+mine+"/check", adminU, ""); w.Code != http.StatusConflict {
		t.Fatalf("check inactive: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/api/follow/"+mine, mamaU, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete own: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/api/follow/"+other, adminU, ""); w.Code != http.StatusNoContent {
		t.Fatalf("admin delete: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/api/follow/"+other, adminU, ""); w.Code != http.StatusNotFound {
		t.Fatalf("delete again: %d %s", w.Code, w.Body)
	}
	if w := e.do("PATCH", "/api/follow/999", adminU, `{"active":true}`); w.Code != http.StatusNotFound {
		t.Fatalf("missing: %d %s", w.Code, w.Body)
	}
	if w := e.do("PATCH", "/api/follow/x", adminU, `{"active":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad id: %d %s", w.Code, w.Body)
	}
}

// TestFollowList_OrphanAdminOnly: a follow whose profile was
// deleted is «Профиль удалён» to admins, who alone list and change it; an
// adult neither lists it nor sees that name on the title page.
func TestFollowList_OrphanAdminOnly(t *testing.T) {
	e := newEnv(t)
	gone, err := e.follows.Add(context.Background(), e.users.User(9), "series", 43) // profile 9 is not on the server
	if err != nil {
		t.Fatal(err)
	}
	l := e.list(adminU)
	if len(l) != 1 || l[0].ID != gone.ID || l[0].AddedBy != "Профиль удалён" {
		t.Fatalf("admin list %+v", l)
	}
	if l := e.list(enU(adminU)); l[0].AddedBy != "Profile deleted" {
		t.Fatalf("admin list in English %+v", l)
	}
	if l := e.list(mamaU); len(l) != 0 {
		t.Fatalf("adult list %+v", l)
	}
	w := e.do("GET", "/api/follow/by-tmdb?kind=series&tmdbId=43", mamaU, "")
	var f follow.Follow
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &f) != nil || f.ID != gone.ID || f.AddedBy != "" {
		t.Fatalf("adult by-tmdb %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/follow/by-tmdb?kind=series&tmdbId=43", adminU, ""); !strings.Contains(w.Body.String(), "Профиль удалён") {
		t.Fatalf("admin by-tmdb %s", w.Body)
	}
	if w := e.do("DELETE", "/api/follow/"+itoa(gone.ID), mamaU, ""); w.Code != http.StatusForbidden {
		t.Fatalf("orphan delete by an adult: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/api/follow/"+itoa(gone.ID), adminU, ""); w.Code != http.StatusNoContent {
		t.Fatalf("orphan delete by the admin: %d %s", w.Code, w.Body)
	}
}

// TestFollowResolution: «Докачивать новые» takes the quality on adding and
// on a PATCH; an unknown one is refused before anything changes; another
// profile's follow keeps its own.
func TestFollowResolution(t *testing.T) {
	e := newEnv(t)
	if w := e.do("POST", "/api/follow", adminU, `{"kind":"series","tmdbId":42,"resolution":"480p"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown resolution: %d %s", w.Code, w.Body)
	}
	if _, err := e.follows.ByTMDB(context.Background(), "series", 42); err == nil {
		t.Fatal("a refused follow was added")
	}
	w := e.do("POST", "/api/follow", adminU, `{"kind":"series","tmdbId":42,"resolution":"1080p"}`)
	var f follow.Follow
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &f) != nil || f.Resolution != "1080p" {
		t.Fatalf("add %d %s", w.Code, w.Body)
	}
	w = e.do("PATCH", "/api/follow/"+itoa(f.ID), adminU, `{"resolution":"2160p"}`)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &f) != nil || f.Resolution != "2160p" || !f.Active {
		t.Fatalf("patch %d %s", w.Code, w.Body)
	}
	w = e.do("PATCH", "/api/follow/"+itoa(f.ID), adminU, `{"active":false,"resolution":""}`)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &f) != nil || f.Resolution != "" || f.Active {
		t.Fatalf("patch both %d %s", w.Code, w.Body)
	}
	if w := e.do("PATCH", "/api/follow/"+itoa(f.ID), adminU, `{"resolution":"8k"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("patch unknown: %d %s", w.Code, w.Body)
	}
	if w := e.do("PATCH", "/api/follow/"+itoa(f.ID), adminU, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("patch nothing: %d %s", w.Code, w.Body)
	}
	w = e.do("POST", "/api/follow", mamaU, `{"kind":"series","tmdbId":42,"resolution":"720p"}`)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &f) != nil || f.Resolution != "" {
		t.Fatalf("other's add %d %s", w.Code, w.Body)
	}
}

// TestFollow_MineByID (addon-M4): whose follow it is goes by the
// profile (its uid), not by name: a second profile with the same name is
// not its author.
func TestFollow_MineByID(t *testing.T) {
	e := newEnv(t)
	if _, err := e.follows.Add(context.Background(), e.users.User(2), "series", 42); err != nil {
		t.Fatal(err)
	}
	twin := `{"id":3,"uid":"uid-3","name":"Мама","isAdmin":false,"isKid":false,"lang":"ru"}` // profile 3, renamed «Мама»
	mine := func(user string) (bool, int) {
		t.Helper()
		w := e.do("GET", "/api/follow/by-tmdb?kind=series&tmdbId=42", user, "")
		var f struct {
			Mine bool `json:"mine"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &f) != nil {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
		return f.Mine, len(e.list(user))
	}
	if m, n := mine(mamaU); !m || n != 1 {
		t.Fatalf("author: mine %v, listed %d", m, n)
	}
	if m, n := mine(twin); m || n != 0 {
		t.Fatalf("same name, other id: mine %v, listed %d", m, n)
	}
	if m, _ := mine(adminU); m {
		t.Fatal("the admin is not the author")
	}
	if l := e.list(adminU); len(l) != 1 || l[0].Mine {
		t.Fatalf("admin list %+v", l)
	}
	if l := e.list(mamaU); !l[0].Mine {
		t.Fatalf("author list %+v", l)
	}
}

// TestFollow_ReusedIDNotMine: profile (3, A) followed a title, was
// deleted and created again as (3, B): B (X-Antresol-User id 3, uid B)
// neither sees it as its own nor changes it, with the core down and at
// once (a restart: follow's TestFollow_UIDReusedID).
func TestFollow_ReusedIDNotMine(t *testing.T) {
	e := newEnv(t)
	e.users.UIDs = map[int64]string{3: "A"}
	sonA := strings.Replace(sonU, `"uid":"uid-3"`, `"uid":"A"`, 1)
	sonB := strings.Replace(sonU, `"uid":"uid-3"`, `"uid":"B"`, 1)
	f := e.follow(sonA, "series", 42)
	if !f.Mine {
		t.Fatalf("A's own follow %+v", f)
	}
	check := func(what string) {
		t.Helper()
		w := e.do("GET", "/api/follow/by-tmdb?kind=series&tmdbId=42", sonB, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), `"mine":true`) {
			t.Fatalf("%s: by-tmdb %d %s", what, w.Code, w.Body)
		}
		if l := e.list(sonB); len(l) != 0 {
			t.Fatalf("%s: B lists %+v", what, l)
		}
		if w := e.do("PATCH", "/api/follow/"+itoa(f.ID), sonB, `{"active":false}`); w.Code != http.StatusForbidden {
			t.Fatalf("%s: B changed A's follow: %d %s", what, w.Code, w.Body)
		}
	}
	e.users.Err = errors.New("core down") // the id came back before any list was read
	check("core down")
	e.users.Err, e.users.UIDs[3] = nil, "B"
	check("at once")
}
