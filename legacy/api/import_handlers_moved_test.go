//go:build ignore

// The torrent-family cases of import_handlers_test.go (the music services'
// sign-ins, the catalog search and the download queue), set aside for the
// addon (moved to legacy/api/). importEnv here is the env
// as it was, with the sign-ins and the router's deps.

package api

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol/internal/acquire"
	"github.com/antresol-app/antresol/internal/auth"
	"github.com/antresol-app/antresol/internal/events"
	"github.com/antresol-app/antresol/internal/library"
	"github.com/antresol-app/antresol/internal/metadata"
	"github.com/antresol-app/antresol/internal/metadata/musicbrainz"
	"github.com/antresol-app/antresol/internal/music"
	"github.com/antresol-app/antresol/internal/musicimport"
	"github.com/antresol-app/antresol/internal/playlistimport"
)

type importEnv struct {
	*libEnv
	svc  *playlistimport.Service
	bus  *events.Bus
	deps *Deps // the router's dependencies (setOAuth remounts the router)
}

func newImportEnv(t *testing.T) *importEnv {
	ie := &importEnv{bus: &events.Bus{}}
	ie.libEnv = newLibEnvWith(t, func(d *Deps, db *sql.DB) {
		ie.svc = &playlistimport.Service{Store: &playlistimport.Store{DB: db}, Bus: ie.bus, Matcher: offlineMatcher{},
			Disk: func() (acquire.DiskUsage, error) {
				return acquire.DiskUsage{Used: 60 << 30, Total: 100 << 30, Level: acquire.LevelOK}, nil
			}}
		// the full build always has connectors; a server without credentials
		// lists the tiles unconfigured (a nil *OAuth answers for it)
		ie.svc.Connectors = (*musicimport.OAuth)(nil)
		d.PlaylistImport = ie.svc
		d.Events = ie.bus
		ie.deps = d
	})
	return ie
}

func TestImportSearchUsesCatalog(t *testing.T) {
	e := newImportEnv(t)
	read := func(f string) []byte {
		b, err := os.ReadFile(filepath.Join("..", "metadata", "musicbrainz", "testdata", f))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	groups, recs := read("release-groups-sad-but-true.json"), read("recordings-sad-but-true.json")
	var mu sync.Mutex
	var queries []string
	seen := func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(queries) }
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws/2/recording" {
			w.Write(recs)
			return
		}
		if r.URL.Path == "/ws/2/artist" { // no artist named «sad but true»
			w.Write([]byte(`{"artists":[]}`))
			return
		}
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("query"))
		mu.Unlock()
		w.Write(groups)
	}))
	defer mb.Close()
	acq := acquire.New(acquire.Options{DB: e.svc.Store.DB, Engine: &tEngine{}, Indexer: tIndexer{}, Metadata: tMeta{},
		Disk: &acquire.Disk{Root: t.TempDir()}, TorrentsDir: t.TempDir(), MoviesDir: t.TempDir(), SeriesDir: t.TempDir(),
		MusicBrainz: &musicbrainz.Client{BaseURL: mb.URL + "/ws/2", CoverBase: "https://caa.test", Limiter: &metadata.Limiter{}}})
	t.Cleanup(acq.Close)
	e.svc.Catalog = musicimport.Catalog{Acq: acq}

	type out struct{ Candidates []playlistimport.Candidate }
	w := e.do("GET", "/api/import/search?q="+url.QueryEscape("sad but true"), "", e.admin)
	res := decodeJSON[out](t, w.Body.Bytes())
	if w.Code != 200 || len(res.Candidates) == 0 || res.Candidates[0].Title != "Metallica" ||
		res.Candidates[0].MatchedTrack != "Sad but True" || res.Candidates[0].Kind != "music" || res.Candidates[0].ExtID == "" {
		t.Fatalf("search %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/import/search?q=", "", e.admin); w.Code != 200 || len(decodeJSON[out](t, w.Body.Bytes()).Candidates) != 0 {
		t.Fatalf("empty query %d %s", w.Code, w.Body)
	}

	// a review song's candidates: the catalog searched for «<artist> <title>»
	w = e.do("POST", "/api/import/jobs", `{"service":"file","tracks":[{"key":"a","artist":"Metallica","title":"Sad but True","liked":true}]}`, e.admin)
	id := decodeJSON[jobOut](t, w.Body.Bytes()).ID
	tid := e.trackIDs(id)["a"]
	mu.Lock()
	queries = nil
	mu.Unlock()
	w = e.do("GET", "/api/import/tracks/"+itoa(tid)+"/candidates", "", e.admin)
	if res := decodeJSON[out](t, w.Body.Bytes()); w.Code != 200 || len(res.Candidates) == 0 || !slices.Equal(seen(), []string{"Metallica Sad but True"}) {
		t.Fatalf("candidates %d %q %s", w.Code, seen(), w.Body)
	}
	if w := e.do("GET", "/api/import/search?q=x", "", e.kid("")); w.Code != 403 {
		t.Fatalf("kid %d", w.Code)
	}
	if w := e.do("GET", "/api/import/tracks/"+itoa(tid)+"/candidates", "", e.adminLogin("Мама")); w.Code != 404 {
		t.Fatalf("other admin %d", w.Code)
	}
}

// groupAcq answers the download state of the groups API.
type groupAcq struct{ done int64 }

func (groupAcq) PlanTracks(context.Context, acquire.TrackAcquireRequest) (acquire.TrackPlan, error) {
	return acquire.TrackPlan{}, acquire.ErrNoRelease
}
func (groupAcq) AcquireTracks(context.Context, acquire.TrackAcquireRequest, acquire.TrackPlan) (acquire.TrackAcquireResult, error) {
	return acquire.TrackAcquireResult{}, acquire.ErrNoRelease
}
func (a groupAcq) TrackDownload(context.Context, string) (acquire.TrackDownload, error) {
	return acquire.TrackDownload{Known: true, Present: true, Size: 300, Done: a.done}, nil
}
func (groupAcq) InProgress(context.Context, string, int64, string) (string, bool, error) {
	return "", false, nil
}
func (groupAcq) Disk() (acquire.DiskUsage, error) {
	return acquire.DiskUsage{Level: acquire.LevelOK}, nil
}
func (groupAcq) Remove(context.Context, string, bool) error { return nil }

func TestImportGroupsAndRetry(t *testing.T) {
	e := newImportEnv(t)
	e.svc.Queue = &musicimport.Queue{Acq: groupAcq{done: 120}, Store: e.svc.Store}
	w := e.do("POST", "/api/import/jobs", importSnapshot, e.admin)
	if w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	id := decodeJSON[jobOut](t, w.Body.Bytes()).ID
	ids := e.trackIDs(id)
	e.exec(`UPDATE import_jobs SET status = 'downloading' WHERE id = ?`, id)
	e.exec(`INSERT INTO import_groups(job_id, release_group, artist, album, year, state, infohash, bytes, updated_at)
		VALUES (?, 'rg-a', 'Кино', 'Чёрный альбом', 1990, 'downloading', 'ih-a', 300, '2026-10-26T12:00:00Z'),
		       (?, 'rg-b', 'Кино', 'Группа крови', 1988, 'not_found', '', 0, '2026-10-26T12:00:00Z')`, id, id)
	e.exec(`UPDATE import_tracks SET state = 'downloading', group_id = (SELECT id FROM import_groups WHERE release_group = 'rg-a') WHERE id = ?`, ids["a"])
	e.exec(`UPDATE import_groups SET error = 'Подходящих раздач не нашлось' WHERE release_group = 'rg-b'`)
	e.exec(`UPDATE import_tracks SET state = 'not_found', group_id = (SELECT id FROM import_groups WHERE release_group = 'rg-b') WHERE id = ?`, ids["b"])

	type groupsOut struct {
		Groups []struct {
			ID                   int64
			Artist, Album, State string
			Year, Songs          int
			Bytes, Done          int64
			Error                string
			InfoHash             string `json:"infoHash"`
		}
	}
	w = e.do("GET", "/api/import/jobs/"+itoa(id)+"/groups", "", e.admin)
	gs := decodeJSON[groupsOut](t, w.Body.Bytes())
	if w.Code != 200 || len(gs.Groups) != 2 {
		t.Fatalf("groups %d %s", w.Code, w.Body)
	}
	a, b := gs.Groups[0], gs.Groups[1]
	if a.Album != "Чёрный альбом" || a.Artist != "Кино" || a.Year != 1990 || a.State != "downloading" || a.Bytes != 300 || a.Done != 120 ||
		a.Songs != 1 || a.InfoHash != "ih-a" {
		t.Fatalf("downloading group %+v", a)
	}
	if b.State != "not_found" || b.Error != "Подходящих раздач не нашлось" || b.Songs != 1 {
		t.Fatalf("not found group %+v", b)
	}

	if w := e.do("POST", "/api/import/groups/"+itoa(a.ID)+"/retry", "", e.admin); w.Code != 409 || errText(w) != "Не найдено" {
		t.Fatalf("retry a downloading group %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/import/groups/"+itoa(b.ID)+"/retry", "", e.admin); w.Code != 204 {
		t.Fatalf("retry %d %s", w.Code, w.Body)
	}
	var gstate, tstate string
	e.svc.Store.DB.QueryRow(`SELECT state FROM import_groups WHERE id = ?`, b.ID).Scan(&gstate)
	e.svc.Store.DB.QueryRow(`SELECT state FROM import_tracks WHERE id = ?`, ids["b"]).Scan(&tstate)
	if gstate != "waiting" || tstate != "matched" {
		t.Fatalf("after retry: group %s, song %s", gstate, tstate)
	}
	other := e.adminLogin("Мама")
	if w := e.do("POST", "/api/import/groups/"+itoa(b.ID)+"/retry", "", other); w.Code != 404 {
		t.Fatalf("other admin %d", w.Code)
	}
	if w := e.do("GET", "/api/import/jobs/"+itoa(id)+"/groups", "", other); w.Code != 404 {
		t.Fatalf("other admin groups %d", w.Code)
	}
	if w := e.do("GET", "/api/import/jobs/"+itoa(id)+"/groups", "", e.kid("")); w.Code != 403 {
		t.Fatalf("kid %d", w.Code)
	}
	if w := e.do("POST", "/api/import/groups/x/retry", "", e.admin); w.Code != 400 {
		t.Fatalf("bad id %d", w.Code)
	}
}

// GET /api/import/services: the tiles of «Импорт». The four sign-in
// services are not configured yet (their tasks add the credentials); Apple
// Music and «Файл» take files.
func TestImportServices(t *testing.T) {
	e := newImportEnv(t)
	w := e.do("GET", "/api/import/services", "", e.admin)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var out struct {
		Services []struct {
			ID         string   `json:"id"`
			Configured bool     `json:"configured"`
			Connect    string   `json:"connect"`
			Setup      []string `json:"setup"`
			Files      []string `json:"files"`
		} `json:"services"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range out.Services {
		ids = append(ids, s.ID)
		switch s.ID {
		case "spotify", "yandex", "youtube", "deezer":
			if s.Configured || len(s.Setup) == 0 || len(s.Files) == 0 || s.Connect == "file" || s.Connect == "" {
				t.Errorf("%s: %+v", s.ID, s)
			}
		case "apple", "file":
			if !s.Configured || s.Connect != "file" || s.Setup == nil || len(s.Setup) != 0 || len(s.Files) == 0 {
				t.Errorf("%s: %+v", s.ID, s)
			}
		}
	}
	if strings.Join(ids, ",") != "spotify,apple,yandex,youtube,deezer,file" {
		t.Fatalf("services %v", ids)
	}
	if !strings.Contains(strings.Join(out.Services[0].Setup, " "), "VAULT_SPOTIFY_CLIENT_ID") {
		t.Fatalf("spotify setup %v", out.Services[0].Setup)
	}
	if w := e.do("GET", "/api/import/services", "", e.kid("")); w.Code != 403 {
		t.Fatalf("kid %d", w.Code)
	}
}

// stubConnector signs in with any code but "bad" and reads one song.
type stubConnector struct{}

func (stubConnector) AuthorizeURL(redirectURI, state, challenge string) string {
	return "https://accounts.spotify.com/authorize?" + url.Values{"redirect_uri": {redirectURI}, "state": {state},
		"code_challenge": {challenge}}.Encode()
}

func (stubConnector) Exchange(_ context.Context, _, code, _ string) (musicimport.Token, error) {
	if code == "bad" {
		return musicimport.Token{}, musicimport.ErrConnect
	}
	return musicimport.Token{Access: "a"}, nil
}

func (stubConnector) Read(context.Context, musicimport.Token, func(int, int)) (playlistimport.Snapshot, error) {
	return playlistimport.Snapshot{Service: "spotify", Tracks: []playlistimport.SnapTrack{{Key: "spotify:1", Artist: "Кино", Title: "Кукушка", Liked: true}}}, nil
}

func (e *importEnv) withOAuth(public string) *musicimport.OAuth {
	o := &musicimport.OAuth{Svc: e.svc, Vault: &musicimport.Vault{Dir: filepath.Join(e.t.TempDir(), "tokens"), Key: make([]byte, 32)},
		Connectors: map[string]musicimport.Connector{"spotify": stubConnector{}}, PublicURL: public}
	e.setOAuth(o)
	e.t.Cleanup(o.Wait)
	return o
}

// setOAuth signs the service's connectors in (nil: none, as when the
// server has no credentials) and remounts the router with them.
func (e *importEnv) setOAuth(o *musicimport.OAuth) {
	e.svc.Connectors, e.svc.Reads = o, o
	e.deps.ImportOAuth = o
	e.libEnv.h = NewRouter(*e.deps)
}

func (e *importEnv) begin(c *http.Cookie) (mode, redirect, state string) {
	e.t.Helper()
	w := e.do("POST", "/api/import/connect/spotify", "{}", c)
	if w.Code != 200 {
		e.t.Fatalf("connect %d %s", w.Code, w.Body)
	}
	b := decodeJSON[struct {
		AuthorizeURL string `json:"authorizeUrl"`
		Mode         string `json:"mode"`
		RedirectURI  string `json:"redirectUri"`
	}](e.t, w.Body.Bytes())
	u, err := url.Parse(b.AuthorizeURL)
	if err != nil || u.Host != "accounts.spotify.com" {
		e.t.Fatalf("authorize %q", b.AuthorizeURL)
	}
	return b.Mode, b.RedirectURI, u.Query().Get("state")
}

func TestSpotifyCallbackRedirects(t *testing.T) {
	e := newImportEnv(t)
	o := e.withOAuth("https://vault.example.org")

	// the tile: configured, signs in by redirect
	w := e.do("GET", "/api/import/services", "", e.admin)
	if !strings.Contains(w.Body.String(), `{"id":"spotify","configured":true,"connect":"redirect"`) {
		t.Fatalf("services %s", w.Body)
	}
	mode, redirect, state := e.begin(e.admin)
	if mode != "redirect" || redirect != "https://vault.example.org/api/import/oauth/spotify/callback" || state == "" {
		t.Fatalf("%s %s %s", mode, redirect, state)
	}
	w = e.do("GET", "/api/import/oauth/spotify/callback?code=c1&state="+url.QueryEscape(state), "", e.admin)
	loc := w.Header().Get("Location")
	if w.Code != http.StatusFound || !strings.HasPrefix(loc, "/manage/import?job=") {
		t.Fatalf("callback %d %q", w.Code, loc)
	}
	o.Wait()
	id, _ := strconv.ParseInt(strings.TrimPrefix(loc, "/manage/import?job="), 10, 64)
	if j, err := e.svc.Store.Job(context.Background(), id); err != nil || j.Status != "preview" {
		t.Fatalf("job %+v %v", j, err)
	}
	// the next sign-ins: the job read is put away first (an open one refuses them)
	if w := e.do("POST", "/api/import/jobs/"+itoa(id)+"/cancel", "", e.admin); w.Code != 204 {
		t.Fatalf("cancel %d %s", w.Code, w.Body)
	}
	// the state is used up: the error comes back to the page
	w = e.do("GET", "/api/import/oauth/spotify/callback?code=c1&state="+url.QueryEscape(state), "", e.admin)
	if w.Code != http.StatusFound || w.Header().Get("Location") != "/manage/import?error="+url.QueryEscape("Ссылка устарела. Подключитесь заново.") {
		t.Fatalf("reuse %d %q", w.Code, w.Header().Get("Location"))
	}
	// the listener said no at Spotify
	_, _, state = e.begin(e.admin)
	w = e.do("GET", "/api/import/oauth/spotify/callback?error=access_denied&state="+url.QueryEscape(state), "", e.admin)
	if w.Header().Get("Location") != "/manage/import?error="+url.QueryEscape("Не удалось подключиться к сервису. Попробуйте ещё раз.") {
		t.Fatalf("denied %q", w.Header().Get("Location"))
	}
	// another admin cannot finish this admin's sign-in
	_, _, state = e.begin(e.admin)
	other := e.adminLogin("Мама")
	w = e.do("GET", "/api/import/oauth/spotify/callback?code=c&state="+url.QueryEscape(state), "", other)
	if !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Fatalf("other admin %q", w.Header().Get("Location"))
	}
	// kids never get here; no session: no redirect
	if w := e.do("GET", "/api/import/oauth/spotify/callback?code=c&state=x", "", e.kid("")); w.Code != 403 {
		t.Fatalf("kid %d", w.Code)
	}
	if w := e.do("GET", "/api/import/oauth/spotify/callback?code=c&state=x", "", nil); w.Code != 401 {
		t.Fatalf("anonymous %d", w.Code)
	}
	if w := e.do("POST", "/api/import/connect/napster", "{}", e.admin); w.Code != 400 || !strings.Contains(w.Body.String(), "Неизвестный сервис") {
		t.Fatalf("unknown %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/import/connect/deezer", "{}", e.admin); w.Code != 400 || !strings.Contains(w.Body.String(), "Сервис не настроен на сервере") {
		t.Fatalf("not configured %d %s", w.Code, w.Body)
	}
}

func TestSpotifyPasteEndpoint(t *testing.T) {
	e := newImportEnv(t)
	o := e.withOAuth("")
	w := e.do("GET", "/api/import/services", "", e.admin)
	if !strings.Contains(w.Body.String(), `{"id":"spotify","configured":true,"connect":"paste"`) {
		t.Fatalf("services %s", w.Body)
	}
	mode, redirect, state := e.begin(e.admin)
	if mode != "paste" || redirect != "http://127.0.0.1:8765/vault-import/spotify" {
		t.Fatalf("%s %s", mode, redirect)
	}
	body := func(u string) string { b, _ := json.Marshal(map[string]string{"url": u}); return string(b) }
	w = e.do("POST", "/api/import/oauth/spotify/paste", body(redirect+"?error=access_denied&state="+url.QueryEscape(state)), e.admin)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Не удалось подключиться к сервису. Попробуйте ещё раз.") {
		t.Fatalf("denied %d %s", w.Code, w.Body)
	}
	_, _, state = e.begin(e.admin)
	w = e.do("POST", "/api/import/oauth/spotify/paste", body(redirect+"?code=c2&state="+url.QueryEscape(state)), e.admin)
	if w.Code != 201 {
		t.Fatalf("paste %d %s", w.Code, w.Body)
	}
	j := decodeJSON[jobOut](t, w.Body.Bytes())
	if j.Service != "spotify" || (j.Status != "reading" && j.Status != "preview") {
		t.Fatalf("job %s", w.Body)
	}
	o.Wait()
	if got, _ := e.svc.Store.Job(context.Background(), j.ID); got.Status != "preview" {
		t.Fatalf("after read %+v", got)
	}
	w = e.do("POST", "/api/import/oauth/spotify/paste", body(redirect+"?code=c2&state="+url.QueryEscape(state)), e.admin)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Ссылка устарела. Подключитесь заново.") {
		t.Fatalf("reuse %d %s", w.Code, w.Body)
	}
}

func TestConnectRefusedWhileJobOpen(t *testing.T) {
	e := newImportEnv(t)
	e.withOAuth("https://vault.example.org")
	if w := e.do("POST", "/api/import/jobs", importSnapshot, e.admin); w.Code != 201 {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	w := e.do("POST", "/api/import/connect/spotify", "{}", e.admin)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "Импорт уже идёт") {
		t.Fatalf("connect %d %s", w.Code, w.Body)
	}
	// the tile reports how it would connect even unconfigured
	e.setOAuth(nil)
	if w := e.do("GET", "/api/import/services", "", e.admin); !strings.Contains(w.Body.String(), `{"id":"spotify","configured":false,"connect":"redirect"`) {
		t.Fatalf("services %s", w.Body)
	}
}

func TestCallbackChecksService(t *testing.T) {
	e := newImportEnv(t)
	e.withOAuth("https://vault.example.org")
	_, _, state := e.begin(e.admin)
	w := e.do("GET", "/api/import/oauth/deezer/callback?code=c&state="+url.QueryEscape(state), "", e.admin)
	if !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Fatalf("deezer %q", w.Header().Get("Location"))
	}
	w = e.do("GET", "/api/import/oauth/spotify/callback?code=c&state="+url.QueryEscape(state), "", e.admin)
	if !strings.HasPrefix(w.Header().Get("Location"), "/manage/import?job=") {
		t.Fatalf("spotify %q", w.Header().Get("Location"))
	}
}

// stubDevice signs in with a device code: pending twice, then a token
// (expired: the code ran out); reads one song.
type stubDevice struct {
	mu      sync.Mutex
	polls   int
	expired bool
}

func (*stubDevice) DeviceCode(context.Context) (musicimport.DeviceCode, error) {
	return musicimport.DeviceCode{DeviceCode: "dc-secret", UserCode: "ABCD1234", VerificationURL: "https://ya.ru/device", Interval: 1, ExpiresIn: 300}, nil
}

func (s *stubDevice) PollToken(_ context.Context, code string) (musicimport.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.polls++
	switch {
	case code != "dc-secret":
		return musicimport.Token{}, musicimport.ErrConnect
	case s.expired:
		return musicimport.Token{}, musicimport.ErrLinkExpired
	case s.polls <= 2:
		return musicimport.Token{}, musicimport.ErrPending
	}
	return musicimport.Token{Access: "y0_token"}, nil
}

func (*stubDevice) Read(_ context.Context, tok musicimport.Token, _ func(int, int)) (playlistimport.Snapshot, error) {
	if tok.Access != "y0_token" {
		return playlistimport.Snapshot{}, musicimport.ErrConnect
	}
	return playlistimport.Snapshot{Service: "yandex", Tracks: []playlistimport.SnapTrack{{Key: "yandex:1", Artist: "Кино", Title: "Кукушка", Liked: true}}}, nil
}

func (e *importEnv) withDevice(d *stubDevice, now *time.Time) *musicimport.OAuth {
	o := e.withOAuth("")
	o.Devices = map[string]musicimport.DeviceConnector{"yandex": d}
	if now != nil {
		o.Now = func() time.Time { return *now }
	}
	return o
}

type deviceOut struct {
	PollID          string `json:"pollId"`
	UserCode        string `json:"userCode"`
	VerificationURL string `json:"verificationUrl"`
	ExpiresIn       int    `json:"expiresIn"`
	Interval        int    `json:"interval"`
}

func TestYandexDeviceFlowAPI(t *testing.T) {
	e := newImportEnv(t)
	now := time.Now()
	o := e.withDevice(&stubDevice{}, &now)
	if w := e.do("GET", "/api/import/services", "", e.admin); !strings.Contains(w.Body.String(), `{"id":"yandex","configured":true,"connect":"device","setup":[]`) {
		t.Fatalf("services %s", w.Body)
	}
	w := e.do("POST", "/api/import/connect/yandex", "{}", e.admin)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "dc-secret") {
		t.Fatalf("connect %d %v %s", w.Code, w.Header(), w.Body)
	}
	d := decodeJSON[deviceOut](t, w.Body.Bytes())
	if d.PollID == "" || d.UserCode != "ABCD1234" || d.VerificationURL != "https://ya.ru/device" || d.Interval != 1 || d.ExpiresIn != 300 {
		t.Fatalf("device %+v", d)
	}
	poll := func(c *http.Cookie, id string) *httptest.ResponseRecorder {
		b, _ := json.Marshal(map[string]string{"pollId": id})
		return e.do("POST", "/api/import/connect/yandex/poll", string(b), c)
	}
	// kids and other admins never finish it
	if w := poll(e.kid(""), d.PollID); w.Code != 403 {
		t.Fatalf("kid %d", w.Code)
	}
	if w := poll(e.adminLogin("Мама"), d.PollID); w.Code != 400 || !strings.Contains(w.Body.String(), "Ссылка устарела. Подключитесь заново.") {
		t.Fatalf("other admin %d %s", w.Code, w.Body)
	}
	for i := 0; i < 2; i++ {
		if w := poll(e.admin, d.PollID); w.Code != 202 || strings.TrimSpace(w.Body.String()) != `{"pending":true}` {
			t.Fatalf("poll %d: %d %s", i, w.Code, w.Body)
		}
		now = now.Add(time.Second)
	}
	w = poll(e.admin, d.PollID)
	if w.Code != 201 {
		t.Fatalf("poll %d %s", w.Code, w.Body)
	}
	j := decodeJSON[jobOut](t, w.Body.Bytes())
	if j.Service != "yandex" || (j.Status != "reading" && j.Status != "preview") {
		t.Fatalf("job %s", w.Body)
	}
	o.Wait()
	if got, _ := e.svc.Store.Job(context.Background(), j.ID); got.Status != "preview" {
		t.Fatalf("after read %+v", got)
	}
	// the job read is open: no second sign-in, and no poll finishes one
	if w := e.do("POST", "/api/import/connect/yandex", "{}", e.admin); w.Code != 409 {
		t.Fatalf("while open %d %s", w.Code, w.Body)
	}
	if w := poll(e.admin, d.PollID); w.Code != 409 || !strings.Contains(w.Body.String(), "Импорт уже идёт") {
		t.Fatalf("poll while open %d %s", w.Code, w.Body)
	}
	e.do("POST", "/api/import/jobs/"+itoa(j.ID)+"/cancel", "", e.admin)
	now = now.Add(time.Second)
	if w := poll(e.admin, d.PollID); w.Code != 400 || !strings.Contains(w.Body.String(), "Ссылка устарела. Подключитесь заново.") {
		t.Fatalf("reuse %d %s", w.Code, w.Body)
	}

	// the code ran out at Yandex
	e.withDevice(&stubDevice{expired: true}, nil)
	d = decodeJSON[deviceOut](t, e.do("POST", "/api/import/connect/yandex", "{}", e.admin).Body.Bytes())
	if w := poll(e.admin, d.PollID); w.Code != 400 || !strings.Contains(w.Body.String(), "Ссылка устарела. Подключитесь заново.") {
		t.Fatalf("expired %d %s", w.Code, w.Body)
	}
	// a Spotify service has no device code
	if w := e.do("POST", "/api/import/connect/spotify/poll", `{"pollId":"x"}`, e.admin); w.Code != 400 {
		t.Fatalf("spotify poll %d %s", w.Code, w.Body)
	}
}

func TestYandexTokenPasteAPI(t *testing.T) {
	e := newImportEnv(t)
	o := e.withDevice(&stubDevice{}, nil)
	if w := e.do("POST", "/api/import/connect/yandex/token", `{"token":""}`, e.admin); w.Code != 400 ||
		!strings.Contains(w.Body.String(), "Не удалось подключиться к сервису. Попробуйте ещё раз.") {
		t.Fatalf("empty %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/import/connect/yandex/token", `{"token":"y0_token"}`, e.kid("")); w.Code != 403 {
		t.Fatalf("kid %d", w.Code)
	}
	w := e.do("POST", "/api/import/connect/yandex/token", `{"token":"y0_token"}`, e.admin)
	if w.Code != 201 {
		t.Fatalf("token %d %s", w.Code, w.Body)
	}
	j := decodeJSON[jobOut](t, w.Body.Bytes())
	o.Wait()
	if got, _ := e.svc.Store.Job(context.Background(), j.ID); got.Status != "preview" || got.Service != "yandex" {
		t.Fatalf("after read %+v", got)
	}
	// an open job of the service refuses another
	if w := e.do("POST", "/api/import/connect/yandex/token", `{"token":"y0_token"}`, e.admin); w.Code != 409 {
		t.Fatalf("while open %d %s", w.Code, w.Body)
	}
	e.do("POST", "/api/import/jobs/"+itoa(j.ID)+"/cancel", "", e.admin)
	// a refused token: the job fails with the text
	w = e.do("POST", "/api/import/connect/yandex/token", `{"token":"y0_wrong"}`, e.admin)
	j = decodeJSON[jobOut](t, w.Body.Bytes())
	o.Wait()
	if got, _ := e.svc.Store.Job(context.Background(), j.ID); got.Status != "failed" || got.Error != "Не удалось подключиться к сервису. Попробуйте ещё раз." {
		t.Fatalf("refused %+v", got)
	}
	if left, _ := filepath.Glob(filepath.Join(o.Vault.Dir, "*")); len(left) != 0 {
		t.Fatalf("tokens left %v", left)
	}
	if w := e.do("POST", "/api/import/connect/deezer/token", `{"token":"x"}`, e.admin); w.Code != 400 ||
		!strings.Contains(w.Body.String(), "Сервис не настроен на сервере") {
		t.Fatalf("deezer %d %s", w.Code, w.Body)
	}
}

// stubResolver names ZZunknown01 (a Takeout video known only by id).
type stubResolver struct {
	mu    sync.Mutex
	asked []string
	gate  chan struct{} // nil: answers at once
}

func (s *stubResolver) Resolve(_ context.Context, ids []string) (map[string]playlistimport.SnapTrack, error) {
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	s.asked = append(s.asked, ids...)
	s.mu.Unlock()
	out := map[string]playlistimport.SnapTrack{}
	for _, id := range ids {
		if id == "ZZunknown01" {
			out[id] = playlistimport.SnapTrack{Key: id, Artist: "Кино", Title: "Группа крови"}
		}
	}
	return out, nil
}

func TestTakeoutUploadResolvesIds(t *testing.T) {
	e := newImportEnv(t)
	o := e.withOAuth("")
	res := &stubResolver{gate: make(chan struct{})}
	e.svc.Resolver = res
	zip, _ := os.ReadFile(filepath.Join("..", "playlistimport", "files", "testdata", "takeout.zip"))

	// the job reads while the videos are named, then shows the preview
	w := e.upload(e.admin, nil, [2]string{"takeout-001.zip", string(zip)})
	if w.Code != 201 {
		t.Fatalf("upload %d %s", w.Code, w.Body)
	}
	j := decodeJSON[jobOut](t, w.Body.Bytes())
	if j.Status != "reading" || j.Service != "youtube" {
		t.Fatalf("job %s", w.Body)
	}
	close(res.gate)
	o.Wait()
	w = e.do("GET", "/api/import/jobs/"+itoa(j.ID), "", e.admin)
	j = decodeJSON[jobOut](t, w.Body.Bytes())
	if j.Status != "preview" || j.Counts.Songs != 4 || !strings.Contains(w.Body.String(), `"unresolved":0`) {
		t.Fatalf("after naming %s", w.Body)
	}
	if strings.Join(res.asked, ",") != "ZZunknown01" {
		t.Fatalf("asked %v", res.asked)
	}
	if w := e.upload(e.admin, nil, [2]string{"takeout-001.zip", string(zip)}); w.Code != 409 || errText(w) != "Импорт уже идёт" {
		t.Fatalf("second upload %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/api/import/jobs/"+itoa(j.ID), "", e.admin); w.Code != 204 {
		t.Fatalf("delete %d %s", w.Code, w.Body)
	}

	// a resolver that names nothing: the report counts what is still unknown
	e.svc.Resolver = &nothingResolver{}
	w = e.upload(e.admin, nil, [2]string{"takeout-001.zip", string(zip)})
	o.Wait()
	w = e.do("GET", "/api/import/jobs/"+itoa(decodeJSON[jobOut](t, w.Body.Bytes()).ID), "", e.admin)
	if j := decodeJSON[jobOut](t, w.Body.Bytes()); j.Status != "preview" || !strings.Contains(w.Body.String(), `"unresolved":1`) {
		t.Fatalf("unnamed %s", w.Body)
	}
	// kids never upload
	if w := e.upload(e.kid(""), nil, [2]string{"takeout-001.zip", string(zip)}); w.Code != 403 {
		t.Fatalf("kid %d", w.Code)
	}
}

type nothingResolver struct{}

func (nothingResolver) Resolve(context.Context, []string) (map[string]playlistimport.SnapTrack, error) {
	return nil, errors.New("oembed: 503")
}

// stubGoogle signs in to YouTube with any code but "bad".
type stubGoogle struct{ stubConnector }

func (stubGoogle) AuthorizeURL(redirectURI, state, challenge string) string {
	return "https://accounts.google.com/o/oauth2/v2/auth?" + url.Values{"redirect_uri": {redirectURI}, "state": {state},
		"code_challenge": {challenge}, "prompt": {"consent"}}.Encode()
}

func TestYouTubeSignInAPI(t *testing.T) {
	e := newImportEnv(t)
	if w := e.do("GET", "/api/import/services", "", e.admin); !strings.Contains(w.Body.String(), `{"id":"youtube","configured":false,"connect":"redirect","setup":["Откройте console.cloud.google.com`) {
		t.Fatalf("unconfigured %s", w.Body)
	}
	o := e.withOAuth("")
	o.Connectors["youtube"] = stubGoogle{}
	if w := e.do("GET", "/api/import/services", "", e.admin); !strings.Contains(w.Body.String(), `{"id":"youtube","configured":true,"connect":"paste","setup":[]`) {
		t.Fatalf("services %s", w.Body)
	}
	w := e.do("POST", "/api/import/connect/youtube", "{}", e.admin)
	b := decodeJSON[struct {
		AuthorizeURL string `json:"authorizeUrl"`
		Mode         string `json:"mode"`
		RedirectURI  string `json:"redirectUri"`
	}](t, w.Body.Bytes())
	u, _ := url.Parse(b.AuthorizeURL)
	if w.Code != 200 || b.Mode != "paste" || b.RedirectURI != "http://127.0.0.1:8765/vault-import/youtube" || u.Host != "accounts.google.com" {
		t.Fatalf("connect %d %s", w.Code, w.Body)
	}
	body, _ := json.Marshal(map[string]string{"url": b.RedirectURI + "?state=" + url.QueryEscape(u.Query().Get("state")) + "&code=4/0A&scope=" +
		url.QueryEscape("https://www.googleapis.com/auth/youtube.readonly")})
	w = e.do("POST", "/api/import/oauth/youtube/paste", string(body), e.admin)
	if w.Code != 201 || decodeJSON[jobOut](t, w.Body.Bytes()).Service != "youtube" {
		t.Fatalf("paste %d %s", w.Code, w.Body)
	}
	o.Wait()
	if got, _ := e.svc.Store.Job(context.Background(), decodeJSON[jobOut](t, w.Body.Bytes()).ID); got.Status != "preview" {
		t.Fatalf("after read %+v", got)
	}
	// reused: the state is spent
	if w := e.do("POST", "/api/import/oauth/youtube/paste", string(body), e.admin); w.Code != 400 && w.Code != 409 {
		t.Fatalf("reuse %d %s", w.Code, w.Body)
	}
	// with VAULT_PUBLIC_URL: the Web application client's callback
	o.PublicURL = "https://vault.example.org"
	if w := e.do("GET", "/api/import/services", "", e.admin); !strings.Contains(w.Body.String(), `{"id":"youtube","configured":true,"connect":"redirect"`) {
		t.Fatalf("redirect services %s", w.Body)
	}
}

// takeoutOf is a Takeout zip of one playlist of the ids (none named).
func takeoutOf(t *testing.T, ids ...string) string {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, _ := z.Create("Takeout/YouTube Music/playlists/Дорога-videos.csv")
	io.WriteString(w, "Video ID,Playlist Video Creation Timestamp\n")
	for _, id := range ids {
		io.WriteString(w, id+",2024-01-01T00:00:00+00:00\n")
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// ctxResolver names id1 at once, then waits for its ctx to end (the
// budget, a cancel) and says so.
type ctxResolver struct{ stopped chan error }

func (c *ctxResolver) Resolve(ctx context.Context, ids []string) (map[string]playlistimport.SnapTrack, error) {
	out := map[string]playlistimport.SnapTrack{"vidKino0001": {Key: "vidKino0001", Artist: "Кино", Title: "Кукушка"}}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second): // a failing test still ends
		return out, nil
	}
	select {
	case c.stopped <- ctx.Err():
	default:
	}
	return out, ctx.Err()
}

func TestTakeoutResolveBudgetKeepsNamed(t *testing.T) {
	e := newImportEnv(t)
	o := e.withOAuth("")
	res := &ctxResolver{stopped: make(chan error, 1)}
	e.svc.Resolver = res
	old := resolveBudget
	resolveBudget = 50 * time.Millisecond
	t.Cleanup(func() { resolveBudget = old })
	w := e.upload(e.admin, nil, [2]string{"takeout.zip", takeoutOf(t, "vidKino0001", "vidSlow0002")})
	if w.Code != 201 {
		t.Fatalf("upload %d %s", w.Code, w.Body)
	}
	if err := <-res.stopped; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stopped by %v", err)
	}
	o.Wait()
	w = e.do("GET", "/api/import/jobs/"+itoa(decodeJSON[jobOut](t, w.Body.Bytes()).ID), "", e.admin)
	if j := decodeJSON[jobOut](t, w.Body.Bytes()); j.Status != "preview" || j.Counts.Songs != 1 || !strings.Contains(w.Body.String(), `"unresolved":1`) {
		t.Fatalf("after the budget %s", w.Body)
	}
}

func TestTakeoutResolveStopsOnCancel(t *testing.T) {
	for _, how := range []string{"cancel", "delete"} {
		t.Run(how, func(t *testing.T) {
			e := newImportEnv(t)
			o := e.withOAuth("")
			res := &ctxResolver{stopped: make(chan error, 4)}
			e.svc.Resolver = res
			zip := takeoutOf(t, "vidKino0001", "vidSlow0002")
			w := e.upload(e.admin, nil, [2]string{"takeout.zip", zip})
			id := decodeJSON[jobOut](t, w.Body.Bytes()).ID
			// one naming at a time per profile
			if w := e.upload(e.admin, nil, [2]string{"takeout.zip", zip}); w.Code != 409 || errText(w) != "Импорт уже идёт" {
				t.Fatalf("second upload %d %s", w.Code, w.Body)
			}
			var w2 *httptest.ResponseRecorder
			if how == "cancel" {
				w2 = e.do("POST", "/api/import/jobs/"+itoa(id)+"/cancel", "", e.admin)
			} else {
				w2 = e.do("DELETE", "/api/import/jobs/"+itoa(id), "", e.admin)
			}
			if w2.Code != 204 {
				t.Fatalf("%s %d %s", how, w2.Code, w2.Body)
			}
			select {
			case err := <-res.stopped:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("stopped by %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the naming goes on after the " + how)
			}
			o.Wait()
			if how == "cancel" {
				if j, _ := e.svc.Store.Job(context.Background(), id); j.Status != "cancelled" {
					t.Fatalf("job %+v", j)
				}
			}
			// the profile may upload again
			w = e.upload(e.admin, nil, [2]string{"takeout.zip", zip})
			if w.Code != 201 {
				t.Fatalf("re-upload %d %s", w.Code, w.Body)
			}
			if w := e.do("POST", "/api/import/jobs/"+itoa(decodeJSON[jobOut](t, w.Body.Bytes()).ID)+"/cancel", "", e.admin); w.Code != 204 {
				t.Fatalf("cancel again %d %s", w.Code, w.Body)
			}
			<-res.stopped
		})
	}
}

// stubDeezer signs in to Deezer with any code but "bad" (no PKCE, and the
// state may not come back).
type stubDeezer struct{ stubConnector }

func (stubDeezer) AuthorizeURL(redirectURI, state, _ string) string {
	return "https://connect.deezer.com/oauth/auth.php?" + url.Values{"app_id": {"app-1"}, "redirect_uri": {redirectURI},
		"perms": {"basic_access,offline_access"}, "state": {state}}.Encode()
}

func (stubDeezer) StateOptional() bool { return true }

func (stubDeezer) Read(context.Context, musicimport.Token, func(int, int)) (playlistimport.Snapshot, error) {
	return playlistimport.Snapshot{Service: "deezer", Tracks: []playlistimport.SnapTrack{{Key: "deezer:1", Artist: "Кино", Title: "Кукушка", Liked: true}}}, nil
}

func TestDeezerSignInAPI(t *testing.T) {
	e := newImportEnv(t)
	if w := e.do("GET", "/api/import/services", "", e.admin); !strings.Contains(w.Body.String(),
		`{"id":"deezer","configured":false,"connect":"redirect","setup":["Откройте developers.deezer.com/myapps`) {
		t.Fatalf("unconfigured %s", w.Body)
	}
	o := e.withOAuth("https://vault.example.org")
	o.Connectors["deezer"] = stubDeezer{}
	if w := e.do("GET", "/api/import/services", "", e.admin); !strings.Contains(w.Body.String(), `{"id":"deezer","configured":true,"connect":"redirect","setup":[]`) {
		t.Fatalf("services %s", w.Body)
	}
	connect := func() {
		t.Helper()
		w := e.do("POST", "/api/import/connect/deezer", "{}", e.admin)
		b := decodeJSON[struct {
			AuthorizeURL string `json:"authorizeUrl"`
			RedirectURI  string `json:"redirectUri"`
		}](t, w.Body.Bytes())
		if u, _ := url.Parse(b.AuthorizeURL); w.Code != 200 || u.Host != "connect.deezer.com" || b.RedirectURI != "https://vault.example.org/api/import/oauth/deezer/callback" {
			t.Fatalf("connect %d %s", w.Code, w.Body)
		}
	}
	// the listener says no: Deezer's error_reason, without the state
	connect()
	w := e.do("GET", "/api/import/oauth/deezer/callback?error_reason=user_denied", "", e.admin)
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/manage/import?error=") ||
		!strings.Contains(loc, url.QueryEscape("Не удалось подключиться к сервису. Попробуйте ещё раз.")) {
		t.Fatalf("denied %q", loc)
	}
	// back without the state: the admin's one Deezer sign-in
	if w := e.do("GET", "/api/import/oauth/deezer/callback?code=fr1", "", e.admin); !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Fatalf("no sign-in left: %q", w.Header().Get("Location"))
	}
	connect()
	// a state that is not the sign-in's is refused, and the sign-in stays
	if w := e.do("GET", "/api/import/oauth/deezer/callback?code=fr1&state=forged", "", e.admin); !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Fatalf("forged state %q", w.Header().Get("Location"))
	}
	w = e.do("GET", "/api/import/oauth/deezer/callback?code=fr1", "", e.admin)
	loc := w.Header().Get("Location")
	if w.Code != http.StatusFound || !strings.HasPrefix(loc, "/manage/import?job=") {
		t.Fatalf("callback %d %q", w.Code, loc)
	}
	o.Wait()
	id, _ := strconv.ParseInt(strings.TrimPrefix(loc, "/manage/import?job="), 10, 64)
	if got, _ := e.svc.Store.Job(context.Background(), id); got.Status != "preview" || got.Service != "deezer" {
		t.Fatalf("after read %+v", got)
	}
	// another admin's callback does not take this admin's sign-in
	other := e.adminLogin("Мама")
	e.svc.Cancel(context.Background(), id)
	connect()
	if w := e.do("GET", "/api/import/oauth/deezer/callback?code=fr2", "", other); !strings.Contains(w.Header().Get("Location"), "error=") {
		t.Fatalf("other admin %q", w.Header().Get("Location"))
	}
	if w := e.do("GET", "/api/import/oauth/deezer/callback?code=fr2", "", e.admin); !strings.HasPrefix(w.Header().Get("Location"), "/manage/import?job=") {
		t.Fatalf("still the admin's %q", w.Header().Get("Location"))
	}
}

// stubPublic finds the playlist of …/playlist/1001 and reads it; read
// waits for release (nil: at once) and reports its ctx's end.
type stubPublic struct {
	release chan struct{}
	stopped chan error
}

func (s *stubPublic) FindPublic(ctx context.Context, link string) (playlistimport.PublicPlaylist, error) {
	switch {
	case strings.HasSuffix(link, "/s/slow"): // Deezer does not answer
		select {
		case <-ctx.Done():
			return playlistimport.PublicPlaylist{}, ctx.Err()
		case <-time.After(10 * time.Second):
			return playlistimport.PublicPlaylist{}, errors.New("not cut short")
		}
	case strings.HasSuffix(link, "/s/down"):
		return playlistimport.PublicPlaylist{}, fmt.Errorf("%w (deezer link: connection refused)", musicimport.ErrConnect)
	case strings.HasSuffix(link, "/playlist/1001"):
		return playlistimport.PublicPlaylist{ID: "1001", Name: "Дорога"}, nil
	case strings.HasSuffix(link, "/playlist/404404"):
		return playlistimport.PublicPlaylist{}, playlistimport.ErrNotFound
	}
	return playlistimport.PublicPlaylist{}, playlistimport.ErrBadLink
}

func (s *stubPublic) ReadPublic(ctx context.Context, p playlistimport.PublicPlaylist) (playlistimport.Snapshot, error) {
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			s.stopped <- ctx.Err()
			return playlistimport.Snapshot{}, ctx.Err()
		case <-time.After(10 * time.Second): // a failing test still ends
		}
	}
	return playlistimport.Snapshot{Service: "deezer", Tracks: []playlistimport.SnapTrack{{Key: "deezer:3135559", Artist: "Кино", Title: "Кукушка"}},
		Playlists: []playlistimport.SnapPlaylist{{ID: p.ID, Name: p.Name, Readable: true, Entries: []string{"deezer:3135559"}}}}, nil
}

func TestDeezerPublicPlaylistAPI(t *testing.T) {
	e := newImportEnv(t)
	o := e.withOAuth("") // Deezer itself not configured: the link needs no sign-in
	pub := &stubPublic{}
	e.svc.Public = pub
	ctx := context.Background()
	body := func(link string) string {
		b, _ := json.Marshal(map[string]string{"url": link})
		return string(b)
	}
	for _, c := range []struct {
		link string
		code int
	}{
		{"https://evil.example/playlist/1001x", 400},
		{"https://www.deezer.com/ru/playlist/404404", 404},
		{strings.Repeat("a", 5000), 400},
	} {
		w := e.do("POST", "/api/import/deezer/public", body(c.link), e.admin)
		if w.Code != c.code || decodeJSON[struct{ Error string }](t, w.Body.Bytes()).Error != "Не найдено" {
			t.Fatalf("%.40s: %d %s", c.link, w.Code, w.Body)
		}
	}
	// the check the admin waits for is bounded; Deezer not answering is «try again»
	old := publicCheckTimeout
	publicCheckTimeout = 50 * time.Millisecond
	t.Cleanup(func() { publicCheckTimeout = old })
	for _, link := range []string{"https://link.deezer.com/s/slow", "https://link.deezer.com/s/down"} {
		start := time.Now()
		w := e.do("POST", "/api/import/deezer/public", body(link), e.admin)
		if w.Code != 400 || decodeJSON[struct{ Error string }](t, w.Body.Bytes()).Error != "Не удалось подключиться к сервису. Попробуйте ещё раз." ||
			time.Since(start) > 5*time.Second {
			t.Fatalf("%s: %d %s in %s", link, w.Code, w.Body, time.Since(start))
		}
	}
	if w := e.do("GET", "/api/import/jobs", "", e.admin); w.Body.String() != `{"jobs":[]}`+"\n" && w.Body.String() != `{"jobs":[]}` {
		t.Fatalf("a job was made: %s", w.Body)
	}
	if w := e.do("POST", "/api/import/deezer/public", body("https://www.deezer.com/ru/playlist/1001"), e.kid("")); w.Code != 403 {
		t.Fatalf("kid %d", w.Code)
	}
	w := e.do("POST", "/api/import/deezer/public", body("https://www.deezer.com/ru/playlist/1001"), e.admin)
	j := decodeJSON[jobOut](t, w.Body.Bytes())
	if w.Code != 201 || j.Service != "deezer" || j.Status != "reading" {
		t.Fatalf("public %d %s", w.Code, w.Body)
	}
	o.Wait()
	got := e.do("GET", "/api/import/jobs/"+itoa(j.ID), "", e.admin)
	if !strings.Contains(got.Body.String(), `"status":"preview"`) || !strings.Contains(got.Body.String(), `"name":"Дорога","songs":1,"readable":true`) ||
		!strings.Contains(got.Body.String(), `"liked":0`) {
		t.Fatalf("preview %s", got.Body)
	}
	// a job of the service is open
	if w := e.do("POST", "/api/import/deezer/public", body("https://www.deezer.com/ru/playlist/1001"), e.admin); w.Code != 409 {
		t.Fatalf("open job %d %s", w.Code, w.Body)
	}
	e.svc.Cancel(ctx, j.ID)

	// cancelling the reading job stops the read
	pub.release, pub.stopped = make(chan struct{}), make(chan error, 1)
	w = e.do("POST", "/api/import/deezer/public", body("https://www.deezer.com/ru/playlist/1001"), e.admin)
	if w.Code != 201 {
		t.Fatalf("second %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/import/jobs/"+itoa(decodeJSON[jobOut](t, w.Body.Bytes()).ID)+"/cancel", "", e.admin); w.Code != 204 {
		t.Fatalf("cancel %d", w.Code)
	}
	select {
	case err := <-pub.stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stopped by %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the read went on")
	}
	o.Wait()

	// no reader on the server
	e.svc.Public = nil
	if w := e.do("POST", "/api/import/deezer/public", body("https://www.deezer.com/ru/playlist/1001"), e.admin); w.Code != 400 {
		t.Fatalf("no reader %d", w.Code)
	}
}
