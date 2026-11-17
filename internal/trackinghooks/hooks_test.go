package trackinghooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/pinned"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings"
	"github.com/antresol-app/antresol/addonkit"
)

// fakeScanner answers Scan with srcs or err and records what it was asked.
type fakeScanner struct {
	mu   sync.Mutex
	srcs []tracking.Source
	err  error
	got  []tracking.Title
	eps  [][]tracking.Episode
}

func (f *fakeScanner) Scan(_ context.Context, t tracking.Title, eps []tracking.Episode) ([]tracking.Source, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, t)
	f.eps = append(f.eps, eps)
	return f.srcs, f.err
}

// syncs records the Sync calls (the title ids).
type syncs struct {
	mu    sync.Mutex
	ids   []int64
	ctxOK []bool // the call's ctx was still alive (the run's, not the hook's)
}

func (s *syncs) sync(ctx context.Context, t tracking.Title) error {
	time.Sleep(20 * time.Millisecond) // past the hook's answer
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = append(s.ids, t.ID)
	s.ctxOK = append(s.ctxOK, ctx.Err() == nil)
	return nil
}

func (s *syncs) seen() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.ids...)
}

func call(t *testing.T, d Deps, hook, body string) (int, []byte) {
	t.Helper()
	m := &addonkit.HookMux{}
	Register(m, d)
	r := httptest.NewRequest("POST", "/hooks/"+hook, strings.NewReader(body))
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

const scanBody = `{"title":{"id":7,"kind":"series","tmdbId":100,"name":"Show","poster":"/p.jpg","status":"airing"},
	"episodes":[{"titleId":7,"season":1,"number":1,"name":"One","airDate":"2026-01-01"},{"titleId":7,"season":1,"number":2}]}`

func pinSources() []tracking.Source {
	i0, i1 := 0, 1
	return []tracking.Source{
		{TitleID: 7, Season: 1, Episode: 1, Kind: "pinned", Ref: "pin:1:0", Provider: "Tracker", Quality: "1080p",
			Labels: []string{"MVO"}, Meta: tracking.SourceMeta{SourceID: "3", Voice: "LostFilm", Lang: "ru", FileIndex: &i0}},
		{TitleID: 7, Season: 1, Episode: 2, Kind: "pinned", Ref: "pin:1:1", Provider: "Tracker", Quality: "1080p",
			Meta: tracking.SourceMeta{SourceID: "3", FileIndex: &i1}},
	}
}

// TestScanPinned_AnswersThenSyncsOnce: the pin's sources are the
// answer; then Sync runs once for the title, in the background on the
// run context (the hook's request has ended by then).
func TestScanPinned_AnswersThenSyncsOnce(t *testing.T) {
	sc := &fakeScanner{srcs: pinSources()}
	var s syncs
	run, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	d := Deps{Pinned: sc, Torrent: &fakeScanner{}, Sync: s.sync, Run: run, wait: &wg}
	code, b := call(t, d, "tracking.scan:pinned", scanBody)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if strings.Contains(string(b), "unchanged") {
		t.Fatalf("answer %s", b)
	}
	var a addonkit.ScanAnswer
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	if len(a.Sources) != 2 || a.Sources[0].Ref != "pin:1:0" || a.Sources[0].Meta.Voice != "LostFilm" ||
		a.Sources[0].Meta.FileIndex == nil || *a.Sources[0].Meta.FileIndex != 0 || a.Sources[1].Episode != 2 {
		t.Fatalf("sources %+v", a.Sources)
	}
	if len(s.seen()) != 0 {
		t.Fatal("Sync ran before the answer")
	}
	// the scanner got the title and its episodes as the core sent them
	want := tracking.Title{ID: 7, Kind: "series", TMDBID: 100, Name: "Show", Poster: "/p.jpg", Status: "airing"}
	if len(sc.got) != 1 || !reflect.DeepEqual(sc.got[0], want) || len(sc.eps[0]) != 2 ||
		sc.eps[0][0] != (tracking.Episode{TitleID: 7, Season: 1, Number: 1, Name: "One", AirDate: "2026-01-01"}) {
		t.Fatalf("scanned %+v %+v", sc.got, sc.eps)
	}
	wg.Wait()
	if got := s.seen(); !reflect.DeepEqual(got, []int64{7}) || !s.ctxOK[0] {
		t.Fatalf("syncs %v (ctx alive %v), want one of title 7", got, s.ctxOK)
	}
	// a scan the pin's scanner failed still syncs once (as the deferred
	// AfterScan did); without follows (Sync nil) nothing runs
	sc.err, sc.srcs = errors.New("db down"), nil
	if code, b := call(t, d, "tracking.scan:pinned", scanBody); code != 500 {
		t.Fatalf("failed scan: %d %s", code, b)
	}
	wg.Wait()
	if got := s.seen(); len(got) != 2 {
		t.Fatalf("syncs %v", got)
	}
	d.Sync = nil
	sc.err = nil
	if code, b := call(t, d, "tracking.scan:pinned", scanBody); code != 200 || string(b) != `{"sources":[]}` {
		t.Fatalf("no pin: %d %q", code, b)
	}
	wg.Wait()
	// the torrent kind never syncs
	d.Sync = s.sync
	if code, _ := call(t, d, "tracking.scan:torrent", scanBody); code != 200 {
		t.Fatal(code)
	}
	wg.Wait()
	if got := s.seen(); len(got) != 2 {
		t.Fatalf("syncs after torrent scan %v", got)
	}
}

// TestScanTorrent_Unchanged: the scanner's ErrUnchanged is
// {unchanged: true}; sources answer {sources}; none is {sources: []}.
func TestScanTorrent_Unchanged(t *testing.T) {
	sc := &fakeScanner{err: pinned.ErrUnchanged}
	d := Deps{Pinned: &fakeScanner{}, Torrent: sc}
	code, b := call(t, d, "tracking.scan:torrent", scanBody)
	if code != 200 || strings.TrimSpace(string(b)) != `{"unchanged":true}` {
		t.Fatalf("%d %s", code, b)
	}
	sc.err = errors.Join(errors.New("wrapped"), pinned.ErrUnchanged)
	if code, b := call(t, d, "tracking.scan:torrent", scanBody); code != 200 || strings.TrimSpace(string(b)) != `{"unchanged":true}` {
		t.Fatalf("wrapped: %d %s", code, b)
	}
	sc.err, sc.srcs = nil, []tracking.Source{{TitleID: 7, Season: 1, Episode: 1, Kind: "torrent", Ref: "https://x/dl/1"}}
	code, b = call(t, d, "tracking.scan:torrent", scanBody)
	var a addonkit.ScanAnswer
	if code != 200 || json.Unmarshal(b, &a) != nil || len(a.Sources) != 1 || a.Sources[0].Ref != "https://x/dl/1" || a.Unchanged {
		t.Fatalf("%d %s", code, b)
	}
	sc.srcs = nil
	if code, b := call(t, d, "tracking.scan:torrent", scanBody); code != 200 || strings.TrimSpace(string(b)) != `{"sources":[]}` {
		t.Fatalf("none: %d %s", code, b)
	}
	if code, b := call(t, d, "tracking.scan:torrent", `{"title":{"id":0}}`); code != 500 {
		t.Fatalf("no title: %d %s", code, b)
	}
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestTitlesHook: the active follows' titles.
func TestTitlesHook(t *testing.T) {
	d := testDB(t)
	if code, b := call(t, Deps{DB: d}, "tracking.titles", `{}`); code != 200 || strings.TrimSpace(string(b)) != `{"titles":[]}` {
		t.Fatalf("none: %d %s", code, b)
	}
	if _, err := d.Exec(`INSERT INTO follows (kind, tmdb_id, title, poster, active, created_at) VALUES
		('series', 100, 'Show', 'https://img/p.jpg', 1, 'x'), ('movie', 8, 'Off', '', 0, 'x')`); err != nil {
		t.Fatal(err)
	}
	code, b := call(t, Deps{DB: d}, "tracking.titles", `{}`)
	var a addonkit.TitlesAnswer
	if code != 200 || json.Unmarshal(b, &a) != nil ||
		!reflect.DeepEqual(a.Titles, []addonkit.ExtraTitle{{Kind: "series", TMDBID: 100, Name: "Show", Poster: "https://img/p.jpg"}}) {
		t.Fatalf("%d %s", code, b)
	}
}

// fakeUsers: the profiles of names, each with the uid "uid-<id>" but
// for those uids names.
type fakeUsers struct {
	names map[int64]string
	uids  map[int64]string
	err   error
	calls int
}

func (u *fakeUsers) Profiles(context.Context) ([]addonkit.User, error) {
	u.calls++
	if u.err != nil {
		return nil, u.err
	}
	out := []addonkit.User{}
	for _, id := range slices.Sorted(maps.Keys(u.names)) {
		uid, ok := u.uids[id]
		if !ok {
			uid = "uid-" + strconv.FormatInt(id, 10)
		}
		out = append(out, addonkit.User{ID: id, UID: uid, Name: u.names[id]})
	}
	return out, nil
}

// TestInterestHook_SkipsDeletedProfiles: the active follow's
// author while that profile is there; a deleted profile, an inactive
// follow, another title or a follow with no author give none; without the
// profile list the hook fails (the core then adds nobody).
func TestInterestHook_SkipsDeletedProfiles(t *testing.T) {
	d := testDB(t)
	if _, err := d.Exec(`INSERT INTO follows (kind, tmdb_id, title, user_id, user_uid, active, created_at) VALUES
		('series', 100, 'Show', 2, 'uid-2', 1, 'x'), ('series', 101, 'Gone', 9, 'uid-9', 1, 'x'), ('series', 102, 'Off', 2, 'uid-2', 0, 'x'),
		('movie', 100, 'Film', 3, 'uid-3', 1, 'x'), ('series', 103, 'Nobody', NULL, NULL, 1, 'x')`); err != nil {
		t.Fatal(err)
	}
	u := &fakeUsers{names: map[int64]string{1: "a", 2: "b", 3: "c"}}
	ask := func(kind string, tmdb int64) (int, []int64, string) {
		t.Helper()
		b, _ := json.Marshal(addonkit.InterestRequest{Kind: kind, TMDBID: tmdb})
		code, out := call(t, Deps{DB: d, Users: u}, "notify.interest", string(b))
		var a addonkit.InterestAnswer
		json.Unmarshal(out, &a)
		return code, a.UserIDs, string(out)
	}
	for _, c := range []struct {
		kind string
		tmdb int64
		want []int64
	}{
		{"series", 100, []int64{2}}, {"movie", 100, []int64{3}},
		{"series", 101, []int64{}}, {"series", 102, []int64{}}, {"series", 103, []int64{}}, {"series", 999, []int64{}},
	} {
		code, got, raw := ask(c.kind, c.tmdb)
		if code != 200 || got == nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %d: %d %s, want %v", c.kind, c.tmdb, code, raw, c.want)
		}
	}
	u.err = errors.New("core down")
	if code, _, raw := ask("series", 100); code != 500 {
		t.Fatalf("profiles unknown: %d %s", code, raw)
	}
	// no follow at all: the profiles are not asked
	u.err, u.calls = nil, 0
	if code, got, _ := ask("series", 999); code != 200 || len(got) != 0 || u.calls != 0 {
		t.Fatalf("%d %v calls %d", code, got, u.calls)
	}
}

// TestInterestHook_ReusedProfileID: a profile deleted while the
// hook ran with the list is not told again when a new profile takes its id.
func TestInterestHook_ReusedProfileID(t *testing.T) {
	d := testDB(t)
	if _, err := d.Exec(`INSERT INTO follows (kind, tmdb_id, title, user_id, user_uid, active, created_at) VALUES ('series', 100, 'Show', 9, 'uid-9', 1, 'x')`); err != nil {
		t.Fatal(err)
	}
	u := &fakeUsers{names: map[int64]string{1: "a"}}
	ask := func() []int64 {
		t.Helper()
		code, out := call(t, Deps{DB: d, Users: u}, "notify.interest", `{"kind":"series","tmdbId":100}`)
		var a addonkit.InterestAnswer
		if code != 200 || json.Unmarshal(out, &a) != nil {
			t.Fatalf("%d %s", code, out)
		}
		return a.UserIDs
	}
	if got := ask(); len(got) != 0 {
		t.Fatalf("deleted profile told: %v", got)
	}
	u.names[9], u.uids = "new", map[int64]string{9: "B"} // a new profile with the deleted one's id
	if got := ask(); len(got) != 0 {
		t.Fatalf("the new profile 9 inherited the letters: %v", got)
	}
}

// TestInterestHook_UID: the follow is told by uid: profile (3, A)
// deleted and created again as (3, B) before any list was read without
// it, B is not told; while A is there under any id, its id now is.
func TestInterestHook_UID(t *testing.T) {
	d := testDB(t)
	if _, err := d.Exec(`INSERT INTO follows (kind, tmdb_id, title, user_id, user_uid, active, created_at) VALUES ('series', 100, 'Show', 3, 'A', 1, 'x')`); err != nil {
		t.Fatal(err)
	}
	u := &fakeUsers{names: map[int64]string{1: "a", 3: "Мама"}, uids: map[int64]string{3: "A"}}
	ask := func() []int64 {
		t.Helper()
		code, out := call(t, Deps{DB: d, Users: u}, "notify.interest", `{"kind":"series","tmdbId":100}`)
		var a addonkit.InterestAnswer
		if code != 200 || json.Unmarshal(out, &a) != nil {
			t.Fatalf("%d %s", code, out)
		}
		return a.UserIDs
	}
	if got := ask(); !reflect.DeepEqual(got, []int64{3}) {
		t.Fatalf("A: %v", got)
	}
	u.names[3], u.uids[3] = "Новый", "B"
	if got := ask(); len(got) != 0 {
		t.Fatalf("B told of A's follow: %v", got)
	}
}

// TestOrigLangHook: the file's title's original language, "" unknown.
func TestOrigLangHook(t *testing.T) {
	var asked []int64
	d := Deps{OrigLang: func(_ context.Context, id int64) string {
		asked = append(asked, id)
		if id == 5 {
			return "ja"
		}
		return ""
	}}
	if code, b := call(t, d, "voiceover.origLang", `{"fileId":5}`); code != 200 || strings.TrimSpace(string(b)) != `{"lang":"ja"}` {
		t.Fatalf("%d %s", code, b)
	}
	if code, b := call(t, d, "voiceover.origLang", `{"fileId":6}`); code != 200 || strings.TrimSpace(string(b)) != `{"lang":""}` {
		t.Fatalf("%d %s", code, b)
	}
	if code, b := call(t, d, "voiceover.origLang", `{"fileId":0}`); code != 200 || strings.TrimSpace(string(b)) != `{"lang":""}` {
		t.Fatalf("no file: %d %s", code, b)
	}
	if !reflect.DeepEqual(asked, []int64{5, 6}) {
		t.Fatalf("asked %v", asked)
	}
}

// TestRegister_AllHooks: every tracking hook the manifest declares has a
// handler (none answers 404), with the minimal deps of a build without
// TMDB (no follows: Sync and Users nil).
func TestRegister_AllHooks(t *testing.T) {
	d := Deps{Pinned: &fakeScanner{}, Torrent: &fakeScanner{}, DB: testDB(t),
		OrigLang: func(context.Context, int64) string { return "" }}
	for hook, body := range map[string]string{
		"tracking.scan:pinned": scanBody, "tracking.scan:torrent": scanBody, "tracking.titles": `{}`,
		"notify.interest": `{"kind":"series","tmdbId":1}`, "voiceover.origLang": `{"fileId":1}`,
	} {
		if code, b := call(t, d, hook, body); code != 200 {
			t.Errorf("%s: %d %s", hook, code, b)
		}
	}
	if !reflect.DeepEqual(Names, []string{"tracking.scan:pinned", "tracking.scan:torrent", "tracking.titles", "notify.interest", "voiceover.origLang"}) {
		t.Fatalf("names %v", Names)
	}
}
