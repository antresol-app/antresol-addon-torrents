package torrent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
	"unsafe"

	atorrent "github.com/anacrolix/torrent"
)

// beginFixture seeds a torrent in <root>/torrents/movies and returns the
// engine, the fixture and the from/to folders of a move.
func beginFixture(t *testing.T, o engOpts) (*Engine, fixture, string, string) {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "torrents", "movies")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := newFixture(t, src)
	e := openEngine(t, t.TempDir(), o)
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	return e, fx, filepath.Join(root, "torrents"), filepath.Join(root, "archive", "torrents")
}

func TestBeginFinish_Renamed(t *testing.T) {
	e, fx, from, to := beginFixture(t, engOpts{})
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if k := r.Keys(); len(k) != 1 || k[0] != fx.ih {
		t.Fatalf("keys %v", k)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, e, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusSeeding && s.Done == testSize })
	if want := filepath.Join(to, "movies"); s.SaveDir != want || s.Downloaded != 0 {
		t.Fatalf("save %q want %q, downloaded %d", s.SaveDir, want, s.Downloaded)
	}
	if done, size, err := e.WaitVerified(context.Background(), fx.ih); err != nil || done != size {
		t.Fatalf("verify %d/%d %v", done, size, err)
	}
}

func TestBeginFinish_NotRenamed(t *testing.T) {
	e, fx, from, to := beginFixture(t, engOpts{})
	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Finish(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, e, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if want := filepath.Join(from, "movies"); s.SaveDir != want {
		t.Fatalf("save %q want %q", s.SaveDir, want)
	}
	if inSwarm(e) != 1 {
		t.Fatal("torrent was not attached again")
	}
}

func TestBegin_BusyWhileMoving(t *testing.T) {
	e, _, from, to := beginFixture(t, engOpts{})
	r, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.BeginRelocate(context.Background(), from, to); !errors.Is(err, ErrBusy) {
		t.Fatalf("second begin: %v", err)
	}
	if err := r.Finish(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	r2, err := e.BeginRelocate(context.Background(), from, to)
	if err != nil {
		t.Fatalf("begin after finish: %v", err)
	}
	if err := r2.Finish(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

// uploadDisallowed reads the client's own flag of the torrent handle (the
// library has no getter): what DisallowDataUpload set. It reads the unexported
// field of anacrolix/torrent v1.61.0; revisit it when that dependency moves.
func uploadDisallowed(tt *atorrent.Torrent) bool {
	f := reflect.ValueOf(tt).Elem().FieldByName("dataUploadDisallowed")
	return reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Bool()
}

// With seeding off a torrent re-added by Finish uploads nothing, whichever
// way the relocation ended. The torrent is still downloading (no seeder), so
// it is attached at Begin and goes through attach again in Finish.
func TestBeginFinish_ReaddedUploadsNothing(t *testing.T) {
	for _, renamed := range []bool{true, false} {
		root := t.TempDir()
		src := filepath.Join(root, "torrents", "movies")
		to := filepath.Join(root, "archive", "torrents")
		from := filepath.Dir(src)
		for _, d := range []string{src, filepath.Dir(to)} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		fx := newFixture(t, t.TempDir())
		e := openEngine(t, t.TempDir(), engOpts{noSeeding: true})
		if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: src}); err != nil {
			t.Fatal(err)
		}
		r, err := e.BeginRelocate(context.Background(), from, to)
		if err != nil {
			t.Fatal(err)
		}
		if k := r.Keys(); len(k) != 1 {
			t.Fatalf("renamed=%v: keys %v (torrent not attached at Begin?)", renamed, k)
		}
		if !r.ms[0].wasOn {
			t.Fatalf("renamed=%v: torrent was not attached at Begin", renamed)
		}
		e.mu.Lock()
		en := e.entries[fx.ih]
		e.mu.Unlock()
		if renamed {
			if err := os.Rename(from, to); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Finish(context.Background(), renamed); err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		attached, off, handle := en.t != nil, en.uploadOff, false
		if attached {
			handle = uploadDisallowed(en.t)
		}
		e.mu.Unlock()
		if !attached || !off || !handle {
			t.Fatalf("renamed=%v: after Finish attached=%v uploadOff=%v handle disallows upload=%v", renamed, attached, off, handle)
		}
	}
}

// A finished torrent is parked before Begin and stays parked after Finish.
func TestBeginFinish_FinishedStaysParked(t *testing.T) {
	for _, renamed := range []bool{true, false} {
		e, fx, from, to := beginFixture(t, engOpts{noSeeding: true})
		waitSwarm(t, e, 0)
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
			t.Fatal(err)
		}
		r, err := e.BeginRelocate(context.Background(), from, to)
		if err != nil {
			t.Fatal(err)
		}
		if renamed {
			if err := os.Rename(from, to); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Finish(context.Background(), renamed); err != nil {
			t.Fatal(err)
		}
		waitSwarm(t, e, 0)
		waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Parked })
	}
}

// staleFlagEngine: no-seeding off, one attached seeding torrent and its
// entry; the caller sets the stale uploadOff and breaks the re-attach
// (TestFailedReattachClearsUploadFlag).
func staleFlagEngine(t *testing.T) (*Engine, fixture, *entry) {
	t.Helper()
	e, fx, _, _ := beginFixture(t, engOpts{})
	e.mu.Lock()
	defer e.mu.Unlock()
	en := e.entries[fx.ih]
	return e, fx, en
}

// An attach made while no-seeding is off must not inherit a stale uploadOff:
// the sweep trusts the flag, so turning no-seeding on later would leave the
// torrent uploading. (In progress: never parked, so the check is not skipped.)
func TestAttachDecidesUploadFlagAnew(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	e := partial(t, engOpts{}, fx)
	e.mu.Lock()
	en := e.entries[fx.ih]
	old := en.t
	en.t, en.uploadOff = nil, true // what a failed re-attach used to leave
	e.mu.Unlock()
	old.Drop()

	e.mu.Lock()
	spec, err := e.specFor(en.rec)
	if err == nil {
		err = e.attach(en, spec)
	}
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	e.SetNoSeeding(true)
	e.mu.Lock()
	defer e.mu.Unlock()
	if en.t == nil || !uploadDisallowed(en.t) {
		t.Fatal("handle still uploads after no-seeding was turned on")
	}
}

// Every path that sets the handle to nil clears the flag with it, so a later
// attach (here a Resume-like one) never inherits it.
func TestFailedReattachClearsUploadFlag(t *testing.T) {
	cases := map[string]func(e *Engine, en *entry){
		"move": func(e *Engine, en *entry) {
			e.mu.Lock()
			en.rec.TargetDir = t.TempDir()
			e.mu.Unlock()
			e.move(en)
		},
		"reattach": func(e *Engine, en *entry) {
			e.mu.Lock()
			en.relocating = true
			tt := en.t
			e.mu.Unlock()
			e.wg.Add(1)
			e.reattach(en, tt)
		},
		"relocate": func(e *Engine, en *entry) {
			e.mu.Lock()
			from := en.rec.SaveDir
			e.mu.Unlock()
			r, err := e.BeginRelocate(context.Background(), from, from+"-x")
			if err != nil {
				t.Fatal(err)
			}
			r.Finish(context.Background(), false)
		},
	}
	for name, run := range cases {
		e, fx, en := staleFlagEngine(t)
		e.mu.Lock()
		en.uploadOff = true // stale
		e.mu.Unlock()
		if err := os.Remove(e.metafilePath(fx.ih)); err != nil {
			t.Fatal(err)
		}
		run(e, en)
		e.mu.Lock()
		gone, off, errStr := en.t == nil, en.uploadOff, en.rec.Error
		e.mu.Unlock()
		if !gone || off || errStr == "" {
			t.Fatalf("%s: handle gone=%v uploadOff=%v error=%q", name, gone, off, errStr)
		}
	}
}
