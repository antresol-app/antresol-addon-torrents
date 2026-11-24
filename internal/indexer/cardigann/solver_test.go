package cardigann

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// solverEnv is a site behind a challenge and a FlareSolverr in front of it:
// the site answers the challenge until a request carries the cookie the
// solver hands out; the solver counts its requests and the most that were
// open at once, and holds each until it is let go (hold) or for delay.
type solverEnv struct {
	site        *httptest.Server
	fs          *httptest.Server
	calls, open atomic.Int32
	peak        atomic.Int32
	delay       time.Duration
	fail        atomic.Bool   // answer "error", as a challenge that was not solved
	hit         chan struct{} // gets a value when a request reaches the solver
	page        []byte
	siteHost    string

	mu   sync.Mutex
	held chan struct{} // when set, an answer waits for it to close (hold)
}

func newSolverEnv(t *testing.T) *solverEnv {
	t.Helper()
	e := &solverEnv{page: readFixture(t, "rutor_search.html"), hit: make(chan struct{}, 64)}
	e.site = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("cf_clearance"); err != nil || c.Value != "cleared" {
			w.Header().Set("Server", "cloudflare")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`<!DOCTYPE html><html><head><title>Just a moment...</title></head><body><div id="challenge-platform"></div></body></html>`))
			return
		}
		w.Write(e.page)
	}))
	t.Cleanup(e.site.Close)
	siteURL, _ := url.Parse(e.site.URL)
	e.siteHost = siteURL.Hostname()
	e.fs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.calls.Add(1)
		n := e.open.Add(1)
		defer e.open.Add(-1)
		for {
			p := e.peak.Load()
			if n <= p || e.peak.CompareAndSwap(p, n) {
				break
			}
		}
		e.hit <- struct{}{}
		var req map[string]any
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		if hold := e.holdCh(); hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		} else {
			time.Sleep(e.delay)
		}
		if e.fail.Load() {
			json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "Error solving the challenge. Timeout after 60.0 seconds."})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "message": "Challenge solved!",
			"solution": map[string]any{
				"url": req["url"], "status": 200, "response": string(e.page), "userAgent": "FS-Browser/1.0",
				"cookies": []map[string]any{{"name": "cf_clearance", "value": "cleared", "domain": e.siteHost, "path": "/"}},
			},
		})
	}))
	t.Cleanup(e.fs.Close)
	// Cleanup runs last in, first out: a held answer is let go before the
	// solver's Close waits for it
	t.Cleanup(func() { e.release() })
	return e
}

func (e *solverEnv) holdCh() chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.held
}

// hold makes the solver keep every answer until release.
func (e *solverEnv) hold() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.held = make(chan struct{})
}

// release lets the held answers go (and later ones answer at once).
func (e *solverEnv) release() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.held != nil {
		close(e.held)
		e.held = nil
	}
}

// runner is a Runner of the site with its own cookie jar (a source of its
// own), behind gate.
func (e *solverEnv) runner(t *testing.T, gate *SolverGate) *Runner {
	t.Helper()
	return &Runner{Def: loadDef(t, "rutor.yml"), Settings: map[string]string{"sitelink": e.site.URL + "/"},
		HTTP: newClient(t), FlareSolverr: e.fs.URL, SolverGate: gate}
}

func (e *solverEnv) waitHit(t *testing.T) {
	t.Helper()
	select {
	case <-e.hit:
	case <-time.After(5 * time.Second):
		t.Fatal("the solver was not asked")
	}
}

// Runners that share a gate never have two requests with FlareSolverr at
// once, whatever the number of searches that need a solve together.
func TestSolverGateAdmitsOneRequestAtATime(t *testing.T) {
	e := newSolverEnv(t)
	e.delay = 30 * time.Millisecond
	gate := NewSolverGate()
	const n = 5
	errs := make(chan error, n)
	for range n {
		r := e.runner(t, gate)
		go func() {
			res, err := r.Search(context.Background(), Query{Keywords: "Дюна"})
			if err == nil && len(res) < 3 {
				err = errors.New("no results")
			}
			errs <- err
		}()
	}
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := e.calls.Load(); got != n {
		t.Fatalf("solver calls = %d, want %d (a jar each)", got, n)
	}
	if got := e.peak.Load(); got != 1 {
		t.Fatalf("%d requests were with FlareSolverr at once, want 1", got)
	}
}

// Without a shared gate a runner has one of its own: its own requests (a
// sign-in page, then the search) still go one at a time.
func TestRunnerWithoutGateHasItsOwn(t *testing.T) {
	e := newSolverEnv(t)
	r := e.runner(t, nil)
	if _, err := r.Search(context.Background(), Query{Keywords: "Дюна"}); err != nil {
		t.Fatal(err)
	}
	if r.SolverGate == nil {
		t.Fatal("no gate was made")
	}
}

// A search that waits for its turn gives up with its context, with an
// error that says the solver was busy, and sends nothing.
func TestSolverWaiterGivesUpWithItsContext(t *testing.T) {
	e := newSolverEnv(t)
	e.hold()
	gate := NewSolverGate()
	first := e.runner(t, gate)
	done := make(chan error, 1)
	go func() {
		_, err := first.Search(context.Background(), Query{Keywords: "Дюна"})
		done <- err
	}()
	e.waitHit(t)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := e.runner(t, gate).Search(ctx, Query{Keywords: "Дюна"})
	if !errors.Is(err, ErrSolverBusy) || !errors.Is(err, ErrCloudflare) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiter: err = %v, want a busy Cloudflare error", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("waiter took %v", took)
	}
	if got := e.calls.Load(); got != 1 {
		t.Fatalf("solver calls = %d: the waiter sent a request", got)
	}
	e.release()
	if err := <-done; err != nil {
		t.Fatalf("the first search: %v", err)
	}
}

// A search whose context ends while FlareSolverr works leaves at once, but
// its request is not abandoned: the slot stays taken until FlareSolverr
// answers (no second browser meanwhile), and what it solved is kept, so the
// next search of that site needs no solve.
func TestSolverRequestOutlivesItsAsker(t *testing.T) {
	e := newSolverEnv(t)
	e.hold()
	gate := NewSolverGate()
	solved := make(chan error, 4)
	asker := e.runner(t, gate)
	asker.OnSolve = func(err error) { solved <- err }

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	began := time.Now()
	_, err := asker.Search(ctx, Query{Keywords: "Дюна"})
	if !errors.Is(err, ErrCloudflare) || errors.Is(err, ErrSolverBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("asker: err = %v, want a Cloudflare error from its deadline", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("asker took %v", took)
	}

	// FlareSolverr is still on the first request: another asker waits
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel2()
	other := e.runner(t, gate)
	if _, err := other.Search(ctx2, Query{Keywords: "Дюна"}); !errors.Is(err, ErrSolverBusy) {
		t.Fatalf("other asker: err = %v, want busy", err)
	}
	if got := e.calls.Load(); got != 1 {
		t.Fatalf("solver calls = %d while the first was open", got)
	}

	e.release()
	select {
	case err := <-solved:
		if err != nil {
			t.Fatalf("OnSolve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnSolve was not called")
	}
	// the orphaned request's cookies are in the asker's jar: no new solve
	res, err := asker.Search(context.Background(), Query{Keywords: "Дюна"})
	if err != nil || len(res) < 3 {
		t.Fatalf("next search: %d results, err %v", len(res), err)
	}
	if got := e.calls.Load(); got != 1 {
		t.Fatalf("solver calls = %d, want 1: the next search solved again", got)
	}
	// and the slot is free again
	if _, err := other.Search(context.Background(), Query{Keywords: "Дюна"}); err != nil {
		t.Fatalf("other asker after: %v", err)
	}
	if got := e.calls.Load(); got != 2 {
		t.Fatalf("solver calls = %d, want 2", got)
	}
}

// OnSolve hears each outcome: nil for a solved page, an ErrCloudflare for a
// challenge FlareSolverr did not get through, and for one that did not
// answer within solverCallTimeout; the slot is free after each.
func TestOnSolveReportsEachOutcome(t *testing.T) {
	e := newSolverEnv(t)
	gate := NewSolverGate()
	var got []error
	r := e.runner(t, gate)
	r.OnSolve = func(err error) { got = append(got, err) }

	e.fail.Store(true)
	if _, err := r.Search(context.Background(), Query{Keywords: "x"}); !errors.Is(err, ErrCloudflare) {
		t.Fatalf("unsolved: err = %v", err)
	}
	e.fail.Store(false)
	if _, err := r.Search(context.Background(), Query{Keywords: "x"}); err != nil {
		t.Fatalf("solved: %v", err)
	}
	// a FlareSolverr that never answers: the call ends at solverCallTimeout
	old := solverCallTimeout
	solverCallTimeout = 80 * time.Millisecond
	t.Cleanup(func() { solverCallTimeout = old })
	e.hold()
	slow := e.runner(t, gate)
	slow.OnSolve = func(err error) { got = append(got, err) }
	if _, err := slow.Search(context.Background(), Query{Keywords: "x"}); !errors.Is(err, ErrCloudflare) {
		t.Fatalf("slow: err = %v", err)
	}
	if len(got) != 3 || !errors.Is(got[0], ErrCloudflare) || got[1] != nil || !errors.Is(got[2], ErrCloudflare) {
		t.Fatalf("OnSolve outcomes = %v", got)
	}
	if err := gate.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	gate.release()
}
