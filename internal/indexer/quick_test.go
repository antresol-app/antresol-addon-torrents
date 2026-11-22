package indexer

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// not the title asked for: the source answers, with nothing acceptable
var otherTitle = page(row{title: "Совсем другой фильм (2019) WEB-DL 1080p | Дубляж", hash: strings.Repeat("c", 40), size: "8 GB", seeders: 80})

// quickRun is a search as «Смотреть сейчас» makes it (WithSettle with the
// production window) and how long it took.
func quickRun(t *testing.T, m *Manager) (SearchResponse, time.Duration) {
	t.Helper()
	return quickRunIn(t, m, SettleGrace, SettleCap)
}

func quickRunIn(t *testing.T, m *Manager, grace, limit time.Duration) (SearchResponse, time.Duration) {
	t.Helper()
	began := time.Now()
	resp, err := m.Search(WithSettle(context.Background(), grace, limit), dune)
	if err != nil {
		t.Fatal(err)
	}
	return resp, time.Since(began)
}

func failedNames(resp SearchResponse) map[string]string {
	out := map[string]string{}
	for _, f := range resp.Failed {
		out[f.Name] = f.Message
	}
	return out
}

// The owner's case: a tracker's cookie is dead, another does not answer,
// the rest have no release at hand, so the quick run has nothing to settle
// on and waits for the hung source to time out. The first run pays that
// once; the source is then flaky, and the next quick runs give it a short
// budget instead of the whole timeout.
//
// Timings with the production numbers (timeout 20 s, flaky budget 4 s)
// are these ones times ten: the first run 20 s, the next ones 4 s (before
// the budget: 20 s every time).
func TestQuickRunNotHeldUpByFlakySource(t *testing.T) {
	e := newEnv(t)
	dead := newSlowDef()
	e.def("dead", "Зависший", dead.handler(time.Hour, dunaSlow))
	e.def("broken", "Сломанный", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	e.def("thin", "Пустой", serve(otherTitle))
	deadSrc := e.add("dead")
	e.add("broken")
	e.add("thin")
	dead.hold()
	e.m.searchTimeout = 2 * time.Second // production: 20 s
	e.m.flakyBudget = 300 * time.Millisecond

	resp, first := quickRun(t, e.m)
	t.Logf("quick run 1 (the hung source is not known yet): %v", first.Round(10*time.Millisecond))
	if first < 1900*time.Millisecond {
		t.Fatalf("first run took %v: nothing acceptable came, it waits for the hung source's timeout", first)
	}
	if msg := failedNames(resp)["Зависший"]; !strings.Contains(msg, "не ответил") {
		t.Fatalf("failed = %+v, want the hung source as not answering", resp.Failed)
	}
	if !e.m.isFlaky(deadSrc.ID) {
		t.Fatal("the source that timed out is not noted")
	}
	for i := 2; i <= 3; i++ {
		resp, d := quickRun(t, e.m)
		t.Logf("quick run %d (the hung source is flaky): %v", i, d.Round(10*time.Millisecond))
		if d > time.Second {
			t.Fatalf("run %d took %v, want about the flaky budget (300ms), not the timeout (2s)", i, d)
		}
		if d < 250*time.Millisecond {
			t.Fatalf("run %d took %v: the flaky source is still asked, for its budget", i, d)
		}
		if msg := failedNames(resp)["Зависший"]; !strings.Contains(msg, "не ответил") {
			t.Fatalf("failed = %+v, want the hung source as not answering", resp.Failed)
		}
		if len(resp.Releases) != 0 {
			t.Fatalf("releases = %+v, want none", resp.Releases)
		}
	}
}

// A flaky source never holds up a quick run past the settle window: the
// first usable release from another source ends it as before.
func TestQuickRunFlakySourceBehindSettleWindow(t *testing.T) {
	e := newEnv(t)
	dead := newSlowDef()
	e.def("dead", "Зависший", dead.handler(time.Hour, dunaSlow))
	e.def("good", "Рабочий", serve(dunaFast))
	deadSrc := e.add("dead")
	e.add("good")
	dead.hold()
	e.m.searchTimeout = 5 * time.Second
	e.m.flakyBudget = 4 * time.Second
	e.m.markFailed(deadSrc.ID)

	resp, d := quickRunIn(t, e.m, 200*time.Millisecond, 3*time.Second)
	if d > time.Second {
		t.Fatalf("took %v, want about the grace (200ms)", d)
	}
	if len(resp.Releases) != 1 || resp.Releases[0].SourceName != "Рабочий" {
		t.Fatalf("releases = %+v, want the working source's one", resp.Releases)
	}
}

// The short budget is for quick runs only: a search that wants every
// source (the web picker) waits the whole timeout for a flaky one.
func TestFullSearchKeepsTimeoutForFlakySource(t *testing.T) {
	e := newEnv(t)
	slow := newSlowDef()
	e.def("slow", "Медленный", slow.handler(400*time.Millisecond, dunaSlow))
	e.def("good", "Рабочий", serve(dunaFast))
	slowSrc := e.add("slow")
	e.add("good")
	slow.hold()
	e.m.searchTimeout = 2 * time.Second
	e.m.flakyBudget = 50 * time.Millisecond
	e.m.markFailed(slowSrc.ID)

	began := time.Now()
	resp, err := e.m.Search(context.Background(), dune)
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(began); d < 350*time.Millisecond {
		t.Fatalf("took %v, want the wait for the slow source (400ms)", d)
	}
	if len(resp.Releases) != 2 || len(resp.Failed) != 0 {
		t.Fatalf("releases %d failed %+v, want both sources", len(resp.Releases), resp.Failed)
	}
	if e.m.isFlaky(slowSrc.ID) {
		t.Fatal("an answer did not end the note")
	}
}

// A flaky source that is back and answers within its short budget is used
// and trusted again.
func TestQuickRunFlakySourceBackIsUsed(t *testing.T) {
	e := newEnv(t)
	back := newSlowDef()
	e.def("back", "Вернулся", back.handler(100*time.Millisecond, dunaFast))
	src := e.add("back")
	back.hold()
	e.m.searchTimeout = 5 * time.Second
	e.m.flakyBudget = time.Second
	e.m.markFailed(src.ID)

	resp, d := quickRunIn(t, e.m, 200*time.Millisecond, 3*time.Second)
	if d > 2*time.Second || len(resp.Releases) != 1 || len(resp.Failed) != 0 {
		t.Fatalf("took %v, releases %d, failed %+v, want its answer", d, len(resp.Releases), resp.Failed)
	}
	if e.m.isFlaky(src.ID) {
		t.Fatal("the source answered but is still held to the short budget")
	}
	if b := e.m.sourceBudget(WithSettle(context.Background(), SettleGrace, SettleCap), src.ID); b != 5*time.Second {
		t.Fatalf("budget = %v, want the timeout", b)
	}
}

// The note ends by itself: flakyFor after the last failure the source gets
// the whole timeout again. A cut-short source is not shown as broken.
func TestFlakyNoteExpiresAndStatusStays(t *testing.T) {
	e := newEnv(t)
	dead := newSlowDef()
	e.def("dead", "Зависший", dead.handler(time.Hour, dunaSlow))
	src := e.add("dead")
	dead.hold()
	e.m.searchTimeout = 3 * time.Second
	e.m.flakyBudget = 200 * time.Millisecond
	now := time.Now()
	e.m.now = func() time.Time { return now }
	e.m.markFailed(src.ID)
	ctx := WithSettle(context.Background(), SettleGrace, SettleCap)

	if b := e.m.sourceBudget(ctx, src.ID); b != 200*time.Millisecond {
		t.Fatalf("budget = %v, want the flaky one", b)
	}
	if b := e.m.sourceBudget(context.Background(), src.ID); b != 3*time.Second {
		t.Fatalf("budget of a search for every source = %v, want the timeout", b)
	}
	before, err := e.m.Get(context.Background(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, d := quickRun(t, e.m); d > time.Second {
		t.Fatalf("took %v, want the flaky budget", d)
	}
	after, err := e.m.Get(context.Background(), src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != before.Status || after.LastError != before.LastError {
		t.Fatalf("status %q/%q after a cut-short search, was %q/%q: the quick budget says nothing of the site", after.Status, after.LastError, before.Status, before.LastError)
	}
	now = now.Add(flakyFor - time.Second)
	if !e.m.isFlaky(src.ID) {
		t.Fatal("the note ended early")
	}
	now = now.Add(2 * time.Second)
	if b := e.m.sourceBudget(ctx, src.ID); b != 3*time.Second {
		t.Fatalf("budget = %v after flakyFor, want the timeout", b)
	}
}
