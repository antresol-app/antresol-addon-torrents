package indexer

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingTracker serves one release and counts the searches it gets;
// while gate is set an answer waits for it to close.
func countingTracker(hits *atomic.Int32, gate func() chan struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			return
		}
		hits.Add(1)
		if g := gate(); g != nil {
			<-g
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(page(row{title: "Дюна (2024) BDRip 1080p", hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", link: "magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", size: "2 GB", seeders: 9})))
	})
}

// The same title asked of a source twice within seconds is searched once;
// a background search, another title or an old answer is a new search.
func TestSearchAnswersAreReusedBriefly(t *testing.T) {
	var hits atomic.Int32
	e := newEnv(t)
	e.def("fake", "Fake", countingTracker(&hits, func() chan struct{} { return nil }))
	e.add("fake")
	now := time.Now()
	e.m.now = func() time.Time { return now }
	ctx := context.Background()
	ask := func(ctx context.Context, text string) SearchResponse {
		t.Helper()
		resp, err := e.m.Search(ctx, SearchRequest{Text: text, Kind: "movie", Year: 2024})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	base := hits.Load()
	first := ask(ctx, "Дюна")
	again := ask(ctx, "Дюна")
	if got := hits.Load() - base; got != 1 {
		t.Fatalf("two searches made %d requests", got)
	}
	if len(first.Releases) == 0 || len(again.Releases) != len(first.Releases) {
		t.Fatalf("releases %d then %d", len(first.Releases), len(again.Releases))
	}
	ask(Background(ctx), "Дюна")
	if got := hits.Load() - base; got != 2 {
		t.Fatalf("a background search must ask the tracker: %d requests", got)
	}
	ask(ctx, "Дюна 2")
	if got := hits.Load() - base; got != 3 {
		t.Fatalf("another title must ask the tracker: %d requests", got)
	}
	now = now.Add(searchTTL + time.Second)
	ask(ctx, "Дюна")
	if got := hits.Load() - base; got != 4 {
		t.Fatalf("an old answer must not be reused: %d requests", got)
	}
}

// Searches that run at the same time share one request to the source.
func TestIdenticalSearchesShareOneRequest(t *testing.T) {
	var hits atomic.Int32
	var mu sync.Mutex
	var gate chan struct{}
	e := newEnv(t)
	e.def("fake", "Fake", countingTracker(&hits, func() chan struct{} { mu.Lock(); defer mu.Unlock(); return gate }))
	e.add("fake")
	base := hits.Load()
	mu.Lock()
	gate = make(chan struct{})
	mu.Unlock()

	const n = 4
	var wg sync.WaitGroup
	counts := make([]int, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := e.m.Search(context.Background(), SearchRequest{Text: "Дюна", Kind: "movie", Year: 2024})
			if err != nil {
				t.Error(err)
			}
			counts[i] = len(resp.Releases)
		}()
	}
	// wait until the one request is in and the others have joined it
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load()-base < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	close(gate)
	gate = nil
	mu.Unlock()
	wg.Wait()
	if got := hits.Load() - base; got != 1 {
		t.Fatalf("%d concurrent searches made %d requests", n, got)
	}
	for i, c := range counts {
		if c == 0 {
			t.Fatalf("search %d found nothing", i)
		}
	}
}

// A failed answer is not kept: the next search asks again.
func TestFailedAnswerIsNotReused(t *testing.T) {
	var hits atomic.Int32
	var broken atomic.Bool
	broken.Store(true)
	ok := countingTracker(&hits, func() chan struct{} { return nil })
	e := newEnv(t)
	e.def("fake", "Fake", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() && r.URL.Path == "/search" {
			hits.Add(1)
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		ok.ServeHTTP(w, r)
	}))
	e.add("fake")
	base := hits.Load()
	req := SearchRequest{Text: "Дюна", Kind: "movie", Year: 2024}
	if resp, err := e.m.Search(context.Background(), req); err != nil || len(resp.Failed) == 0 {
		t.Fatalf("a broken tracker: %+v, %v", resp.Failed, err)
	}
	broken.Store(false)
	resp, err := e.m.Search(context.Background(), req)
	if err != nil || len(resp.Releases) == 0 {
		t.Fatalf("the recovered tracker: %d releases, %v", len(resp.Releases), err)
	}
	if got := hits.Load() - base; got != 2 {
		t.Fatalf("%d requests: the failure was kept", got)
	}
}
