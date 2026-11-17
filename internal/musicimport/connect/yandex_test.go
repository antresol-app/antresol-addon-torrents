//go:build ignore

package connect

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
	"github.com/antresol-app/antresol-addon-torrents/internal/musicimport"
)

// fakeYandex serves testdata/yandex as api.music.yandex.net and
// oauth.yandex.ru would (the shapes MarshalX/yandex-music-api documents).
type fakeYandex struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	paths    []string
	batches  [][]string // the track-ids of every POST /tracks
	access   string     // the token the API accepts
	pending  int        // token polls answered authorization_pending first
	expired  bool       // token polls answer expired_token
	polls    int
	seq      []string   // what the next token polls answer first (slow_down, 429, 503, access_denied)
	verify   string     // overrides verification_url
	codeForm url.Values // the last device code request
	pollForm url.Values // the last token poll
	hook     func(w http.ResponseWriter, r *http.Request) (done bool)
}

func newFakeYandex(t *testing.T) *fakeYandex {
	f := &fakeYandex{t: t, access: "y0_AgAAAAfake-access-token"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeYandex) file(w http.ResponseWriter, name string, code int) {
	b, err := os.ReadFile(filepath.Join("testdata", "yandex", name))
	if err != nil {
		f.t.Errorf("testdata %s: %v", name, err)
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(b)
}

func (f *fakeYandex) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	hook := f.hook
	f.mu.Unlock()
	switch r.URL.Path {
	case "/device/code":
		r.ParseForm()
		f.mu.Lock()
		f.codeForm = r.PostForm
		verify := f.verify
		f.mu.Unlock()
		if verify != "" {
			w.Write([]byte(`{"device_code":"dc","user_code":"ABCD1234","verification_url":"` + verify + `","interval":5,"expires_in":300}`))
			return
		}
		f.file(w, "device-code.json", 200)
		return
	case "/token":
		r.ParseForm()
		f.mu.Lock()
		f.pollForm = r.PostForm
		f.polls++
		n, pending, expired := f.polls, f.pending, f.expired
		next := ""
		if len(f.seq) > 0 {
			next, f.seq = f.seq[0], f.seq[1:]
		}
		f.mu.Unlock()
		switch next {
		case "slow_down", "access_denied":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"` + next + `","error_description":"…"}`))
			return
		case "pending":
			f.file(w, "authorization-pending.json", 400)
			return
		case "429":
			w.WriteHeader(429)
			return
		case "503":
			w.WriteHeader(503)
			return
		}
		switch {
		case expired:
			f.file(w, "expired-token.json", 400)
		case n <= pending:
			f.file(w, "authorization-pending.json", 400)
		default:
			f.file(w, "token.json", 200)
		}
		return
	}
	if hook != nil && hook(w, r) {
		return
	}
	if r.Header.Get("Authorization") != "OAuth "+f.access {
		f.file(w, "session-expired.json", 401)
		return
	}
	switch p := r.URL.Path; {
	case p == "/account/status":
		f.file(w, "account-status.json", 200)
	case p == "/users/1130000/likes/tracks":
		f.file(w, "likes-tracks.json", 200)
	case p == "/users/1130000/likes/albums":
		if r.URL.Query().Get("rich") != "true" {
			f.t.Errorf("albums without rich: %s", r.URL)
		}
		f.file(w, "likes-albums.json", 200)
	case p == "/users/1130000/playlists/list":
		f.file(w, "playlists-list.json", 200)
	case p == "/users/1130000/likes/playlists":
		f.file(w, "likes-playlists.json", 200)
	case p == "/users/1130000/playlists/1001":
		f.file(w, "playlist-1130000-1001.json", 200)
	case p == "/users/2220000/playlists/3001":
		f.file(w, "playlist-2220000-3001.json", 200)
	case p == "/users/1130000/playlists/1002":
		f.file(w, "not-found.json", 404)
	case p == "/tracks" && r.Method == http.MethodPost:
		f.tracks(w, r)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
		w.WriteHeader(404)
	}
}

// tracks answers POST /tracks with the asked songs of testdata's catalog.
func (f *fakeYandex) tracks(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	ids := strings.Split(r.PostForm.Get("track-ids"), ",")
	f.mu.Lock()
	f.batches = append(f.batches, ids)
	f.mu.Unlock()
	b, _ := os.ReadFile(filepath.Join("testdata", "yandex", "tracks.json"))
	var all struct {
		Result []map[string]any `json:"result"`
	}
	json.Unmarshal(b, &all)
	by := map[string]map[string]any{}
	for _, t := range all.Result {
		by[jsonString(t["id"])] = t
	}
	out := []map[string]any{}
	for _, id := range ids {
		id, _, _ = strings.Cut(id, ":")
		if t, ok := by[id]; ok {
			out = append(out, t)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"invocationInfo": map[string]any{"req-id": "1"}, "result": out})
}

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

func (f *fakeYandex) reader(sl *sleeps) *Yandex {
	y := NewYandex("yandex-client", "yandex-secret")
	y.BaseURL, y.OAuthURL, y.HTTP = f.srv.URL, f.srv.URL, f.srv.Client()
	y.Limiter = nil
	if sl != nil {
		y.Sleep = sl.sleep
	}
	return y
}

func (f *fakeYandex) requested(prefix string) int {
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

func readYandex(t *testing.T, f *fakeYandex, sl *sleeps) playlistimport.Snapshot {
	t.Helper()
	snap, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return snap
}

func trackByKey(s playlistimport.Snapshot, key string) *playlistimport.SnapTrack {
	for i := range s.Tracks {
		if s.Tracks[i].Key == key {
			return &s.Tracks[i]
		}
	}
	return nil
}

func TestYandexReadsLibrary(t *testing.T) {
	f := newFakeYandex(t)
	snap := readYandex(t, f, nil)
	if snap.Service != playlistimport.ServiceYandex || snap.Account != "1130000" {
		t.Fatalf("service %q account %q", snap.Service, snap.Account)
	}

	// 240 likes: their details in 3 calls of at most 100, as id:albumId
	f.mu.Lock()
	batches := f.batches
	f.mu.Unlock()
	if len(batches) < 3 {
		t.Fatalf("POST /tracks %d times", len(batches))
	}
	liked := 0
	for _, b := range batches[:3] {
		if len(b) > 100 {
			t.Fatalf("a batch of %d", len(b))
		}
		liked += len(b)
	}
	if liked != 240 || batches[0][0] != "1001:7001" || batches[0][1] != "1002:5001" {
		t.Fatalf("liked batches %d, first %v", liked, batches[0][:2])
	}
	n := 0
	for _, tr := range snap.Tracks {
		if tr.Liked {
			n++
		}
	}
	if n != 240 {
		t.Fatalf("liked %d", n)
	}

	// title + version, the album and its artist, the date liked
	song := trackByKey(snap, "yandex:1001")
	if song == nil || song.Title != "Song (Live)" || song.Artist != "Metallica" || song.Album != "S&M" || song.AlbumArtist != "Metallica" ||
		song.DurationMs != 180001 || !song.Liked || song.AddedAt == nil || !song.AddedAt.Equal(time.Date(2026, 9, 1, 18, 30, 0, 0, time.UTC)) {
		t.Fatalf("Song %+v", song)
	}
	// an id the API gives as a number
	if tr := trackByKey(snap, "yandex:1002"); tr == nil || tr.Title != "Песня 2" || tr.Artist != "Аквариум" || tr.Album != "Радио Африка" {
		t.Fatalf("1002 %+v", tr)
	}

	// liked albums with their dates
	if len(snap.Albums) != 2 {
		t.Fatalf("albums %+v", snap.Albums)
	}
	a := snap.Albums[0]
	if a.Key != "yandex:album:5000" || a.Artist != "Кино" || a.Title != "Группа крови" || a.Year != 1988 || a.AddedAt == nil ||
		!a.AddedAt.Equal(time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("album %+v", a)
	}
	if b := snap.Albums[1]; b.Title != "Чёрный альбом" || b.Year != 1990 || !b.AddedAt.Equal(time.Date(2024, 12, 24, 8, 15, 0, 0, time.UTC)) {
		t.Fatalf("album %+v", b)
	}

	// playlists: own in order (a song without its body is fetched), the
	// deleted one unreadable, the liked one of another listener read
	// through its owner
	road := playlistByName(snap, "Дорога")
	if road == nil || !road.Readable || strings.Join(road.Entries, ",") != "yandex:1003,yandex:9001,yandex:1001" {
		t.Fatalf("Дорога %+v", road)
	}
	if star := trackByKey(snap, "yandex:9001"); star == nil || star.Title != "Звезда по имени Солнце" || star.Liked {
		t.Fatalf("9001 %+v", star)
	}
	if gone := playlistByName(snap, "Удалённый"); gone == nil || gone.Readable || len(gone.Entries) != 0 {
		t.Fatalf("Удалённый %+v", gone)
	}
	mix := playlistByName(snap, "Чужой микс")
	if mix == nil || !mix.Readable || strings.Join(mix.Entries, ",") != "yandex:9002" || mix.ID != "2220000:3001" {
		t.Fatalf("Чужой микс %+v", mix)
	}
	if road.ID != "1130000:1001" {
		t.Fatalf("road id %q", road.ID)
	}
	if err := snap.Normalize(); err != nil {
		t.Fatal(err)
	}
}

func TestYandexLibraryListRefused(t *testing.T) {
	for _, path := range []string{"/account/status", "/users/1130000/likes/tracks", "/users/1130000/likes/albums", "/users/1130000/playlists/list",
		"/users/1130000/likes/playlists", "/tracks"} {
		t.Run(path, func(t *testing.T) {
			f := newFakeYandex(t)
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != path {
					return false
				}
				f.file(w, "not-found.json", 403)
				return true
			}
			_, err := f.reader(nil).Read(context.Background(), musicimport.Token{Access: f.access}, nil)
			if !errors.Is(err, musicimport.ErrConnect) {
				t.Fatalf("read: %v", err)
			}
		})
	}
}

func TestYandexBacksOff(t *testing.T) {
	f := newFakeYandex(t)
	hits := 0
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/users/1130000/likes/albums" {
			return false
		}
		hits++
		if hits <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return false
	}
	sl := &sleeps{}
	readYandex(t, f, sl)
	if len(sl.d) != 2 || sl.d[0] != 5*time.Second || sl.d[1] != 5*time.Second {
		t.Fatalf("waits %v", sl.d)
	}

	// always 503: 3 waits, then the read fails
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/account/status" {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	sl = &sleeps{}
	if _, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, nil); !errors.Is(err, musicimport.ErrConnect) {
		t.Fatalf("read: %v", err)
	}
	if len(sl.d) != 3 {
		t.Fatalf("waits %v", sl.d)
	}
}

func TestYandexExpiredToken(t *testing.T) {
	f := newFakeYandex(t)
	_, err := f.reader(nil).Read(context.Background(), musicimport.Token{Access: "y0_revoked-secret"}, nil)
	if !errors.Is(err, musicimport.ErrConnect) || strings.Contains(err.Error(), "y0_revoked-secret") {
		t.Fatalf("read: %v", err)
	}
}

func yandexOAuth(t *testing.T, f *fakeYandex, dir string, now *time.Time) *musicimport.OAuth {
	o := &musicimport.OAuth{Svc: &playlistimport.Service{Store: newStore(t)}, Vault: &musicimport.Vault{Dir: dir, Key: bytes.Repeat([]byte{1}, 32)},
		Devices: map[string]musicimport.DeviceConnector{playlistimport.ServiceYandex: f.reader(nil)}}
	if now != nil {
		o.Now = func() time.Time { return *now }
	}
	return o
}

func TestYandexDeviceFlow(t *testing.T) {
	ctx := context.Background()
	f := newFakeYandex(t)
	f.pending = 2
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "tokens")
	o := yandexOAuth(t, f, dir, &now)
	if !o.Configured(playlistimport.ServiceYandex) {
		t.Fatal("not configured")
	}
	d, err := o.BeginDevice(ctx, 1, playlistimport.ServiceYandex)
	if err != nil {
		t.Fatal(err)
	}
	if d.UserCode != "ABCD1234" || d.VerificationURL != "https://ya.ru/device" || d.Interval != 5 || d.ExpiresIn != 300 || len(d.PollID) < 32 {
		t.Fatalf("device %+v", d)
	}
	if f.codeForm.Get("client_id") != "yandex-client" || f.codeForm.Get("device_id") == "" || f.codeForm.Get("device_name") == "" ||
		f.codeForm.Has("client_secret") {
		t.Fatalf("device code form %v", f.codeForm)
	}
	// the device code never leaves the server
	if b, _ := json.Marshal(d); strings.Contains(string(b), "4c5b9b0a2d1e4f7a9c3b5d6e7f8a9b0c") {
		t.Fatalf("device code in %s", b)
	}

	// another admin's poll reads as expired and leaves the sign-in alone
	if _, err := o.Poll(ctx, 2, playlistimport.ServiceYandex, d.PollID); !errors.Is(err, musicimport.ErrLinkExpired) {
		t.Fatalf("other admin: %v", err)
	}
	// pending twice; a poll sooner than the interval does not ask Yandex
	for i := 0; i < 2; i++ {
		if _, err := o.Poll(ctx, 1, playlistimport.ServiceYandex, d.PollID); !errors.Is(err, musicimport.ErrPending) {
			t.Fatalf("poll %d: %v", i, err)
		}
		if _, err := o.Poll(ctx, 1, playlistimport.ServiceYandex, d.PollID); !errors.Is(err, musicimport.ErrPending) {
			t.Fatalf("early poll %d: %v", i, err)
		}
		now = now.Add(5 * time.Second)
	}
	if f.polls != 2 {
		t.Fatalf("Yandex polled %d times", f.polls)
	}
	id, err := o.Poll(ctx, 1, playlistimport.ServiceYandex, d.PollID)
	if err != nil {
		t.Fatal(err)
	}
	if p := f.pollForm; p.Get("grant_type") != "device_code" || p.Get("code") != "4c5b9b0a2d1e4f7a9c3b5d6e7f8a9b0c" ||
		p.Get("client_id") != "yandex-client" || p.Get("client_secret") != "yandex-secret" {
		t.Fatalf("poll form %v", p)
	}
	j, err := o.Svc.Store.Job(ctx, id)
	if err != nil || (j.Status != playlistimport.StatusReading && j.Status != playlistimport.StatusPreview) || j.Service != playlistimport.ServiceYandex {
		t.Fatalf("job %+v %v", j, err)
	}
	o.Wait()
	if j, _ = o.Svc.Store.Job(ctx, id); j.Status != playlistimport.StatusPreview {
		t.Fatalf("after read %+v", j)
	}
	// single use: the same poll id is gone
	now = now.Add(5 * time.Second)
	if _, err := o.Poll(ctx, 1, playlistimport.ServiceYandex, d.PollID); !errors.Is(err, musicimport.ErrLinkExpired) {
		t.Fatalf("reuse: %v", err)
	}

	// Yandex says the code expired
	f2 := newFakeYandex(t)
	f2.expired = true
	o2 := yandexOAuth(t, f2, filepath.Join(t.TempDir(), "tokens"), nil)
	d2, err := o2.BeginDevice(ctx, 1, playlistimport.ServiceYandex)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o2.Poll(ctx, 1, playlistimport.ServiceYandex, d2.PollID); !errors.Is(err, musicimport.ErrLinkExpired) ||
		err.Error() != "Ссылка устарела. Подключитесь заново." {
		t.Fatalf("expired: %v", err)
	}
	// the code ran out here too: no further polls reach Yandex
	if _, err := o2.Poll(ctx, 1, playlistimport.ServiceYandex, d2.PollID); !errors.Is(err, musicimport.ErrLinkExpired) || f2.polls != 1 {
		t.Fatalf("after expiry: %v, %d polls", err, f2.polls)
	}

	// past expires_in the server does not ask either
	f3 := newFakeYandex(t)
	f3.pending = 100
	now3 := now
	o3 := yandexOAuth(t, f3, filepath.Join(t.TempDir(), "tokens"), &now3)
	d3, _ := o3.BeginDevice(ctx, 1, playlistimport.ServiceYandex)
	now3 = now3.Add(301 * time.Second)
	if _, err := o3.Poll(ctx, 1, playlistimport.ServiceYandex, d3.PollID); !errors.Is(err, musicimport.ErrLinkExpired) || f3.polls != 0 {
		t.Fatalf("timed out: %v, %d polls", err, f3.polls)
	}
}

func TestYandexTokenPaste(t *testing.T) {
	ctx := context.Background()
	f := newFakeYandex(t)
	o := yandexOAuth(t, f, filepath.Join(t.TempDir(), "tokens"), nil)
	// copied with its scheme word and spaces
	id, err := o.FinishToken(ctx, 1, playlistimport.ServiceYandex, "  OAuth "+f.access+"\n")
	if err != nil {
		t.Fatal(err)
	}
	o.Wait()
	j, _ := o.Svc.Store.Job(ctx, id)
	if j.Status != playlistimport.StatusPreview || j.Service != playlistimport.ServiceYandex {
		t.Fatalf("%+v", j)
	}
	for _, bad := range []string{"", "   ", "two words", strings.Repeat("a", 2000)} {
		if _, err := o.FinishToken(ctx, 1, playlistimport.ServiceYandex, bad); !errors.Is(err, musicimport.ErrConnect) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if _, err := o.FinishToken(ctx, 1, playlistimport.ServiceSpotify, f.access); !errors.Is(err, musicimport.ErrNotConfigured) {
		t.Fatalf("spotify: %v", err)
	}
}

func TestYandexTokenDeleted(t *testing.T) {
	ctx := context.Background()
	for _, fail := range []bool{false, true} {
		f := newFakeYandex(t)
		if fail {
			f.access = "another-token" // the API refuses the pasted one
		}
		dir := filepath.Join(t.TempDir(), "tokens")
		o := yandexOAuth(t, f, dir, nil)
		id, err := o.FinishToken(ctx, 1, playlistimport.ServiceYandex, "y0_AgAAAAfake-access-token")
		if err != nil {
			t.Fatal(err)
		}
		o.Wait()
		j, _ := o.Svc.Store.Job(ctx, id)
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
	// through the device code as well
	f := newFakeYandex(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/users/1130000/likes/tracks" {
			f.file(w, "session-expired.json", 401)
			return true
		}
		return false
	}
	dir := filepath.Join(t.TempDir(), "tokens")
	o := yandexOAuth(t, f, dir, nil)
	d, _ := o.BeginDevice(ctx, 1, playlistimport.ServiceYandex)
	id, err := o.Poll(ctx, 1, playlistimport.ServiceYandex, d.PollID)
	if err != nil {
		t.Fatal(err)
	}
	o.Wait()
	if j, _ := o.Svc.Store.Job(ctx, id); j.Status != playlistimport.StatusFailed {
		t.Fatalf("%+v", j)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("token left %v", left)
	}
}

// RFC 8628 §3.5: slow_down (and a 429 from /token) adds 5 s to the
// interval for the rest of the sign-in.
func TestYandexSlowDown(t *testing.T) {
	for _, answer := range []string{"slow_down", "429"} {
		t.Run(answer, func(t *testing.T) {
			ctx := context.Background()
			f := newFakeYandex(t)
			f.seq = []string{answer, "pending"}
			f.pending = 0
			now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
			o := yandexOAuth(t, f, filepath.Join(t.TempDir(), "tokens"), &now)
			d, err := o.BeginDevice(ctx, 1, playlistimport.ServiceYandex)
			if err != nil {
				t.Fatal(err)
			}
			poll := func() error { _, err := o.Poll(ctx, 1, playlistimport.ServiceYandex, d.PollID); return err }
			if err := poll(); !errors.Is(err, musicimport.ErrPending) || f.polls != 1 {
				t.Fatalf("first: %v, %d polls", err, f.polls)
			}
			// the old 5 s is not enough any more
			now = now.Add(5 * time.Second)
			if err := poll(); !errors.Is(err, musicimport.ErrPending) || f.polls != 1 {
				t.Fatalf("at the old interval: %v, %d polls", err, f.polls)
			}
			now = now.Add(5 * time.Second)
			if err := poll(); !errors.Is(err, musicimport.ErrPending) || f.polls != 2 {
				t.Fatalf("at 10 s: %v, %d polls", err, f.polls)
			}
			// and it stays 10 s for the rest of the sign-in
			now = now.Add(5 * time.Second)
			if err := poll(); !errors.Is(err, musicimport.ErrPending) || f.polls != 2 {
				t.Fatalf("5 s later: %v, %d polls", err, f.polls)
			}
			now = now.Add(5 * time.Second)
			if err := poll(); err != nil || f.polls != 3 {
				t.Fatalf("signed in: %v, %d polls", err, f.polls)
			}
			o.Wait()
		})
	}
}

func TestYandexPollFailures(t *testing.T) {
	ctx := context.Background()
	// a 503 is passing: the sign-in goes on
	f := newFakeYandex(t)
	f.seq = []string{"503"}
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	o := yandexOAuth(t, f, filepath.Join(t.TempDir(), "tokens"), &now)
	d, _ := o.BeginDevice(ctx, 1, playlistimport.ServiceYandex)
	if _, err := o.Poll(ctx, 1, playlistimport.ServiceYandex, d.PollID); !errors.Is(err, musicimport.ErrPending) {
		t.Fatalf("503: %v", err)
	}
	now = now.Add(5 * time.Second)
	if _, err := o.Poll(ctx, 1, playlistimport.ServiceYandex, d.PollID); err != nil {
		t.Fatalf("after 503: %v", err)
	}
	o.Wait()

	// access_denied: refused, and the sign-in is used up
	f = newFakeYandex(t)
	f.seq = []string{"access_denied"}
	o = yandexOAuth(t, f, filepath.Join(t.TempDir(), "tokens"), &now)
	d, _ = o.BeginDevice(ctx, 1, playlistimport.ServiceYandex)
	if _, err := o.Poll(ctx, 1, playlistimport.ServiceYandex, d.PollID); !errors.Is(err, musicimport.ErrConnect) {
		t.Fatalf("access_denied: %v", err)
	}
	now = now.Add(5 * time.Second)
	if _, err := o.Poll(ctx, 1, playlistimport.ServiceYandex, d.PollID); !errors.Is(err, musicimport.ErrLinkExpired) || f.polls != 1 {
		t.Fatalf("after access_denied: %v, %d polls", err, f.polls)
	}

	// a page that is not https is never shown
	f = newFakeYandex(t)
	f.verify = "javascript:alert(1)"
	o = yandexOAuth(t, f, filepath.Join(t.TempDir(), "tokens"), nil)
	if _, err := o.BeginDevice(ctx, 1, playlistimport.ServiceYandex); !errors.Is(err, musicimport.ErrConnect) {
		t.Fatalf("javascript: %v", err)
	}
}

// A liked playlist listed without its owner is skipped (never read as the
// listener's own playlist of that kind).
func TestYandexLikedPlaylistWithoutOwner(t *testing.T) {
	f := newFakeYandex(t)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/users/1130000/likes/playlists" {
			return false
		}
		w.Write([]byte(`{"result":[{"playlist":{"kind":7777,"title":"Без владельца"}}]}`))
		return true
	}
	snap := readYandex(t, f, nil)
	if p := playlistByName(snap, "Без владельца"); p != nil {
		t.Fatalf("read %+v", p)
	}
	if n := f.requested("GET /users/1130000/playlists/7777"); n != 0 {
		t.Fatalf("read as the listener's own %d times", n)
	}
}
