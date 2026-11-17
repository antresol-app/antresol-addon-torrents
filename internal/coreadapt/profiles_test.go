package coreadapt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// Profiles: the profile's «Языки», else its language; one ask per 60 s;
// a failing core keeps the list it had.
func TestProfiles_LanguagesCached(t *testing.T) {
	var asks atomic.Int32
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asks.Add(1)
		if down.Load() {
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(`[{"id":1,"lang":"ru","languages":["en","ru"]},{"id":2,"lang":"en","languages":[]},{"id":3}]`))
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	p := &Profiles{C: addonkit.NewClient(addonkit.Env{CoreURL: srv.URL}), now: func() time.Time { return now }}
	ctx := context.Background()
	for id, want := range map[int64]string{1: "en,ru", 2: "en", 3: "", 9: ""} {
		if got := strings.Join(p.Languages(ctx, id), ","); got != want {
			t.Errorf("%d: %q, want %q", id, got, want)
		}
	}
	if n := asks.Load(); n != 1 {
		t.Fatalf("%d asks within the TTL", n)
	}
	now = now.Add(profilesTTL + time.Second)
	down.Store(true)
	if got := strings.Join(p.Languages(ctx, 1), ","); got != "en,ru" || asks.Load() != 2 {
		t.Fatalf("a failing core: %q after %d asks", got, asks.Load())
	}
	fresh := &Profiles{C: addonkit.NewClient(addonkit.Env{CoreURL: srv.URL})}
	if got := fresh.Languages(ctx, 1); got != nil {
		t.Fatalf("no list ever: %v", got)
	}
}

// A failing core is asked again only after failWait; the ask holds no
// lock (a slow core does not block a reader that has a list).
func TestProfiles_FailureWaitAndNoLock(t *testing.T) {
	var asks atomic.Int32
	gate := make(chan struct{})
	var slow atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asks.Add(1)
		if slow.Load() {
			<-gate
		}
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"no"}`))
	}))
	defer srv.Close()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	p := &Profiles{C: addonkit.NewClient(addonkit.Env{CoreURL: srv.URL}), now: clock}
	ctx := context.Background()
	for range 5 {
		if l := p.Languages(ctx, 1); l != nil {
			t.Fatalf("%v", l)
		}
	}
	if n := asks.Load(); n != 1 {
		t.Fatalf("%d asks within failWait", n)
	}
	mu.Lock()
	now = now.Add(failWait + time.Second)
	mu.Unlock()
	p.Languages(ctx, 1)
	if n := asks.Load(); n != 2 {
		t.Fatalf("%d asks after failWait", n)
	}
	// a known list and a slow ask: a second reader is not held by the first
	p.mu.Lock()
	p.list, p.at, p.failedAt = []addonkit.User{{ID: 1, Languages: []string{"de"}}}, now.Add(-2*profilesTTL), time.Time{}
	p.mu.Unlock()
	slow.Store(true)
	done := make(chan struct{})
	go func() { p.Languages(ctx, 1); close(done) }()
	for asks.Load() != 3 {
		time.Sleep(time.Millisecond)
	}
	p.mu.Lock() // not held by the ask in flight
	p.mu.Unlock()
	close(gate)
	<-done
}
