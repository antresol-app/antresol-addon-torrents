package torrent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A season of a pack is cancelled: its files leave the
// selection and their own data is deleted; the other files stay as they
// are, verified; selecting the files again downloads them afresh.
func TestRemoveFilesDropsOnlyTheirData(t *testing.T) {
	ctx := context.Background()
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10, 32<<10)
	seed := multiSeeder(t, fx, nil)
	leech := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	if _, err := leech.AddTorrentFile(ctx, fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: []int{0, 1, 2}}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })

	if err := leech.RemoveFiles(ctx, fx.ih, []int{2}, anyPath); err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, leech, fx.ih, 5*time.Second, func(s Status) bool { return s.SelectedFiles == 2 })
	if s.Status != StatusSeeding || s.Size != 64<<10 || s.Done != 64<<10 {
		t.Fatalf("after the removal: %+v", s)
	}
	if st, err := os.Stat(fx.path(save, 2)); err != nil || st.Size() != 0 {
		t.Fatalf("the removed file's data is still there: %v %v", st, err)
	}
	for _, i := range []int{0, 1} {
		if got, _ := os.ReadFile(fx.path(save, i)); !bytes.Equal(got, fx.files[i]) {
			t.Fatalf("file %d changed", i)
		}
	}
	if got := filesJSON(t, leech.db, fx.ih); got.String != "[0,1]" {
		t.Fatalf("files_json %+v", got)
	}
	// selected again: downloaded again, not taken for done
	if err := leech.AddFiles(ctx, fx.ih, []int{2}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	s = waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.SelectedFiles == 3 && s.Status == StatusSeeding })
	if s.Downloaded < 128<<10 { // 96 KiB, then the 32 KiB file again
		t.Fatalf("selected again, taken for done without a download: %+v", s)
	}
	if got, err := os.ReadFile(fx.path(save, 2)); !bytes.Equal(got, fx.files[2]) {
		s, _ := status(leech, fx.ih)
		t.Fatalf("the file selected again differs: %d bytes, %v; %+v", len(got), err, s)
	}
}

// a dropped file hard-linked into the library, or one the
// caller does not allow, keeps its data; only a file of no other link
// that the caller allows is emptied.
func TestRemoveFilesNeverEmptiesALinkedFile(t *testing.T) {
	ctx := context.Background()
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10, 32<<10)
	seed := multiSeeder(t, fx, nil)
	leech := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	if _, err := leech.AddTorrentFile(ctx, fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	lib := filepath.Join(t.TempDir(), "S01E02.flac")
	if err := os.Link(fx.path(save, 1), lib); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{fx.path(save, 1): true, fx.path(save, 3): true}
	if err := leech.RemoveFiles(ctx, fx.ih, []int{1, 2, 3}, func(p string) bool { return allowed[p] }); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{lib, fx.path(save, 1)} {
		if got, _ := os.ReadFile(p); !bytes.Equal(got, fx.files[1]) {
			t.Fatalf("the hard-linked file %s lost its data", p)
		}
	}
	if got, _ := os.ReadFile(fx.path(save, 2)); !bytes.Equal(got, fx.files[2]) {
		t.Fatal("a file the caller did not allow was emptied")
	}
	if st, err := os.Stat(fx.path(save, 3)); err != nil || st.Size() != 0 {
		t.Fatalf("the allowed unlinked file kept its data: %v %v", st, err)
	}
	if s, _ := status(leech, fx.ih); s.SelectedFiles != 1 {
		t.Fatalf("status %+v", s)
	}
}

// A dropped file that shares a piece with a kept one stays on disk (the
// kept file's piece is verified across both); the refusals.
func TestRemoveFilesKeepsSharedPieces(t *testing.T) {
	ctx := context.Background()
	fx := newMultiFixture(t, 16<<10, 20<<10, 20<<10, 20<<10)
	seed := multiSeeder(t, fx, nil)
	leech := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	if _, err := leech.AddTorrentFile(ctx, fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: []int{0, 1}}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if err := leech.RemoveFiles(ctx, fx.ih, []int{0, 1}, anyPath); err == nil {
		t.Fatal("removing every selected file accepted")
	}
	if err := leech.RemoveFiles(ctx, fx.ih, []int{1}, anyPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fx.path(save, 1)); err != nil {
		t.Fatalf("a file sharing a piece with a kept one was deleted: %v", err)
	}
	if got, _ := os.ReadFile(fx.path(save, 0)); !bytes.Equal(got, fx.files[0]) {
		t.Fatal("the kept file changed")
	}
	if s, _ := status(leech, fx.ih); s.Status != StatusSeeding || s.SelectedFiles != 1 {
		t.Fatalf("status %+v", s)
	}
	// nothing selected that is not: no change
	if err := leech.RemoveFiles(ctx, fx.ih, []int{2}, anyPath); err != nil {
		t.Fatal(err)
	}
}

// anyPath allows every path to be emptied (RemoveFiles).
func anyPath(string) bool { return true }
