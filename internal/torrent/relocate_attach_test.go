package torrent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	atorrent "github.com/anacrolix/torrent"
)

// orphans lists the client's torrents no entry owns: nothing can park or
// disallow them any more.
func orphans(e *Engine) []*atorrent.Torrent {
	e.mu.Lock()
	defer e.mu.Unlock()
	owned := map[*atorrent.Torrent]bool{}
	for _, en := range e.entries {
		if en.t != nil {
			owned[en.t] = true
		}
	}
	var out []*atorrent.Torrent
	for _, t := range e.client.Torrents() {
		if !owned[t] {
			out = append(out, t)
		}
	}
	return out
}

// parkedForMove: a finished, parked torrent (one file of two selected) in
// <root>/torrents/movies, plus the from/to folders of a move.
func parkedForMove(t *testing.T) (*Engine, multiFixture, string, string) {
	t.Helper()
	root := t.TempDir()
	save := filepath.Join(root, "torrents", "movies")
	to := filepath.Join(root, "archive", "torrents")
	for _, d := range []string{save, filepath.Dir(to)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	e, _ := downloaded(t, t.TempDir(), save, engOpts{noSeeding: true}, fx, []int{0})
	waitSwarm(t, e, 0)
	waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Parked })
	return e, fx, filepath.Dir(save), to
}

// Turning seeding on while a parked torrent is moving must not attach it at
// the old place: Finish would lose the handle, and turning seeding off again
// could never park it (it would upload with the setting on).
func TestRelocate_SeedingToggledDuringMoveLeavesNoOrphan(t *testing.T) {
	e, _, from, to := parkedForMove(t)
	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	e.SetNoSeeding(false)
	if n := inSwarm(e); n != 0 {
		t.Fatalf("the sweep attached %d torrents while moving", n)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	e.SetNoSeeding(true)
	if o := orphans(e); len(o) != 0 {
		t.Fatalf("%d client torrents no entry owns", len(o))
	}
	waitSwarm(t, e, 0)
}

// An attach asked for while the torrent is moving (more files, here) is
// refused, and Finish leaves nothing in the client.
func TestRelocate_AddFilesDuringMoveRefused(t *testing.T) {
	e, fx, from, to := parkedForMove(t)
	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.AddFiles(context.Background(), fx.ih, []int{1}); !errors.Is(err, ErrBusy) {
		t.Fatalf("AddFiles while moving: %v, want ErrBusy", err)
	}
	if n := inSwarm(e); n != 0 {
		t.Fatalf("attached %d torrents while moving", n)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if o := orphans(e); len(o) != 0 {
		t.Fatalf("%d client torrents no entry owns", len(o))
	}
	waitSwarm(t, e, 0)
}

// Whatever attached the torrent meanwhile, Finish drops a handle that is
// not the one Begin dropped.
func TestRelocate_FinishDropsHandleAttachedMeanwhile(t *testing.T) {
	e, fx, from, to := parkedForMove(t)
	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.unpark(e.entries[fx.ih]) // any attach path that slips past the guards
	e.mu.Unlock()
	if inSwarm(e) != 1 {
		t.Fatal("setup: the torrent was not attached")
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if o := orphans(e); len(o) != 0 {
		t.Fatalf("%d client torrents no entry owns", len(o))
	}
	// never left neither parked nor attached: attached at the new place with
	// no upload, then parked again by the sweep
	e.mu.Lock()
	en := e.entries[fx.ih]
	parked, attached, off := en.parked, en.t != nil, en.t != nil && uploadDisallowed(en.t)
	e.mu.Unlock()
	if !parked && !(attached && off) {
		t.Fatalf("after Finish: parked=%v attached=%v upload disallowed=%v", parked, attached, off)
	}
	s := waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Parked })
	if want := filepath.Join(to, "movies"); s.SaveDir != want {
		t.Fatalf("save dir %q, want %q", s.SaveDir, want)
	}
	waitSwarm(t, e, 0)
}

// A stray handle of a torrent that was attached when Begin ran is dropped
// before Finish attaches it again: the client would otherwise hand the stray
// back, attached at the old place, and the deferred drop would leave en.t a
// dropped handle.
func TestRelocate_FinishStrayOfAttachedTorrent(t *testing.T) {
	e, fx := newSeeder(t, engOpts{})
	e.mu.Lock()
	en := e.entries[fx.ih]
	from := en.rec.SaveDir
	e.mu.Unlock()
	to := from + "-moved"
	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	spec, err := e.specFor(en.rec)
	if err == nil {
		err = e.attach(en, spec) // any attach path that slips past the guards
	}
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if o := orphans(e); len(o) != 0 {
		t.Fatalf("%d client torrents no entry owns", len(o))
	}
	e.mu.Lock()
	tt := en.t
	e.mu.Unlock()
	if tt == nil || !slices.Contains(e.client.Torrents(), tt) {
		t.Fatal("after Finish the entry holds no live handle")
	}
	s := waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.SaveDir == to })
	if s.Error != "" {
		t.Fatalf("error %q", s.Error)
	}
	waitSwarm(t, e, 1)
}

// Resuming a torrent whose data was missing attaches it: refused while it moves.
func TestRelocate_ResumeMissingDuringMoveRefused(t *testing.T) {
	e, fx, from, to := parkedForMove(t)
	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	en := e.entries[fx.ih]
	en.parked, en.parkedInfo, en.dataMissing = false, nil, true
	e.mu.Unlock()
	if err := e.Resume(context.Background(), fx.ih); !errors.Is(err, ErrBusy) {
		t.Fatalf("Resume while moving: %v, want ErrBusy", err)
	}
	if n := inSwarm(e); n != 0 {
		t.Fatalf("attached %d torrents while moving", n)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if o := orphans(e); len(o) != 0 {
		t.Fatalf("%d client torrents no entry owns", len(o))
	}
}

// Leaving stream mode unparks the torrent: refused while it moves. (A
// relocation never moves a stream torrent; the state is set by hand.)
func TestSetModeFromStreamWhileMovingRefused(t *testing.T) {
	e, fx, _, _ := parkedForMove(t)
	e.mu.Lock()
	en := e.entries[fx.ih]
	en.rec.Mode, en.moving = ModeStream, true
	e.mu.Unlock()
	if err := e.SetModeTo(context.Background(), fx.ih, ModeDownload, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("SetModeTo while moving: %v, want ErrBusy", err)
	}
	if n := inSwarm(e); n != 0 {
		t.Fatalf("attached %d torrents while moving", n)
	}
	e.mu.Lock()
	mode := en.rec.Mode
	en.moving = false
	e.mu.Unlock()
	if mode != ModeStream {
		t.Fatalf("mode changed to %q by a refused call", mode)
	}
}

// A namesake re-attach running while the torrent is moving leaves it to
// Finish, which attaches it at the new place.
func TestRelocate_ReattachDuringMoveLeavesNoOrphan(t *testing.T) {
	e, fx := newSeeder(t, engOpts{})
	e.mu.Lock()
	en := e.entries[fx.ih]
	from := en.rec.SaveDir
	e.mu.Unlock()
	to := from + "-moved"
	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	en.relocating = true
	tt := en.t
	e.mu.Unlock()
	e.wg.Add(1)
	e.reattach(en, tt)
	e.mu.Lock()
	same := en.t == tt
	e.mu.Unlock()
	if !same {
		t.Fatal("reattach attached the torrent while moving")
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if o := orphans(e); len(o) != 0 {
		t.Fatalf("%d client torrents no entry owns", len(o))
	}
	waitSwarm(t, e, 1)
}

// A plain data move (not a parked torrent) attaches nothing new: growing the
// selection meanwhile still works. (The move window is set by hand.)
func TestAddFilesDuringPlainMoveWorks(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	e, _ := downloaded(t, t.TempDir(), t.TempDir(), engOpts{}, fx, []int{0})
	e.mu.Lock()
	en := e.entries[fx.ih]
	en.moving = true
	e.mu.Unlock()
	err := e.AddFiles(context.Background(), fx.ih, []int{1})
	e.mu.Lock()
	en.moving = false
	files := en.rec.Files
	e.mu.Unlock()
	if err != nil {
		t.Fatalf("AddFiles during a move: %v", err)
	}
	if len(files) != 0 && !slices.Equal(files, []int{0, 1}) {
		t.Fatalf("selection %v", files)
	}
}

// Growing a complete selection during a move by a file that is verified
// already (here wholly inside a piece of the selected file) completes once,
// after Finish, at the new place: never early from the handle Begin dropped,
// with the save dir being left.
func TestAddFilesDuringMoveCompletesOnceAtNewPlace(t *testing.T) {
	root := t.TempDir()
	save := filepath.Join(root, "torrents", "movies")
	to := filepath.Join(root, "archive", "torrents")
	for _, d := range []string{save, filepath.Dir(to)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// piece 16K: file 0 is 0..20K, file 1 (4K) lies inside piece 1
	fx := newMultiFixture(t, 16<<10, 20<<10, 4<<10)
	e, _ := downloaded(t, t.TempDir(), save, engOpts{}, fx, []int{0})
	from := filepath.Dir(save)
	newSave := filepath.Join(to, "movies")
	var mu sync.Mutex
	var done []string
	e.OnComplete(func(s Status) { mu.Lock(); done = append(done, s.SaveDir); mu.Unlock() })
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(done) }

	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	n := inSwarm(e)
	if err := e.AddFiles(context.Background(), fx.ih, []int{1}); err != nil {
		t.Fatalf("AddFiles during a move: %v", err)
	}
	if got := inSwarm(e); got != n {
		t.Fatalf("AddFiles attached a torrent while moving: %d -> %d", n, got)
	}
	time.Sleep(time.Second) // several watcher ticks
	if early := count(); early != 0 {
		t.Fatalf("OnComplete fired %d times while moving", early)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.CompletedAt != nil })
	if s.SaveDir != newSave || s.SelectedFiles != 2 {
		t.Fatalf("status save=%q selected=%d", s.SaveDir, s.SelectedFiles)
	}
	deadline := time.Now().Add(10 * time.Second)
	for count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // a second call would land now
	mu.Lock()
	defer mu.Unlock()
	if len(done) != 1 || done[0] != newSave {
		t.Fatalf("OnComplete save dirs %q, want once %q", done, newSave)
	}
}

// A stray attached without clearing parked (no path does so today) is not
// left both attached and parked after Finish: the sweep would never park it
// again, so it would stay in the swarm.
func TestRelocate_StrayIsNotLeftParked(t *testing.T) {
	e, fx, from, to := parkedForMove(t)
	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	en := e.entries[fx.ih]
	info := en.parkedInfo
	e.unpark(en)
	en.parked, en.parkedInfo = true, info // an attach that forgot to unpark
	e.mu.Unlock()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	parked, attached := en.parked, en.t != nil
	e.mu.Unlock()
	if parked && attached {
		t.Fatal("after Finish the torrent is both parked and attached")
	}
	if o := orphans(e); len(o) != 0 {
		t.Fatalf("%d client torrents no entry owns", len(o))
	}
	waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Parked })
	waitSwarm(t, e, 0)
}
