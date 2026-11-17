//go:build ignore

package connect

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport/files"
	"github.com/antresol-app/antresol-addon-torrents/internal/musicimport"
)

// fakeGoogle serves testdata/youtube as the YouTube Data API v3, Google's
// token endpoint and YouTube's oEmbed would: answers can be overridden per
// request (hook).
type fakeGoogle struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	paths  []string
	access string
	hook   func(w http.ResponseWriter, r *http.Request) (done bool)
	form   url.Values // the last token request
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{t: t, access: "ya29.fake-access-token"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoogle) file(w http.ResponseWriter, name string, code int) {
	b, err := os.ReadFile(filepath.Join("testdata", "youtube", name))
	if err != nil {
		f.t.Errorf("testdata %s: %v", name, err)
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(code)
	w.Write(b)
}

func (f *fakeGoogle) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path+"?"+r.URL.RawQuery)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil && hook(w, r) {
		return
	}
	q := r.URL.Query()
	switch p := r.URL.Path; {
	case p == "/token":
		r.ParseForm()
		f.mu.Lock()
		f.form = r.PostForm
		f.mu.Unlock()
		if r.PostForm.Get("code") == "bad" {
			f.file(w, "invalid-grant.json", 400)
			return
		}
		f.file(w, "token.json", 200)
		return
	case p == "/oembed":
		f.t.Errorf("unexpected oEmbed %s", r.URL)
		w.WriteHeader(404)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.access {
		f.file(w, "unauthorized.json", 401)
		return
	}
	page := q.Get("pageToken")
	if page == "" {
		page = "0"
	}
	switch p := strings.TrimPrefix(r.URL.Path, "/youtube/v3"); {
	case p == "/channels" && q.Get("mine") == "true" && q.Get("part") == "contentDetails":
		f.file(w, "channels.json", 200)
	case p == "/playlists" && q.Get("mine") == "true" && q.Get("part") == "snippet,contentDetails" && q.Get("maxResults") == "50":
		f.file(w, "playlists-"+page+".json", 200)
	case p == "/playlistItems" && q.Get("part") == "snippet,contentDetails" && q.Get("maxResults") == "50":
		switch pl := q.Get("playlistId"); pl {
		case "LL", "PLroad", "PLvlog":
			f.file(w, "playlistItems-"+pl+"-"+page+".json", 200)
		case "PLgone":
			f.file(w, "playlist-not-found.json", 404)
		default:
			f.t.Errorf("unexpected playlist %s", r.URL)
			w.WriteHeader(404)
		}
	case p == "/videos" && q.Get("part") == "snippet,contentDetails":
		ids := strings.Split(q.Get("id"), ",")
		if len(ids) > 50 {
			f.t.Errorf("videos.list with %d ids", len(ids))
		}
		b, _ := os.ReadFile(filepath.Join("testdata", "youtube", "videos.json"))
		var all struct {
			Items []map[string]any `json:"items"`
		}
		json.Unmarshal(b, &all)
		out := []map[string]any{}
		for _, v := range all.Items {
			for _, id := range ids {
				if v["id"] == id {
					out = append(out, v)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=UTF-8")
		json.NewEncoder(w).Encode(map[string]any{"kind": "youtube#videoListResponse", "items": out,
			"pageInfo": map[string]int{"totalResults": len(out), "resultsPerPage": len(out)}})
	default:
		f.t.Errorf("unexpected %s", r.URL)
		w.WriteHeader(404)
	}
}

func (f *fakeGoogle) requested(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, p := range f.paths {
		if strings.HasPrefix(p, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeGoogle) reader(sl *sleeps) *YouTube {
	y := NewYouTube("client-g.apps.googleusercontent.com", "GOCSPX-fake-secret")
	y.BaseURL, y.TokenURL, y.HTTP = f.srv.URL+"/youtube/v3", f.srv.URL+"/token", f.srv.Client()
	y.Limiter = nil
	if sl != nil {
		y.Sleep = sl.sleep
	}
	return y
}

func readYouTube(t *testing.T, f *fakeGoogle, sl *sleeps) playlistimport.Snapshot {
	t.Helper()
	var last [2]int
	snap, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, func(done, total int) { last = [2]int{done, total} })
	if err != nil {
		t.Fatal(err)
	}
	if last[0] == 0 || last[0] != last[1] {
		t.Errorf("progress %v", last)
	}
	return snap
}

func keyed(s playlistimport.Snapshot) map[string]playlistimport.SnapTrack {
	m := map[string]playlistimport.SnapTrack{}
	for _, t := range s.Tracks {
		m[t.Key] = t
	}
	return m
}

func TestYouTubeReadsLikedMusicOnly(t *testing.T) {
	f := newFakeGoogle(t)
	snap := readYouTube(t, f, nil)
	if snap.Service != playlistimport.ServiceYouTube || snap.Account != "UCme0000000000000000000" {
		t.Fatalf("service %q account %q", snap.Service, snap.Account)
	}
	if n := f.requested("/youtube/v3/playlistItems?"); n < 2 {
		t.Fatalf("playlistItems requests %d", n)
	}
	var liked []string
	for _, tr := range snap.Tracks {
		if tr.Liked {
			liked = append(liked, tr.Key)
		}
	}
	// 5 liked videos: the vlog and the game are no music
	if strings.Join(liked, ",") != "youtube:kinoBlood01,youtube:metSadTrue1,youtube:kinoKukush1" {
		t.Fatalf("liked %v", liked)
	}
	by := keyed(snap)
	blood := by["youtube:kinoBlood01"]
	if blood.Artist != "Кино" || blood.Title != "Группа крови" || blood.Album != "Группа крови" || blood.DurationMs != 286000 ||
		blood.AddedAt == nil || !blood.AddedAt.Equal(time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("Группа крови %+v", blood)
	}
	// the « - Topic» channel is the artist; the album is in the description
	if k := by["youtube:kinoKukush1"]; k.Artist != "Кино" || k.Title != "Кукушка" || k.Album != "Звезда по имени Солнце" || k.DurationMs != 397000 {
		t.Fatalf("Кукушка %+v", k)
	}
	// not auto-generated: «Artist - Title» from the title, the noise dropped
	if m := by["youtube:metSadTrue1"]; m.Artist != "Metallica" || m.Title != "Sad But True" || m.Album != "" || m.DurationMs != 325000 {
		t.Fatalf("Metallica %+v", m)
	}
	for _, k := range []string{"youtube:vlogVideo01", "youtube:gameVideo01", "youtube:delVideo001"} {
		if _, ok := by[k]; ok {
			t.Fatalf("%s kept", k)
		}
	}
	if err := snap.Normalize(); err != nil {
		t.Fatal(err)
	}
}

func TestYouTubePlaylistsOrder(t *testing.T) {
	f := newFakeGoogle(t)
	snap := readYouTube(t, f, nil)
	road := playlistByName(snap, "Дорога")
	if road == nil || !road.Readable || road.ID != "PLroad" {
		t.Fatalf("Дорога %+v", road)
	}
	// both pages, in the playlist's order; the deleted video is left out
	if got := strings.Join(road.Entries, ","); got != "youtube:metSadTrue1,youtube:kinoKukush1,youtube:kinoBlood01,youtube:akvGorod001" {
		t.Fatalf("Дорога entries %s", got)
	}
	by := keyed(snap)
	if a := by["youtube:akvGorod001"]; a.Liked || a.Artist != "Аквариум" || a.Title != "Город золотой" {
		t.Fatalf("Город золотой %+v", a)
	}
	// a playlist of no music is no playlist; a playlist gone is unreadable
	if p := playlistByName(snap, "Влоги"); p != nil {
		t.Fatalf("Влоги %+v", p)
	}
	gone := playlistByName(snap, "Недоступный")
	if gone == nil || gone.Readable || len(gone.Entries) != 0 {
		t.Fatalf("Недоступный %+v", gone)
	}
	if len(snap.Playlists) != 2 {
		t.Fatalf("playlists %+v", snap.Playlists)
	}
	// each video's details asked once, ≤ 50 ids a request
	if n := f.requested("/youtube/v3/videos?"); n > 4 {
		t.Fatalf("videos.list %d times", n)
	}
}

func TestYouTubeQuotaExceeded(t *testing.T) {
	// the day's quota is gone: no wait, the read fails
	f := newFakeGoogle(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/playlistItems") && r.URL.Query().Get("pageToken") == "LLpage2" {
			f.file(w, "quota-exceeded.json", 403)
			return true
		}
		return false
	}
	sl := &sleeps{}
	if _, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, nil); !errors.Is(err, musicimport.ErrConnect) {
		t.Fatalf("quota: %v", err)
	}
	if len(sl.d) != 0 {
		t.Fatalf("waited %v", sl.d)
	}

	// the rate limit (403 rateLimitExceeded, 429 Retry-After): waits, then goes on
	f = newFakeGoogle(t)
	var once sync.Map
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		p := strings.TrimPrefix(r.URL.Path, "/youtube/v3")
		if _, done := once.LoadOrStore(p, true); done {
			return false
		}
		switch p {
		case "/channels":
			f.file(w, "rate-limit-exceeded.json", 403)
			return true
		case "/playlists":
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"code":429,"message":"Too many requests","errors":[{"reason":"rateLimitExceeded"}]}}`))
			return true
		}
		return false
	}
	sl = &sleeps{}
	snap := readYouTube(t, f, sl)
	if len(liked(snap)) != 3 || len(sl.d) != 2 || sl.d[0] < time.Second || sl.d[1] != 3*time.Second {
		t.Fatalf("liked %d, waits %v", len(liked(snap)), sl.d)
	}

	// a rate limit that does not pass gives up
	f = newFakeGoogle(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/channels") {
			f.file(w, "rate-limit-exceeded.json", 403)
			return true
		}
		return false
	}
	sl = &sleeps{}
	if _, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, nil); !errors.Is(err, musicimport.ErrConnect) || len(sl.d) != youtubeRetries {
		t.Fatalf("constant rate limit: %v, waits %v", err, sl.d)
	}

	// through the sign-in: the job fails with the text, the token goes
	f = newFakeGoogle(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/videos") {
			f.file(w, "quota-exceeded.json", 403)
			return true
		}
		return false
	}
	o, dir := youtubeOAuth(t, f)
	j := signInYouTube(t, o)
	if j.Status != playlistimport.StatusFailed || j.Error != "Не удалось подключиться к сервису. Попробуйте ещё раз." {
		t.Fatalf("job %+v", j)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("token files left %v", left)
	}
}

func liked(s playlistimport.Snapshot) []playlistimport.SnapTrack {
	var out []playlistimport.SnapTrack
	for _, t := range s.Tracks {
		if t.Liked {
			out = append(out, t)
		}
	}
	return out
}

func TestYouTubeLibraryListRefused(t *testing.T) {
	cases := []struct {
		name, path, page string
		code             int
		file, body       string
	}{
		{"channels 403", "/channels", "", 403, "forbidden.json", ""},
		{"channels without items", "/channels", "", 200, "", `{"kind":"youtube#channelListResponse"}`},
		{"likes 404 mid-pagination", "/playlistItems", "LLpage2", 404, "playlist-not-found.json", ""},
		{"likes without items", "/playlistItems", "0", 200, "", `{"pageInfo":{"totalResults":5}}`},
		{"playlists 403", "/playlists", "", 403, "forbidden.json", ""},
		{"videos 403", "/videos", "", 403, "forbidden.json", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeGoogle(t)
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				q := r.URL.Query()
				if strings.TrimPrefix(r.URL.Path, "/youtube/v3") != c.path {
					return false
				}
				if c.path == "/playlistItems" && (q.Get("playlistId") != "LL" || (q.Get("pageToken") != c.page && !(c.page == "0" && q.Get("pageToken") == ""))) {
					return false
				}
				if c.file != "" {
					f.file(w, c.file, c.code)
				} else {
					w.WriteHeader(c.code)
					w.Write([]byte(c.body))
				}
				return true
			}
			if _, err := f.reader(nil).Read(context.Background(), musicimport.Token{Access: f.access}, nil); !errors.Is(err, musicimport.ErrConnect) {
				t.Fatalf("read: %v", err)
			}
		})
	}
}

func TestYouTubeExpiredToken(t *testing.T) {
	f := newFakeGoogle(t)
	_, err := f.reader(nil).Read(context.Background(), musicimport.Token{Access: "ya29.expired-secret"}, nil)
	if !errors.Is(err, musicimport.ErrConnect) || strings.Contains(err.Error(), "ya29") {
		t.Fatalf("err %v", err)
	}
}

func TestYouTubeRetries5xx(t *testing.T) {
	f := newFakeGoogle(t)
	var n sync.Map
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if _, done := n.LoadOrStore(r.URL.Path, true); !done && strings.HasSuffix(r.URL.Path, "/playlists") {
			w.WriteHeader(503)
			return true
		}
		return false
	}
	sl := &sleeps{}
	readYouTube(t, f, sl)
	if len(sl.d) != 1 {
		t.Fatalf("waits %v", sl.d)
	}
	// a 5xx that does not pass: 2 waits, then the read fails
	f = newFakeGoogle(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/playlists") {
			w.WriteHeader(502)
			return true
		}
		return false
	}
	sl = &sleeps{}
	if _, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, nil); !errors.Is(err, musicimport.ErrConnect) || len(sl.d) != 2 {
		t.Fatalf("constant 502: %v waits %v", err, sl.d)
	}
}

func TestYouTubeSignIn(t *testing.T) {
	f := newFakeGoogle(t)
	y := f.reader(nil)
	u, err := url.Parse(y.AuthorizeURL("http://127.0.0.1:8765/vault-import/youtube", "st", "ch"))
	if err != nil || u.Scheme+"://"+u.Host+u.Path != "https://accounts.google.com/o/oauth2/v2/auth" {
		t.Fatalf("authorize %v %v", u, err)
	}
	want := url.Values{"client_id": {"client-g.apps.googleusercontent.com"}, "redirect_uri": {"http://127.0.0.1:8765/vault-import/youtube"},
		"response_type": {"code"}, "scope": {"https://www.googleapis.com/auth/youtube.readonly"}, "state": {"st"},
		"code_challenge": {"ch"}, "code_challenge_method": {"S256"}, "prompt": {"consent"}}
	if got := u.Query(); got.Encode() != want.Encode() {
		t.Fatalf("authorize query\n got %s\nwant %s", got.Encode(), want.Encode())
	}
	tok, err := y.Exchange(context.Background(), "http://127.0.0.1:8765/vault-import/youtube", "c0de", "verif")
	if err != nil || tok.Access != "ya29.fake-access-token" || tok.Refresh != "1//fake-refresh" || tok.Expiry.IsZero() {
		t.Fatalf("token %v %v", tok, err)
	}
	form := url.Values{"code": {"c0de"}, "client_id": {"client-g.apps.googleusercontent.com"}, "client_secret": {"GOCSPX-fake-secret"},
		"redirect_uri": {"http://127.0.0.1:8765/vault-import/youtube"}, "grant_type": {"authorization_code"}, "code_verifier": {"verif"}}
	if f.form.Encode() != form.Encode() {
		t.Fatalf("token form\n got %s\nwant %s", f.form.Encode(), form.Encode())
	}
	_, err = y.Exchange(context.Background(), "http://127.0.0.1:8765/vault-import/youtube", "bad", "verif")
	if err == nil || strings.Contains(err.Error(), "GOCSPX") || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("bad code: %v", err)
	}
}

func youtubeOAuth(t *testing.T, f *fakeGoogle) (*musicimport.OAuth, string) {
	dir := filepath.Join(t.TempDir(), "tokens")
	return &musicimport.OAuth{Svc: &playlistimport.Service{Store: newStore(t)}, Vault: &musicimport.Vault{Dir: dir, Key: bytes.Repeat([]byte{1}, 32)},
		Connectors: map[string]musicimport.Connector{playlistimport.ServiceYouTube: f.reader(&sleeps{})}}, dir
}

// signInYouTube signs in by the paste mode (a Desktop app client) and
// waits for the read.
func signInYouTube(t *testing.T, o *musicimport.OAuth) playlistimport.Job {
	t.Helper()
	ctx := context.Background()
	b, err := o.Begin(1, playlistimport.ServiceYouTube)
	if err != nil {
		t.Fatal(err)
	}
	if b.Mode != musicimport.ModePaste || b.RedirectURI != "http://127.0.0.1:8765/vault-import/youtube" {
		t.Fatalf("begin %+v", b)
	}
	u, _ := url.Parse(b.AuthorizeURL)
	back := b.RedirectURI + "?" + url.Values{"state": {u.Query().Get("state")}, "code": {"4/0Afake"},
		"scope": {"https://www.googleapis.com/auth/youtube.readonly"}}.Encode()
	id, err := o.FinishPasted(ctx, 1, back)
	if err != nil {
		t.Fatal(err)
	}
	o.Wait()
	j, err := o.Svc.Store.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestYouTubeTokenDeleted(t *testing.T) {
	for _, fail := range []bool{false, true} {
		f := newFakeGoogle(t)
		if fail {
			f.access = "another-token" // the API refuses the one the sign-in gave
		}
		o, dir := youtubeOAuth(t, f)
		j := signInYouTube(t, o)
		switch {
		case !fail && j.Status != playlistimport.StatusPreview:
			t.Fatalf("success: %+v", j)
		case fail && (j.Status != playlistimport.StatusFailed || j.Error != "Не удалось подключиться к сервису. Попробуйте ещё раз."):
			t.Fatalf("failure: %+v", j)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
			t.Fatalf("fail=%v: token files left %v", fail, left)
		}
	}
}

func TestYouTubeSongNames(t *testing.T) {
	cases := []struct{ title, channel, desc, artist, song, album string }{
		{"Кино - Группа крови", "Кино", "", "Кино", "Группа крови", ""},
		{"Земфира — Хочешь? (Official Video)", "ZEMFIRA", "", "Земфира", "Хочешь?", ""},
		{"Sad But True [Remastered]", "MetallicaVEVO", "", "Metallica", "Sad But True [Remastered]", ""},
		{"Перемен", "Кино - Topic", "", "Кино", "Перемен", ""},
		{"Город золотой", "Аквариум - Topic", "Provided to YouTube by X\r\n\r\nГород золотой · Аквариум\r\n\r\n℗ 1987 X", "Аквариум", "Город золотой", ""},
	}
	for _, c := range cases {
		a, s, al := songName(c.title, c.channel, c.desc)
		if a != c.artist || s != c.song || al != c.album {
			t.Errorf("%q / %q: %q %q %q", c.title, c.channel, a, s, al)
		}
	}
}

// fakeOEmbed answers YouTube's oEmbed for a few videos.
func fakeOEmbed(t *testing.T) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var asked []string
	f := &fakeGoogle{t: t}
	limited := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := url.Parse(r.URL.Query().Get("url"))
		if r.URL.Path != "/oembed" || r.URL.Query().Get("format") != "json" || err != nil || v.Host != "www.youtube.com" || v.Path != "/watch" {
			t.Errorf("unexpected %s", r.URL)
			w.WriteHeader(400)
			return
		}
		id := v.Query().Get("v")
		mu.Lock()
		asked = append(asked, id)
		first := !limited
		limited = true
		mu.Unlock()
		if first {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			return
		}
		switch id {
		case "kinoBlood01":
			f.file(w, "oembed-blood.json", 200)
		case "kinoKukush1":
			f.file(w, "oembed-kukushka.json", 200)
		case "privateVid1":
			w.WriteHeader(401)
			w.Write([]byte("Unauthorized"))
		default:
			w.WriteHeader(404)
			w.Write([]byte("Not Found"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func TestTakeoutResolvesUnknownIds(t *testing.T) {
	srv, asked := fakeOEmbed(t)
	var zb bytes.Buffer
	zw := newZip(&zb)
	zw.add("Takeout/YouTube and YouTube Music/playlists/Дорога-videos.csv",
		"Video ID,Playlist Video Creation Timestamp\nkinoBlood01,2024-01-01T00:00:00+00:00\nkinoKukush1,2024-01-02T00:00:00+00:00\n"+
			"privateVid1,2024-01-03T00:00:00+00:00\nbad/../id,2024-01-04T00:00:00+00:00\n")
	zw.close(t)
	snap, err := files.Parse("takeout.zip", zb.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if files.Unresolved(snap) != 4 {
		t.Fatalf("unresolved %d", files.Unresolved(snap))
	}
	r := NewYouTubeResolver()
	if r.Limiter == nil || r.Limiter.Interval != 500*time.Millisecond {
		t.Fatalf("limiter %+v (2 req/s)", r.Limiter)
	}
	sl := &sleeps{}
	r.OEmbedURL, r.HTTP, r.Limiter, r.Sleep = srv.URL+"/oembed", srv.Client(), nil, sl.sleep
	snap, err = files.Resolve(context.Background(), snap, r)
	if err != nil {
		t.Fatal(err)
	}
	by := keyed(snap)
	if b := by["kinoBlood01"]; b.Artist != "Кино" || b.Title != "Группа крови" {
		t.Fatalf("Группа крови %+v", b)
	}
	if k := by["kinoKukush1"]; k.Artist != "Кино" || k.Title != "Кукушка" {
		t.Fatalf("Кукушка %+v", k)
	}
	if files.Unresolved(snap) != 2 { // the private video and the id that is no id
		t.Fatalf("unresolved after %d", files.Unresolved(snap))
	}
	// the 429 waited out; a malformed id never asked for
	if len(sl.d) != 1 || sl.d[0] != 2*time.Second {
		t.Fatalf("waits %v", sl.d)
	}
	for _, id := range *asked {
		if id == "bad/../id" {
			t.Fatalf("asked for %q", id)
		}
	}
	if err := snap.Normalize(); err != nil || len(snap.Tracks) != 2 || len(snap.Playlists[0].Entries) != 2 {
		t.Fatalf("normalized %+v %v", snap, err)
	}
}

// the resolver with a token: videos.list, ≤ 50 ids a request, music only
func TestYouTubeResolverWithToken(t *testing.T) {
	f := newFakeGoogle(t)
	r := &YouTubeResolver{YouTube: f.reader(nil), Token: musicimport.Token{Access: f.access}}
	got, err := r.Resolve(context.Background(), []string{"kinoBlood01", "vlogVideo01", "metSadTrue1", "missingVid1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["kinoBlood01"].Album != "Группа крови" || got["metSadTrue1"].Title != "Sad But True" {
		t.Fatalf("resolved %+v", got)
	}
}

type testZip struct{ w *zip.Writer }

func newZip(b *bytes.Buffer) *testZip { return &testZip{zip.NewWriter(b)} }

func (z *testZip) add(name, body string) {
	w, _ := z.w.Create(name)
	w.Write([]byte(body))
}

func (z *testZip) close(t *testing.T) {
	if err := z.w.Close(); err != nil {
		t.Fatal(err)
	}
}

// oEmbedServer answers each id with answer(id): a status and a body.
func oEmbedServer(t *testing.T, answer func(id string, n int) (int, string, string)) (*YouTubeResolver, *sleeps, func() []string) {
	var mu sync.Mutex
	var asked []string
	tries := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, _ := url.Parse(r.URL.Query().Get("url"))
		id := v.Query().Get("v")
		mu.Lock()
		asked = append(asked, id)
		tries[id]++
		n := tries[id]
		mu.Unlock()
		code, retry, body := answer(id, n)
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(code)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	sl := &sleeps{}
	r := NewYouTubeResolver()
	r.OEmbedURL, r.HTTP, r.Limiter, r.Sleep = srv.URL+"/oembed", srv.Client(), nil, sl.sleep
	return r, sl, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(asked) }
}

func ytID(i int) string { return fmt.Sprintf("vid%08d", i) }

func TestYouTubeResolverFailures(t *testing.T) {
	ctx := context.Background()
	song := `{"title":"Кино - Кукушка","author_name":"Кино"}`
	// one video failing (4 × 503) stays unknown; the next is named
	r, sl, _ := oEmbedServer(t, func(id string, _ int) (int, string, string) {
		if id == ytID(0) {
			return 503, "", ""
		}
		return 200, "", song
	})
	got, err := r.Resolve(ctx, []string{ytID(0), ytID(1)})
	if err != nil || len(got) != 1 || got[ytID(1)].Title != "Кукушка" || len(sl.d) != oembedRetries {
		t.Fatalf("one failing: %v %v waits %v", got, err, sl.d)
	}
	// a Retry-After over a minute is not waited
	r, sl, _ = oEmbedServer(t, func(string, int) (int, string, string) { return 429, "120", "" })
	if _, err := r.Resolve(ctx, []string{ytID(0)}); err != nil || len(sl.d) != 0 {
		t.Fatalf("long Retry-After: %v waits %v", err, sl.d)
	}
	// YouTube refusing video after video: naming stops, what was named is kept
	r, _, asked := oEmbedServer(t, func(id string, _ int) (int, string, string) {
		if id == ytID(0) {
			return 200, "", song
		}
		return 429, "1", ""
	})
	ids := make([]string, 20)
	for i := range ids {
		ids[i] = ytID(i)
	}
	got, err = r.Resolve(ctx, ids)
	if err == nil || len(got) != 1 || len(asked()) != 1+oembedMaxFails*(oembedRetries+1) {
		t.Fatalf("refusing: %v %v, %d asked", got, err, len(asked()))
	}
	// a video that is no song (a vlog on a plain channel) stays unknown
	r, _, _ = oEmbedServer(t, func(id string, _ int) (int, string, string) {
		if id == ytID(0) {
			return 200, "", `{"title":"Мой день в Москве","author_name":"Блогер"}`
		}
		return 200, "", `{"title":"Sad But True","author_name":"MetallicaVEVO"}`
	})
	if got, err := r.Resolve(ctx, []string{ytID(0), ytID(1)}); err != nil || len(got) != 1 || got[ytID(1)].Artist != "Metallica" {
		t.Fatalf("vlog: %v %v", got, err)
	}
	// at most youtubeMaxResolve ids are asked for
	r, _, asked = oEmbedServer(t, func(string, int) (int, string, string) { return 404, "", "Not Found" })
	many := make([]string, youtubeMaxResolve+5)
	for i := range many {
		many[i] = ytID(i)
	}
	if _, err := r.Resolve(ctx, many); err != nil || len(asked()) != youtubeMaxResolve {
		t.Fatalf("cap: %v, %d asked", err, len(asked()))
	}
	// a cancelled naming stops at once with what it named
	cctx, cancel := context.WithCancel(ctx)
	r, _, asked = oEmbedServer(t, func(id string, _ int) (int, string, string) {
		if id == ytID(1) {
			cancel()
		}
		return 200, "", song
	})
	got, err = r.Resolve(cctx, ids)
	if !errors.Is(err, context.Canceled) || len(got) != 1 || len(asked()) != 2 {
		t.Fatalf("cancelled: %v %v, %d asked", got, err, len(asked()))
	}
}
