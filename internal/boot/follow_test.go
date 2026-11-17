package boot

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol/addonkit"
	"github.com/antresol-app/antresol/addonkit/addonkittest"
)

// trackCore is a core with TMDB on (its pass-through probe answers 400)
// that records the tracking calls; ensure answers title 7 (or the
// status in ensureFail while that many calls are left), kick answers
// kickStatus (204 when 0). Everything else is the goldens.
type trackCore struct {
	mu         sync.Mutex
	calls      []string // "ensure <kind> <tmdb>", "kick <id>"
	ensureFail atomic.Int32
	kickStatus atomic.Int32
}

func (tc *trackCore) client(t *testing.T) *addonkit.Client {
	t.Helper()
	gold := addonkittest.ReplayGolden(t, addonkittest.GoldenDir())
	u, _ := url.Parse(gold.URL)
	proxy := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/api/addon-core/v1"
		switch {
		case r.Method == http.MethodHead && r.URL.Path == base+"/metadata/tmdb":
			w.WriteHeader(http.StatusBadRequest)
		case r.Method == http.MethodPost && r.URL.Path == base+"/tracking/titles":
			var b struct {
				Kind   string `json:"kind"`
				TMDBID int64  `json:"tmdbId"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			tc.mu.Lock()
			tc.calls = append(tc.calls, "ensure "+b.Kind+" "+itoa(b.TMDBID))
			tc.mu.Unlock()
			if tc.ensureFail.Add(-1) >= 0 {
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(`{"error":"busy"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":7,"kind":"` + b.Kind + `","tmdbId":` + itoa(b.TMDBID) + `}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, base+"/tracking/titles/") && strings.HasSuffix(r.URL.Path, "/kick"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base+"/tracking/titles/"), "/kick")
			tc.mu.Lock()
			tc.calls = append(tc.calls, "kick "+id)
			tc.mu.Unlock()
			if st := tc.kickStatus.Swap(0); st != 0 {
				w.WriteHeader(int(st))
				w.Write([]byte(`{"error":"Тайтл не отслеживается"}`))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == base+"/events":
			var b struct {
				Name string `json:"name"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			tc.mu.Lock()
			tc.calls = append(tc.calls, "event "+b.Name)
			tc.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			proxy.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func (tc *trackCore) seen() []string {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	return append([]string(nil), tc.calls...)
}

// waitCalls waits until the core has seen want (in order), up to 5 s.
func (tc *trackCore) waitCalls(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := tc.seen()
		if strings.Join(got, "|") == strings.Join(want, "|") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("core calls %q, want %q", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestBuild_WiresFollowAndPins: with TMDB the follows get
// the core's profiles (Users), its unmatched titles (Titles) and library
// (Library), the pins its tracking, library (renamed re-uploads' old
// files) and acquire's records; the pin actions, the follows'
// pinned hooks and OnFollowed are set and the follows' loops join Loops.
// The pin's scan no longer syncs itself (AfterScan nil): the
// tracking.scan:pinned hook runs Sync after its answer (TrackingHooks).
// Without TMDB there are pins and no follows.
func TestBuild_WiresFollowAndPins(t *testing.T) {
	b, c := realBoot(t, "")
	client := (&trackCore{}).client(t)
	s, err := Build(context.Background(), b, c, client, addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Acq.Close()
	f := s.Follows
	if f == nil {
		t.Fatal("no follow service with TMDB on")
	}
	if u, ok := f.Users.(coreadapt.Users); !ok || u.C != client {
		t.Fatalf("follow Users %#v", f.Users)
	}
	if l, ok := f.Titles.(coreadapt.Library); !ok || l.C != client {
		t.Fatalf("follow Titles %#v", f.Titles)
	}
	if l, ok := f.Library.(coreadapt.Library); !ok || l.C != client {
		t.Fatalf("follow Library %#v", f.Library)
	}
	if f.TMDB == nil || f.Acquire == nil || f.Bus != s.Bus || f.OnFollowed == nil || f.Covered == nil || f.FilmPinned == nil {
		t.Fatalf("follow service %+v", f)
	}
	p := s.Pins
	if l, ok := p.Library.(coreadapt.Library); !ok || l.C != client {
		t.Fatalf("pins Library %#v", p.Library)
	}
	if tr, ok := p.Tracking.(coreadapt.Tracking); !ok || tr.C != client {
		t.Fatalf("pins Tracking %#v", p.Tracking)
	}
	if p.Imported == nil || p.Replaced == nil || p.AfterScan != nil || p.Indexer == nil || p.Engine == nil || s.sweep == nil {
		t.Fatalf("pins %+v", p)
	}
	if a := s.PinActions; a == nil || a.Pins != p || a.Follows != f {
		t.Fatalf("pin actions %+v", a)
	}
	if len(s.Loops) != 2 {
		t.Fatalf("loops %d, want the follows' checks and kicks", len(s.Loops))
	}

	b2, c2 := realBoot(t, "")
	s2, err := Build(context.Background(), b2, c2, goldenClient(t), addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Acq.Close()
	if s2.Follows != nil || s2.PinActions != nil || s2.Pins == nil || s2.Pins.AfterScan != nil || len(s2.Loops) != 0 {
		t.Fatalf("without TMDB: follows %v pins %v loops %d", s2.Follows, s2.Pins, len(s2.Loops))
	}
}

// buildTracked builds the services over tc and runs the kick loop.
func buildTracked(t *testing.T, tc *trackCore) *Services {
	t.Helper()
	b, c := realBoot(t, "")
	s, err := Build(context.Background(), b, c, tc.client(t), addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Acq.Close)
	s.KickRetryEvery = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.kicks.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return s
}

// TestFollow_AddEnsuresAndKicks: a follow turned on has the core track its
// title and refresh it soon, without waiting on the core.
func TestFollow_AddEnsuresAndKicks(t *testing.T) {
	tc := &trackCore{}
	s := buildTracked(t, tc)
	start := time.Now()
	s.Follows.OnFollowed(context.Background(), follow.Follow{Kind: "series", TMDBID: 42, Title: "Сёгун"})
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("OnFollowed waited %v", d)
	}
	tc.waitCalls(t, "ensure series 42", "kick 7")
}

// TestFollow_CoreUnreachable: the core refuses the first ensure: the
// follow's ensure+kick is kept in memory and sent again until it goes
// through; a kick of an id the core no longer tracks (404) ensures again
// at once.
func TestFollow_CoreUnreachable(t *testing.T) {
	tc := &trackCore{}
	tc.ensureFail.Store(1)
	s := buildTracked(t, tc)
	s.Follows.OnFollowed(context.Background(), follow.Follow{Kind: "movie", TMDBID: 5, Title: "Дюна"})
	tc.waitCalls(t, "ensure movie 5", "ensure movie 5", "kick 7")

	tc2 := &trackCore{}
	tc2.kickStatus.Store(http.StatusNotFound)
	s2 := buildTracked(t, tc2)
	s2.Follows.OnFollowed(context.Background(), follow.Follow{Kind: "series", TMDBID: 9})
	tc2.waitCalls(t, "ensure series 9", "kick 7", "ensure series 9", "kick 7")
}

// TestFinished_SweepsStale: a finished download that did not fail has the
// pins of its release sweep their stale files in the background, on the
// run context under sweepTimeout, then the holds go out again; a
// failed one sweeps nothing. A sweep error is logged, the callback never
// waits on it.
func TestFinished_SweepsStale(t *testing.T) {
	var mu sync.Mutex
	var swept []string
	var changed atomic.Int32
	release := make(chan struct{})
	b, c := realBoot(t, "")
	s, err := Build(context.Background(), b, c, goldenClient(t), addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Acq.Close()
	if s.sweep == nil {
		t.Fatal("Build gave the pins no sweep")
	}
	s.sweep = func(ctx context.Context, ih string) error {
		<-release
		if _, ok := ctx.Deadline(); !ok {
			t.Error("sweep without a timeout")
		}
		mu.Lock()
		swept = append(swept, ih)
		mu.Unlock()
		return errors.New("disk gone")
	}
	// the callback Build gave acquire; HoldsChanged set after it, as main
	// does with the pusher
	cb := s.finished
	s.HoldsChanged = func() { changed.Add(1) }
	start := time.Now()
	cb(acquire.Finished{Key: "aaaa", Failed: true})
	cb(acquire.Finished{Key: "bbbb"})
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("the callback waited %v", d)
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for changed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(swept) != 1 || swept[0] != "bbbb" || changed.Load() != 1 {
		t.Fatalf("swept %v, holds changed %d", swept, changed.Load())
	}
}

// TestPinnedReplace_HoldsChanged: a pinned release the engine swapped
// moves its download record and has the holds pushed at once (once), with
// the HoldsChanged main sets after Build.
func TestPinnedReplace_HoldsChanged(t *testing.T) {
	b, c := realBoot(t, "")
	s, err := Build(context.Background(), b, c, goldenClient(t), addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Acq.Close()
	var n atomic.Int32
	s.HoldsChanged = func() { n.Add(1) }
	if err := s.Pins.Replaced(context.Background(), strings.Repeat("a", 40), strings.Repeat("b", 40)); err != nil {
		t.Fatal(err)
	}
	if n.Load() != 1 {
		t.Fatalf("HoldsChanged %d times", n.Load())
	}
}
