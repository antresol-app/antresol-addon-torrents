package holds

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

type fakeSource struct {
	mu   sync.Mutex
	list []Info
}

func (s *fakeSource) Torrents() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Info(nil), s.list...)
}

func (s *fakeSource) set(l []Info) { s.mu.Lock(); s.list = l; s.mu.Unlock() }

// recorder is a fake PUT /files/holds.
type recorder struct {
	mu   sync.Mutex
	sets [][]addonkit.Hold
	errs []error // answers in order; nil after them
	put  chan struct{}
}

func (r *recorder) Put(_ context.Context, h []addonkit.Hold) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sets = append(r.sets, h)
	var err error
	if len(r.errs) > 0 {
		err, r.errs = r.errs[0], r.errs[1:]
	}
	if r.put != nil {
		r.put <- struct{}{}
	}
	return err
}

func (r *recorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.sets) }

func one(ih, name string) []Info {
	return []Info{{Status: torrent.Status{InfoHash: ih, Name: name, SaveDir: "/m/dl", Status: torrent.StatusSeeding}}}
}

// fakeTicker hands Run a ticker the test drives.
type fakeTicker struct {
	c     chan time.Time
	every chan time.Duration
}

func newFakeTicker() *fakeTicker {
	return &fakeTicker{c: make(chan time.Time), every: make(chan time.Duration, 1)}
}

func (f *fakeTicker) tick(d time.Duration) (<-chan time.Time, func()) {
	f.every <- d
	return f.c, func() {}
}

// 10 changes within a second are one push, at the next 5 s tick; a tick
// with nothing changed pushes nothing.
func TestPusher_Debounce(t *testing.T) {
	src := &fakeSource{list: one("AA", "A")}
	rec := &recorder{put: make(chan struct{}, 10)}
	ft := newFakeTicker()
	p := &Pusher{Src: src, Put: rec.Put, FileRoots: []string{"/m"}, tick: ft.tick}
	if err := p.PushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-rec.put
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	if d := <-ft.every; d != 5*time.Second {
		t.Fatalf("debounce %v, want 5 s", d)
	}
	for i := 0; i < 10; i++ {
		p.Changed()
		time.Sleep(100 * time.Millisecond)
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("pushed before the tick: %d", n)
	}
	ft.c <- time.Now()
	<-rec.put
	ft.c <- time.Now() // nothing changed
	ft.c <- time.Now()
	if n := rec.count(); n != 2 {
		t.Fatalf("puts %d, want 2", n)
	}
	// a change the engine made without telling (a new download) is pushed
	// at the next tick too
	src.set(append(one("AA", "A"), one("bb", "B")...))
	ft.c <- time.Now()
	<-rec.put
	if got := rec.sets[2]; len(got) != 2 || got[1].Key != "bb" {
		t.Fatalf("new download: %+v", got)
	}
	cancel()
	<-done
}

// The first push fails: PushNow says so (boot retries it and stays not
// ready meanwhile); the next one succeeds and the outside count is kept.
func TestPusher_FirstPushBeforeReady(t *testing.T) {
	src := &fakeSource{list: append(one("aa", "A"), Info{Status: torrent.Status{InfoHash: "cc", Name: "C", SaveDir: "/media/.torrents"}})}
	rec := &recorder{errs: []error{&addonkit.APIError{Status: http.StatusServiceUnavailable, Message: "busy"}}}
	var told []int
	p := &Pusher{Src: src, Put: rec.Put, FileRoots: []string{"/m"}, Outside: func(n int) { told = append(told, n) }}
	if err := p.PushNow(context.Background()); err == nil {
		t.Fatal("a failed first push reported ok")
	}
	if err := p.PushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec.count() != 2 || len(rec.sets[1]) != 1 || rec.sets[1][0].Key != "aa" {
		t.Fatalf("sets %+v", rec.sets)
	}
	if p.LastOutside() != 1 || len(told) != 1 || told[0] != 1 {
		t.Fatalf("outside %d, told %v", p.LastOutside(), told)
	}
}

// A set the core refuses (400) is logged and not sent again until it
// changes or Changed asks; any other failure is retried at every tick.
func TestPusher_RefusedSetRetriedOnChange(t *testing.T) {
	src := &fakeSource{list: one("aa", "A")}
	rec := &recorder{put: make(chan struct{}, 10), errs: []error{
		&addonkit.APIError{Status: http.StatusBadRequest, Message: "hold aa: /m/dl/A is outside the file browser"},
		errors.New("connection refused"),
	}}
	ft := newFakeTicker()
	p := &Pusher{Src: src, Put: rec.Put, FileRoots: []string{"/m"}, tick: ft.tick}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	<-ft.every
	p.Changed()
	ft.c <- time.Now()
	<-rec.put // 400
	ft.c <- time.Now()
	ft.c <- time.Now()
	if n := rec.count(); n != 1 {
		t.Fatalf("a refused set sent again unchanged: %d", n)
	}
	p.Changed()
	ft.c <- time.Now()
	<-rec.put // transport error
	ft.c <- time.Now()
	<-rec.put // retried without a change
	ft.c <- time.Now()
	ft.c <- time.Now()
	if n := rec.count(); n != 3 {
		t.Fatalf("puts %d, want 3", n)
	}
}

// rootsFrom is a Roots func whose answer the test changes.
type rootsFrom struct {
	mu      sync.Mutex
	roots   []string
	missing []string
	err     error
}

func (r *rootsFrom) get(context.Context) ([]string, []string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.roots, r.missing, r.err
}

func (r *rootsFrom) set(roots []string, err error) {
	r.mu.Lock()
	r.roots, r.err = roots, err
	r.mu.Unlock()
}

// A root missing at start (/info leaves it out) counts once it appears:
// /info is read again before every push.
func TestPusher_RootAppearsLater(t *testing.T) {
	src := &fakeSource{list: one("aa", "A")}
	rec := &recorder{put: make(chan struct{}, 10)}
	roots := &rootsFrom{}
	ft := newFakeTicker()
	p := &Pusher{Src: src, Put: rec.Put, FileRoots: nil, Roots: roots.get, tick: ft.tick}
	if err := p.PushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-rec.put
	if len(rec.sets[0]) != 0 || p.LastOutside() != 1 {
		t.Fatalf("first push %+v, outside %d", rec.sets[0], p.LastOutside())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	<-ft.every
	roots.set([]string{"/m"}, nil)
	ft.c <- time.Now()
	<-rec.put
	if got := rec.sets[1]; len(got) != 1 || got[0].Key != "aa" || p.LastOutside() != 0 {
		t.Fatalf("after the root appeared: %+v, outside %d", got, p.LastOutside())
	}
	// /info failing: no push, the last roots stay
	roots.set(nil, errors.New("core down"))
	p.Changed()
	ft.c <- time.Now()
	ft.c <- time.Now()
	if n := rec.count(); n != 2 {
		t.Fatalf("pushed without the roots: %d", n)
	}
	if p.FileRoots[0] != "/m" {
		t.Fatalf("roots lost: %v", p.FileRoots)
	}
}

// A set that would shrink to nothing while downloads exist is not sent
// (retried next cycle); an engine with no downloads may clear the set.
func TestPusher_NeverShrinksToEmpty(t *testing.T) {
	src := &fakeSource{list: one("aa", "A")}
	rec := &recorder{put: make(chan struct{}, 10)}
	roots := &rootsFrom{roots: []string{"/m"}}
	ft := newFakeTicker()
	p := &Pusher{Src: src, Put: rec.Put, Roots: roots.get, tick: ft.tick}
	if err := p.PushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-rec.put
	roots.set([]string{}, nil) // the root left the file browser for a moment
	if err := p.PushNow(context.Background()); err == nil {
		t.Fatal("an empty set with a download pushed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	<-ft.every
	ft.c <- time.Now()
	ft.c <- time.Now()
	if n := rec.count(); n != 1 {
		t.Fatalf("puts %d, want 1 (no empty set)", n)
	}
	src.set(nil) // the last download is gone: [] is the truth
	ft.c <- time.Now()
	<-rec.put
	if got := rec.sets[1]; got == nil || len(got) != 0 {
		t.Fatalf("empty engine: %+v", got)
	}
}

// The core kept holds from the last run and a root is missing at start (a
// media mount not up yet): the first push does not clear them; the addon
// may turn ready (PushNow nil: the saved set protects the data); once the
// root appears the real set goes. Reading the saved set failing pushes
// nothing; a core with nothing saved may get [].
func TestPusher_SavedSetGuardsFirstPush(t *testing.T) {
	src := &fakeSource{list: one("aa", "A")}
	rec := &recorder{put: make(chan struct{}, 10)}
	roots := &rootsFrom{}
	savedErr := errors.New("core busy")
	var reads int
	saved := func(context.Context) ([]addonkit.Hold, error) {
		reads++
		if reads == 1 {
			return nil, savedErr
		}
		return []addonkit.Hold{{Key: "aa", Root: "/m/dl/A"}}, nil
	}
	ft := newFakeTicker()
	p := &Pusher{Src: src, Put: rec.Put, Roots: roots.get, Saved: saved, tick: ft.tick}
	if err := p.PushNow(context.Background()); !errors.Is(err, savedErr) || rec.count() != 0 {
		t.Fatalf("saved set unreadable: %v, puts %d", err, rec.count())
	}
	if err := p.PushNow(context.Background()); err != nil {
		t.Fatalf("the core's saved set protects the data: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("pushed %+v over the saved set", rec.sets)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	<-ft.every
	ft.c <- time.Now()
	ft.c <- time.Now()
	if rec.count() != 0 {
		t.Fatalf("pushed at a tick: %+v", rec.sets)
	}
	roots.set([]string{"/m"}, nil)
	ft.c <- time.Now()
	<-rec.put
	if got := rec.sets[0]; len(got) != 1 || got[0].Key != "aa" {
		t.Fatalf("after the root appeared: %+v", got)
	}
	if reads != 2 {
		t.Fatalf("saved set read %d times, want once after the error", reads)
	}

	// nothing saved in the core: [] may go
	rec2 := &recorder{}
	p2 := &Pusher{Src: src, Put: rec2.Put, Roots: (&rootsFrom{}).get,
		Saved: func(context.Context) ([]addonkit.Hold, error) { return []addonkit.Hold{}, nil }}
	if err := p2.PushNow(context.Background()); err != nil || rec2.count() != 1 || len(rec2.sets[0]) != 0 {
		t.Fatalf("nothing saved: %v %+v", err, rec2.sets)
	}
}

// Two roots, one missing on disk: the set pushed is the full one (the
// hold under the missing root stays), not a shrunk one.
func TestPusher_OneOfTwoRootsMissing(t *testing.T) {
	src := &fakeSource{list: append(one("aa", "A"), Info{Status: torrent.Status{InfoHash: "bb", Name: "B", SaveDir: "/n/dl", Status: torrent.StatusSeeding}})}
	rec := &recorder{}
	roots := &rootsFrom{roots: []string{"/m"}, missing: []string{"/n"}}
	p := &Pusher{Src: src, Put: rec.Put, Roots: roots.get}
	if err := p.PushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := rec.sets[0]; len(got) != 2 || got[0].Key != "aa" || got[1].Key != "bb" || p.LastOutside() != 0 {
		t.Fatalf("pushed %+v, outside %d", got, p.LastOutside())
	}
}

// lateCore is a fake core for a late mount: /info resolves its one root
// on every call (fileRoots when on disk, missingRoots when not), GET and
// PUT /files/holds keep the saved set, PUT refuses a hold under neither.
type lateCore struct {
	root string
	mu   sync.Mutex
	set  []addonkit.Hold
	puts int
}

func (c *lateCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/addon-core/v1/info":
		info := addonkit.Info{FileRoots: []string{}, MissingRoots: []string{}}
		if real, err := filepath.EvalSymlinks(c.root); err == nil {
			info.FileRoots = append(info.FileRoots, real)
		} else {
			info.MissingRoots = append(info.MissingRoots, c.root)
		}
		json.NewEncoder(w).Encode(info)
	case r.Method == http.MethodGet && r.URL.Path == "/api/addon-core/v1/files/holds":
		json.NewEncoder(w).Encode(map[string][]addonkit.Hold{"holds": c.set})
	case r.Method == http.MethodPut && r.URL.Path == "/api/addon-core/v1/files/holds":
		var b struct{ Holds []addonkit.Hold }
		json.NewDecoder(r.Body).Decode(&b)
		for _, x := range b.Holds {
			if !within(c.root, x.Root) {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"outside"}`))
				return
			}
		}
		c.set = b.Holds
		c.puts++
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"none"}`))
	}
}

func (c *lateCore) saved() ([]addonkit.Hold, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]addonkit.Hold(nil), c.set...), c.puts
}

// The host reboots and the media mount comes up late: the core kept the
// set, the addon starts with the root missing, pushes the full set (never
// []), and keeps it once the mount is up.
func TestPusher_LateMount(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "dl") // not mounted yet
	core := &lateCore{root: root, set: []addonkit.Hold{{Key: "aa", Root: filepath.Join(root, "A")}}}
	srv := httptest.NewServer(core)
	defer srv.Close()
	client := addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})
	src := &fakeSource{list: []Info{{Status: torrent.Status{InfoHash: "AA", Name: "A", SaveDir: root, Status: torrent.StatusSeeding},
		Files: []torrent.FileInfo{{FullPath: filepath.Join(root, "A", "a.mkv"), Size: 1, Selected: true}}}}}
	ft := newFakeTicker()
	p := &Pusher{Src: src, Put: client.PutHolds, Saved: client.Holds, tick: ft.tick,
		Roots: func(ctx context.Context) ([]string, []string, error) {
			i, err := client.Info(ctx)
			return i.FileRoots, i.MissingRoots, err
		}}
	if err := p.PushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if set, puts := core.saved(); puts != 1 || len(set) != 1 || set[0].Key != "aa" || len(set[0].Files) != 1 {
		t.Fatalf("first push with the root missing: %+v (%d puts)", set, puts)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx)
	<-ft.every
	mustMkdir(t, filepath.Join(root, "A")) // the mount is up
	p.Changed()
	ft.c <- time.Now()
	ft.c <- time.Now()
	if set, _ := core.saved(); len(set) != 1 || set[0].Key != "aa" || p.LastOutside() != 0 {
		t.Fatalf("after the mount: %+v, outside %d", set, p.LastOutside())
	}
}
