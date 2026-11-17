package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/boot"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

var (
	admin = addonkit.User{ID: 1, Name: "admin", IsAdmin: true, Lang: "ru"}
	adult = addonkit.User{ID: 2, Name: "adult", Lang: "ru"}
)

// fakeAcquire is the downloads service: three torrents, pause/resume flip
// their status, every call is recorded.
type fakeAcquire struct {
	mu      sync.Mutex
	views   []acquire.TorrentView
	calls   []string
	cache   []int64
	conn    *torrent.Connectivity
	status  acquire.StreamStatus
	failIH  string
	promote string
}

func newFakeAcquire() *fakeAcquire {
	return &fakeAcquire{views: []acquire.TorrentView{
		{Status: torrent.Status{InfoHash: "aa", Name: "Один", Status: "downloading"}},
		{Status: torrent.Status{InfoHash: "bb", Name: "Два", Status: "seeding"}},
		{Status: torrent.Status{InfoHash: "cc", Name: "Три", Status: "paused"}},
	}}
}

func (f *fakeAcquire) call(s string, ih string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s+" "+ih)
	if ih == f.failIH {
		return acquire.ErrNotFound
	}
	return nil
}

func (f *fakeAcquire) set(ih, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.views {
		if f.views[i].InfoHash == ih {
			f.views[i].Status.Status = status
		}
	}
}

func (f *fakeAcquire) Torrents() []acquire.TorrentView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]acquire.TorrentView(nil), f.views...)
}
func (f *fakeAcquire) VideoItems(context.Context) map[string]int64 { return nil }
func (f *fakeAcquire) Pause(_ context.Context, ih string) error {
	if err := f.call("pause", ih); err != nil {
		return err
	}
	f.set(ih, "paused")
	return nil
}
func (f *fakeAcquire) Resume(_ context.Context, ih string) error {
	if err := f.call("resume", ih); err != nil {
		return err
	}
	f.set(ih, "downloading")
	return nil
}
func (f *fakeAcquire) RetryOrganize(_ context.Context, ih string) error {
	return f.call("organize", ih)
}
func (f *fakeAcquire) ForceOrganize(_ context.Context, ih string) error {
	return f.call("organize-force", ih)
}
func (f *fakeAcquire) DismissAutoNote(_ context.Context, ih string) error {
	return f.call("dismiss-note", ih)
}
func (f *fakeAcquire) Remove(_ context.Context, ih string, data bool) error {
	return f.call(fmt.Sprintf("remove data=%v", data), ih)
}
func (f *fakeAcquire) Promote(_ context.Context, ih string, force bool) (string, error) {
	if err := f.call(fmt.Sprintf("promote force=%v", force), ih); err != nil {
		return "", err
	}
	return f.promote, nil
}
func (f *fakeAcquire) TorrentStatus(ih string, file int) (acquire.StreamStatus, error) {
	if err := f.call(fmt.Sprintf("status file=%d", file), ih); err != nil {
		return acquire.StreamStatus{}, err
	}
	return f.status, nil
}
func (f *fakeAcquire) Connectivity() (torrent.Connectivity, bool) {
	if f.conn == nil {
		return torrent.Connectivity{}, false
	}
	return *f.conn, true
}
func (f *fakeAcquire) SetStreamCacheBytes(n int64) {
	f.mu.Lock()
	f.cache = append(f.cache, n)
	f.mu.Unlock()
}

// fakeEngine records every no-seeding value it is given, with the state
// file as it was at that moment (the file is written first), and the
// upload limits.
type fakeEngine struct {
	mu      sync.Mutex
	dir     string
	noSeed  []string // "true|false:<state file>"
	uploads []int64
}

func (e *fakeEngine) SetNoSeeding(on bool) {
	b, _ := os.ReadFile(filepath.Join(e.dir, boot.StateFile))
	e.mu.Lock()
	e.noSeed = append(e.noSeed, fmt.Sprintf("%v:%s", on, strings.TrimSpace(string(b))))
	e.mu.Unlock()
}
func (e *fakeEngine) SetUploadLimit(bps int64) {
	e.mu.Lock()
	e.uploads = append(e.uploads, bps)
	e.mu.Unlock()
}
func (e *fakeEngine) Close() error { return nil }

func (e *fakeEngine) sawSeeding() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, v := range e.noSeed {
		if strings.HasPrefix(v, "false:") {
			return true
		}
	}
	return false
}

type env struct {
	t            *testing.T
	profileLangs map[int64][]string // Deps.Languages by profile
	dir          string
	acq          *fakeAcquire
	eng          *fakeEngine
	st           store.Settings
	ns           *boot.NoSeed
	src          *indexer.Manager
	defs         string
	h            http.Handler
	out          int
	trick        bool
}

// newEnv: forced is ANTRESOL_TORRENTS_NO_SEEDING=on; saved is the stored
// «Не раздавать» before the start. The state file is written as at boot.
func newEnv(t *testing.T, forced, saved bool) *env {
	t.Helper()
	dir := t.TempDir()
	d, err := store.Open(filepath.Join(dir, "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	e := &env{t: t, dir: dir, acq: newFakeAcquire(), eng: &fakeEngine{dir: dir}, st: store.Settings{DB: d}}
	// the stored setting from before, saved the only way there is (NoSeed)
	if err := boot.NewNoSeed(false, dir, e.st, e.eng).Set(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	e.ns = boot.NewNoSeed(forced, dir, e.st, e.eng)
	if err := e.ns.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.eng.noSeed = nil // what boot did is not the routes' doing
	e.defs = filepath.Join(dir, "definitions")
	if err := os.MkdirAll(e.defs, 0o755); err != nil {
		t.Fatal(err)
	}
	e.reload()
	return e
}

func (e *env) langs(_ context.Context, id int64) []string { return e.profileLangs[id] }

func (e *env) reload() {
	e.src = indexer.NewManager(e.st.DB, indexer.NewCatalog(e.defs), indexer.Options{})
	e.h = Routes(Deps{Acquire: e.acq, Sources: e.src, Settings: e.st, NoSeed: e.ns, Upload: e.eng,
		StreamCacheDefaultGB: 50, Outside: func() int { return e.out }, Languages: e.langs})
}

func (e *env) do(method, path, body string, u *addonkit.User) *httptest.ResponseRecorder {
	e.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	if u != nil {
		b, _ := json.Marshal(u)
		r.Header.Set(addonkit.UserHeader, string(b))
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) state() string {
	b, err := os.ReadFile(filepath.Join(e.dir, boot.StateFile))
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func errorOf(w *httptest.ResponseRecorder) string {
	var out struct{ Error string }
	json.Unmarshal(w.Body.Bytes(), &out)
	return out.Error
}

func russian(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

var everyRoute = [][2]string{
	{"GET", "/api/admin/torrents"},
	{"POST", "/api/admin/torrents/aa/pause"},
	{"POST", "/api/admin/torrents/aa/resume"},
	{"POST", "/api/admin/torrents/aa/organize"},
	{"POST", "/api/admin/torrents/aa/organize-force"},
	{"POST", "/api/admin/torrents/aa/dismiss-note"},
	{"POST", "/api/admin/torrents/aa/promote"},
	{"DELETE", "/api/admin/torrents/aa?data=1"},
	{"GET", "/api/admin/torrents/aa/status"},
	{"GET", "/api/admin/engine"},
	{"GET", "/api/admin/sources/catalog"},
	{"GET", "/api/admin/sources/catalog/x"},
	{"GET", "/api/admin/sources"},
	{"POST", "/api/admin/sources"},
	{"POST", "/api/admin/sources/import-prowlarr"},
	{"PATCH", "/api/admin/sources/1"},
	{"DELETE", "/api/admin/sources/1"},
	{"POST", "/api/admin/sources/1/test"},
	{"GET", "/api/admin/settings"},
	{"PUT", "/api/admin/settings"},
	{"PUT", "/api/admin/settings/no-seeding"},
}

func TestAdmin_NonAdminRefused(t *testing.T) {
	e := newEnv(t, false, false)
	kid := addonkit.User{ID: 3, IsAdmin: true, IsKid: true}
	for _, rt := range everyRoute {
		for _, u := range []*addonkit.User{&adult, &kid, nil} {
			if w := e.do(rt[0], rt[1], `{}`, u); w.Code != http.StatusForbidden {
				t.Errorf("%s %s as %+v: %d %s", rt[0], rt[1], u, w.Code, w.Body)
			}
		}
	}
	if len(e.acq.calls) != 0 || len(e.eng.noSeed) != 0 || len(e.eng.uploads) != 0 {
		t.Fatalf("a refused request reached a service: %v %v %v", e.acq.calls, e.eng.noSeed, e.eng.uploads)
	}
}

func TestTorrents_ListAndPause(t *testing.T) {
	e := newEnv(t, false, false)
	var list []acquire.TorrentView
	w := e.do("GET", "/api/admin/torrents", "", &admin)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list) != 3 {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/admin/torrents/aa/pause", "", &admin); w.Code != http.StatusNoContent {
		t.Fatalf("pause: %d %s", w.Code, w.Body)
	}
	json.Unmarshal(e.do("GET", "/api/admin/torrents", "", &admin).Body.Bytes(), &list)
	if list[0].Status.Status != "paused" {
		t.Fatalf("after pause %+v", list[0])
	}
	for _, a := range []string{"resume", "organize", "organize-force", "dismiss-note"} {
		if w := e.do("POST", "/api/admin/torrents/bb/"+a, "", &admin); w.Code != http.StatusNoContent {
			t.Errorf("%s: %d %s", a, w.Code, w.Body)
		}
	}
	if w := e.do("POST", "/api/admin/torrents/bb/explode", "", &admin); w.Code != http.StatusNotFound {
		t.Errorf("unknown action: %d", w.Code)
	}
	want := "pause aa,resume bb,organize bb,organize-force bb,dismiss-note bb"
	if got := strings.Join(e.acq.calls, ","); got != want {
		t.Fatalf("calls %s, want %s", got, want)
	}
	// an error without a user message (acquire.UserMessage) is ours: 500,
	// with a Russian text, as the old build answered
	e.acq.failIH = "zz"
	if w := e.do("POST", "/api/admin/torrents/zz/pause", "", &admin); w.Code != http.StatusInternalServerError || !russian(errorOf(w)) {
		t.Fatalf("failing torrent: %d %s", w.Code, w.Body)
	}
}

func TestTorrents_PromoteStatusEngine(t *testing.T) {
	e := newEnv(t, false, false)
	e.acq.promote = "queued"
	w := e.do("POST", "/api/admin/torrents/aa/promote", `{"force":true}`, &admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"queued"`) {
		t.Fatalf("promote: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/admin/torrents/aa/status?file=2", "", &admin); w.Code != 200 {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/admin/engine", "", &admin); w.Code != http.StatusNoContent {
		t.Fatalf("engine unknown: %d", w.Code)
	}
	e.acq.conn = &torrent.Connectivity{ListenPort: 6881, IncomingBlocked: true}
	w = e.do("GET", "/api/admin/engine", "", &admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "6881") {
		t.Fatalf("engine: %d %s", w.Code, w.Body)
	}
	if got := strings.Join(e.acq.calls, ","); got != "promote force=true aa,status file=2 aa" {
		t.Fatalf("calls %s", got)
	}
}

func TestTorrents_RemoveKeepsData(t *testing.T) {
	e := newEnv(t, false, false)
	for _, q := range []string{"?data=0", "", "?data=1"} {
		if w := e.do("DELETE", "/api/admin/torrents/aa"+q, "", &admin); w.Code != http.StatusNoContent {
			t.Fatalf("%q: %d %s", q, w.Code, w.Body)
		}
	}
	if got := strings.Join(e.acq.calls, ","); got != "remove data=false aa,remove data=false aa,remove data=true aa" {
		t.Fatalf("calls %s", got)
	}
}

// a definition served by a local test server
func (e *env) def(id, name string) {
	e.t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body><table></table></body></html>")
	}))
	e.t.Cleanup(srv.Close)
	def := fmt.Sprintf(`---
id: %s
name: %s
description: test tracker
language: ru-RU
type: public
links: [%q]
caps:
  categorymappings:
    - {id: 1, cat: Movies, desc: Movies}
  modes: {search: [q]}
settings: []
search:
  paths:
    - path: search
  inputs:
    q: "{{ .Keywords }}"
  rows:
    selector: tr
  fields:
    title: {selector: td.t}
    size: {selector: td.s}
`, id, name, srv.URL+"/")
	if err := os.WriteFile(filepath.Join(e.defs, id+".yml"), []byte(def), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.reload()
}

func TestSources_CRUD(t *testing.T) {
	e := newEnv(t, false, false)
	e.def("fakesrc", "Фейк")
	w := e.do("GET", "/api/admin/sources/catalog?q=фейк", "", &admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"fakesrc"`) {
		t.Fatalf("catalog: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/admin/sources/catalog/fakesrc", "", &admin); w.Code != 200 || !strings.Contains(w.Body.String(), `"fields":[`) {
		t.Fatalf("catalog entry: %d %s", w.Code, w.Body)
	}
	if w := e.do("GET", "/api/admin/sources/catalog/nope", "", &admin); w.Code != 404 || !russian(errorOf(w)) {
		t.Fatalf("missing entry: %d %s", w.Code, w.Body)
	}
	w = e.do("POST", "/api/admin/sources", `{"definitionId":"fakesrc","settings":{}}`, &admin)
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	var src indexer.Source
	json.Unmarshal(w.Body.Bytes(), &src)
	var list []indexer.Source
	json.Unmarshal(e.do("GET", "/api/admin/sources", "", &admin).Body.Bytes(), &list)
	if len(list) != 1 || list[0].ID != src.ID {
		t.Fatalf("list %+v", list)
	}
	id := fmt.Sprint(src.ID)
	w = e.do("PATCH", "/api/admin/sources/"+id, `{"enabled":false}`, &admin)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("patch: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", "/api/admin/sources/"+id+"/test", "", &admin); w.Code != 200 {
		t.Fatalf("test: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/api/admin/sources/"+id, "", &admin); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/api/admin/sources/"+id, "", &admin); w.Code != 404 || errorOf(w) != "Источник не найден" {
		t.Fatalf("delete again: %d %s", w.Code, w.Body)
	}
	if w := e.do("PATCH", "/api/admin/sources/x", `{}`, &admin); w.Code != 400 {
		t.Fatalf("bad id: %d", w.Code)
	}
}

type view struct {
	UploadLimited    bool     `json:"uploadLimited"`
	UploadLimitKBs   int64    `json:"uploadLimitKBs"`
	StreamCacheGB    float64  `json:"streamCacheGB"`
	NoSeeding        bool     `json:"noSeeding"`
	NoSeedingForced  bool     `json:"noSeedingForced"`
	Quality          int      `json:"quality"`
	Voice            string   `json:"voice"`
	Voices           []string `json:"voices"`
	Languages        []string `json:"languages"`
	BookLangs        []string `json:"bookLangs"`
	MaxSizeGB        float64  `json:"maxSizeGB"`
	PreferLossless   *bool    `json:"preferLossless"`
	MusicFormat      string   `json:"musicFormat"`
	MP3Bitrate       string   `json:"mp3Bitrate"`
	OutsideFileRoots *int     `json:"outsideFileRoots"`
}

func (e *env) get(u *addonkit.User) view {
	e.t.Helper()
	w := e.do("GET", "/api/admin/settings", "", u)
	if w.Code != 200 {
		e.t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	var v view
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		e.t.Fatal(err)
	}
	return v
}

func TestSettings_RoundTrip(t *testing.T) {
	e := newEnv(t, false, false)
	e.out = 3
	v := e.get(&admin)
	if v.StreamCacheGB != 50 || v.Quality != 1080 || v.Voice != "dub" || len(v.Voices) != 1 || v.MusicFormat != "lossless" ||
		v.MP3Bitrate != "320" || v.PreferLossless == nil || !*v.PreferLossless || v.NoSeeding || v.NoSeedingForced {
		t.Fatalf("defaults %+v", v)
	}
	if len(v.Languages) == 0 || v.Languages[0] != "ru" || v.OutsideFileRoots == nil || *v.OutsideFileRoots != 3 {
		t.Fatalf("languages/outside %+v", v)
	}
	body := `{"uploadLimited":true,"uploadLimitKBs":100,"streamCacheGB":30,"quality":720,"voices":["original","dub"],
	  "maxSizeGB":20,"bookLangs":["en","ru"],"musicFormat":"mp3","mp3Bitrate":"v0","noSeeding":true}`
	w := e.do("PUT", "/api/admin/settings", body, &admin)
	if w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	var out view
	json.Unmarshal(w.Body.Bytes(), &out)
	got := e.get(&admin)
	if out.Quality != 720 || got.Quality != 720 || !got.UploadLimited || got.UploadLimitKBs != 100 || got.StreamCacheGB != 30 ||
		strings.Join(got.Voices, ",") != "original,dub" || got.Voice != "original" || got.MaxSizeGB != 20 ||
		strings.Join(got.BookLangs, ",") != "ru,en" || got.MusicFormat != "mp3" || got.MP3Bitrate != "v0" || *got.PreferLossless {
		t.Fatalf("after put %+v (answer %+v)", got, out)
	}
	if got.NoSeeding || e.state() != "off" || len(e.eng.noSeed) != 0 {
		t.Fatalf("a settings save changed no-seeding: %+v state %s engine %v", got, e.state(), e.eng.noSeed)
	}
	if len(e.eng.uploads) != 1 || e.eng.uploads[0] != 100*1024 || len(e.acq.cache) != 1 || e.acq.cache[0] != 30<<30 {
		t.Fatalf("live apply: uploads %v cache %v", e.eng.uploads, e.acq.cache)
	}
	// a client that sends only some fields keeps the rest
	if w := e.do("PUT", "/api/admin/settings", `{"quality":2160}`, &admin); w.Code != 200 {
		t.Fatalf("partial: %d %s", w.Code, w.Body)
	}
	if g := e.get(&admin); g.Quality != 2160 || !g.UploadLimited || g.MusicFormat != "mp3" || g.StreamCacheGB != 30 {
		t.Fatalf("partial kept %+v", g)
	}
	// an older client: one «Озвучка»
	e.do("PUT", "/api/admin/settings", `{"voice":"dub"}`, &admin)
	if g := e.get(&admin); strings.Join(g.Voices, ",") != "dub" || g.Voice != "dub" {
		t.Fatalf("voice only %+v", g)
	}
	if w := e.do("PUT", "/api/admin/settings", `{"uploadLimited":true,"uploadLimitKBs":0}`, &admin); w.Code != 400 ||
		errorOf(w) != "Укажите скорость отдачи больше нуля" {
		t.Fatalf("upload 0: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "/api/admin/settings", `{nope`, &admin); w.Code != 400 {
		t.Fatalf("bad json: %d", w.Code)
	}
}

// The validation cases of the old build's /api/settings (moved
// out of the core): each one is a 400 with a Russian message, nothing is
// saved and nothing applied.
func TestSettingsValidation(t *testing.T) {
	e := newEnv(t, false, false)
	raw := func() string { return e.do("GET", "/api/admin/settings", "", &admin).Body.String() }
	before := raw()
	for _, body := range []string{
		`{"quality":999}`,
		`{"voice":"klingon"}`,
		`{"voices":["dub","dub"]}`,
		`{"voices":[]}`,
		`{"maxSizeGB":-1}`,
		`{"maxSizeGB":1001}`,
		`{"streamCacheGB":0}`,
		`{"uploadLimited":true,"uploadLimitKBs":0}`,
		`{"bookLangs":["xx"]}`,
		`{"musicFormat":"ogg"}`,
		`{"musicFormat":"mp3","mp3Bitrate":"999"}`,
	} {
		w := e.do("PUT", "/api/admin/settings", body, &admin)
		if w.Code != http.StatusBadRequest || !russian(errorOf(w)) {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	if after := raw(); after != before {
		t.Fatalf("saved on invalid input: %+v → %+v", before, after)
	}
	if len(e.eng.uploads)+len(e.acq.cache) != 0 {
		t.Fatalf("applied on invalid input: %v %v", e.eng.uploads, e.acq.cache)
	}
}

func TestSettingsMusicFormat(t *testing.T) {
	e := newEnv(t, false, false)
	put := func(body string) int { return e.do("PUT", "/api/admin/settings", body, &admin).Code }
	if put(`{"musicFormat":"mp3","mp3Bitrate":"v0"}`) != 200 {
		t.Fatal("mp3 v0")
	}
	if g := e.get(&admin); g.MusicFormat != "mp3" || g.MP3Bitrate != "v0" || *g.PreferLossless {
		t.Fatalf("mp3 %+v", g)
	}
	// a client that knows only «без потерь» switches the format, keeps the bitrate
	if put(`{"preferLossless":true}`) != 200 {
		t.Fatal("lossless")
	}
	if g := e.get(&admin); g.MusicFormat != "lossless" || g.MP3Bitrate != "v0" || !*g.PreferLossless {
		t.Fatalf("legacy on %+v", g)
	}
	if put(`{"preferLossless":false}`) != 200 {
		t.Fatal("lossless off")
	}
	if g := e.get(&admin); g.MusicFormat != "mp3" {
		t.Fatalf("legacy off %+v", g)
	}
	if put(`{"musicFormat":"any"}`) != 200 {
		t.Fatal("any")
	}
	if g := e.get(&admin); g.MusicFormat != "any" || *g.PreferLossless {
		t.Fatalf("any %+v", g)
	}
}

func TestNoSeeding_ForcedRefused(t *testing.T) {
	e := newEnv(t, true, false)
	if g := e.get(&admin); !g.NoSeeding || !g.NoSeedingForced {
		t.Fatalf("forced view %+v", g)
	}
	for _, on := range []string{"false", "true"} {
		w := e.do("PUT", "/api/admin/settings/no-seeding", `{"on":`+on+`}`, &admin)
		if w.Code != http.StatusConflict || errorOf(w) != "Задано на сервере" {
			t.Fatalf("on=%s: %d %s", on, w.Code, w.Body)
		}
	}
	if saved, _ := e.st.NoSeeding(context.Background()); saved {
		t.Fatal("the setting changed while forced")
	}
	if e.state() != "on forced" || len(e.eng.noSeed) != 0 {
		t.Fatalf("state %q engine %v", e.state(), e.eng.noSeed)
	}
}

func TestNoSeeding_ToggleWritesStateFirst(t *testing.T) {
	e := newEnv(t, false, false)
	if w := e.do("PUT", "/api/admin/settings/no-seeding", `{"on":true}`, &admin); w.Code != http.StatusNoContent {
		t.Fatalf("on: %d %s", w.Code, w.Body)
	}
	if strings.Join(e.eng.noSeed, ",") != "true:on" || e.state() != "on" {
		t.Fatalf("engine %v state %q", e.eng.noSeed, e.state())
	}
	if g := e.get(&admin); !g.NoSeeding || g.NoSeedingForced {
		t.Fatalf("view %+v", g)
	}
	if w := e.do("PUT", "/api/admin/settings/no-seeding", `{"on":false}`, &admin); w.Code != http.StatusNoContent {
		t.Fatalf("off: %d %s", w.Code, w.Body)
	}
	if strings.Join(e.eng.noSeed, ",") != "true:on,false:off" {
		t.Fatalf("engine %v", e.eng.noSeed)
	}
	for _, body := range []string{`{}`, `{"on":"yes"}`, `{nope`} {
		if w := e.do("PUT", "/api/admin/settings/no-seeding", body, &admin); w.Code != 400 {
			t.Errorf("%s: %d", body, w.Code)
		}
	}
}

// Forced, the stored setting off from before: a general settings save
// (even one that sends noSeeding:false) never unparks the engine.
func TestSettingsSave_ForcedKeepsParked(t *testing.T) {
	e := newEnv(t, true, false)
	body := `{"uploadLimited":true,"uploadLimitKBs":100,"streamCacheGB":20,"noSeeding":false,"noSeedingForced":false}`
	if w := e.do("PUT", "/api/admin/settings", body, &admin); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if e.eng.sawSeeding() || e.state() != "on forced" {
		t.Fatalf("engine %v state %q", e.eng.noSeed, e.state())
	}
	if g := e.get(&admin); !g.NoSeeding || !g.NoSeedingForced {
		t.Fatalf("view %+v", g)
	}
}

func TestSettingsSave_SettingOnKeepsParked(t *testing.T) {
	e := newEnv(t, false, true)
	body := `{"uploadLimited":true,"uploadLimitKBs":100,"noSeeding":false}`
	if w := e.do("PUT", "/api/admin/settings", body, &admin); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if e.eng.sawSeeding() || e.state() != "on" {
		t.Fatalf("engine %v state %q", e.eng.noSeed, e.state())
	}
	if saved, _ := e.st.NoSeeding(context.Background()); !saved {
		t.Fatal("the general save turned the setting off")
	}
}

func TestAdmin_EnglishErrors(t *testing.T) {
	e := newEnv(t, true, false)
	en := admin
	en.Lang = "en"
	if w := e.do("PUT", "/api/admin/settings", `{"uploadLimited":true,"uploadLimitKBs":0}`, &en); errorOf(w) != "Enter an upload speed greater than zero" {
		t.Fatalf("validation: %d %s", w.Code, w.Body)
	}
	if w := e.do("PUT", "/api/admin/settings/no-seeding", `{"on":false}`, &en); w.Code != 409 || russian(errorOf(w)) {
		t.Fatalf("forced: %d %s", w.Code, w.Body)
	}
	if w := e.do("DELETE", "/api/admin/sources/9", "", &en); w.Code != 404 || russian(errorOf(w)) {
		t.Fatalf("missing source: %d %s", w.Code, w.Body)
	}
	if g := e.get(&en); len(g.Languages) == 0 || g.Languages[0] != "en" {
		t.Fatalf("languages of an English profile %v", g.Languages)
	}
}

var _ = errors.New

// The settings' language order is the profile's «Языки», then the
// other known languages; a profile without them: its interface language.
func TestSettings_LanguagesOfProfile(t *testing.T) {
	e := newEnv(t, false, false)
	e.profileLangs = map[int64][]string{1: {"de", "en"}}
	g := e.get(&admin)
	if len(g.Languages) < 3 || g.Languages[0] != "de" || g.Languages[1] != "en" || g.Languages[2] != "ru" {
		t.Fatalf("languages %v", g.Languages)
	}
	e.profileLangs = nil
	if g := e.get(&admin); g.Languages[0] != "ru" {
		t.Fatalf("without «Языки» %v", g.Languages)
	}
}
