package followapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/pinned"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

type pinResp struct {
	Pin *struct {
		SourceID     string `json:"sourceId"`
		Link         string `json:"link"`
		ReleaseTitle string `json:"releaseTitle"`
		InfoHash     string `json:"infoHash"`
		Quality      string `json:"quality"`
		Episodes     int    `json:"episodes"`
		LastError    string `json:"lastError"`
		Suggestion   *struct {
			ReleaseTitle string `json:"releaseTitle"`
		} `json:"suggestion"`
	} `json:"pin"`
	Changed bool `json:"changed"`
}

func (e *env) pin(w *httptest.ResponseRecorder) pinResp {
	e.t.Helper()
	if w.Code != 200 {
		e.t.Fatalf("%d %s", w.Code, w.Body)
	}
	var out pinResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) title(kind string, tmdbID int64) (tracking.Title, bool) {
	t, ok, err := e.track.TitleByTMDB(context.Background(), kind, tmdbID)
	if err != nil {
		e.t.Fatal(err)
	}
	return t, ok
}

// An adult pins from a search result: the title is tracked at the core
// (made with the page's name) and kicked; a link off the source's site is
// refused; unpin.
func TestPinRoutes(t *testing.T) {
	e := newEnv(t)
	if got := e.pin(e.do("GET", "/api/pinned/series/100", mamaU, "")); got.Pin != nil {
		t.Fatalf("pin before pinning: %+v", got.Pin)
	}
	got := e.pin(e.do("POST", "/api/pinned/series/100", mamaU, `{"sourceId":3,"link":"https://tracker.test/dl/1","details":"https://tracker.test/t/1","releaseTitle":"Show S01 1080p","name":"Шоу"}`))
	if got.Pin == nil || got.Pin.SourceID != "3" || got.Pin.Quality != "1080p" || got.Pin.Episodes != 2 || got.Pin.ReleaseTitle != "Show S01 1080p" {
		t.Fatalf("pin %+v", got.Pin)
	}
	ti, ok := e.title("series", 100)
	if !ok || ti.Name != "Шоу" || len(e.track.Kicks) != 1 || e.track.Kicks[0] != ti.ID {
		t.Fatalf("title %+v %v, kicks %v", ti, ok, e.track.Kicks)
	}
	if w := e.do("POST", "/api/pinned/series/100", mamaU, `{"sourceId":3,"link":"https://elsewhere.test/x"}`); w.Code < 400 {
		t.Fatalf("foreign link: %d", w.Code)
	}
	if w := e.do("GET", "/api/pinned/album/100", mamaU, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", w.Code)
	}
	if w := e.do("DELETE", "/api/pinned/series/100", mamaU, ""); w.Code != http.StatusNoContent {
		t.Fatalf("unpin %d %s", w.Code, w.Body)
	}
	if got := e.pin(e.do("GET", "/api/pinned/series/100", mamaU, "")); got.Pin != nil {
		t.Fatalf("after unpin: %+v", got.Pin)
	}
}

// TestPinnedPlay_Input: «Смотреть» of a pinned episode streams it
// from the pin's link (never a follow or a download) and answers the core
// input of the stream's file; an own .torrent pin plays its infohash as
// a magnet; no pin, nothing to play.
func TestPinnedPlay_Input(t *testing.T) {
	e := newEnv(t)
	if w := e.do("POST", "/api/pinned/series/100/play", adminU, `{"season":1,"episode":2}`); w.Code != http.StatusNotFound {
		t.Fatalf("play without a pin: %d %s", w.Code, w.Body)
	}
	e.pin(e.do("POST", "/api/pinned/series/100", mamaU, `{"sourceId":3,"link":"https://tracker.test/dl/1"}`))
	w := e.do("POST", "/api/pinned/series/100/play", mamaU, `{"season":1,"episode":2}`)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"inputId":"in-1"}` {
		t.Fatalf("play %d %s", w.Code, w.Body)
	}
	c := e.player.plays[0]
	if c.Mode != acquire.ModeStream || c.ReleaseLink != "https://tracker.test/dl/1" || c.SourceID != 3 || c.Season != 1 || c.Episode != 2 ||
		c.Kind != acquire.KindSeries || c.TMDBID != 100 || c.UserID != 2 || !c.CheckLink {
		t.Fatalf("request %+v", c)
	}
	if want := (watched{2, "abcdef0123456789abcdef0123456789abcdef01", 1}); len(e.watches) != 1 || e.watches[0] != want {
		t.Fatalf("watch %+v", e.watches)
	}
	if e.acq.n() != 0 || e.added != 1 {
		t.Fatalf("follow acquisitions %d, holds changes %d", e.acq.n(), e.added)
	}
	// an own .torrent file pin plays its infohash as a magnet; an admin's
	// link is not checked
	e.pin(e.do("POST", "/api/pinned/movie/5/own", adminU, `{"file":"`+base64.StdEncoding.EncodeToString(e.mf)+`"}`))
	if w := e.do("POST", "/api/pinned/movie/5/play", adminU, ""); w.Code != 200 {
		t.Fatalf("film play %d %s", w.Code, w.Body)
	}
	if c := e.player.plays[1]; c.Kind != acquire.KindMovie || c.ReleaseLink != e.magnet || c.SourceID != 0 || c.CheckLink || c.Season != 0 {
		t.Fatalf("film request %+v", c)
	}
}

// TestPinnedOwn_Base64: an own .torrent file comes as base64 in
// JSON, at most 8 MiB decoded; a magnet as {input}; a multipart form is
// refused (the proxy takes JSON only).
func TestPinnedOwn_Base64(t *testing.T) {
	e := newEnv(t)
	ih, _, _, _ := torrent.ParseFiles(e.mf)
	b64 := base64.StdEncoding.EncodeToString(e.mf)
	got := e.pin(e.do("POST", "/api/pinned/series/100/own", mamaU, `{"file":"`+b64+`","name":"Шоу"}`))
	if got.Pin == nil || got.Pin.InfoHash != ih || got.Pin.ReleaseTitle != "Show.S01.1080p.WEB-DL" || got.Pin.SourceID != "" || got.Pin.Episodes != 2 {
		t.Fatalf("own file pin %+v", got.Pin)
	}
	if ti, ok := e.title("series", 100); !ok || ti.Name != "Шоу" {
		t.Fatalf("title %+v", ti)
	}
	got = e.pin(e.do("POST", "/api/pinned/series/101/own", mamaU, `{"input":"`+e.magnet+`"}`))
	if got.Pin == nil || got.Pin.Link != e.magnet {
		t.Fatalf("own magnet pin %+v", got.Pin)
	}
	// a 6 MiB metafile (a long series': accepted before the move) is pinned
	big := metafile(t, "Big.S01", (6<<20/20+1)*20, "Big.S01E01.mkv")
	if len(big) < 6<<20 {
		t.Fatalf("metafile %d bytes", len(big))
	}
	got = e.pin(e.do("POST", "/api/pinned/series/102/own", mamaU, `{"file":"`+base64.StdEncoding.EncodeToString(big)+`"}`))
	if got.Pin == nil || got.Pin.ReleaseTitle != "Big.S01" {
		t.Fatalf("6 MiB pin %+v", got.Pin)
	}
	titles := len(e.track.Titles)
	for _, c := range []struct {
		name, body, ct, want string
	}{
		{"8 MiB + 1", `{"file":"` + base64.StdEncoding.EncodeToString(make([]byte, 8<<20+1)) + `"}`, "application/json", "Torrent file is too large"},
		{"over the body cap", `{"input":"` + strings.Repeat("x", 12<<20) + `"}`, "application/json", "Torrent file is too large"},
		{"bad base64", `{"file":"not*base64!"}`, "application/json", "error"},
		{"multipart", multipartBody(t, e.mf), "multipart/form-data; boundary=b0undary", "error"},
	} {
		w := e.do("POST", "/api/pinned/series/300/own", enU(mamaU), c.body, "Content-Type", c.ct)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %d %.200s", c.name, w.Code, w.Body)
		}
	}
	// exactly 8 MiB passes the size checks (it is no metafile: 422)
	if w := e.do("POST", "/api/pinned/series/300/own", enU(mamaU), `{"file":"`+base64.StdEncoding.EncodeToString(make([]byte, 8<<20))+`"}`); w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "Not a torrent file") {
		t.Fatalf("8 MiB: %d %.200s", w.Code, w.Body)
	}
	if len(e.track.Titles) != titles {
		t.Fatalf("refused uploads made %d titles", len(e.track.Titles)-titles)
	}
}

func multipartBody(t *testing.T, mf []byte) string {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.SetBoundary("b0undary")
	fw, err := mw.CreateFormFile("torrent", "show.torrent")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(mf)
	mw.Close()
	return b.String()
}

// A refused pin leaves no tracked title behind; each refusal in the
// request's language.
func TestRefusedPinCreatesNoTitle(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		path, body string
		code       int
		en         string
	}{
		{"", `{"sourceId":3,"link":"https://elsewhere.test/x"}`, http.StatusUnprocessableEntity, "not from the selected source"},
		{"", `{"sourceId":3,"link":""}`, http.StatusBadRequest, ""},
		{"/own", `{"input":""}`, http.StatusUnprocessableEntity, "Enter a magnet link"},
		{"/own", `{"input":"https://elsewhere.test/t/1"}`, http.StatusUnprocessableEntity, "isn’t connected"},
		{"/own", `{"file":"` + base64.StdEncoding.EncodeToString([]byte("not bencode")) + `"}`, http.StatusUnprocessableEntity, "Not a torrent file"},
	} {
		w := e.do("POST", "/api/pinned/series/300"+c.path, enU(adminU), c.body)
		if w.Code != c.code || !strings.Contains(w.Body.String(), c.en) {
			t.Errorf("%s %s: %d %s", c.path, c.body, w.Code, w.Body)
		}
	}
	if n := len(e.track.Titles); n != 0 {
		t.Fatalf("refused pins made %d tracked titles", n)
	}
}

// The last refresh's reason and a suggestion show in the request's
// language; accepting with none left says so; a refresh that kept the
// pin answers with its reason.
func TestPinnedLastErrorAndRefresh(t *testing.T) {
	e := newEnv(t)
	e.pin(e.do("POST", "/api/pinned/series/100", adminU, `{"sourceId":3,"link":"https://tracker.test/dl/1"}`))
	ti, _ := e.title("series", 100)
	if err := e.pins.SetSuggestion(context.Background(), ti.ID, &pinned.Suggestion{Link: e.magnet, ReleaseTitle: "Show S01 2160p", Episodes: 2}); err != nil {
		t.Fatal(err)
	}
	if got := e.pin(e.do("GET", "/api/pinned/series/100", adminU, "")); got.Pin.Suggestion == nil || got.Pin.Suggestion.ReleaseTitle != "Show S01 2160p" {
		t.Fatalf("suggestion %+v", got.Pin)
	}
	if err := e.pins.SetSuggestion(context.Background(), ti.ID, nil); err != nil {
		t.Fatal(err)
	}
	if w := e.do("POST", "/api/pinned/series/100/accept-suggestion", adminU, ""); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("accept without one: %d %s", w.Code, w.Body)
	}
	e.dl["https://tracker.test/dl/1"] = metafile(t, "Show.S01.1080p.WEB-DL", 0, "Show.S01E01.1080p.mkv")
	for i := 0; i < 2; i++ {
		got := e.pin(e.do("POST", "/api/pinned/series/100/refresh", enU(adminU), ""))
		if got.Changed || got.Pin.LastError != "The updated torrent is missing some episodes — the previous one is kept" || got.Pin.Episodes != 2 {
			t.Fatalf("refresh %d: %+v", i, got)
		}
	}
	if got := e.pin(e.do("GET", "/api/pinned/series/100", enU(adminU), "")); got.Pin.LastError != "The updated torrent is missing some episodes — the previous one is kept" {
		t.Fatalf("pin %+v", got.Pin)
	}
	e.dl["https://tracker.test/dl/1"] = e.mf
	if got := e.pin(e.do("POST", "/api/pinned/series/100/refresh", adminU, "")); got.Changed || got.Pin.LastError != "" {
		t.Fatalf("refresh same %+v", got.Pin)
	}
	if w := e.do("POST", "/api/pinned/series/999/refresh", adminU, ""); w.Code != http.StatusNotFound {
		t.Fatalf("refresh no pin %d", w.Code)
	}
}

// «Скачать серию»: given episodes of the pin download to the server, for
// any adult (the link checked unless an admin's); 409 when the pin holds
// none of them; 404 with no pin.
func TestPinnedDownloadEpisodes(t *testing.T) {
	e := newEnv(t)
	if w := e.do("POST", "/api/pinned/series/100/download", adminU, `{"season":1,"episodes":[2]}`); w.Code != http.StatusNotFound {
		t.Fatalf("download without a pin: %d %s", w.Code, w.Body)
	}
	e.pin(e.do("POST", "/api/pinned/series/100", adminU, `{"sourceId":3,"link":"https://tracker.test/dl/1"}`))
	if w := e.do("POST", "/api/pinned/series/100/download", adminU, `{"season":1,"episodes":[2]}`); w.Code != 200 {
		t.Fatalf("download %d %s", w.Code, w.Body)
	}
	c := e.player.dl[0]
	ih, _, _, _ := torrent.ParseFiles(e.mf)
	if c.ih != ih || fmt.Sprint(c.files) != "[1]" || c.req.ReleaseLink != "https://tracker.test/dl/1" || c.req.SourceID != 3 || c.req.TMDBID != 100 ||
		c.req.Kind != acquire.KindSeries || c.req.Season != 1 || c.req.CheckLink {
		t.Fatalf("call %+v", c)
	}
	if w := e.do("POST", "/api/pinned/series/100/download", adminU, `{"season":1,"episodes":[9]}`); w.Code != http.StatusConflict {
		t.Fatalf("missing episode: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/pinned/series/100/download", adminU, `{"season":1,"episodes":[]}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no episodes: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/pinned/series/100/download", mamaU, `{"season":1,"episodes":[1]}`); w.Code != 200 {
		t.Fatalf("adult: %d %s", w.Code, w.Body)
	}
	if c := e.player.dl[1]; !c.req.CheckLink || fmt.Sprint(c.files) != "[0]" || c.req.UserID != 2 {
		t.Fatalf("adult call %+v", c)
	}
	if e.added != 2 {
		t.Fatalf("holds changes %d", e.added)
	}
}
