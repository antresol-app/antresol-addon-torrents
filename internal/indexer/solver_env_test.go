package indexer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// cfEnv is n sources behind a Cloudflare challenge with one FlareSolverr in
// front of them: a site passes a request that carries the clearance cookie
// of the current generation (expire makes every cookie stale), and the
// solver solves, fails or is slow as the test sets it.
type cfEnv struct {
	*env
	gen     atomic.Int32
	fs      *httptest.Server
	fsCalls atomic.Int32
	fsFails atomic.Bool  // answer "error", as a challenge that was not solved
	fsDelay atomic.Int64 // ns each answer waits
	now     time.Time    // the manager's clock
}

func newCFEnv(t *testing.T, n int) *cfEnv {
	t.Helper()
	c := &cfEnv{env: newEnv(t), now: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)}
	body := page(row{title: "Дюна (2024) WEB-DL 1080p | Дубляж", hash: strings.Repeat("a", 40), size: "10 GB", seeders: 50})
	for i := 1; i <= n; i++ {
		c.def("cf"+strconv.Itoa(i), "Сайт"+strconv.Itoa(i), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ck, err := r.Cookie("cf_clearance"); err != nil || ck.Value != strconv.Itoa(int(c.gen.Load())) {
				w.Header().Set("Server", "cloudflare")
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`<!DOCTYPE html><html><head><title>Just a moment...</title></head><body><div id="challenge-platform"></div></body></html>`))
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(body))
		}))
	}
	c.fs = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.fsCalls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(raw, &req)
		select {
		case <-time.After(time.Duration(c.fsDelay.Load())):
		case <-r.Context().Done():
			return
		}
		if c.fsFails.Load() {
			json.NewEncoder(w).Encode(map[string]any{"status": "error", "message": "Error solving the challenge. Timeout after 60.0 seconds."})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "message": "Challenge solved!",
			"solution": map[string]any{
				"url": req["url"], "status": 200, "response": body, "userAgent": "FS-Browser/1.0",
				"cookies": []map[string]any{{"name": "cf_clearance", "value": strconv.Itoa(int(c.gen.Load())), "path": "/"}},
			},
		})
	}))
	t.Cleanup(c.fs.Close)
	c.m.SetFlareSolverr(c.fs.URL)
	c.m.now = func() time.Time { return c.now }
	return c
}

// expire makes every clearance cookie stale: the next request to a site meets
// the challenge again.
func (c *cfEnv) expire() { c.gen.Add(1) }

func (c *cfEnv) search() SearchResponse {
	c.t.Helper()
	resp, err := c.m.Search(context.Background(), SearchRequest{Text: "Дюна", Kind: "movie", Year: 2024})
	if err != nil {
		c.t.Fatal(err)
	}
	return resp
}

func (c *cfEnv) source(id int64) Source {
	c.t.Helper()
	s, err := c.m.Get(context.Background(), id)
	if err != nil {
		c.t.Fatal(err)
	}
	return s
}

// failedWith reports the one failed source of resp and its message.
func failedMessage(t *testing.T, resp SearchResponse) string {
	t.Helper()
	if len(resp.Failed) != 1 {
		t.Fatalf("failed = %+v, want one source", resp.Failed)
	}
	return resp.Failed[0].Message
}

// Sources that need FlareSolverr at once share its one slot: the one that
// waits for its turn and runs out of time says that FlareSolverr was busy,
// and neither changes its own status nor is paused for it.
func TestSearchWaitingForSolverIsNotAFailureOfTheSource(t *testing.T) {
	c := newCFEnv(t, 2)
	a, b := c.add("cf1"), c.add("cf2")
	c.expire()
	c.fsDelay.Store(int64(500 * time.Millisecond))
	c.m.searchTimeout = 150 * time.Millisecond
	calls := c.fsCalls.Load()

	resp := c.search()
	if len(resp.Failed) != 2 {
		t.Fatalf("failed = %+v, want both sources", resp.Failed)
	}
	busy := 0
	for _, f := range resp.Failed {
		if strings.Contains(f.Message, "FlareSolverr занят") {
			busy++
			if got := c.source(f.SourceID); got.Status != StatusOK {
				t.Errorf("source %q that only waited: status %q (%s)", f.Name, got.Status, got.LastError)
			}
		}
	}
	if busy != 1 {
		t.Fatalf("busy messages = %d in %+v, want 1", busy, resp.Failed)
	}
	if got := c.fsCalls.Load(); got != calls+1 {
		t.Fatalf("solver calls = %d, want %d: two requests were with it at once", got, calls+1)
	}
	for _, id := range []int64{a.ID, b.ID} {
		if _, paused := c.m.solverPausedUntil(id); paused {
			t.Errorf("source %d is paused though FlareSolverr solved", id)
		}
	}
}
