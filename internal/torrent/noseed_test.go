package torrent

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"
)

// inSwarm: how many torrents the BitTorrent client knows. A torrent that is
// not there announces to nobody and takes no connection.
func inSwarm(e *Engine) int { return len(e.client.Torrents()) }

func waitSwarm(t *testing.T, e *Engine, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for inSwarm(e) != want {
		if time.Now().After(deadline) {
			t.Fatalf("client has %d torrents, want %d", inSwarm(e), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// downloaded adds fx to a fresh engine and waits until a seeder delivered it.
func downloaded(t *testing.T, dir, save string, o engOpts, fx multiFixture, files []int) (*Engine, *Engine) {
	t.Helper()
	seed := multiSeeder(t, fx, nil)
	e := openEngine(t, dir, o)
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: files}); err != nil {
		t.Fatal(err)
	}
	connect(t, e, seed, fx.ih)
	waitFor(t, e, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding && s.CompletedAt != nil })
	return e, seed
}

// With seeding off a finished torrent leaves the swarm altogether, while
// its status, files and data stay what they were.
func TestNoSeedingLeavesTheSwarmAfterDownload(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	save := t.TempDir()
	e, _ := downloaded(t, t.TempDir(), save, engOpts{noSeeding: true}, fx, nil)
	waitSwarm(t, e, 0)

	h, ok := e.Get(fx.ih)
	if !ok {
		t.Fatal("torrent is gone")
	}
	s := h.Status()
	if s.Status != StatusSeeding || !s.Parked || s.Size != s.Done || s.Size != 64<<10 || s.TotalFiles != 2 || s.SelectedFiles != 2 || s.Peers != 0 {
		t.Fatalf("status %+v", s)
	}
	if err := h.WaitInfo(context.Background()); err != nil {
		t.Fatal(err)
	}
	fs := h.Files()
	if len(fs) != 2 || fs[1].FullPath != fx.path(save, 1) || !fs[1].Selected || fs[1].Progress != 1 {
		t.Fatalf("files %+v", fs)
	}
	done, size, err := e.WaitVerified(context.Background(), fx.ih)
	if err != nil || done != size || size != 64<<10 {
		t.Fatalf("verified %d/%d %v", done, size, err)
	}
	r, err := h.NewReader(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, fx.files[1]) {
		t.Fatalf("read %d bytes, %v", len(got), err)
	}
	if inSwarm(e) != 0 {
		t.Fatal("reading a finished torrent brought it back to the swarm")
	}
}

// A restart with seeding off never brings a finished torrent to the swarm,
// not even for a moment.
func TestNoSeedingRestartStaysOutOfTheSwarm(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	dir, save := t.TempDir(), t.TempDir()
	e, _ := downloaded(t, dir, save, engOpts{}, fx, []int{0})
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2 := openEngine(t, dir, engOpts{noSeeding: true})
	if n := inSwarm(e2); n != 0 {
		t.Fatalf("attached %d torrents at start", n)
	}
	h, _ := e2.Get(fx.ih)
	s := h.Status()
	if s.Status != StatusSeeding || !s.Parked || s.Size != 32<<10 || s.Done != 32<<10 || s.SelectedFiles != 1 || s.TotalFiles != 2 {
		t.Fatalf("status %+v", s)
	}
}

// The toggle works live, both ways.
func TestNoSeedingToggleLive(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	e, _ := downloaded(t, t.TempDir(), t.TempDir(), engOpts{}, fx, nil)
	if inSwarm(e) != 1 {
		t.Fatal("seeding by default: must be in the swarm")
	}
	e.SetNoSeeding(true)
	waitSwarm(t, e, 0)
	e.SetNoSeeding(false)
	waitSwarm(t, e, 1)
	s := waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding && s.Size == s.Done })
	if s.Parked {
		t.Fatalf("status %+v", s)
	}
}

// More files of a finished torrent: it comes back, downloads them, leaves.
func TestNoSeedingSelectionGrows(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	save := t.TempDir()
	e, seed := downloaded(t, t.TempDir(), save, engOpts{noSeeding: true}, fx, []int{0})
	waitSwarm(t, e, 0)
	if err := e.AddFiles(context.Background(), fx.ih, []int{1}); err != nil {
		t.Fatal(err)
	}
	waitSwarm(t, e, 1)
	connect(t, e, seed, fx.ih)
	waitFor(t, e, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding && s.SelectedFiles == 2 && s.Parked })
	waitSwarm(t, e, 0)
	if got, err := os.ReadFile(fx.path(save, 1)); err != nil || !bytes.Equal(got, fx.files[1]) {
		t.Fatalf("file 1: %v", err)
	}
}

// A player reading the torrent keeps it attached (dropping it would break
// the stream), but nothing is uploaded; it leaves when the player closes.
func TestNoSeedingWaitsForOpenReader(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	e, _ := downloaded(t, t.TempDir(), t.TempDir(), engOpts{}, fx, nil)
	h, _ := e.Get(fx.ih)
	r, err := h.NewReader(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	e.SetNoSeeding(true)
	time.Sleep(400 * time.Millisecond)
	if inSwarm(e) != 1 {
		t.Fatal("dropped under an open reader")
	}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("read: %v", err)
	}
	r.Close()
	waitSwarm(t, e, 0)
}

// A detached torrent is removed like any other, its files with it.
func TestNoSeedingRemoveWithData(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	save := t.TempDir()
	e, _ := downloaded(t, t.TempDir(), save, engOpts{noSeeding: true}, fx, nil)
	waitSwarm(t, e, 0)
	if err := e.Remove(context.Background(), fx.ih, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Get(fx.ih); ok {
		t.Fatal("still there")
	}
	if _, err := os.Stat(fx.path(save, 0)); !os.IsNotExist(err) {
		t.Fatalf("file stays: %v", err)
	}
}

// A paused finished torrent stays paused, and out of the swarm too.
func TestNoSeedingPausedFinishedTorrent(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	e, _ := downloaded(t, t.TempDir(), t.TempDir(), engOpts{}, fx, nil)
	if err := e.Pause(context.Background(), fx.ih); err != nil {
		t.Fatal(err)
	}
	e.SetNoSeeding(true)
	waitSwarm(t, e, 0)
	h, _ := e.Get(fx.ih)
	if s := h.Status(); s.Status != StatusPaused {
		t.Fatalf("status %+v", s)
	}
	if err := e.Resume(context.Background(), fx.ih); err != nil {
		t.Fatal(err)
	}
	if s := h.Status(); s.Status != StatusSeeding || !s.Parked || inSwarm(e) != 0 {
		t.Fatalf("status %+v, swarm %d", s, inSwarm(e))
	}
}

// partial adds fx to a fresh engine, wanting every file, from a seeder that
// has only file 0: the torrent holds file 0 and stays in progress.
func partial(t *testing.T, o engOpts, fx multiFixture) *Engine {
	t.Helper()
	seed := multiSeeder(t, fx, []int{0})
	e := openEngine(t, t.TempDir(), o)
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	connect(t, e, seed, fx.ih)
	waitFor(t, e, fx.ih, 15*time.Second, func(s Status) bool { return s.Done == int64(len(fx.files[0])) })
	if s, _ := status(e, fx.ih); s.CompletedAt != nil {
		t.Fatalf("finished: %+v", s)
	}
	return e
}

// leech is a peer that wants fx and has nothing of it, dialling e — what a
// rights-holder monitor does.
func leech(t *testing.T, e *Engine, fx multiFixture) *Engine {
	t.Helper()
	l := openEngine(t, t.TempDir(), engOpts{})
	if _, err := l.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	connect(t, l, e, fx.ih)
	return l
}

// getsNothing: over a while, l receives nothing and e uploads nothing.
func getsNothing(t *testing.T, e, l *Engine, ih string) {
	t.Helper()
	base, _ := status(e, ih)
	met := false
	for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		ls, _ := status(l, ih)
		es, _ := status(e, ih)
		if ls.Done != 0 || es.Uploaded != base.Uploaded {
			t.Fatalf("uploaded while downloading: leech done %d, ours uploaded %d (was %d)", ls.Done, es.Uploaded, base.Uploaded)
		}
		met = met || ls.Peers > 0
	}
	if !met {
		t.Fatal("the leech never connected: nothing was tested")
	}
}

// With seeding off a torrent still downloading uploads nothing either: a
// monitor asking for a piece it already has gets none. Live both ways.
func TestNoSeedingInProgressUploadsNothing(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	e := partial(t, engOpts{noSeeding: true}, fx)
	l := leech(t, e, fx)
	getsNothing(t, e, l, fx.ih)

	e.SetNoSeeding(false)
	waitFor(t, l, fx.ih, 15*time.Second, func(s Status) bool { return s.Done == int64(len(fx.files[0])) })

	e.SetNoSeeding(true)
	l2 := leech(t, e, fx)
	getsNothing(t, e, l2, fx.ih)
}

// Seeding off turned on while a torrent is downloading takes effect at once.
func TestNoSeedingTurnedOnMidDownload(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	e := partial(t, engOpts{}, fx)
	e.SetNoSeeding(true)
	l := leech(t, e, fx)
	getsNothing(t, e, l, fx.ih)
}

// Resuming a paused download keeps seeding off.
func TestNoSeedingResumeInProgress(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	e := partial(t, engOpts{noSeeding: true}, fx)
	if err := e.Pause(context.Background(), fx.ih); err != nil {
		t.Fatal(err)
	}
	if err := e.Resume(context.Background(), fx.ih); err != nil {
		t.Fatal(err)
	}
	l := leech(t, e, fx)
	getsNothing(t, e, l, fx.ih)
}
