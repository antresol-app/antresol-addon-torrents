package torrent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// A selection grows («Выбрать треки…» again on the same release): the new
// files download too, the ones done stay as they are, OnComplete fires
// once more when the larger selection is complete, and the selection is
// kept across a restart.
func TestSelectionGrowsAfterComplete(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10, 32<<10)
	seed := multiSeeder(t, fx, nil)
	dir := t.TempDir()
	leech := openEngine(t, dir, engOpts{})
	var hooks hookCount
	leech.OnComplete(hooks.add)
	save := t.TempDir()
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: []int{0}}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	waitHooks(t, &hooks, 1, 2*time.Second)
	first, err := os.Stat(fx.path(save, 0))
	if err != nil {
		t.Fatal(err)
	}

	if err := leech.AddFiles(context.Background(), fx.ih, []int{2, 0}); err != nil {
		t.Fatal(err)
	}
	if got := filesJSON(t, leech.db, fx.ih); got.String != "[0,2]" {
		t.Fatalf("files_json %+v", got)
	}
	if f := fileByIndex(t, leech, fx.ih, 2); !f.Selected {
		t.Fatalf("file 2 not selected: %+v", f)
	}
	s := waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding && s.SelectedFiles == 2 })
	waitHooks(t, &hooks, 2, 2*time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := hooks.get(); n != 2 {
		t.Fatalf("OnComplete fired %d times", n)
	}
	if s.Size != 65536 || s.Done != 65536 || s.TotalFiles != 4 {
		t.Fatalf("status %+v", s)
	}
	for _, i := range []int{0, 2} {
		if got, err := os.ReadFile(fx.path(save, i)); err != nil || !bytes.Equal(got, fx.files[i]) {
			t.Fatalf("file %d differs: %v", i, err)
		}
	}
	// the file that was done is the same file, not written again
	if again, err := os.Stat(fx.path(save, 0)); err != nil || !os.SameFile(first, again) || !again.ModTime().Equal(first.ModTime()) {
		t.Fatalf("file 0 rewritten: %v", err)
	}
	for _, i := range []int{1, 3} {
		if f := fileByIndex(t, leech, fx.ih, i); f.Selected {
			t.Fatalf("file %d selected: %+v", i, f)
		}
	}

	// nothing new: no change, no second completion
	if err := leech.AddFiles(context.Background(), fx.ih, []int{2}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := hooks.get(); n != 2 {
		t.Fatalf("OnComplete fired %d times after a no-op", n)
	}
}

// Grown while it still downloads: it completes once, when every file of
// the larger selection is done.
func TestSelectionGrowsWhileDownloading(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	seed := multiSeeder(t, fx, nil)
	leech := openEngine(t, t.TempDir(), engOpts{})
	var hooks hookCount
	leech.OnComplete(hooks.add)
	save := t.TempDir()
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: []int{0}}); err != nil {
		t.Fatal(err)
	}
	if err := leech.AddFiles(context.Background(), fx.ih, []int{2}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	s := waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	waitHooks(t, &hooks, 1, 2*time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := hooks.get(); n != 1 {
		t.Fatalf("OnComplete fired %d times", n)
	}
	if s.SelectedFiles != 2 {
		t.Fatalf("status %+v", s)
	}
	for _, i := range []int{0, 2} {
		if got, err := os.ReadFile(fx.path(save, i)); err != nil || !bytes.Equal(got, fx.files[i]) {
			t.Fatalf("file %d differs: %v", i, err)
		}
	}
}

func TestAddFilesRefusals(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)
	e := openEngine(t, t.TempDir(), engOpts{})
	ctx := context.Background()
	if err := e.AddFiles(ctx, fx.ih, []int{1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown torrent: %v", err)
	}
	if _, err := e.AddTorrentFile(ctx, fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir(), Files: []int{0}}); err != nil {
		t.Fatal(err)
	}
	if err := e.AddFiles(ctx, fx.ih, []int{5}); err == nil {
		t.Fatal("out of range accepted")
	}
	if err := e.AddFiles(ctx, fx.ih, nil); err == nil {
		t.Fatal("no files accepted")
	}
	// every file: the whole torrent (no selection left)
	if err := e.AddFiles(ctx, fx.ih, []int{1}); err != nil {
		t.Fatal(err)
	}
	if got := filesJSON(t, e.db, fx.ih); got.Valid {
		t.Fatalf("files_json %+v", got)
	}
}
