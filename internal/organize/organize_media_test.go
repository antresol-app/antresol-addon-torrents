package organize

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
)

// torrentFiles writes the files of a torrent under dir and returns their
// FileInfos with in-torrent paths (rel, "/"-separated).
func torrentFiles(t *testing.T, dir string, rels ...string) []File {
	t.Helper()
	out := make([]File, len(rels))
	for i, rel := range rels {
		p := writeFile(t, dir, rel, "data:"+rel)
		out[i] = File{Path: rel, FullPath: p, Size: int64(len("data:" + rel))}
	}
	return out
}

func mediaJob(root, kind string, files []File) Job {
	return Job{Kind: kind, Files: files,
		MusicDir: filepath.Join(root, "media/music"), EbooksDir: filepath.Join(root, "media/books/ebooks"),
		AudiobooksDir: filepath.Join(root, "media/books/audiobooks"), ComicsDir: filepath.Join(root, "media/books/manga")}
}

// bindToDownload binds a placed path to its download the way the full
// edition does (bind_full_test.go); the library edition has none.
var bindToDownload = func(*testing.T, *coretest.Library, string) {}

// bindRef is the download bind_full_test.go binds to.
const bindRef = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// rels returns the organized paths relative to base, sorted.
func rels(t *testing.T, base string, paths []string) []string {
	t.Helper()
	out := make([]string, len(paths))
	for i, p := range paths {
		r, err := filepath.Rel(base, p)
		if err != nil {
			t.Fatal(err)
		}
		out[i] = filepath.ToSlash(r)
	}
	slices.Sort(out)
	return out
}

func sameInode(t *testing.T, a, b string) {
	t.Helper()
	sa, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	sb, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(sa, sb) {
		t.Fatalf("%s is not a hard link of %s", b, a)
	}
}

func TestOrganizeAlbumKeepsDiscFolders(t *testing.T) {
	root := t.TempDir()
	tor := filepath.Join(root, "torrents/music")
	files := torrentFiles(t, tor,
		"Pink Floyd - The Wall (1979) [FLAC]/CD1/01.flac",
		"Pink Floyd - The Wall (1979) [FLAC]/CD2/01.flac",
		"Pink Floyd - The Wall (1979) [FLAC]/Covers/front.jpg",
		"Pink Floyd - The Wall (1979) [FLAC]/rip.log",
		"Pink Floyd - The Wall (1979) [FLAC]/The Wall.cue",
		"Pink Floyd - The Wall (1979) [FLAC]/info.txt",
	)
	j := mediaJob(root, library.KindMusic, files)
	j.Title, j.Artist, j.Year = "The Wall", "Pink Floyd", 1979
	paths, err := Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	album := filepath.Join(j.MusicDir, "Pink Floyd", "The Wall (1979)")
	want := []string{"CD1/01.flac", "CD2/01.flac", "Covers/front.jpg", "The Wall.cue", "rip.log"}
	if got := rels(t, album, paths); !slices.Equal(got, want) {
		t.Fatalf("placed %q, want %q", got, want)
	}
	sameInode(t, files[0].FullPath, filepath.Join(album, "CD1", "01.flac"))
	if _, err := os.Stat(filepath.Join(album, "Covers", "front.jpg")); err != nil {
		t.Fatal(err)
	}
	// idempotent
	again, err := Organize(context.Background(), j)
	if err != nil || !slices.Equal(rels(t, album, again), want) {
		t.Fatalf("second run %q %v", again, err)
	}
}

func TestOrganizeAlbumFlatAndSuffixDiscs(t *testing.T) {
	root := t.TempDir()
	tor := filepath.Join(root, "torrents/music")
	files := torrentFiles(t, tor,
		"Кино - Группа крови (1988)/cover.jpg",
		"Кино - Группа крови (1988)/Группа крови (Disc 1)/01. Группа крови.mp3",
		"Кино - Группа крови (1988)/Группа крови (Disc 2)/01. Бонус.mp3",
		"Кино - Группа крови (1988)/__MACOSX/._01.mp3",
	)
	j := mediaJob(root, library.KindMusic, files)
	j.Title, j.Artist = "Группа крови", "Кино"
	paths, err := Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	// no year asked: the album folder of the torrent gives it
	album := filepath.Join(j.MusicDir, "Кино", "Группа крови (1988)")
	want := []string{"CD1/01. Группа крови.mp3", "CD2/01. Бонус.mp3", "cover.jpg"}
	if got := rels(t, album, paths); !slices.Equal(got, want) {
		t.Fatalf("placed %q, want %q", got, want)
	}
}

// newScanner is the core's library for the media folders of a job (an
// in-memory one: the addon has no scanner; what the scanners index is
// pinned by the copied file rules, see indexed).
func newScanner(t *testing.T, root string, j Job) *coretest.Library {
	t.Helper()
	repo := &coretest.Library{}
	for _, dir := range []string{filepath.Join(root, "media/movies"), filepath.Join(root, "media/series"), j.MusicDir, j.EbooksDir, j.AudiobooksDir, j.ComicsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

// indexed: the scanners index p as a file of kind (the core's file
// rules, copied).
func indexed(kind, p string) bool {
	switch kind {
	case library.KindMusic:
		return library.IsMusicFile(p) || strings.EqualFold(filepath.Ext(p), ".cue")
	case library.KindBook:
		return library.IsBookFile(p)
	case library.KindAudiobook:
		return library.IsAudiobookFile(p)
	case library.KindComic:
		return library.IsComicVolume(p)
	}
	return false
}

func TestOrganizeDiscographyPerAlbum(t *testing.T) {
	root := t.TempDir()
	files := torrentFiles(t, filepath.Join(root, "t"),
		"Ария - Дискография/1985 - Мания величия/01.mp3",
		"Ария - Дискография/1985 - Мания величия/cover.jpg",
		"Ария - Дискография/С кем ты (1986) [MP3]/CD1/01.mp3",
		"Ария - Дискография/С кем ты (1986) [MP3]/CD2/01.mp3",
		"Ария - Дискография/Герой асфальта/01.mp3",
		"Ария - Дискография/folder.jpg",
	)
	j := mediaJob(root, library.KindMusic, files)
	j.Title, j.Artist, j.Year = "Герой асфальта", "Ария", 1987
	paths, err := Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Герой асфальта (1987)/01.mp3", "Мания величия (1985)/01.mp3", "Мания величия (1985)/cover.jpg",
		"С кем ты (1986)/CD1/01.mp3", "С кем ты (1986)/CD2/01.mp3"}
	if got := rels(t, filepath.Join(j.MusicDir, "Ария"), paths); !slices.Equal(got, want) {
		t.Fatalf("placed %q, want %q", got, want)
	}
	repo := newScanner(t, root, j)
	for _, p := range paths {
		if Bindable(library.KindMusic, p) {
			bindToDownload(t, repo, p)
		}
	}
	if n := len(repo.Bound(bindRef)); n != 4 {
		t.Fatalf("bound %d tracks, want 4", n)
	}

	// the reviewer's case: two albums, artist-only request
	files = torrentFiles(t, filepath.Join(root, "t2"), "Ария - Дискография/1985 - Мания величия/01.mp3", "Ария - Дискография/1986 - С кем ты/01.mp3")
	j = mediaJob(root, library.KindMusic, files)
	j.Artist = "Ария"
	paths, err = Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(t, filepath.Join(j.MusicDir, "Ария"), paths); !slices.Equal(got, []string{"Мания величия (1985)/01.mp3", "С кем ты (1986)/01.mp3"}) {
		t.Fatalf("placed %q", got)
	}
}

func TestOrganizeSameDiscTwiceKeepsBoth(t *testing.T) {
	root := t.TempDir()
	files := torrentFiles(t, filepath.Join(root, "t"), "A/CD1/01.flac", "A/A (Disc 1)/01.flac")
	j := mediaJob(root, library.KindMusic, files)
	j.Title, j.Artist = "A", "B"
	paths, err := Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(t, filepath.Join(j.MusicDir, "B", "A"), paths); !slices.Equal(got, []string{"CD1/01 (2).flac", "CD1/01.flac"}) {
		t.Fatalf("placed %q", got)
	}
	// idempotent: the same names again
	again, err := Organize(context.Background(), j)
	if err != nil || !slices.Equal(rels(t, filepath.Join(j.MusicDir, "B", "A"), again), []string{"CD1/01 (2).flac", "CD1/01.flac"}) {
		t.Fatalf("again %q %v", again, err)
	}
}

func TestOrganizeDotLeadingAlbum(t *testing.T) {
	root := t.TempDir()
	files := torrentFiles(t, filepath.Join(root, "t"), "...And Justice for All (1988)/01. Blackened.flac", "...And Justice for All (1988)/._01. Blackened.flac")
	j := mediaJob(root, library.KindMusic, files)
	j.Title, j.Artist, j.Year = "...And Justice for All", "Metallica", 1988
	paths, err := Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(t, filepath.Join(j.MusicDir, "Metallica"), paths); !slices.Equal(got, []string{"And Justice for All (1988)/01. Blackened.flac"}) {
		t.Fatalf("placed %q", got)
	}
}

func TestOrganizeBookFormats(t *testing.T) {
	root := t.TempDir()
	files := torrentFiles(t, filepath.Join(root, "torrents/books"),
		"Булгаков - Мастер и Маргарита/master.fb2",
		"Булгаков - Мастер и Маргарита/master.epub",
		"Булгаков - Мастер и Маргарита/master.djvu",
		"Булгаков - Мастер и Маргарита/cover.jpg",
	)
	j := mediaJob(root, library.KindBook, files)
	j.Title, j.Author = "Мастер и Маргарита", "Булгаков"
	paths, err := Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Булгаков/Мастер и Маргарита.epub", "Булгаков/Мастер и Маргарита.fb2"}
	if got := rels(t, j.EbooksDir, paths); !slices.Equal(got, want) {
		t.Fatalf("placed %q, want %q", got, want)
	}
	sameInode(t, files[0].FullPath, filepath.Join(j.EbooksDir, "Булгаков", "Мастер и Маргарита.fb2"))
}

func TestOrganizeBookCollectionKeepsNames(t *testing.T) {
	root := t.TempDir()
	files := torrentFiles(t, filepath.Join(root, "t"),
		"Чехов - Рассказы/Чехов - Дама с собачкой.fb2",
		"Чехов - Рассказы/Чехов - Каштанка.fb2",
		"Чехов - Рассказы/scan.djvu",
	)
	j := mediaJob(root, library.KindBook, files)
	j.Title, j.Author = "Рассказы", "Чехов"
	paths, err := Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Чехов/Чехов - Дама с собачкой.fb2", "Чехов/Чехов - Каштанка.fb2"}
	if got := rels(t, j.EbooksDir, paths); !slices.Equal(got, want) {
		t.Fatalf("placed %q, want %q", got, want)
	}

	// collected works in volume folders: every file is placed, named with its volume
	files = torrentFiles(t, filepath.Join(root, "t2"), "Собрание/Том 1/book.fb2", "Собрание/Том 2/book.fb2", "Собрание/Том 3/other.fb2")
	j = mediaJob(root, library.KindBook, files)
	j.Title, j.Author = "Собрание сочинений", "Толстой"
	paths, err = Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"Толстой/Том 1 - book.fb2", "Толстой/Том 2 - book.fb2", "Толстой/Том 3 - other.fb2"}
	if got := rels(t, j.EbooksDir, paths); !slices.Equal(got, want) {
		t.Fatalf("placed %q, want %q", got, want)
	}
}

func TestUniqueTargets(t *testing.T) {
	got := uniqueTargets([]placement{{"/s/b", "/l/x.fb2.zip"}, {"/s/a", "/l/x.fb2.zip"}, {"/s/a", "/l/x.fb2.zip"}, {"/s/c", "/l/y.mp3"}})
	want := []placement{{"/s/b", "/l/x (2).fb2.zip"}, {"/s/a", "/l/x.fb2.zip"}, {"/s/c", "/l/y.mp3"}}
	if !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestOrganizeBookOnlyDJVU(t *testing.T) {
	root := t.TempDir()
	j := mediaJob(root, library.KindBook, torrentFiles(t, filepath.Join(root, "t"), "book.djvu"))
	j.Title = "Книга"
	if _, err := Organize(context.Background(), j); err == nil || !strings.Contains(err.Error(), "книг") {
		t.Fatalf("err = %v", err)
	}
}

func TestOrganizeAudiobook(t *testing.T) {
	root := t.TempDir()
	files := torrentFiles(t, filepath.Join(root, "torrents/audiobooks"),
		"Лем - Солярис [Иванов, 2019]/Часть 1/01.mp3",
		"Лем - Солярис [Иванов, 2019]/Часть 2/01.mp3",
		"Лем - Солярис [Иванов, 2019]/cover.jpg",
		"Лем - Солярис [Иванов, 2019]/readme.txt",
	)
	j := mediaJob(root, library.KindAudiobook, files)
	j.Title, j.Author = "Солярис", "Станислав Лем"
	paths, err := Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	book := filepath.Join(j.AudiobooksDir, "Станислав Лем", "Солярис")
	want := []string{"cover.jpg", "Часть 1/01.mp3", "Часть 2/01.mp3"}
	if got := rels(t, book, paths); !slices.Equal(got, want) {
		t.Fatalf("placed %q, want %q", got, want)
	}

	// a single file torrent, an m4b
	files = torrentFiles(t, filepath.Join(root, "t2"), "Пикник на обочине.m4b")
	j = mediaJob(root, library.KindAudiobook, files)
	j.Title, j.Author = "Пикник на обочине", "Стругацкие"
	paths, err = Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(t, j.AudiobooksDir, paths); !slices.Equal(got, []string{"Стругацкие/Пикник на обочине/Пикник на обочине.m4b"}) {
		t.Fatalf("placed %q", got)
	}
}

func TestOrganizeComics(t *testing.T) {
	root := t.TempDir()
	files := torrentFiles(t, filepath.Join(root, "torrents/comics"),
		"Berserk [RUS]/Berserk v01.cbz",
		"Berserk [RUS]/Berserk v02.cbz",
		"Berserk [RUS]/cover.jpg",
		"Berserk [RUS]/Extra/001.jpg",
		"Berserk [RUS]/Extra/002.png",
		"Berserk [RUS]/Berserk.pdf",
		"Berserk [RUS]/info.nfo",
	)
	j := mediaJob(root, library.KindComic, files)
	j.Title = "Берсерк"
	paths, err := Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	series := filepath.Join(j.ComicsDir, "Берсерк")
	want := []string{"Berserk v01.cbz", "Berserk v02.cbz", "Extra/001.jpg", "Extra/002.png", "cover.jpg"}
	if got := rels(t, series, paths); !slices.Equal(got, want) {
		t.Fatalf("placed %q, want %q", got, want)
	}

	// one folder of pages keeps its folder (it is the volume)
	files = torrentFiles(t, filepath.Join(root, "t2"), "Akira Vol 1/001.jpg", "Akira Vol 1/002.jpg")
	j = mediaJob(root, library.KindComic, files)
	j.Title = "Akira"
	paths, err = Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(t, filepath.Join(j.ComicsDir, "Akira"), paths); !slices.Equal(got, []string{"Akira Vol 1/001.jpg", "Akira Vol 1/002.jpg"}) {
		t.Fatalf("placed %q", got)
	}
}

func TestMediaBindable(t *testing.T) {
	for p, want := range map[string]bool{
		"/m/music/A/B/01.flac": true, "/m/music/A/B/x.cue": false, "/m/music/A/B/rip.log": false, "/m/music/A/B/Covers/f.jpg": false,
		"/m/e/A/B.epub": true, "/m/e/A/B.fb2.zip": true, "/m/c/S/v1.cbz": true, "/m/c/S/v1.part2.rar": false, "/m/c/S/p/001.jpg": false,
		"/m/a/A/B/01.m4b": true, "/m/music/A/B/01.m4b": false, "/m/e/A/B.djvu": false,
	} {
		kind := library.KindMusic
		switch {
		case strings.HasPrefix(p, "/m/e/"):
			kind = library.KindBook
		case strings.HasPrefix(p, "/m/c/"):
			kind = library.KindComic
		case strings.HasPrefix(p, "/m/a/"):
			kind = library.KindAudiobook
		}
		if got := Bindable(kind, p); got != want {
			t.Errorf("Bindable(%s, %s) = %v", kind, p, got)
		}
	}
}

// TestOrganizedMediaScans: what planMedia lays out is what the library
// scanners index (one album with its two discs, one book with two formats,
// one audiobook, one comic series).
func TestOrganizedMediaScans(t *testing.T) {
	root := t.TempDir()
	base := mediaJob(root, "", nil)
	repo := newScanner(t, root, base)
	tor := filepath.Join(root, "torrents")
	jobs := []Job{
		{Kind: library.KindMusic, Title: "The Wall", Artist: "Pink Floyd", Year: 1979, Files: torrentFiles(t, tor,
			"Pink Floyd - The Wall (1979) [FLAC]/CD1/01. In the Flesh.flac", "Pink Floyd - The Wall (1979) [FLAC]/CD2/01. Hey You.flac",
			"Pink Floyd - The Wall (1979) [FLAC]/Covers/front.jpg", "Pink Floyd - The Wall (1979) [FLAC]/rip.log")},
		{Kind: library.KindBook, Title: "Мастер и Маргарита", Author: "Булгаков", Files: torrentFiles(t, tor,
			"Булгаков/master.fb2", "Булгаков/master.epub", "Булгаков/master.djvu")},
		{Kind: library.KindAudiobook, Title: "Солярис", Author: "Станислав Лем", Files: torrentFiles(t, tor,
			"Лем - Солярис/Часть 1/01.mp3", "Лем - Солярис/Часть 2/01.mp3", "Лем - Солярис/cover.jpg")},
		{Kind: library.KindComic, Title: "Берсерк", Files: torrentFiles(t, tor, "Berserk/Berserk v01.cbz", "Berserk/Berserk v02.cbz")},
	}
	var bind []string
	for _, j := range jobs {
		j.MusicDir, j.EbooksDir, j.AudiobooksDir, j.ComicsDir = base.MusicDir, base.EbooksDir, base.AudiobooksDir, base.ComicsDir
		paths, err := Organize(context.Background(), j)
		if err != nil {
			t.Fatalf("%s: %v", j.Kind, err)
		}
		for _, p := range paths {
			if Bindable(j.Kind, p) {
				if !indexed(j.Kind, p) {
					t.Errorf("%s: bindable %s is not a file the scanners index", j.Kind, p)
				}
				bind = append(bind, p)
			}
		}
	}
	if len(bind) != 8 {
		t.Fatalf("Bindable %q", bind)
	}
	for _, p := range bind {
		bindToDownload(t, repo, p)
	}
	if got := repo.Bound(bindRef); len(got) != 8 {
		t.Fatalf("bound %q", got)
	}
}

func TestOrganizePickedNamesByRequestedRoot(t *testing.T) {
	const (
		blood = "Кино - Дискография/1988 - Группа крови"
		star  = "Кино - Дискография/1989 - Звезда по имени Солнце"
		remas = "Кино - Дискография/1988 - Группа крови (Remastered 2012)"
	)
	cases := []struct {
		name, requested string
		kept            []string
		want            []string
	}{
		{"only another album kept", blood, []string{star + "/01.flac", "Кино - Дискография/cover.jpg"},
			[]string{"Звезда по имени Солнце (1989)/01.flac", "Звезда по имени Солнце (1989)/cover.jpg"}},
		{"both kept", blood, []string{blood + "/01.flac", star + "/01.flac", "Кино - Дискография/cover.jpg", "Кино - Дискография/info.log"},
			[]string{"Группа крови (1988)/01.flac", "Группа крови (1988)/cover.jpg", "Звезда по имени Солнце (1989)/01.flac"}},
		{"requested reissue folder", remas, []string{remas + "/01.flac", star + "/01.flac"},
			[]string{"Группа крови (1988)/01.flac", "Звезда по имени Солнце (1989)/01.flac"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			j := mediaJob(root, library.KindMusic, torrentFiles(t, filepath.Join(root, "t"), c.kept...))
			j.Title, j.Artist, j.Year, j.Picked, j.RequestedRoot = "Группа крови", "Кино", 1988, true, c.requested
			paths, err := Organize(context.Background(), j)
			if err != nil {
				t.Fatal(err)
			}
			if got := rels(t, filepath.Join(j.MusicDir, "Кино"), paths); !slices.Equal(got, c.want) {
				t.Fatalf("placed %q, want %q", got, c.want)
			}
		})
	}
}
