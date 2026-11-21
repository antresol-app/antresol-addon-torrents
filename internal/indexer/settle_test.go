package indexer

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowDef serves body after wait, ending early when the request is
// cancelled. It answers Add's own test request at once: hold switches the
// delay on after the sources are added.
type slowDef struct {
	mu        sync.Mutex
	holding   bool
	cancelled chan struct{} // closed when a held request was cancelled
	once      sync.Once
}

func newSlowDef() *slowDef { return &slowDef{cancelled: make(chan struct{})} }

func (d *slowDef) hold() {
	d.mu.Lock()
	d.holding = true
	d.mu.Unlock()
}

func (d *slowDef) handler(wait time.Duration, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		h := d.holding
		d.mu.Unlock()
		if h {
			select {
			case <-time.After(wait):
			case <-r.Context().Done():
				d.once.Do(func() { close(d.cancelled) })
				return
			}
		}
		serve(body).ServeHTTP(w, r)
	})
}

var (
	dunaFast = page(row{title: "Дюна (2024) WEB-DL 1080p | Дубляж", hash: strings.Repeat("a", 40), size: "10 GB", seeders: 50})
	dunaSlow = page(row{title: "Дюна (2024) WEB-DL 2160p | Дубляж", hash: strings.Repeat("b", 40), size: "30 GB", seeders: 5})
	noRows   = page()
)

var dune = SearchRequest{Text: "Дюна", Kind: "movie", Year: 2024}

// A settling search returns shortly after the first usable release without
// the slow source, which is reported as not answering and is cancelled; its
// status is left alone.
func TestSearchSettleDropsSlowSource(t *testing.T) {
	e := newEnv(t)
	slow := newSlowDef()
	e.def("slow", "Медленный", slow.handler(10*time.Second, dunaSlow))
	e.def("good", "Рабочий", serve(dunaFast))
	slowSrc := e.add("slow")
	e.add("good")
	e.m.searchTimeout = 15 * time.Second
	slow.hold()

	began := time.Now()
	resp, err := e.m.Search(WithSettle(context.Background(), 200*time.Millisecond, 3*time.Second), dune)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("took %v, want about the grace (200ms), not the slow source's 10s", d)
	}
	if len(resp.Releases) != 1 || resp.Releases[0].SourceName != "Рабочий" {
		t.Fatalf("releases = %+v, want the fast source's one", resp.Releases)
	}
	if len(resp.Failed) != 1 || resp.Failed[0].SourceID != slowSrc.ID || resp.Failed[0].Name != "Медленный" ||
		!strings.Contains(resp.Failed[0].Message, "не ответил") {
		t.Fatalf("failed = %+v, want the slow source as not answering", resp.Failed)
	}
	select {
	case <-slow.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the slow source's request was not cancelled")
	}
	srcs, err := e.m.Sources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if s.ID == slowSrc.ID && s.Status == StatusError {
			t.Errorf("the dropped source is marked %q (%s): a cancelled source says nothing of itself", s.Status, s.LastError)
		}
	}
}

// When every source answers before the grace is over, nothing waits for it
// and nothing is reported.
func TestSearchSettleAllAnswered(t *testing.T) {
	e := newEnv(t)
	e.def("one", "Один", serve(dunaFast))
	e.def("two", "Два", serve(dunaSlow))
	e.add("one")
	e.add("two")

	began := time.Now()
	resp, err := e.m.Search(WithSettle(context.Background(), 5*time.Second, 6*time.Second), dune)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("took %v, want no wait for the grace", d)
	}
	if len(resp.Releases) != 2 || len(resp.Failed) != 0 {
		t.Fatalf("releases %d failed %+v, want 2 and none", len(resp.Releases), resp.Failed)
	}
}

// A source that answers within the grace still counts.
func TestSearchSettleTakesAnswerWithinGrace(t *testing.T) {
	e := newEnv(t)
	slow := newSlowDef()
	e.def("slow", "Медленный", slow.handler(150*time.Millisecond, dunaSlow))
	e.def("good", "Рабочий", serve(dunaFast))
	e.add("slow")
	e.add("good")
	slow.hold()

	resp, err := e.m.Search(WithSettle(context.Background(), 2*time.Second, 6*time.Second), dune)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Releases) != 2 || len(resp.Failed) != 0 {
		t.Fatalf("releases %d failed %+v, want both sources", len(resp.Releases), resp.Failed)
	}
}

// With no usable release yet the search keeps waiting for the sources,
// whatever the grace and the cap: a title only the slow source has is found.
func TestSearchSettleWaitsForFirstUsableRelease(t *testing.T) {
	e := newEnv(t)
	slow := newSlowDef()
	e.def("slow", "Медленный", slow.handler(700*time.Millisecond, dunaSlow))
	e.def("empty", "Пустой", serve(noRows))
	e.add("slow")
	e.add("empty")
	slow.hold()

	resp, err := e.m.Search(WithSettle(context.Background(), 50*time.Millisecond, 200*time.Millisecond), dune)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Releases) != 1 || resp.Releases[0].SourceName != "Медленный" || len(resp.Failed) != 0 {
		t.Fatalf("releases %+v failed %+v, want the slow source's release", resp.Releases, resp.Failed)
	}
}

// The cap bounds the wait once there is a usable release, even when the
// grace is longer.
func TestSearchSettleCap(t *testing.T) {
	e := newEnv(t)
	slow := newSlowDef()
	e.def("slow", "Медленный", slow.handler(10*time.Second, dunaSlow))
	e.def("good", "Рабочий", serve(dunaFast))
	e.add("slow")
	e.add("good")
	e.m.searchTimeout = 15 * time.Second
	slow.hold()

	began := time.Now()
	resp, err := e.m.Search(WithSettle(context.Background(), 8*time.Second, 400*time.Millisecond), dune)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d < 300*time.Millisecond || d > 3*time.Second {
		t.Fatalf("took %v, want about the cap (400ms)", d)
	}
	if len(resp.Releases) != 1 || len(resp.Failed) != 1 {
		t.Fatalf("releases %d failed %+v, want 1 and the slow source", len(resp.Releases), resp.Failed)
	}
}

// Without WithSettle a search waits for every source, as before.
func TestSearchWithoutSettleWaits(t *testing.T) {
	e := newEnv(t)
	slow := newSlowDef()
	e.def("slow", "Медленный", slow.handler(500*time.Millisecond, dunaSlow))
	e.def("good", "Рабочий", serve(dunaFast))
	e.add("slow")
	e.add("good")
	slow.hold()

	began := time.Now()
	resp, err := e.m.Search(context.Background(), dune)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d < 450*time.Millisecond {
		t.Fatalf("took %v, want a wait for the slow source", d)
	}
	if len(resp.Releases) != 2 {
		t.Fatalf("releases = %d, want 2", len(resp.Releases))
	}
}
