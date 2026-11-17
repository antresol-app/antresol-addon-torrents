//go:build ignore

package connect

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
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
	"github.com/antresol-app/antresol-addon-torrents/internal/musicimport"
)

// fakeDeezer serves testdata/deezer as Deezer's API and its OAuth token
// endpoint would: errors come as HTTP 200 {"error":{…}}; answers can be
// overridden per request (hook).
type fakeDeezer struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	paths  []string // path?query of every API request
	access string
	// queryString: the token endpoint answers a query string even when
	// asked for JSON (an old app or a changed endpoint).
	queryString bool
	hook        func(w http.ResponseWriter, r *http.Request) (done bool)
}

func newFakeDeezer(t *testing.T) *fakeDeezer {
	f := &fakeDeezer{t: t, access: "frDeezerFakeAccessToken"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDeezer) file(w http.ResponseWriter, name string) {
	b, err := os.ReadFile(filepath.Join("testdata", "deezer", name))
	if err != nil {
		f.t.Errorf("testdata %s: %v", name, err)
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Write(b) // Deezer answers 200, errors too
}

func (f *fakeDeezer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path+"?"+r.URL.RawQuery)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil && hook(w, r) {
		return
	}
	q := r.URL.Query()
	if r.URL.Path == "/oauth/access_token.php" {
		if q.Get("app_id") != "app-1" || q.Get("secret") != "secret-1" || q.Get("code") == "bad" {
			w.Write([]byte("wrong code"))
			return
		}
		if q.Get("output") == "json" && !f.queryString {
			f.file(w, "token.json")
			return
		}
		w.Write([]byte("access_token=" + f.access + "&expires=0"))
		return
	}
	if q.Get("limit") != "" && q.Get("limit") != "100" {
		f.t.Errorf("limit %s", r.URL)
	}
	index := q.Get("index")
	if index == "" {
		index = "0"
	}
	tok := q.Get("access_token")
	p := r.URL.Path
	if strings.HasPrefix(p, "/user/") && tok != f.access || tok != "" && tok != f.access {
		f.file(w, "error-token.json")
		return
	}
	switch {
	case p == "/user/me":
		f.file(w, "user-me.json")
	case p == "/user/me/tracks":
		f.file(w, "tracks-"+index+".json")
	case p == "/user/me/albums":
		f.file(w, "albums-0.json")
	case p == "/user/me/playlists":
		f.file(w, "playlists-0.json")
	case p == "/playlist/1001":
		f.file(w, "playlist-1001.json")
	case p == "/playlist/1001/tracks" || p == "/playlist/1002/tracks":
		f.file(w, strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "-")+"-"+index+".json")
	case p == "/playlist/1003/tracks" || p == "/playlist/404404" || p == "/playlist/404404/tracks":
		f.file(w, "error-not-found.json")
	default:
		f.t.Errorf("unexpected %s", r.URL)
		f.file(w, "error-not-found.json")
	}
}

func (f *fakeDeezer) requested(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.paths {
		if strings.HasPrefix(p, prefix) {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeDeezer) reader(sl *sleeps) *Deezer {
	d := NewDeezer("app-1", "secret-1")
	d.BaseURL, d.ConnectURL, d.HTTP = f.srv.URL, f.srv.URL, f.srv.Client()
	d.Limiter = nil
	if sl != nil {
		d.Sleep = sl.sleep
	}
	return d
}

func readDeezer(t *testing.T, f *fakeDeezer, sl *sleeps) playlistimport.Snapshot {
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

func TestDeezerReadsFavourites(t *testing.T) {
	f := newFakeDeezer(t)
	snap := readDeezer(t, f, nil)
	if snap.Service != playlistimport.ServiceDeezer || snap.Account != "5123456" {
		t.Fatalf("snapshot %q %q", snap.Service, snap.Account)
	}
	// favourites over 2 pages: the second asked at the index «next» gave
	if got := f.requested("/user/me/tracks"); len(got) != 2 || !strings.Contains(got[0], "index=0") || !strings.Contains(got[1], "index=3") {
		t.Fatalf("pages %v", got)
	}
	var liked []playlistimport.SnapTrack
	for _, tr := range snap.Tracks {
		if tr.Liked {
			liked = append(liked, tr)
		}
	}
	if len(liked) != 5 {
		t.Fatalf("liked %d: %+v", len(liked), liked)
	}
	k := keyed(snap)
	blood := k["deezer:3135556"]
	if blood.Artist != "Кино" || blood.Title != "Группа крови" || blood.Album != "Группа крови" || blood.DurationMs != 285000 || !blood.Liked ||
		blood.AddedAt == nil || !blood.AddedAt.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("Группа крови %+v", blood)
	}
	// title + title_version, not twice when the title has it already
	if k["deezer:3135557"].Title != "Sad But True (Live)" || k["deezer:3135558"].Title != "One More Time (Radio Edit)" {
		t.Fatalf("versions %q %q", k["deezer:3135557"].Title, k["deezer:3135558"].Title)
	}
	// «Любимые треки» is the favourites, not a playlist (never read again)
	if playlistByName(snap, "Любимые треки") != nil || len(f.requested("/playlist/9001")) != 0 {
		t.Fatalf("the loved tracks playlist was read: %+v", snap.Playlists)
	}
	road := playlistByName(snap, "Дорога")
	if road == nil || !road.Readable || road.ID != "1001" ||
		!slices.Equal(road.Entries, []string{"deezer:3135559", "deezer:4000001", "deezer:3135556"}) {
		t.Fatalf("Дорога %+v", road)
	}
	if k["deezer:4000001"].Liked || k["deezer:4000001"].Artist != "Аквариум" {
		t.Fatalf("a playlist song %+v", k["deezer:4000001"])
	}
	if other := playlistByName(snap, "Чужой плейлист"); other == nil || !other.Readable || len(other.Entries) != 1 {
		t.Fatalf("Чужой плейлист %+v", other)
	}
	if gone := playlistByName(snap, "Удалённый"); gone == nil || gone.Readable || len(gone.Entries) != 0 {
		t.Fatalf("Удалённый %+v", gone)
	}
	if len(snap.Tracks) != 7 || len(snap.Playlists) != 3 {
		t.Fatalf("%d songs, %d playlists", len(snap.Tracks), len(snap.Playlists))
	}
	if len(snap.Albums) != 2 || snap.Albums[0] != (playlistimport.SnapAlbum{Key: "deezer:album:2001", Artist: "Кино", Title: "Группа крови", Year: 1988,
		AddedAt: snap.Albums[0].AddedAt}) || !snap.Albums[0].AddedAt.Equal(time.Unix(1690000000, 0)) {
		t.Fatalf("albums %+v", snap.Albums)
	}
}

func quota(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"error":{"type":"Exception","message":"Quota limit exceeded","code":4}}`))
}

func TestDeezerQuotaCode4(t *testing.T) {
	f := newFakeDeezer(t)
	n := 0
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/user/me/albums" && n == 0 {
			n++
			quota(w)
			return true
		}
		return false
	}
	sl := &sleeps{}
	snap := readDeezer(t, f, sl)
	if !slices.Equal(sl.d, []time.Duration{5 * time.Second}) || len(snap.Albums) != 2 || len(f.requested("/user/me/albums")) != 2 {
		t.Fatalf("waits %v, albums %d", sl.d, len(snap.Albums))
	}

	// the quota never comes back: a few waits, then the read ends
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/user/me/playlists" {
			quota(w)
			return true
		}
		return false
	}
	sl = &sleeps{}
	_, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, nil)
	if !errors.Is(err, musicimport.ErrConnect) || len(sl.d) != deezerQuotaRetries {
		t.Fatalf("err %v, waits %v", err, sl.d)
	}
	for _, d := range sl.d {
		if d != 5*time.Second {
			t.Fatalf("waits %v", sl.d)
		}
	}
}

func TestDeezerTokenQueryStringAnswer(t *testing.T) {
	f := newFakeDeezer(t)
	d := f.reader(nil)
	ctx := context.Background()
	// the sign-in page: no PKCE (Deezer has none)
	u, _ := url.Parse(d.AuthorizeURL("http://127.0.0.1:8765/vault-import/deezer", "st", "ignored"))
	want := url.Values{"app_id": {"app-1"}, "redirect_uri": {"http://127.0.0.1:8765/vault-import/deezer"},
		"perms": {"basic_access,offline_access"}, "state": {"st"}}
	if u.Path != "/oauth/auth.php" || u.Query().Encode() != want.Encode() {
		t.Fatalf("authorize %s", u)
	}
	for _, qs := range []bool{false, true} {
		f.queryString = qs
		tok, err := d.Exchange(ctx, "http://127.0.0.1:8765/vault-import/deezer", "fr123", "")
		if err != nil || tok.Access != f.access || !tok.Expiry.IsZero() {
			t.Fatalf("query string %v: %+v %v", qs, tok, err)
		}
	}
	last := f.requested("/oauth/access_token.php")
	if q, _ := url.ParseQuery(strings.SplitN(last[len(last)-1], "?", 2)[1]); q.Get("output") != "json" || q.Get("code") != "fr123" {
		t.Fatalf("token request %v", last)
	}
	// an expiring token
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		w.Write([]byte(`{"access_token":"frShort","expires":3600}`))
		return true
	}
	if tok, err := d.Exchange(ctx, "", "fr123", ""); err != nil || tok.Access != "frShort" || time.Until(tok.Expiry) < 59*time.Minute {
		t.Fatalf("expiring %+v %v", tok, err)
	}
	f.hook = nil
	// a refused code: the error has neither the code nor the secret
	_, err := d.Exchange(ctx, "", "bad", "")
	if err == nil || strings.Contains(err.Error(), "secret-1") || strings.Contains(err.Error(), "bad") {
		t.Fatalf("bad code: %v", err)
	}
	// the server unreachable: the URL (with the secret) is not in the error
	d.ConnectURL = "http://127.0.0.1:1"
	if _, err := d.Exchange(ctx, "", "fr123", ""); err == nil || strings.Contains(err.Error(), "secret-1") || strings.Contains(err.Error(), "fr123") {
		t.Fatalf("unreachable: %v", err)
	}
}

func TestDeezerExpiredToken(t *testing.T) {
	f := newFakeDeezer(t)
	_, err := f.reader(nil).Read(context.Background(), musicimport.Token{Access: "frRevoked"}, nil)
	if !errors.Is(err, musicimport.ErrConnect) || strings.Contains(err.Error(), "frRevoked") {
		t.Fatalf("err %v", err)
	}
	// a transport error does not show the token either (it is in the query)
	d := f.reader(nil)
	d.BaseURL = "http://127.0.0.1:1"
	if _, err := d.Read(context.Background(), musicimport.Token{Access: "frSecretToken"}, nil); !errors.Is(err, musicimport.ErrConnect) ||
		strings.Contains(err.Error(), "frSecretToken") {
		t.Fatalf("unreachable: %v", err)
	}
}

func TestDeezerLibraryListRefused(t *testing.T) {
	cases := []struct {
		name string
		hook func(w http.ResponseWriter, r *http.Request) bool
	}{
		{"me refused", func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/user/me" {
				w.Write([]byte(`{"error":{"type":"PermissionException","message":"no","code":200}}`))
				return true
			}
			return false
		}},
		{"favourites gone mid-pagination", func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/user/me/tracks" && r.URL.Query().Get("index") == "3" {
				w.Write([]byte(`{"error":{"type":"DataException","message":"no data","code":800}}`))
				return true
			}
			return false
		}},
		{"albums without data", func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/user/me/albums" {
				w.Write([]byte(`{"total":2}`))
				return true
			}
			return false
		}},
		{"playlists refused", func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/user/me/playlists" {
				w.WriteHeader(http.StatusForbidden)
				return true
			}
			return false
		}},
		{"next going back", func(w http.ResponseWriter, r *http.Request) bool {
			if r.URL.Path == "/user/me/tracks" && r.URL.Query().Get("index") == "3" {
				w.Write([]byte(`{"data":[],"total":5,"next":"https://api.deezer.com/user/me/tracks?index=0&limit=100"}`))
				return true
			}
			return false
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeDeezer(t)
			f.hook = c.hook
			if _, err := f.reader(&sleeps{}).Read(context.Background(), musicimport.Token{Access: f.access}, nil); !errors.Is(err, musicimport.ErrConnect) {
				t.Fatalf("read: %v", err)
			}
		})
	}
}

func TestDeezerRetries5xx(t *testing.T) {
	f := newFakeDeezer(t)
	n := 0
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/user/me/albums" && n == 0 {
			n++
			w.WriteHeader(http.StatusBadGateway)
			return true
		}
		return false
	}
	sl := &sleeps{}
	if snap := readDeezer(t, f, sl); len(snap.Albums) != 2 || !slices.Equal(sl.d, []time.Duration{time.Second}) {
		t.Fatalf("waits %v", sl.d)
	}
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/user/me/albums" {
			w.Write([]byte(`{"error":{"type":"Exception","message":"Service busy","code":700}}`))
			return true
		}
		return false
	}
	sl = &sleeps{}
	if _, err := f.reader(sl).Read(context.Background(), musicimport.Token{Access: f.access}, nil); !errors.Is(err, musicimport.ErrConnect) ||
		!slices.Equal(sl.d, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("busy: %v, waits %v", err, sl.d)
	}
}

// linkServer stands in for the short-link hosts: every host dials it
// (https), and it answers by host and path; hosts lists every Host asked.
func linkServer(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) (*http.Client, func() []string) {
	var mu sync.Mutex
	var hosts []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.Host+r.URL.Path)
		mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // the test certificate names none of these hosts
	}}
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(hosts)
	}
}

func TestDeezerPublicPlaylist(t *testing.T) {
	f := newFakeDeezer(t)
	links, asked := linkServer(t, func(w http.ResponseWriter, r *http.Request) {
		to := map[string]string{
			"link.deezer.com/s/30aBc":    "https://www.deezer.com/ru/playlist/1001?host=0&utm_campaign=clipboard-generic",
			"link.deezer.com/s/httphop":  "http://link.deezer.com/s/30aBc",
			"link.deezer.com/s/userhop":  "https://u:p@link.deezer.com/s/30aBc",
			"link.deezer.com/s/porthop":  "https://link.deezer.com:444/s/30aBc",
			"link.deezer.com/s/metadata": "https://169.254.169.254/latest/meta-data/",
			"link.deezer.com/s/evil":     "https://evil.example/playlist/1001",
			"link.deezer.com/s/loop":     "https://link.deezer.com/s/loop",
			"link.deezer.com/s/album":    "https://www.deezer.com/ru/album/2001",
			"link.deezer.com/s/relative": "/s/30aBc",
		}[r.Host+r.URL.Path]
		// pages instead of redirects: only where the page says it is counts
		const other = `<a href="https://www.deezer.com/en/playlist/999">Рекомендуем</a>`
		page := map[string]string{
			"link.deezer.com/s/page":      other + `<meta content="https://www.deezer.com/en/playlist/1001" property="og:url">`,
			"link.deezer.com/s/canon":     `<html><head>` + other + `<link href='https://www.deezer.com/ru/playlist/1001' rel='canonical'></head></html>`,
			"link.deezer.com/s/both":      `<meta property="og:url" content="https://www.deezer.com/en/playlist/1001"><link rel="canonical" href="https://www.deezer.com/en/playlist/999">`,
			"link.deezer.com/s/trackpage": `<meta property="og:url" content="https://www.deezer.com/en/track/3135556">` + other,
			"link.deezer.com/s/nocanon":   `<html><body>` + other + `</body></html>`,
			"link.deezer.com/s/ogevil":    `<meta property="og:url" content="https://evil.example/playlist/1001">`,
		}[r.Host+r.URL.Path]
		switch {
		case page != "":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(page))
		case to != "":
			http.Redirect(w, r, to, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	})
	d := f.reader(&sleeps{})
	d.LinkHTTP = links
	ctx := context.Background()
	for _, link := range []string{
		"https://www.deezer.com/ru/playlist/1001",
		" https://www.deezer.com/playlist/1001?utm_source=deezer ",
		"https://deezer.com/en/playlist/1001/",
		"https://www.deezer.com/pt-br/playlist/1001",
		"https://link.deezer.com/s/30aBc",
		"https://link.deezer.com/s/relative",
		"https://link.deezer.com/s/page",
		"https://link.deezer.com/s/canon",
		"https://link.deezer.com/s/both",
	} {
		snap, err := d.Public(ctx, link)
		if err != nil {
			t.Fatalf("%s: %v", link, err)
		}
		if len(snap.Playlists) != 1 || snap.Playlists[0].Name != "Дорога" || !snap.Playlists[0].Readable ||
			!slices.Equal(snap.Playlists[0].Entries, []string{"deezer:3135559", "deezer:4000001", "deezer:3135556"}) {
			t.Fatalf("%s: %+v", link, snap.Playlists)
		}
		for _, tr := range snap.Tracks {
			if tr.Liked {
				t.Fatalf("%s: a liked song %+v", link, tr)
			}
		}
		if snap.Service != playlistimport.ServiceDeezer || len(snap.Albums) != 0 || len(snap.Tracks) != 3 {
			t.Fatalf("%s: %+v", link, snap)
		}
	}
	// no sign-in: no token is ever sent
	for _, p := range f.requested("/playlist/") {
		if strings.Contains(p, "access_token") {
			t.Fatalf("a token was sent: %s", p)
		}
	}
	// the direct links need no request to resolve; the short ones one a hop
	if got := asked(); !slices.Equal(got, []string{"link.deezer.com/s/30aBc", "link.deezer.com/s/relative", "link.deezer.com/s/30aBc",
		"link.deezer.com/s/page", "link.deezer.com/s/canon", "link.deezer.com/s/both"}) {
		t.Fatalf("link requests %v", got)
	}

	// a playlist that does not exist (or is private)
	if _, err := d.Public(ctx, "https://www.deezer.com/en/playlist/404404"); !errors.Is(err, playlistimport.ErrNotFound) {
		t.Fatalf("404: %v", err)
	}

	// untrusted input: only Deezer's hosts, every hop checked
	before := len(asked())
	for _, link := range []string{
		"",
		"not a link",
		"https://evil.example/playlist/1001",
		"https://www.deezer.com.evil.example/playlist/1001",
		"https://evildeezer.com/playlist/1001",
		"https://user:pw@www.deezer.com/playlist/1001",
		"https://www.deezer.com:8443/playlist/1001",
		"https://www.deezer.com/ru/album/2001",
		"https://www.deezer.com/ru/playlist/12ab",
		"ftp://www.deezer.com/playlist/1001",
		"http://link.deezer.com/s/30aBc",
		"https://link.deezer.com/s/metadata",
		"https://link.deezer.com/s/evil",
		"https://link.deezer.com/s/loop",
		"https://link.deezer.com/s/album",
		"https://link.deezer.com/s/unknown",
		"https://deezer.page.link/xYz", // Firebase Dynamic Links are gone
		"https://link.deezer.com/s/trackpage",
		"https://link.deezer.com/s/nocanon",
		"https://link.deezer.com/s/ogevil",
		"https://link.deezer.com/s/httphop",
		"https://link.deezer.com/s/userhop",
		"https://link.deezer.com/s/porthop",
		"https://www.deezer.com/ru/playlist/" + strings.Repeat("1", 30),
	} {
		if _, err := d.Public(ctx, link); !errors.Is(err, playlistimport.ErrBadLink) {
			t.Fatalf("%q: %v", link, err)
		}
	}
	got := asked()[before:]
	for _, h := range got {
		if !strings.HasPrefix(h, "link.deezer.com/") {
			t.Fatalf("asked a host it must not: %v", got)
		}
	}
	// the loop stops after deezerMaxHops; the others: one request each
	if n := len(got); n != 10+deezerMaxHops {
		t.Fatalf("hops %d: %v", n, got)
	}
}

// deezerOAuth is the real sign-in with the real connector against the fake.
func deezerOAuth(t *testing.T, f *fakeDeezer) (*musicimport.OAuth, string) {
	dir := filepath.Join(t.TempDir(), "tokens")
	return &musicimport.OAuth{Svc: &playlistimport.Service{Store: newStore(t)}, Vault: &musicimport.Vault{Dir: dir, Key: bytes.Repeat([]byte{1}, 32)},
		Connectors: map[string]musicimport.Connector{playlistimport.ServiceDeezer: f.reader(&sleeps{})}}, dir
}

// signInDeezer signs in by the paste mode; Deezer may send the browser
// back without the state: the profile's one sign-in is taken.
func signInDeezer(t *testing.T, o *musicimport.OAuth, withState bool) int64 {
	t.Helper()
	b, err := o.Begin(1, playlistimport.ServiceDeezer)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(b.AuthorizeURL)
	if b.Mode != musicimport.ModePaste || b.RedirectURI != "http://127.0.0.1:8765/vault-import/deezer" || u.Query().Get("app_id") != "app-1" {
		t.Fatalf("begin %+v", b)
	}
	q := url.Values{"code": {"fr123"}}
	if withState {
		q.Set("state", u.Query().Get("state"))
	}
	id, err := o.FinishPasted(context.Background(), 1, b.RedirectURI+"?"+q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestDeezerTokenDeleted(t *testing.T) {
	ctx := context.Background()
	for _, fail := range []bool{false, true} {
		f := newFakeDeezer(t)
		o, dir := deezerOAuth(t, f)
		if fail {
			// the API refuses the token the sign-in gave (code 300)
			f.hook = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path == "/user/me" {
					w.Write([]byte(`{"error":{"type":"OAuthException","message":"Invalid OAuth access token.","code":300}}`))
					return true
				}
				return false
			}
		}
		id := signInDeezer(t, o, !fail)
		o.Wait()
		j, err := o.Svc.Store.Job(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case !fail && (j.Status != playlistimport.StatusPreview || j.Service != playlistimport.ServiceDeezer):
			t.Fatalf("success: %+v", j)
		case fail && (j.Status != playlistimport.StatusFailed || j.Error != "Не удалось подключиться к сервису. Попробуйте ещё раз."):
			t.Fatalf("failure: %+v", j)
		}
		if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
			t.Fatalf("fail=%v: token files left %v", fail, left)
		}
	}
}

func TestDeezerCancelStopsRead(t *testing.T) {
	ctx := context.Background()
	f := newFakeDeezer(t)
	o, dir := deezerOAuth(t, f)
	o.Svc.Reads = o
	reached := make(chan struct{}, 1)
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/user/me/albums" {
			select {
			case reached <- struct{}{}:
			default:
			}
			quota(w) // the read waits 5 s and asks again: a cancel cuts it short
			return true
		}
		return false
	}
	o.Connectors[playlistimport.ServiceDeezer].(*Deezer).Sleep = nil // a real timer
	id := signInDeezer(t, o, true)
	<-reached
	if err := o.Svc.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { o.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the read went on after the cancel")
	}
	if j, _ := o.Svc.Store.Job(ctx, id); j.Status != playlistimport.StatusCancelled {
		t.Fatalf("job %+v", j)
	}
	if n := len(f.requested("/user/me/albums")); n != 1 {
		t.Fatalf("albums asked %d times", n)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("token files left %v", left)
	}
}

func TestDeezerPublicLinkFailures(t *testing.T) {
	ctx := context.Background()
	f := newFakeDeezer(t)
	// a short link that cannot be reached: «try again», not «not found»
	down, _ := linkServer(t, func(w http.ResponseWriter, r *http.Request) {})
	down.Transport.(*http.Transport).DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	d := f.reader(&sleeps{})
	d.LinkHTTP = down
	if _, err := d.FindPublic(ctx, "https://link.deezer.com/s/30aBc"); !errors.Is(err, musicimport.ErrConnect) || errors.Is(err, playlistimport.ErrBadLink) {
		t.Fatalf("unreachable: %v", err)
	}
	// a short link's page bigger than the cap: its tags past it are not read
	big, _ := linkServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte(" "), deezerPageMax))
		w.Write([]byte(`<meta property="og:url" content="https://www.deezer.com/en/playlist/1001">`))
	})
	d.LinkHTTP = big
	if _, err := d.FindPublic(ctx, "https://link.deezer.com/s/big"); !errors.Is(err, playlistimport.ErrBadLink) {
		t.Fatalf("past the cap: %v", err)
	}
	// the quota on the playlist itself: waited out
	n := 0
	f.hook = func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/playlist/1001" && n == 0 {
			n++
			quota(w)
			return true
		}
		return false
	}
	sl := &sleeps{}
	d.Sleep = sl.sleep
	if p, err := d.FindPublic(ctx, "https://www.deezer.com/ru/playlist/1001"); err != nil || p.Name != "Дорога" || !slices.Equal(sl.d, []time.Duration{5 * time.Second}) {
		t.Fatalf("quota: %+v %v, waits %v", p, err, sl.d)
	}
}
