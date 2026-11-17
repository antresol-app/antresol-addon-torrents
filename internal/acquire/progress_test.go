package acquire

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/events"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

const ihLib, ihStream = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type postedEvent struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data"`
}

// eventCore is a fake core recording POST /events.
func eventCore(t *testing.T) (*addonkit.Client, func() []postedEvent) {
	t.Helper()
	var mu sync.Mutex
	var got []postedEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p postedEvent
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return addonkit.NewClient(addonkit.Env{CoreURL: srv.URL}), func() []postedEvent {
		mu.Lock()
		defer mu.Unlock()
		return append([]postedEvent(nil), got...)
	}
}

func waitEvents(t *testing.T, got func() []postedEvent, n int) []postedEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(got()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d events, want %d: %+v", len(got()), n, got())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond) // nothing more follows
	return got()
}

// The adults' `progress` event: the library downloads of the tick's one
// list (no stream, no error text, no infohash of a stream), every tick
// while one is active, else only on a change; admin.progress goes on.
func TestForward_Progress(t *testing.T) {
	e := newEnv(t)
	bus := &events.Bus{}
	e.svc.o.Bus = bus
	c, got := eventCore(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { coreadapt.Forward(ctx, bus, c); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	for !bus.Wants(EventLibraryProgress) || !bus.Wants(EventProgress) {
		time.Sleep(time.Millisecond)
	}
	for _, q := range []string{
		`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, organize_error) VALUES ('` + ihLib + `','series',42,'Сёгун',1,'')`,
		`INSERT INTO acquisitions (infohash, kind, tmdb_id, title) VALUES ('` + ihStream + `','movie',603,'Матрица')`,
	} {
		if _, err := e.svc.o.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	e.eng.put(torrent.Status{InfoHash: ihLib, Mode: torrent.ModeDownload, Status: torrent.StatusDownloading, Size: 100, Done: 40,
		Stalled: torrent.StalledNoPeers, TrackerError: "passkey 123 of user Петя", TrackerErrorKind: torrent.TrackerRejected})
	e.eng.put(torrent.Status{InfoHash: ihStream, Mode: torrent.ModeStream, Status: torrent.StatusDownloading, Size: 50})

	e.svc.publishProgress()
	evs := waitEvents(t, got, 2)
	byName := map[string]string{}
	for _, ev := range evs {
		byName[ev.Name] = string(ev.Data)
	}
	if !strings.Contains(byName["admin.progress"], ihStream) || !strings.Contains(byName["admin.progress"], ihLib) {
		t.Fatalf("admin.progress %s", byName["admin.progress"])
	}
	var lp []map[string]any
	if err := json.Unmarshal([]byte(byName["progress"]), &lp); err != nil || len(lp) != 1 {
		t.Fatalf("progress %s %v", byName["progress"], err)
	}
	p := lp[0]
	if p["key"] != "series:42" || p["kind"] != "series" || p["tmdbId"] != float64(42) || p["season"] != float64(1) ||
		p["status"] != "active" || p["done"] != float64(40) || p["size"] != float64(100) {
		t.Fatalf("progress %v", p)
	}
	hint, _ := p["hint"].(map[string]any)
	if hint["ru"] != "Нет участников раздачи — ждём…" || hint["en"] != "No peers for this release yet — waiting…" {
		t.Fatalf("hint %v", p["hint"])
	}
	if s := byName["progress"]; strings.Contains(s, ihStream) || strings.Contains(s, "Петя") || strings.Contains(s, "rror") || strings.Contains(s, "name") {
		t.Fatalf("more than adults may see: %s", s)
	}

	// active: every tick
	e.svc.publishProgress()
	if evs := waitEvents(t, got, 4); evs[2].Name == evs[3].Name {
		t.Fatalf("second tick %+v", evs[2:])
	}
	// paused: once on the change, then not again
	e.eng.put(torrent.Status{InfoHash: ihLib, Mode: torrent.ModeDownload, Status: torrent.StatusPaused, Size: 100, Done: 40})
	e.svc.publishProgress()
	waitEvents(t, got, 6)
	e.svc.publishProgress()
	evs = waitEvents(t, got, 7)
	if len(evs) != 7 || evs[6].Name != "admin.progress" {
		t.Fatalf("an unchanged, inactive list went out again: %+v", evs[6:])
	}
}

func TestEventLibraryProgressForwarded(t *testing.T) {
	if EventLibraryProgress != coreadapt.EventLibraryProgress {
		t.Fatalf("acquire's %q != coreadapt's %q", EventLibraryProgress, coreadapt.EventLibraryProgress)
	}
}

// Remove tells the watch inputs (SetOnRemoved), only when it removed.
func TestRemove_CallsOnRemoved(t *testing.T) {
	e := newEnv(t)
	var got []string
	e.svc.SetOnRemoved(func(k string) { got = append(got, k) })
	e.eng.put(torrent.Status{InfoHash: ihLib, Mode: torrent.ModeDownload})
	if err := e.svc.Remove(context.Background(), ihLib, false); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Remove(context.Background(), "direct-nope", false); err == nil {
		t.Fatal("an unknown direct download removed")
	}
	if len(got) != 1 || got[0] != ihLib {
		t.Fatalf("%v", got)
	}
}

// WatchTitle: the acquisition's title, a pack's episode from the file.
func TestWatchTitle(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season) VALUES (?, 'series', 42, 'Сёгун', 1)`, ihLib); err != nil {
		t.Fatal(err)
	}
	kind, tmdb, season, ep, ok := e.svc.WatchTitle(context.Background(), ihLib, "Shogun.S01/Shogun.S01E03.mkv")
	if !ok || kind != "series" || tmdb != 42 || season != 1 || ep != 3 {
		t.Fatalf("%s %d %d %d %v", kind, tmdb, season, ep, ok)
	}
	if _, _, _, _, ok := e.svc.WatchTitle(context.Background(), ihStream, "x.mkv"); ok {
		t.Fatal("no acquisition")
	}
}

// The adults' list is the pending cards' set, under their keys: an
// organized title the library has, or one organized long ago, is not in
// it (so a finished title reaches done and the list does not grow with
// history); the torrents of one title are one entry.
func TestLibraryProgress_PendingSet(t *testing.T) {
	e := newEnv(t)
	old := time.Now().Add(-2 * organizedGrace).UTC().Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)
	const ihOld, ihFresh, ih2 = "cccccccccccccccccccccccccccccccccccccccc", "dddddddddddddddddddddddddddddddddddddddd", "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	for _, q := range []struct {
		ih, kind string
		tmdb     int
		season   int
		at       any
	}{
		{ihLib, "series", 42, 1, nil}, {ih2, "series", 42, 2, nil},
		{ihOld, "movie", 603, 0, old}, {ihFresh, "movie", 550, 0, fresh},
	} {
		// organized ones placed a file (paths_json): not NoFiles
		if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, organized_at, paths_json) VALUES (?,?,?,?,?,?,?)`,
			q.ih, q.kind, q.tmdb, "T", q.season, q.at, `["/media/movies/T/t.mkv"]`); err != nil {
			t.Fatal(err)
		}
	}
	for _, ih := range []string{ihLib, ih2, ihOld, ihFresh} {
		st := torrent.StatusDownloading
		if ih == ihOld || ih == ihFresh {
			st = torrent.StatusSeeding
		}
		e.eng.put(torrent.Status{InfoHash: ih, Mode: torrent.ModeDownload, Status: st, Size: 10, Done: 5})
	}
	// the freshly organized movie is in the library already
	e.lib.items[550] = library.Item{ID: 1, Kind: "movie", TmdbID: 550, Title: "T"}
	lp := e.svc.libraryProgress(context.Background(), e.svc.Torrents())
	cards := e.svc.LibraryDownloads(context.Background())
	if len(lp) != 1 || lp[0].Key != "series:42" || lp[0].Season != 0 || lp[0].Size != 20 || lp[0].Done != 10 || lp[0].Status != DownloadActive {
		t.Fatalf("progress %+v", lp)
	}
	if len(cards) != len(lp) || cards[0].Key != lp[0].Key {
		t.Fatalf("cards %+v vs progress %+v", cards, lp)
	}
	// one season alone: its number
	e.eng.mu.Lock()
	delete(e.eng.st, ih2)
	e.eng.mu.Unlock()
	if lp := e.svc.libraryProgress(context.Background(), e.svc.Torrents()); len(lp) != 1 || lp[0].Season != 1 {
		t.Fatalf("one season %+v", lp)
	}
}

// RequestTitle: the newest acquisition of the catalog id names it; a
// movie, an unknown id or one without a title: false.
func TestRequestTitle(t *testing.T) {
	e := newEnv(t)
	for _, q := range []string{
		`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, ext_id, meta_json, year) VALUES ('` + ihLib + `','music',0,'Группа крови','mb-1','{"artist":"Кино"}',1988)`,
		`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, ext_id, meta_json) VALUES ('` + ihStream + `','book',0,'','ol-1','{"author":"X"}')`,
	} {
		if _, err := e.svc.o.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	got, ok := e.svc.RequestTitle(context.Background(), KindMusic, "mb-1")
	if !ok || got.Title != "Группа крови" || got.Creator != "Кино" || got.Year != 1988 {
		t.Fatalf("%+v %v", got, ok)
	}
	for _, c := range [][2]string{{KindMusic, "mb-9"}, {KindBook, "ol-1"}, {KindMovie, "mb-1"}, {KindMusic, ""}} {
		if _, ok := e.svc.RequestTitle(context.Background(), c[0], c[1]); ok {
			t.Errorf("%v: found", c)
		}
	}
}

// slowLib is the fake library with a slow title lookup that counts its
// calls and the most running at once.
type slowLib struct {
	*fakeLib
	delay      time.Duration
	mu         sync.Mutex
	calls, now int
	peak       int
}

func (l *slowLib) FindTitle(ctx context.Context, kind string, tmdbID int64, title, creator string) (library.Item, bool, error) {
	l.mu.Lock()
	l.calls++
	l.now++
	l.peak = max(l.peak, l.now)
	l.mu.Unlock()
	time.Sleep(l.delay)
	l.mu.Lock()
	l.now--
	l.mu.Unlock()
	return l.fakeLib.FindTitle(ctx, kind, tmdbID, title, creator)
}

// The pending cards ask the library once per title, a few at a time: 12
// downloads of 6 titles over a core taking 200 ms a lookup answer well
// within the hook's 2 s (one after another they took 2.4 s; one per title in turn, 1.2 s).
func TestLibraryDownloads_LookupsBatched(t *testing.T) {
	e := newEnv(t)
	slow := &slowLib{fakeLib: e.lib, delay: 200 * time.Millisecond}
	e.svc.o.Library = slow
	for i := range 12 {
		ih := strings.Repeat(string(rune('a'+i)), 40)
		if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season) VALUES (?, 'series', ?, 'T', ?)`, ih, 100+i/2, 1+i%2); err != nil {
			t.Fatal(err)
		}
		e.eng.put(torrent.Status{InfoHash: ih, Mode: torrent.ModeDownload, Status: torrent.StatusDownloading})
	}
	start := time.Now()
	ds := e.svc.LibraryDownloads(context.Background())
	took := time.Since(start)
	if len(ds) != 6 || slow.calls != 6 || slow.peak > libraryLookups || took > 900*time.Millisecond {
		t.Fatalf("%d titles, %d lookups, %d at once, %v", len(ds), slow.calls, slow.peak, took)
	}
}

// hangLib is a core that never answers a library lookup until the call's
// context ends (as addonkit's client gives up at its deadline).
type hangLib struct{ *fakeLib }

func (hangLib) FindTitle(ctx context.Context, kind string, tmdbID int64, title, creator string) (library.Item, bool, error) {
	select {
	case <-ctx.Done():
		return library.Item{}, false, ctx.Err()
	case <-time.After(30 * time.Second):
		return library.Item{}, false, nil
	}
}

// The progress tick asks the core about organized titles; a core that
// hangs costs the tick its short deadline, never acquire's loop 30 s.
func TestPublishProgress_SlowCoreBounded(t *testing.T) {
	e := newEnv(t)
	e.svc.o.Library = hangLib{e.lib}
	bus := &events.Bus{}
	e.svc.o.Bus = bus
	ch, stop := bus.Subscribe()
	defer stop()
	go func() {
		for range ch {
		}
	}()
	fresh := time.Now().UTC().Format(time.RFC3339)
	if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, organized_at, paths_json) VALUES (?,?,?,?,?,?)`,
		ihLib, "movie", 550, "T", fresh, `["/media/movies/T/t.mkv"]`); err != nil {
		t.Fatal(err)
	}
	e.eng.put(torrent.Status{InfoHash: ihLib, Mode: torrent.ModeDownload, Status: torrent.StatusSeeding, Size: 10, Done: 10})
	start := time.Now()
	e.svc.publishProgress()
	if took := time.Since(start); took > 2500*time.Millisecond {
		t.Fatalf("the tick waited %v on a hanging core", took)
	}
}

// The adults' event is a non-admin's pending cards: a failed download
// (an error card, admins only) is not in it.
func TestLibraryProgress_NoErrors(t *testing.T) {
	e := newEnv(t)
	const ihErr = "cccccccccccccccccccccccccccccccccccccccc"
	for _, q := range []struct {
		ih   string
		tmdb int
	}{{ihLib, 42}, {ihErr, 43}} {
		if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title) VALUES (?, 'movie', ?, 'T')`, q.ih, q.tmdb); err != nil {
			t.Fatal(err)
		}
	}
	e.eng.put(torrent.Status{InfoHash: ihLib, Mode: torrent.ModeDownload, Status: torrent.StatusDownloading, Size: 10, Done: 5})
	e.eng.put(torrent.Status{InfoHash: ihErr, Mode: torrent.ModeDownload, Status: torrent.StatusError, Size: 10})
	lp := e.svc.libraryProgress(context.Background(), e.svc.Torrents())
	if len(lp) != 1 || lp[0].Key != "movie:42" {
		t.Fatalf("progress %+v", lp)
	}
}

// TitlesProgress: one read of the acquisitions for many titles, each the
// same as its own TitleProgress; a title with nothing is left out.
func TestTitlesProgress(t *testing.T) {
	e := newEnv(t)
	const ihB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, q := range []struct {
		ih   string
		tmdb int
	}{{ihLib, 7}, {ihB, 8}} {
		if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title) VALUES (?, 'movie', ?, 'T')`, q.ih, q.tmdb); err != nil {
			t.Fatal(err)
		}
	}
	e.eng.put(torrent.Status{InfoHash: ihLib, Mode: torrent.ModeDownload, Status: torrent.StatusDownloading, Size: 100, Done: 40})
	e.eng.put(torrent.Status{InfoHash: ihB, Mode: torrent.ModeDownload, Status: torrent.StatusPaused, Size: 100, Done: 30})
	keys := []TitleKey{{Kind: "movie", TMDBID: 7}, {Kind: "movie", TMDBID: 8}, {Kind: "movie", TMDBID: 9}}
	got := e.svc.TitlesProgress(context.Background(), keys)
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	for _, k := range keys[:2] {
		want, _ := e.svc.TitleProgress(context.Background(), k.Kind, k.TMDBID, k.Season)
		if got[k] != want {
			t.Fatalf("%+v: %+v, alone %+v", k, got[k], want)
		}
	}
}
