package channelfetch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
	_ "modernc.org/sqlite"
)

type fakeAcq struct {
	mu       sync.Mutex
	asked    []acquire.AcquireRequest
	pending  []acquire.PendingAcquisition
	removed  []string
	err      error
	nextHash string
}

func (f *fakeAcq) Acquire(_ context.Context, r acquire.AcquireRequest) (acquire.AcquireResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, r)
	if f.err != nil {
		return acquire.AcquireResult{}, f.err
	}
	f.pending = append(f.pending, acquire.PendingAcquisition{Kind: r.Kind, TMDBID: r.TMDBID, Season: r.Season, Episode: r.Episode, InfoHash: f.nextHash})
	return acquire.AcquireResult{InfoHash: f.nextHash}, nil
}

func (f *fakeAcq) Pending(context.Context) ([]acquire.PendingAcquisition, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]acquire.PendingAcquisition(nil), f.pending...), nil
}

func (f *fakeAcq) Remove(_ context.Context, ih string, data bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !data {
		panic("a dropped download keeps its data")
	}
	f.removed = append(f.removed, ih)
	return nil
}

type fakeFollows map[int]bool

func (f fakeFollows) ByTMDB(_ context.Context, _ string, id int) (follow.Follow, error) {
	if f[id] {
		return follow.Follow{Active: true}, nil
	}
	return follow.Follow{}, follow.ErrNotFound
}

func newHook(t *testing.T, acq *fakeAcq, ts *[]torrent.Status) *Hook {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return Register(context.Background(), &addonkit.HookMux{}, Deps{Acquire: acq, DB: db, Follows: fakeFollows{7: true},
		Torrents: func() []torrent.Status { return *ts }})
}

func call(t *testing.T, h *Hook, req addonkit.ChannelFetch) map[string]addonkit.ChannelFetchState {
	t.Helper()
	b, _ := json.Marshal(req)
	ans, err := h.serve(context.Background(), nil, b)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]addonkit.ChannelFetchState{}
	for _, s := range ans.(addonkit.ChannelFetchAnswer).States {
		out[s.Key] = s
	}
	return out
}

func waitIdle(t *testing.T, h *Hook) {
	t.Helper()
	for i := 0; i < 200; i++ {
		h.mu.Lock()
		n := len(h.searching)
		h.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("still searching")
}

// gateAcq holds every Acquire until released and counts how many run at once.
type gateAcq struct {
	*fakeAcq
	gate    chan struct{}
	running atomic.Int32
	peak    atomic.Int32
}

func (g *gateAcq) Acquire(ctx context.Context, r acquire.AcquireRequest) (acquire.AcquireResult, error) {
	n := g.running.Add(1)
	defer g.running.Add(-1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	<-g.gate
	return g.fakeAcq.Acquire(ctx, r)
}

// Wanted programmes are searched maxSearches at a time; the others wait and
// show «searching» meanwhile.
func TestChannelFetchSearchesAFewAtOnce(t *testing.T) {
	acq := &gateAcq{fakeAcq: &fakeAcq{nextHash: "AB"}, gate: make(chan struct{})}
	var ts []torrent.Status
	h := newHook(t, acq.fakeAcq, &ts)
	h.d.Acquire = acq
	var want []addonkit.ChannelFetchTitle
	for i := 1; i <= 6; i++ {
		want = append(want, addonkit.ChannelFetchTitle{Key: fmt.Sprintf("episode:7:1:%d", i), Kind: "episode", TMDBID: 7, Season: 1, Episode: i})
	}
	got := call(t, h, addonkit.ChannelFetch{Want: want})
	for _, w := range want {
		if got[w.Key].State != "searching" {
			t.Fatalf("%s: %+v", w.Key, got[w.Key])
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for acq.running.Load() < maxSearches && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a third would be running by now
	if n := acq.running.Load(); n != maxSearches {
		t.Fatalf("%d searches at once, want %d", n, maxSearches)
	}
	close(acq.gate)
	waitIdle(t, h)
	if p := acq.peak.Load(); p > maxSearches {
		t.Fatalf("peak %d searches", p)
	}
	if len(acq.asked) != 6 {
		t.Fatalf("asked %d, want 6", len(acq.asked))
	}
}

func TestChannelFetchStartsReportsDrops(t *testing.T) {
	acq := &fakeAcq{nextHash: "AB"}
	var ts []torrent.Status
	h := newHook(t, acq, &ts)
	ep := addonkit.ChannelFetchTitle{Key: "episode:7:1:2", Kind: "episode", TMDBID: 7, Season: 1, Episode: 2}
	got := call(t, h, addonkit.ChannelFetch{Want: []addonkit.ChannelFetchTitle{ep}})
	if got[ep.Key].State != "searching" || !got[ep.Key].Keep {
		t.Fatalf("first answer %+v", got)
	}
	waitIdle(t, h)
	if len(acq.asked) != 1 || acq.asked[0].Mode != acquire.ModeDownload || acq.asked[0].Kind != acquire.KindSeries || acq.asked[0].Episode != 2 || !acq.asked[0].Background {
		t.Fatalf("asked %+v", acq.asked)
	}
	ts = []torrent.Status{{InfoHash: "ab", Size: 100, Done: 40, DownRate: 9}}
	got = call(t, h, addonkit.ChannelFetch{Want: []addonkit.ChannelFetchTitle{ep}})
	if s := got[ep.Key]; s.State != "downloading" || s.Size != 100 || s.Done != 40 || s.Rate != 9 || s.Existing {
		t.Fatalf("downloading: %+v", s)
	}
	if len(acq.asked) != 1 {
		t.Fatal("started twice")
	}
	call(t, h, addonkit.ChannelFetch{Drop: []string{ep.Key}})
	if len(acq.removed) != 1 || acq.removed[0] != "AB" {
		t.Fatalf("an unfinished dropped download stays: %v", acq.removed)
	}
}

// Someone's download of the programme is theirs: reported as existing,
// never started again, never removed.
func TestChannelFetchExisting(t *testing.T) {
	acq := &fakeAcq{pending: []acquire.PendingAcquisition{{Kind: acquire.KindSeries, TMDBID: 9, Season: 1, InfoHash: "CD"}}}
	var ts []torrent.Status
	h := newHook(t, acq, &ts)
	ep := addonkit.ChannelFetchTitle{Key: "episode:9:1:3", Kind: "episode", TMDBID: 9, Season: 1, Episode: 3}
	got := call(t, h, addonkit.ChannelFetch{Want: []addonkit.ChannelFetchTitle{ep}})
	if !got[ep.Key].Existing || got[ep.Key].Keep {
		t.Fatalf("%+v", got[ep.Key])
	}
	call(t, h, addonkit.ChannelFetch{Drop: []string{ep.Key}})
	if len(acq.asked) != 0 || len(acq.removed) != 0 {
		t.Fatalf("touched someone's download: %v %v", acq.asked, acq.removed)
	}
}

func TestChannelFetchFailed(t *testing.T) {
	acq := &fakeAcq{err: acquire.ErrNoRelease}
	var ts []torrent.Status
	h := newHook(t, acq, &ts)
	mv := addonkit.ChannelFetchTitle{Key: "movie:5:0:0", Kind: "movie", TMDBID: 5}
	call(t, h, addonkit.ChannelFetch{Want: []addonkit.ChannelFetchTitle{mv}})
	waitIdle(t, h)
	got := call(t, h, addonkit.ChannelFetch{Want: []addonkit.ChannelFetchTitle{mv}})
	if got[mv.Key].State != "failed" || got[mv.Key].Error == "" {
		t.Fatalf("%+v", got[mv.Key])
	}
	// asked again later: a new search
	call(t, h, addonkit.ChannelFetch{Want: []addonkit.ChannelFetchTitle{mv}})
	waitIdle(t, h)
	if len(acq.asked) != 2 {
		t.Fatalf("asked %d times", len(acq.asked))
	}
}

// Register answers the hook the manifest declares.
func TestRegisterAnswersChannelsFetch(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	mux := &addonkit.HookMux{}
	Register(context.Background(), mux, Deps{Acquire: &fakeAcq{}, DB: db})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/channels.fetch", strings.NewReader(`{"want":[]}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"states"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// TestChannelFetchServedByMux: Register puts the handler on the mux under
// "channels.fetch", the name the manifest declares and the core calls.
func TestChannelFetchServedByMux(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	var ts []torrent.Status
	mux := &addonkit.HookMux{}
	Register(context.Background(), mux, Deps{Acquire: &fakeAcq{}, DB: db, Follows: fakeFollows{},
		Torrents: func() []torrent.Status { return ts }})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/"+"channels.fetch", strings.NewReader(`{}`)))
	if rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
		t.Fatalf("POST /hooks/channels.fetch: %d %s", rec.Code, rec.Body)
	}
}
