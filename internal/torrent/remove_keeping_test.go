package torrent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeTree(t *testing.T, root string, files ...string) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var left []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != root {
			rel, _ := filepath.Rel(root, p)
			left = append(left, rel)
		}
		return nil
	})
	sort.Strings(left)
	return left
}

// removeOwnFiles deletes the torrent's own files only — never another file
// in its folder (a voice-over, subtitles, an .nfo placed beside a video
// later) — and of those not the ones it is told to keep; a folder goes only
// when it ends up empty.
func TestRemoveOwnFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Альбом")
	own := []string{"01.flac", "02.flac", "CD2/01.flac", "CD2/02.flac", "Scans/front.jpg", "album.log"}
	writeTree(t, root, own...)
	writeTree(t, root, "02.ru.srt", "CD2/02.mka", "album.nfo") // not the torrent's
	var paths []string
	for _, f := range own {
		paths = append(paths, filepath.Join(root, f))
	}
	keep := map[string]bool{filepath.Join(root, "02.flac"): true, filepath.Join(root, "CD2", "01.flac"): true}
	if err := removeOwnFiles(root, paths, func(p string) bool { return keep[p] }); err != nil {
		t.Fatal(err)
	}
	want := []string{"02.flac", "02.ru.srt", "CD2", filepath.Join("CD2", "01.flac"), filepath.Join("CD2", "02.mka"), "album.nfo"}
	if got := treeOf(t, root); len(got) != len(want) || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("left %v, want %v", got, want)
	}
	// nothing kept and nothing else there: the folder goes, as before
	root2 := filepath.Join(t.TempDir(), "Фильм")
	writeTree(t, root2, "film.mkv", "Sub/ru.srt")
	if err := removeOwnFiles(root2, []string{filepath.Join(root2, "film.mkv"), filepath.Join(root2, "Sub", "ru.srt")},
		func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root2); !os.IsNotExist(err) {
		t.Fatalf("root left: %v", err)
	}
	// a single-file torrent: the file is the root
	one := filepath.Join(t.TempDir(), "film.mkv")
	os.WriteFile(one, []byte("x"), 0o644)
	if err := removeOwnFiles(one, []string{one}, func(p string) bool { return p == one }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(one); err != nil {
		t.Fatalf("a kept single file went: %v", err)
	}
	if err := removeOwnFiles(one, []string{one}, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(one); !os.IsNotExist(err) {
		t.Fatalf("single file left: %v", err)
	}
	// a path outside the root (a hostile file name) is never touched
	outside := filepath.Join(t.TempDir(), "other.txt")
	os.WriteFile(outside, []byte("x"), 0o644)
	if err := removeOwnFiles(root, []string{outside}, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a file outside the root went: %v", err)
	}
}

// RemoveKeeping removes only the data its keep was made for: a torrent
// whose data root is not that one (moved meanwhile) stays, untouched.
func TestRemoveKeepingRefusesAnotherRoot(t *testing.T) {
	e, fx := newSeeder(t, engOpts{})
	root, ok := e.DataRoot(fx.ih)
	if !ok || filepath.Base(root) != fx.name {
		t.Fatalf("data root %q %v", root, ok)
	}
	err := e.RemoveKeeping(context.Background(), fx.ih, root+"-elsewhere", func(string) bool { return false })
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err %v", err)
	}
	if _, ok := e.Get(fx.ih); !ok {
		t.Fatal("removed")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("data touched: %v", err)
	}
	if err := e.RemoveKeeping(context.Background(), fx.ih, root, func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("data left: %v", err)
	}
}
