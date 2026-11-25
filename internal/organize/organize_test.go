package organize

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
)

// writeFile creates a file with content under root and returns its path.
func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func fileInfos(paths ...string) []File {
	out := make([]File, len(paths))
	for i, p := range paths {
		st, _ := os.Stat(p)
		var size int64
		if st != nil {
			size = st.Size()
		}
		out[i] = File{Path: filepath.Base(p), FullPath: p, Size: size}
	}
	return out
}

func TestOrganizeMovieHardlink(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "torrents/movies/Dune.Part.Two.2024.1080p/Dune.Part.Two.2024.1080p.mkv", strings.Repeat("v", 1000))
	sample := writeFile(t, root, "torrents/movies/Dune.Part.Two.2024.1080p/sample.mkv", "s")
	movies, series := filepath.Join(root, "media/movies"), filepath.Join(root, "media/series")

	paths, err := Organize(context.Background(), Job{
		Kind: library.KindMovie, Title: "Дюна", Year: 2024, TMDBID: 693134,
		Files: fileInfos(src, sample), MoviesDir: movies, SeriesDir: series,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(movies, "Дюна (2024)", "Дюна (2024).mkv")
	if len(paths) != 1 || paths[0] != want {
		t.Fatalf("paths %v, want [%s]", paths, want)
	}
	a, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(want)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("library file is not a hard link of the torrent file")
	}
	nfo, err := os.ReadFile(filepath.Join(movies, "Дюна (2024)", "Дюна (2024).nfo"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(nfo), "<tmdbid>693134</tmdbid>") || !strings.Contains(string(nfo), "<title>Дюна</title>") {
		t.Fatalf("nfo: %s", nfo)
	}
	// The torrent keeps seeding from its own copy.
	if _, err := os.Stat(src); err != nil {
		t.Fatal(err)
	}
	// Idempotent: a second run (restart) neither fails nor duplicates.
	again, err := Organize(context.Background(), Job{
		Kind: library.KindMovie, Title: "Дюна", Year: 2024, TMDBID: 693134,
		Files: fileInfos(src, sample), MoviesDir: movies, SeriesDir: series,
	})
	if err != nil || len(again) != 1 || again[0] != want {
		t.Fatalf("second run: %v %v", again, err)
	}
	es, _ := os.ReadDir(filepath.Join(movies, "Дюна (2024)"))
	if len(es) != 2 {
		t.Fatalf("entries after second run: %d", len(es))
	}
}

func TestOrganizeEpisode(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "torrents/series/Shogun.S01.1080p/Shogun.S01E03.1080p.WEB-DL.mkv", "episode3")
	src4 := writeFile(t, root, "torrents/series/Shogun.S01.1080p/Shogun.S01E04.1080p.WEB-DL.mkv", "episode4")
	series := filepath.Join(root, "media/series")
	paths, err := Organize(context.Background(), Job{
		Kind: library.KindSeries, Title: "Сёгун", Year: 2024, TMDBID: 126308, Season: 1, Episode: 3,
		Files: fileInfos(src, src4), MoviesDir: filepath.Join(root, "media/movies"), SeriesDir: series,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(series, "Сёгун", "Season 01", "Сёгун - S01E03.mkv")
	if len(paths) != 2 || paths[0] != want || paths[1] != filepath.Join(series, "Сёгун", "Season 01", "Сёгун - S01E04.mkv") {
		t.Fatalf("paths %v", paths)
	}
	b, err := os.ReadFile(want)
	if err != nil || string(b) != "episode3" {
		t.Fatalf("%q %v", b, err)
	}
	nfo, err := os.ReadFile(filepath.Join(series, "Сёгун", "tvshow.nfo"))
	if err != nil || !strings.Contains(string(nfo), "<tmdbid>126308</tmdbid>") {
		t.Fatalf("tvshow.nfo: %s %v", nfo, err)
	}
}

func TestOrganizeEpisodeWithoutNumbersUsesRequest(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "torrents/series/x/Сёгун (серия третья).mkv", "e")
	series := filepath.Join(root, "media/series")
	paths, err := Organize(context.Background(), Job{
		Kind: library.KindSeries, Title: "Сёгун", TMDBID: 1, Season: 1, Episode: 3,
		Files: fileInfos(src), SeriesDir: series,
	})
	if err != nil || len(paths) != 1 || paths[0] != filepath.Join(series, "Сёгун", "Season 01", "Сёгун - S01E03.mkv") {
		t.Fatalf("%v %v", paths, err)
	}
}

func TestOrganizeSanitizesName(t *testing.T) {
	for _, in := range []string{"AC/DC: Live", `a\b*c?d"e<f>g|h`, "..", "  ", "Name. "} {
		got := SafeName(in)
		if got == "" || strings.ContainsAny(got, `/\:*?"<>|`) || got == "." || got == ".." || strings.HasSuffix(got, ".") || strings.HasSuffix(got, " ") {
			t.Errorf("SafeName(%q) = %q", in, got)
		}
	}
	if got := SafeName("AC/DC: Live"); got != "AC-DC - Live" {
		t.Errorf("AC/DC: Live → %q", got)
	}

	root := t.TempDir()
	src := writeFile(t, root, "torrents/movies/acdc.mkv", "x")
	movies := filepath.Join(root, "media/movies")
	paths, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "AC/DC: Live", Year: 1992, TMDBID: 5, Files: fileInfos(src), MoviesDir: movies})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != filepath.Join(movies, "AC-DC - Live (1992)", "AC-DC - Live (1992).mkv") {
		t.Fatalf("%v", paths)
	}
}

func TestOrganizeCopiesWhenLinkFails(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "torrents/movies/m.mkv", "data")
	movies := filepath.Join(root, "media/movies")
	old := linkFile
	linkFile = func(string, string) error { return &os.LinkError{Op: "link", Err: os.ErrPermission} }
	t.Cleanup(func() { linkFile = old })
	paths, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "Фильм", Year: 2001, TMDBID: 1, Files: fileInfos(src), MoviesDir: movies})
	if err != nil || len(paths) != 1 {
		t.Fatalf("%v %v", paths, err)
	}
	b, err := os.ReadFile(paths[0])
	if err != nil || string(b) != "data" {
		t.Fatalf("%q %v", b, err)
	}
	a, _ := os.Stat(src)
	c, _ := os.Stat(paths[0])
	if os.SameFile(a, c) {
		t.Fatal("expected a copy")
	}
}

// Jobs placing different files under one name (links failing: the copy
// path) all end up in the library, each under its own name; none replaces
// another's file and no partial copy is left.
func TestPlaceConcurrentCopiesKeepAll(t *testing.T) {
	root := t.TempDir()
	old := linkFile
	linkFile = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { linkFile = old })
	dst := filepath.Join(root, "lib", "Show S01E01.mkv")
	const n = 8
	want := map[string]string{} // source → its content
	srcs := make([]string, n)
	for i := range srcs {
		body := strings.Repeat("x", i+1)
		srcs[i] = writeFile(t, root, fmt.Sprintf("torrents/%d/e.mkv", i), body)
		want[srcs[i]] = body
	}
	got := make([]string, n)
	var wg sync.WaitGroup
	for i := range srcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := Place(context.Background(), srcs[i], dst)
			if err != nil {
				t.Errorf("place %d: %v", i, err)
			}
			got[i] = p
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, p := range got {
		b, err := os.ReadFile(p)
		if err != nil || string(b) != want[srcs[i]] {
			t.Errorf("job %d: %q at %s (%v), want %q", i, b, p, err, want[srcs[i]])
		}
		if seen[p] {
			t.Errorf("two jobs share %s", p)
		}
		seen[p] = true
	}
	if left, _ := filepath.Glob(filepath.Join(root, "lib", "*"+PartialSuffix)); len(left) != 0 {
		t.Fatalf("partial copies left: %v", left)
	}
}

// A dangling symlink at the target name is taken (Stat says free, link and
// copy say exists): the file goes to the numbered sibling, the loop ends,
// the symlink stays. Both with links and with the copy path.
func TestPlaceSkipsDanglingSymlink(t *testing.T) {
	for _, tc := range []struct {
		name string
		link func(string, string) error
	}{
		{"link", os.Link},
		{"copy", func(string, string) error { return os.ErrPermission }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			old := linkFile
			linkFile = tc.link
			t.Cleanup(func() { linkFile = old })
			src := writeFile(t, root, "torrents/e.mkv", "data")
			dst := filepath.Join(root, "lib", "Show S01E01.mkv")
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "gone"), dst); err != nil {
				t.Skip("no symlinks here:", err)
			}
			type res struct {
				p   string
				err error
			}
			done := make(chan res, 1)
			go func() { p, err := Place(context.Background(), src, dst); done <- res{p, err} }()
			select {
			case r := <-done:
				want := filepath.Join(root, "lib", "Show S01E01 (2).mkv")
				if r.err != nil || r.p != want {
					t.Fatalf("placed %q, %v; want %q", r.p, r.err, want)
				}
				if b, err := os.ReadFile(r.p); err != nil || string(b) != "data" {
					t.Fatalf("%q %v", b, err)
				}
				if _, err := os.Readlink(dst); err != nil {
					t.Fatalf("the symlink was replaced: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Place does not return on a dangling symlink")
			}
		})
	}
}

// copyFile never replaces a file that is there.
func TestCopyFileKeepsExisting(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "a/src.mkv", "new")
	dst := writeFile(t, root, "b/dst.mkv", "old")
	if err := copyFile(context.Background(), src, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("err = %v, want ErrExist", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "old" {
		t.Fatalf("dst replaced: %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "b", "*"+PartialSuffix)); len(left) != 0 {
		t.Fatalf("partial copies left: %v", left)
	}
}

func TestOrganizeReadOnlyLibraryHumanError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	root := t.TempDir()
	src := writeFile(t, root, "torrents/movies/m.mkv", "data")
	movies := filepath.Join(root, "media/movies")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chmod(movies, 0o555)
	t.Cleanup(func() { os.Chmod(movies, 0o755) })
	_, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "Фильм", Year: 2001, TMDBID: 1, Files: fileInfos(src), MoviesDir: movies})
	if err == nil || !strings.Contains(err.Error(), "Нет доступа на запись") {
		t.Fatalf("err = %v", err)
	}
}

func TestOrganizeNoVideo(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "torrents/movies/readme.txt", "x")
	_, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "Фильм", TMDBID: 1, Files: fileInfos(src), MoviesDir: filepath.Join(root, "m")})
	if err == nil || !strings.Contains(err.Error(), "видео") {
		t.Fatalf("err = %v", err)
	}
}

func TestOrganizeDoesNotAdoptForeignSameSizeFile(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "torrents/movies/m.mkv", "aaaa")
	movies := filepath.Join(root, "media/movies")
	foreign := writeFile(t, movies, "Фильм (2001)/Фильм (2001).mkv", "bbbb") // same size, other content
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(foreign, old, old)
	paths, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "Фильм", Year: 2001, TMDBID: 1, Files: fileInfos(src), MoviesDir: movies})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != filepath.Join(movies, "Фильм (2001)", "Фильм (2001) (2).mkv") {
		t.Fatalf("%v", paths)
	}
}

func TestOrganizeCopyIsIdempotent(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "torrents/movies/m.mkv", "data")
	movies := filepath.Join(root, "media/movies")
	old := linkFile
	linkFile = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { linkFile = old })
	job := Job{Kind: library.KindMovie, Title: "Фильм", Year: 2001, TMDBID: 1, Files: fileInfos(src), MoviesDir: movies}
	a, err := Organize(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Organize(context.Background(), job)
	if err != nil || len(b) != 1 || b[0] != a[0] {
		t.Fatalf("second run %v %v (first %v)", b, err, a)
	}
}

func TestOrganizeKeepsPartInTitle(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "torrents/movies/m.mkv", "x")
	movies := filepath.Join(root, "media/movies")
	paths, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "Kill Bill Part 2", TMDBID: 3, Files: fileInfos(src), MoviesDir: movies})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(strings.TrimSuffix(paths[0], ".mkv") + ".nfo"); err != nil {
		t.Fatalf("nfo does not match the video: %v", err)
	}
}

// onDisk lists files under root/torrents as the engine reports them
// (Path inside the torrent, with its folder).
func onDisk(t *testing.T, root string, rels ...string) []File {
	var out []File
	for _, rel := range rels {
		p := filepath.Join(root, "torrents", filepath.FromSlash(rel))
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, File{Path: rel, FullPath: p, Size: st.Size()})
	}
	return out
}

const ruSrt = "1\n00:00:01,000 --> 00:00:02,000\nПривет, как дела?\n"

func TestOrganizeCopiesTorrentSubs(t *testing.T) {
	root := t.TempDir()
	tor := "Dune.Part.Two.2024.1080p"
	writeFile(t, root, "torrents/"+tor+"/Dune.Part.Two.2024.1080p.mkv", strings.Repeat("v", 1000))
	writeFile(t, root, "torrents/"+tor+"/Subs/2_Russian.srt", ruSrt)
	writeFile(t, root, "torrents/"+tor+"/Movie.en.srt", "1\n00:00:01,000 --> 00:00:02,000\nHi\n")
	movies := filepath.Join(root, "media/movies")
	job := Job{Kind: library.KindMovie, Title: "Дюна", Year: 2024, TMDBID: 693134, MoviesDir: movies, SeriesDir: filepath.Join(root, "media/series"),
		Files: onDisk(t, root, tor+"/Dune.Part.Two.2024.1080p.mkv", tor+"/Subs/2_Russian.srt", tor+"/Movie.en.srt")}
	paths, err := Organize(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || filepath.Base(paths[0]) != "Дюна (2024).mkv" {
		t.Fatalf("videos only: %v", paths)
	}
	dir := filepath.Join(movies, "Дюна (2024)")
	for name, want := range map[string]string{"Дюна (2024).ru.srt": "Привет", "Дюна (2024).en.srt": "Hi"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !strings.Contains(string(b), want) {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	if _, err := Organize(context.Background(), job); err != nil { // idempotent
		t.Fatal(err)
	}
	es, _ := os.ReadDir(dir)
	if len(es) != 4 { // mkv, nfo, ru, en
		var names []string
		for _, e := range es {
			names = append(names, e.Name())
		}
		t.Fatalf("second run: %v", names)
	}
}

func TestOrganizeSubsTwoCD(t *testing.T) {
	root := t.TempDir()
	tor := "Moscow does not believe in tears (2CD)"
	writeFile(t, root, "torrents/"+tor+"/Moscow Does Not Believe in Tears CD1.avi", strings.Repeat("a", 500))
	writeFile(t, root, "torrents/"+tor+"/Moscow Does Not Believe in Tears CD2.avi", strings.Repeat("b", 600))
	writeFile(t, root, "torrents/"+tor+"/Moscow Does Not Believe in Tears CD1.srt", ruSrt+"cd1\n")
	writeFile(t, root, "torrents/"+tor+"/Moscow Does Not Believe in Tears CD2.srt", ruSrt+"cd2\n")
	movies := filepath.Join(root, "media/movies")
	_, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "Москва слезам не верит", Year: 1979, MoviesDir: movies,
		Files: onDisk(t, root, tor+"/Moscow Does Not Believe in Tears CD1.avi", tor+"/Moscow Does Not Believe in Tears CD2.avi",
			tor+"/Moscow Does Not Believe in Tears CD1.srt", tor+"/Moscow Does Not Believe in Tears CD2.srt")})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(movies, "Москва слезам не верит (1979)")
	for _, n := range []string{"1", "2"} {
		b, err := os.ReadFile(filepath.Join(dir, "Москва слезам не верит (1979) - cd"+n+".ru.srt"))
		if err != nil || !strings.Contains(string(b), "cd"+n) {
			t.Fatalf("cd%s: %q %v", n, b, err)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.ru.2.srt")); len(m) != 0 {
		t.Fatalf("%v", m)
	}
}

func TestOrganizeSeriesSubsByEpisode(t *testing.T) {
	root := t.TempDir()
	tor := "Shogun.S01.1080p"
	writeFile(t, root, "torrents/"+tor+"/Shogun.S01E01.mkv", "1")
	writeFile(t, root, "torrents/"+tor+"/Shogun.S01E02.mkv", "2")
	writeFile(t, root, "torrents/"+tor+"/Subs/Shogun.S01E01/2_English.srt", "e1")
	writeFile(t, root, "torrents/"+tor+"/Subs/Shogun.S01E01/3_English.srt", "e1b")
	writeFile(t, root, "torrents/"+tor+"/Subs/Shogun.S01E02.rus.srt", "e2")
	writeFile(t, root, "torrents/"+tor+"/Subs/Unknown.srt", "?")
	series := filepath.Join(root, "media/series")
	_, err := Organize(context.Background(), Job{Kind: library.KindSeries, Title: "Сёгун", Season: 1, SeriesDir: series, MoviesDir: filepath.Join(root, "media/movies"),
		Files: onDisk(t, root, tor+"/Shogun.S01E01.mkv", tor+"/Shogun.S01E02.mkv", tor+"/Subs/Shogun.S01E01/2_English.srt",
			tor+"/Subs/Shogun.S01E01/3_English.srt", tor+"/Subs/Shogun.S01E02.rus.srt", tor+"/Subs/Unknown.srt")})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(series, "Сёгун", "Season 01")
	for name, want := range map[string]string{"Сёгун - S01E01.en.srt": "e1", "Сёгун - S01E01.en.2.srt": "e1b", "Сёгун - S01E02.ru.srt": "e2"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.srt")); len(m) != 3 {
		t.Fatalf("%v", m)
	}
}

// A plugin hands the organizer a plain file set: no download is involved.
func TestOrganizeTakesAPlainFileSet(t *testing.T) {
	root := t.TempDir()
	video := writeFile(t, root, "set/Film.2020.1080p.mkv", strings.Repeat("v", 500))
	sub := writeFile(t, root, "set/Film.2020.1080p.srt", ruSrt)
	m := filepath.Join(root, "movies")
	paths, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "Фильм", Year: 2020, TMDBID: 1, MoviesDir: m,
		Files: []File{
			{Path: "set/Film.2020.1080p.mkv", FullPath: video, Size: 500},
			{Path: "set/Film.2020.1080p.srt", FullPath: sub, Size: int64(len(ruSrt))},
		}})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(m, "Фильм (2020)", "Фильм (2020).mkv")
	if len(paths) != 1 || paths[0] != want {
		t.Fatalf("paths = %q, want [%q]", paths, want)
	}
	if _, err := os.Stat(strings.TrimSuffix(want, ".mkv") + ".ru.srt"); err != nil {
		t.Fatalf("subtitle: %v", err)
	}
	nfo, err := os.ReadFile(strings.TrimSuffix(want, ".mkv") + ".nfo")
	if err != nil || !strings.Contains(string(nfo), "<tmdbid>1</tmdbid>") {
		t.Fatalf("nfo = %q, %v", nfo, err)
	}
}

// a new season of a series that is on disk under another
// folder name goes into that folder.
func TestOrganizeSeasonIntoExistingFolder(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "t/Planet.Earth.II.S02/Planet.Earth.II.S02E01.mkv", "x")
	paths, err := Organize(context.Background(), Job{
		Kind: library.KindSeries, Title: "Планета Земля 2", TMDBID: 68595, Season: 2,
		Files:     []File{{Path: "Planet.Earth.II.S02/Planet.Earth.II.S02E01.mkv", FullPath: src, Size: 1}},
		SeriesDir: filepath.Join(root, "series"), SeriesFolder: "Planet Earth II",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "series", "Planet Earth II", "Season 02", "Планета Земля 2 - S02E01.mkv")
	if len(paths) != 1 || paths[0] != want {
		t.Fatalf("paths %v, want %s", paths, want)
	}
}

// a re-run after a copy onto a file system with 2 s mtime
// precision adopts the copy instead of making a "(2)" duplicate.
func TestPlaceAdoptsCopyWithCoarseMtime(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "src.mkv", "data")
	dst := writeFile(t, root, "lib/dst.mkv", "data")
	si, _ := os.Stat(src)
	coarse := si.ModTime().Truncate(2 * time.Second)
	if err := os.Chtimes(dst, coarse, coarse); err != nil {
		t.Fatal(err)
	}
	got, err := Place(context.Background(), src, dst)
	if err != nil || got != dst {
		t.Fatalf("place %q %v, want %q", got, err, dst)
	}
}

// Anime-style names number their episodes with « - NN» (and « - NN-MM» for
// two in one file): organize places them by that number, as the pinned
// release counted them.
func TestOrganizeDashNumberedAnimeEpisodes(t *testing.T) {
	root := t.TempDir()
	src := writeFile(t, root, "t/[Group] Show [1080p]/[Group] Show - 08 [1080p].mkv", "e8")
	src2 := writeFile(t, root, "t/[Group] Show [1080p]/[Group] Show - 09-10 (1080p).mkv", "e9")
	series := filepath.Join(root, "series")
	paths, err := Organize(context.Background(), Job{
		Kind: library.KindSeries, Title: "Шоу", TMDBID: 5,
		Files: fileInfos(src, src2), SeriesDir: series,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != filepath.Join(series, "Шоу", "Season 01", "Шоу - S01E08.mkv") ||
		!strings.Contains(paths[1], "S01E09") {
		t.Fatalf("paths %v", paths)
	}
}
