package cardigann

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// FlareSolverr's browser resolves names and follows redirects itself, past
// the dial check of our client: a site that answered a guarded request with
// a challenge must not get a page fetched through the solver from a host
// the runner may not reach, nor a page the solver ended up on elsewhere.
func TestSolverIsNotUsedToReachBlockedHosts(t *testing.T) {
	t.Run("the site is refused before the solver is asked", func(t *testing.T) {
		e := newSolverEnv(t)
		r := e.runner(t, NewSolverGate())
		r.SolverHostOK = func(context.Context, string) bool { return false }
		_, err := r.Search(context.Background(), Query{Keywords: "x"})
		if !errors.Is(err, ErrLocalAddress) || errors.Is(err, ErrCloudflare) {
			t.Fatalf("err = %v, want ErrLocalAddress and no ErrCloudflare", err)
		}
		if n := e.calls.Load(); n != 0 {
			t.Fatalf("the solver was asked %d times", n)
		}
	})

	t.Run("a page the solver ended on elsewhere is refused", func(t *testing.T) {
		var asked atomic.Int32
		fs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "solution": map[string]any{
				"url": "http://internal.example.test/admin", "status": 200, "response": "<html>secret</html>"}})
		}))
		defer fs.Close()
		e := newSolverEnv(t)
		r := e.runner(t, NewSolverGate())
		r.FlareSolverr = fs.URL
		var judged []string
		r.SolverHostOK = func(_ context.Context, host string) bool {
			judged = append(judged, host)
			return host != "internal.example.test"
		}
		if _, err := r.Search(context.Background(), Query{Keywords: "x"}); !errors.Is(err, ErrLocalAddress) {
			t.Fatalf("err = %v, want ErrLocalAddress", err)
		}
		if asked.Load() != 1 || len(judged) != 2 {
			t.Fatalf("solver asked %d times, hosts judged %v", asked.Load(), judged)
		}
	})

	t.Run("without a check the solver works as before", func(t *testing.T) {
		e := newSolverEnv(t)
		if _, err := e.runner(t, NewSolverGate()).Search(context.Background(), Query{Keywords: "x"}); err != nil {
			t.Fatal(err)
		}
		if e.calls.Load() != 1 {
			t.Fatalf("solver calls %d", e.calls.Load())
		}
	})
}
