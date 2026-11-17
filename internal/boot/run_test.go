package boot

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/importer"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
	"github.com/antresol-app/antresol/addonkit/addonkittest"
)

// realBoot boots with the real store, importer and a LocalOnly engine in
// a temp dir; from != "" imports that old-build vault.db first.
func realBoot(t *testing.T, from string) (Booted, Conf) {
	t.Helper()
	dir := t.TempDir()
	c := Conf{DataDir: dir, DownloadDir: filepath.Join(dir, "dl"), StreamDir: filepath.Join(dir, "stream"),
		LocalOnly: true, ImportFrom: from, StreamCacheBytes: 20 << 30}
	b, err := Boot(context.Background(), c, Deps{
		OpenDB: store.Open, Import: importer.Phase1, Fingerprint: importer.Fingerprint,
		NewEngine: func(cfg torrent.EngineConfig, db *sql.DB) (Engine, error) {
			e, err := torrent.New(cfg, db)
			if err != nil {
				return nil, err
			}
			return e, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Engine.Close(); b.DB.Close() })
	return b, c
}

func goldenClient(t *testing.T) *addonkit.Client {
	srv := addonkittest.ReplayGolden(t, addonkittest.GoldenDir())
	return addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})
}

func TestBuild_WiresServices(t *testing.T) {
	b, c := realBoot(t, source(t))
	info := addonkit.Info{LibraryRoots: addonkit.LibraryRoots{Movies: filepath.Join(c.DataDir, "movies")}}
	s, err := Build(context.Background(), b, c, goldenClient(t), info)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Acq.Close()
	if s.Engine == nil || s.Sources == nil || s.Acq == nil || s.Bus == nil || s.NoSeed != b.NoSeed || s.Client == nil {
		t.Fatalf("services %+v", s)
	}
	src, err := s.Sources.Sources(context.Background())
	if err != nil || len(src) != 2 {
		t.Fatalf("sources %d %v; want the fixture's 2", len(src), err)
	}
	if n := len(s.Acq.Torrents()); n != 3 {
		t.Fatalf("Acq.Torrents() lists %d; want the imported fixture's 3", n)
	}
}

// TestBuild_StreamCacheFromSettings: the saved size wins over the env's.
func TestBuild_StreamCacheFromSettings(t *testing.T) {
	b, c := realBoot(t, "")
	if err := (store.Settings{DB: b.DB}).SetStreamCacheGB(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	s, err := Build(context.Background(), b, c, goldenClient(t), addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Acq.Close()
	if s.StreamCacheBytes != 3<<30 {
		t.Fatalf("stream cache %d, want 3 GiB", s.StreamCacheBytes)
	}
}

func TestRun_ShutdownClosesEngine(t *testing.T) {
	b, c := realBoot(t, "")
	s, err := Build(context.Background(), b, c, goldenClient(t), addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	ih := "dddddddddddddddddddddddddddddddddddddddd"
	if _, err := s.Engine.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+ih, torrent.AddOptions{Mode: torrent.ModeDownload, SaveDir: c.DownloadDir}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	s.Run(ctx, &wg)
	time.Sleep(200 * time.Millisecond)
	// the engine's periodic save is long past by now; only Close writes
	// the row again
	if _, err := b.DB.Exec(`UPDATE torrents SET status = 'stale' WHERE infohash = ?`, ih); err != nil {
		t.Fatal(err)
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop within 5 s")
	}
	if _, err := s.Engine.AddMagnet(context.Background(), "magnet:?xt=urn:btih:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", torrent.AddOptions{Mode: torrent.ModeDownload, SaveDir: c.DownloadDir}); !errors.Is(err, torrent.ErrClosed) {
		t.Fatalf("add after shutdown: %v, want ErrClosed (engine closed)", err)
	}
	// the row says "stale" until Engine.Close saves the torrent's state
	var status string
	if err := b.DB.QueryRow(`SELECT status FROM torrents WHERE infohash = ?`, ih).Scan(&status); err != nil || status == "" || status == "stale" {
		t.Fatalf("saved row: %q %v (Engine.Close did not save it)", status, err)
	}
}

// holdsCore is a fake core: PUT /files/holds fails the first time, then
// blocks until release is closed.
type holdsCore struct {
	calls   atomic.Int32
	arrived chan struct{}
	release chan struct{}
}

func (h *holdsCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut && r.URL.Path == "/api/addon-core/v1/files/holds" {
		if h.calls.Add(1) == 1 {
			http.Error(w, `{"error":"busy"}`, http.StatusServiceUnavailable)
			return
		}
		close(h.arrived)
		<-h.release
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	w.Write([]byte(`{"error":"none"}`))
}

func TestRun_ReadyStateAfterFirstPush(t *testing.T) {
	b, c := realBoot(t, "")
	// the process started at x.2 s, now is x.5 s: the write waits 500 ms
	start := time.Date(2026, 11, 15, 10, 0, 0, 200e6, time.UTC)
	var slept atomic.Int64
	defer setClock(start, func() time.Time { return start.Add(300 * time.Millisecond) }, func(d time.Duration) { slept.Add(int64(d)) })()

	fc := &holdsCore{arrived: make(chan struct{}), release: make(chan struct{})}
	srv := httptest.NewServer(fc)
	defer srv.Close()
	defer func() {
		select {
		case <-fc.release:
		default:
			close(fc.release)
		}
	}()
	client := addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})
	s, err := Build(context.Background(), b, c, client, addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	var ready atomic.Bool
	s.FirstPush = func(ctx context.Context) error { return client.PutHolds(ctx, nil) }
	s.OnReady = func() { ready.Store(true) }
	s.RetryEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	readyFile := filepath.Join(c.DataDir, ReadyFile)
	if _, err := os.Stat(readyFile); !os.IsNotExist(err) {
		t.Fatalf("ready.state before Run: %v", err)
	}
	s.Run(ctx, &wg)
	select {
	case <-fc.arrived: // the second push (the first failed) is in flight
	case <-time.After(5 * time.Second):
		t.Fatal("no retried PUT /files/holds")
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(readyFile); !os.IsNotExist(err) || ready.Load() {
		t.Fatalf("ready before the first push succeeded: %v %v", err, ready.Load())
	}
	close(fc.release)
	deadline := time.Now().Add(5 * time.Second)
	for !ready.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ready.Load() {
		t.Fatal("not ready after the first push")
	}
	body, err := os.ReadFile(readyFile)
	if err != nil || string(body) != "2026-11-15T10:00:00Z\n" {
		t.Fatalf("ready.state %q %v; want the start time, RFC 3339", body, err)
	}
	if d := time.Duration(slept.Load()); d != 500*time.Millisecond {
		t.Fatalf("slept %v; ready.state must not be written in the start second", d)
	}
	if left, _ := filepath.Glob(filepath.Join(c.DataDir, ReadyFile+".tmp-*")); len(left) > 0 {
		t.Fatalf("temp left: %v", left)
	}
}

// TestRun_StopAfterPushWritesNoReady: a stop that comes while the first
// push answers (SIGTERM between the push and the write) leaves no
// ready.state and never calls OnReady.
func TestRun_StopAfterPushWritesNoReady(t *testing.T) {
	b, c := realBoot(t, "")
	s, err := Build(context.Background(), b, c, goldenClient(t), addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.FirstPush = func(context.Context) error { cancel(); return nil } // ok, but the stop came
	var ready atomic.Bool
	s.OnReady = func() { ready.Store(true) }
	var wg sync.WaitGroup
	s.Run(ctx, &wg)
	wg.Wait()
	if _, err := os.Stat(filepath.Join(c.DataDir, ReadyFile)); !os.IsNotExist(err) || ready.Load() {
		t.Fatalf("ready.state after a stop: %v, ready %v", err, ready.Load())
	}
}

// TestRun_ReadyWithoutPusher: with no FirstPush (no holds pusher
// wired) ready follows Run's start.
func TestRun_ReadyWithoutPusher(t *testing.T) {
	b, c := realBoot(t, "")
	s, err := Build(context.Background(), b, c, goldenClient(t), addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	s.OnReady = func() { close(ready) }
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	s.Run(ctx, &wg)
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	cancel()
	wg.Wait()
	if _, err := os.Stat(filepath.Join(c.DataDir, ReadyFile)); err != nil {
		t.Fatal(err)
	}
}

// TestClearReady: a ready.state of an earlier run goes at the start.
func TestClearReady(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ReadyFile)
	os.WriteFile(p, []byte("x\n"), 0o644)
	if err := ClearReady(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("ready.state left: %v", err)
	}
	if err := ClearReady(dir); err != nil {
		t.Fatalf("absent: %v", err)
	}
}

// fakeBinder answers BindRefs: an error on the first n calls.
type fakeBinder struct {
	mu    sync.Mutex
	fails int
	calls int
}

func (f *fakeBinder) BindRefs(_ context.Context, pairs []addonkit.RefPath) (addonkit.Bound, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.fails {
		return addonkit.Bound{}, errors.New("core down")
	}
	return addonkit.Bound{}, nil
}

// TestPhase2Loop_RetriesUntilDone: an error is retried after RetryEvery
// (60 s in production) until phase 2 is done, then the loop ends.
func TestPhase2Loop_RetriesUntilDone(t *testing.T) {
	b, _ := realBoot(t, source(t))
	f := &fakeBinder{fails: 2}
	s := &Services{DB: b.DB, binder: f, Phase2Every: 10 * time.Millisecond}
	done := make(chan struct{})
	go func() { s.phase2(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("phase 2 loop did not finish")
	}
	if f.calls < 3 {
		t.Fatalf("BindRefs calls %d, want the 2 failures retried", f.calls)
	}
	st, ok, err := importer.LoadState(context.Background(), b.DB)
	if err != nil || !ok || st.Phase != 2 {
		t.Fatalf("state %+v %v %v", st, ok, err)
	}
}

// A first push the core refuses (400) reaches OnNotReady with the status
// and the core's text, so /health can show it while the addon is not ready.
func TestRun_RefusedFirstPushReported(t *testing.T) {
	b, c := realBoot(t, "")
	var puts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/api/addon-core/v1/files/holds" {
			if puts.Add(1) == 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"outside the file roots: /x/Pack"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client := addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})
	s, err := Build(context.Background(), b, c, client, addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reported []string
	var ready atomic.Bool
	s.FirstPush = func(ctx context.Context) error { return client.PutHolds(ctx, nil) }
	s.OnNotReady = func(err error) { mu.Lock(); reported = append(reported, err.Error()); mu.Unlock() }
	s.OnReady = func() { ready.Store(true) }
	s.RetryEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	s.Run(ctx, &wg)
	deadline := time.Now().Add(5 * time.Second)
	for !ready.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ready.Load() {
		t.Fatal("not ready after the second push")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 1 || !strings.Contains(reported[0], "400") || !strings.Contains(reported[0], "outside the file roots: /x/Pack") {
		t.Fatalf("reported %q; want the 400 and the core's text once", reported)
	}
}
