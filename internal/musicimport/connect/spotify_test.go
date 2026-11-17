//go:build ignore

package connect

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/auth"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
	"github.com/antresol-app/antresol-addon-torrents/internal/musicimport"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// fakeSpotify serves testdata/spotify as the Web API and the accounts
// server would: answers can be overridden per path (status, headers).
type fakeSpotify struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	paths  []string
	access string                                                   // the token the API accepts
	hook   func(w http.ResponseWriter, r *http.Request) (done bool) // nil: none
	form   url.Values                                               // the last token request
}

func newFakeSpotify(t *testing.T) *fakeSpotify {
	f := &fakeSpotify{t: t, access: "BQD-fake-access-token"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSpotify) file(w http.ResponseWriter, name string, code int) {
	b, err := os.ReadFile(filepath.Join("testdata", "spotify", name))
	if err != nil {
		f.t.Errorf("testdata %s: %v", name, err)
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

func (f *fakeSpotify) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path+"?"+r.URL.RawQuery)
	hook := f.hook
	f.mu.Unlock()
	if r.URL.Path == "/api/token" {
		r.ParseForm()
		f.mu.Lock()
		f.form = r.PostForm
		f.mu.Unlock()
		if r.PostForm.Get("code") == "bad" {
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant","error_description":"Invalid authorization code"}`))
			return
		}
		f.file(w, "token.json", 200)
		return
	}
	if hook != nil && hook(w, r) {
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+f.access {
		f.file(w, "expired.json", 401)
		return
	}
	q := r.URL.Query()
	off := q.Get("offset")
	if off == "" {
		off = "0"
	}
	switch p := r.URL.Path; {
	case p == "/v1/me":
		f.file(w, "me.json", 200)
	case p == "/v1/me/tracks":
		f.file(w, "tracks-"+off+".json", 200)
	case p == "/v1/me/albums":
		f.file(w, "albums-"+off+".json", 200)
	case p == "/v1/me/playlists":
		f.file(w, "playlists-"+off+".json", 200)
	case p == "/v1/playlists/road/items" || p == "/v1/playlists/shared/items":
		if q.Get("additional_types") != "track" {
			f.t.Errorf("items without additional_types: %s", r.URL)
		}
		f.file(w, "playlist-"+strings.Split(p, "/")[3]+"-"+off+".json", 200)
	case p == "/v1/playlists/friendmix/items":
		f.file(w, "forbidden.json", 403) // not the listener's: Development Mode refuses
	default:
		f.t.Errorf("unexpected %s", r.URL)
		w.WriteHeader(404)
	}
}

func (f *fakeSpotify) requested(prefix string) int {
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

// sleeps is a fake clock: the waits a reader asked for.
type sleeps struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.d = append(s.d, d)
	s.mu.Unlock()
	return nil
}

func (f *fakeSpotify) reader(sl *sleeps) *Spotify {
	sp := NewSpotify("client-1")
	sp.BaseURL, sp.AccountsURL, sp.HTTP = f.srv.URL, f.srv.URL, f.srv.Client()
	sp.Limiter = nil
	if sl != nil {
		sp.Sleep = sl.sleep
	}
	return sp
}

func readFake(t *testing.T, f *fakeSpotify, sl *sleeps) playlistimport.Snapshot {
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

func playlistByName(s playlistimport.Snapshot, name string) *playlistimport.SnapPlaylist {
	for i := range s.Playlists {
		if s.Playlists[i].Name == name {
			return &s.Playlists[i]
		}
	}
	return nil
}

func TestSpotifyReadsLibrary(t *testing.T) {
	f := newFakeSpotify(t)
	snap := readFake(t, f, nil)
	if snap.Service != playlistimport.ServiceSpotify || snap.Account != "acc-me" {
		t.Fatalf("service %q account %q", snap.Service, snap.Account)
	}
	if n := f.requested("/v1/me/tracks"); n != 3 {
		t.Fatalf("liked pages %d", n)
	}
	byKey := map[string]playlistimport.SnapTrack{}
	liked := 0
	for _, tr := range snap.Tracks {
		byKey[tr.Key] = tr
		if tr.Liked {
			liked++
		}
	}
	if liked != 120 {
		t.Fatalf("liked %d", liked)
	}
	t1 := byKey["spotify:t001"]
	if !t1.Liked || t1.Title != "Песня 1" || t1.Artist != "Аквариум" || t1.Album != "Радио Африка" || t1.AlbumArtist != "Аквариум" ||
		t1.ISRC != "RUA1D8800001" || t1.DurationMs != 181000 || t1.AddedAt == nil || !t1.AddedAt.Equal(time.Date(2026, 2, 2, 10, 1, 0, 0, time.UTC)) {
		t.Fatalf("t001 %+v", t1)
	}
	if tr := byKey["spotify:t010"]; tr.Artist != "Metallica" { // the first credited artist
		t.Fatalf("t010 artist %q", tr.Artist)
	}
	if tr := byKey["spotify:t120"]; !tr.Liked || tr.ISRC != "RUA1D8800120" {
		t.Fatalf("t120 %+v", tr)
	}

	if len(snap.Albums) != 2 || snap.Albums[0].Key != "spotify:album:alb1" || snap.Albums[0].Title != "Группа крови" ||
		snap.Albums[0].Artist != "Кино" || snap.Albums[0].Year != 1988 || snap.Albums[1].Year != 1991 || snap.Albums[0].AddedAt == nil {
		t.Fatalf("albums %+v", snap.Albums)
	}

	road := playlistByName(snap, "Дорога")
	// the local file and the episode are left out; the order is the service's
	if road == nil || !road.Readable || road.ID != "road" || strings.Join(road.Entries, ",") != "spotify:t003,spotify:t001,spotify:p007" {
		t.Fatalf("Дорога %+v", road)
	}
	if p7, ok := byKey["spotify:p007"]; !ok || p7.Liked || p7.Title != "Песня 7" {
		t.Fatalf("a playlist-only song %+v", p7)
	}
	shared := playlistByName(snap, "Общий")
	if shared == nil || !shared.Readable || strings.Join(shared.Entries, ",") != "spotify:t005,spotify:s008" {
		t.Fatalf("Общий %+v", shared)
	}
	if err := snap.Normalize(); err != nil || len(snap.Tracks) != 122 {
		t.Fatalf("normalize %v, %d songs", err, len(snap.Tracks))
	}
}

func TestSpotifyFollowedPlaylistUnreadable(t *testing.T) {
	f := newFakeSpotify(t)
	snap := readFake(t, f, nil)
	for _, name := range []string{"Чужой плейлист", "Микс друга"} {
		p := playlistByName(snap, name)
		if p == nil || p.Readable || len(p.Entries) != 0 {
			t.Fatalf("%s %+v", name, p)
		}
	}
	// a playlist listed without items is not asked for
	if n := f.requested("/v1/playlists/followed"); n != 0 {
		t.Fatalf("followed asked %d", n)
	}

	// the preview shows both unticked
	st := newStore(t)
	id, err := st.CreateJob(context.Background(), 1, snap)
	if err != nil {
		t.Fatal(err)
	}
	pls, err := st.Playlists(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pls {
		unreadable := p.Name == "Чужой плейлист" || p.Name == "Микс друга"
		if p.Readable == unreadable || p.Selected == unreadable {
			t.Fatalf("%+v", p)
		}
	}
}

func TestSpotify429RetryAfter(t *testing.T) {
	f := newFakeSpotify(t)
	var once sync.Once
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		hit := false
		if r.URL.Path == "/v1/me/tracks" && r.URL.Query().Get("offset") == "50" {
			once.Do(func() { hit = true })
		}
		if hit {
			w.Header().Set("Retry-After", "3")
			f.file(w, "rate-limited.json", 429)
		}
		return hit
	}
	sl := &sleeps{}
	snap := readFake(t, f, sl)
	if len(sl.d) != 1 || sl.d[0] != 3*time.Second {
		t.Fatalf("waited %v", sl.d)
	}
	if len(snap.Tracks) < 120 {
		t.Fatalf("songs %d", len(snap.Tracks))
	}

	// longer than a minute: give up
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Retry-After", "120")
		f.file(w, "rate-limited.json", 429)
		return true
	}
	sl = &sleeps{}
	_, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, nil)
	if !errors.Is(err, musicimport.ErrConnect) || len(sl.d) != 0 {
		t.Fatalf("%v, waited %v", err, sl.d)
	}

	// the quota is used up: waiting does not help
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"status":429,"message":"quota","reason":"QUOTA_EXCEEDED"}}`))
		return true
	}
	if _, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, nil); !errors.Is(err, musicimport.ErrConnect) || len(sl.d) != 0 {
		t.Fatalf("quota: %v, waited %v", err, sl.d)
	}
}

func TestSpotifyExpiredToken(t *testing.T) {
	f := newFakeSpotify(t)
	// the token expires after the first page of liked songs
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1/me/tracks" && r.URL.Query().Get("offset") == "100" {
			f.file(w, "expired.json", 401)
			return true
		}
		return false
	}
	_, err := f.reader(nil).Read(context.Background(), musicimport.Token{Access: f.access}, nil)
	if !errors.Is(err, musicimport.ErrConnect) || strings.Contains(err.Error(), f.access) {
		t.Fatalf("%v", err)
	}
}

func TestSpotifySignIn(t *testing.T) {
	f := newFakeSpotify(t)
	sp := f.reader(nil)
	u, err := url.Parse(sp.AuthorizeURL("http://127.0.0.1:8765/vault-import/spotify", "st", "ch"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/authorize" || q.Get("client_id") != "client-1" || q.Get("response_type") != "code" ||
		q.Get("redirect_uri") != "http://127.0.0.1:8765/vault-import/spotify" || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") != "ch" || q.Get("state") != "st" ||
		q.Get("scope") != "user-library-read playlist-read-private playlist-read-collaborative" {
		t.Fatalf("authorize %s", u)
	}
	tok, err := sp.Exchange(context.Background(), "http://127.0.0.1:8765/vault-import/spotify", "good", "verifier-1")
	if err != nil || tok.Access != "BQD-fake-access-token" || tok.Refresh != "AQD-fake-refresh-token" || time.Until(tok.Expiry) < 50*time.Minute {
		t.Fatalf("%+v %v", tok, err)
	}
	if f.form.Get("grant_type") != "authorization_code" || f.form.Get("code") != "good" || f.form.Get("client_id") != "client-1" ||
		f.form.Get("code_verifier") != "verifier-1" || f.form.Get("redirect_uri") != "http://127.0.0.1:8765/vault-import/spotify" ||
		f.form.Has("client_secret") {
		t.Fatalf("form %v", f.form)
	}
	if _, err := sp.Exchange(context.Background(), "x", "bad", "v"); err == nil {
		t.Fatal("bad code accepted")
	}
}

func newStore(t *testing.T) *playlistimport.Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := auth.NewStore(d).CreateFirstAdmin(context.Background(), "Папа", "secret"); err != nil {
		t.Fatal(err)
	}
	return &playlistimport.Store{DB: d}
}

func TestSpotifyTokenDeletedAfterRead(t *testing.T) {
	ctx := context.Background()
	for _, fail := range []bool{false, true} {
		f := newFakeSpotify(t)
		if fail {
			f.access = "another-token" // the API refuses the one the sign-in gave
		}
		dir := filepath.Join(t.TempDir(), "tokens")
		o := &musicimport.OAuth{Svc: &playlistimport.Service{Store: newStore(t)}, Vault: &musicimport.Vault{Dir: dir, Key: bytes.Repeat([]byte{1}, 32)},
			Connectors: map[string]musicimport.Connector{playlistimport.ServiceSpotify: f.reader(nil)}, PublicURL: "https://vault.example.org"}
		b, err := o.Begin(1, playlistimport.ServiceSpotify)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(b.AuthorizeURL)
		id, err := o.Finish(ctx, 1, u.Query().Get("state"), "good")
		if err != nil {
			t.Fatal(err)
		}
		o.Wait()
		j, err := o.Svc.Store.Job(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case !fail && j.Status != playlistimport.StatusPreview:
			t.Fatalf("success: %+v", j)
		case fail && (j.Status != playlistimport.StatusFailed || j.Error != "Не удалось подключиться к сервису. Попробуйте ещё раз."):
			t.Fatalf("failure: %+v", j)
		}
		left, _ := filepath.Glob(filepath.Join(dir, "*"))
		if len(left) != 0 {
			t.Fatalf("fail=%v: token files left %v", fail, left)
		}
	}
}

func TestSpotifyLibraryListRefused(t *testing.T) {
	cases := []struct {
		name string
		path string
		off  string
		code int
		body string
	}{
		{"liked 403 mid-pagination", "/v1/me/tracks", "50", 403, `{"error":{"status":403,"message":"Forbidden"}}`},
		{"liked without items", "/v1/me/tracks", "0", 200, `{"total":5,"offset":0,"limit":50}`},
		{"albums 404", "/v1/me/albums", "0", 404, `{"error":{"status":404,"message":"Not found"}}`},
		{"playlists without items", "/v1/me/playlists", "0", 200, `{"total":4}`},
		{"playlists 403", "/v1/me/playlists", "0", 403, `{"error":{"status":403,"message":"Forbidden"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeSpotify(t)
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				off := r.URL.Query().Get("offset")
				if r.URL.Path != c.path || off != c.off {
					return false
				}
				w.WriteHeader(c.code)
				w.Write([]byte(c.body))
				return true
			}
			if _, err := f.reader(nil).Read(context.Background(), musicimport.Token{Access: f.access}, nil); !errors.Is(err, musicimport.ErrConnect) {
				t.Fatalf("read: %v", err)
			}
		})
	}

	// through the sign-in: the job fails with the text, the token goes
	ctx := context.Background()
	f := newFakeSpotify(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v1/me/tracks" && r.URL.Query().Get("offset") == "50" {
			f.file(w, "forbidden.json", 403)
			return true
		}
		return false
	}
	dir := filepath.Join(t.TempDir(), "tokens")
	o := &musicimport.OAuth{Svc: &playlistimport.Service{Store: newStore(t)}, Vault: &musicimport.Vault{Dir: dir, Key: bytes.Repeat([]byte{1}, 32)},
		Connectors: map[string]musicimport.Connector{playlistimport.ServiceSpotify: f.reader(nil)}, PublicURL: "https://vault.example.org"}
	b, _ := o.Begin(1, playlistimport.ServiceSpotify)
	u, _ := url.Parse(b.AuthorizeURL)
	id, err := o.Finish(ctx, 1, u.Query().Get("state"), "good")
	if err != nil {
		t.Fatal(err)
	}
	o.Wait()
	j, _ := o.Svc.Store.Job(ctx, id)
	if j.Status != playlistimport.StatusFailed || j.Error != "Не удалось подключиться к сервису. Попробуйте ещё раз." {
		t.Fatalf("%+v", j)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("token left %v", left)
	}
}

func TestSpotifyPartialPlaylistLeavesNoSongs(t *testing.T) {
	f := newFakeSpotify(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1/playlists/road/items" {
			return false
		}
		if r.URL.Query().Get("offset") == "0" {
			w.Write([]byte(`{"total":60,"items":[{"added_at":"2026-04-01T10:00:00Z","is_local":false,"item":{"id":"orph1","type":"track","name":"Сирота","artists":[{"name":"Кино"}],"album":{"name":"А"}}}]}`))
		} else {
			f.file(w, "forbidden.json", 403)
		}
		return true
	}
	snap := readFake(t, f, nil)
	if p := playlistByName(snap, "Дорога"); p == nil || p.Readable || len(p.Entries) != 0 {
		t.Fatalf("Дорога %+v", p)
	}
	for _, tr := range snap.Tracks {
		if tr.Key == "spotify:orph1" {
			t.Fatal("a song of the unreadable playlist was kept")
		}
	}
}

func TestSpotifyRetries5xx(t *testing.T) {
	f := newFakeSpotify(t)
	var once sync.Once
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		hit := false
		if r.URL.Path == "/v1/me/albums" {
			once.Do(func() { hit = true })
		}
		if hit {
			w.WriteHeader(503)
		}
		return hit
	}
	sl := &sleeps{}
	if snap := readFake(t, f, sl); len(snap.Albums) != 2 || len(sl.d) != 1 {
		t.Fatalf("albums %d, waited %v", len(snap.Albums), sl.d)
	}
	f.hook = func(w http.ResponseWriter, r *http.Request) bool { w.WriteHeader(502); return true }
	sl = &sleeps{}
	if _, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, nil); !errors.Is(err, musicimport.ErrConnect) || len(sl.d) != 2 {
		t.Fatalf("%v, waited %v", err, sl.d)
	}
}
