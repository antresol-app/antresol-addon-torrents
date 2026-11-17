package organize

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
)

// A season pack over a library that already holds some of its episodes
// (under other names) places only the missing ones: no "(2)" copies.
func TestOrganizeSkipsEpisodesInLibrary(t *testing.T) {
	root := t.TempDir()
	tor := "Shogun.S01.1080p"
	var rels []string
	for e := 1; e <= 4; e++ {
		rel := fmt.Sprintf("%s/Shogun.S01E%02d.mkv", tor, e)
		writeFile(t, root, "torrents/"+rel, fmt.Sprint(e))
		rels = append(rels, rel)
	}
	series := filepath.Join(root, "media/series")
	dir := filepath.Join(series, "Сёгун", "Season 01")
	writeFile(t, root, "media/series/Сёгун/Season 01/Shogun.S01E01.WEB.mkv", "old1")
	writeFile(t, root, "media/series/Сёгун/Season 01/Сёгун - S01E02.mkv", "old2")
	job := Job{Kind: library.KindSeries, Title: "Сёгун", Season: 1, SeriesDir: series, MoviesDir: filepath.Join(root, "media/movies"),
		Files: onDisk(t, root, rels...), Have: map[[2]int]bool{{1, 1}: true, {1, 2}: true}}
	paths, err := Organize(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "Сёгун - S01E03.mkv"), filepath.Join(dir, "Сёгун - S01E04.mkv")}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths %v", paths)
	}
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"Shogun.S01E01.WEB.mkv", "tvshow.nfo", "Сёгун - S01E02.mkv", "Сёгун - S01E03.mkv", "Сёгун - S01E04.mkv"}) &&
		!reflect.DeepEqual(names, []string{"Shogun.S01E01.WEB.mkv", "Сёгун - S01E02.mkv", "Сёгун - S01E03.mkv", "Сёгун - S01E04.mkv"}) {
		t.Fatalf("files %v", names)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "Сёгун - S01E02.mkv")); string(b) != "old2" {
		t.Fatalf("existing episode touched: %q", b)
	}
	// run again (the scan now has E03-E04 too): our own links still count
	job.Have = map[[2]int]bool{{1, 1}: true, {1, 2}: true, {1, 3}: true, {1, 4}: true}
	again, err := Organize(context.Background(), job)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("again %v %v", again, err)
	}
	// everything already there: nothing to place, no error
	job.Files = onDisk(t, root, rels[0], rels[1])
	none, err := Organize(context.Background(), job)
	if err != nil || len(none) != 0 {
		t.Fatalf("all had: %v %v", none, err)
	}
}
