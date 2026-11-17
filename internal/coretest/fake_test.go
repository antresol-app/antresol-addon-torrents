package coretest_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
)

// One interface, both implementations (compile time).
var (
	_ coreadapt.Core = coreadapt.Library{}
	_ coreadapt.Core = &coretest.Library{}
)

func TestCoretest_SameMethods(t *testing.T) {
	a := reflect.TypeOf((*coreadapt.Core)(nil)).Elem()
	for _, typ := range []reflect.Type{reflect.TypeOf(coreadapt.Library{}), reflect.TypeOf(&coretest.Library{})} {
		for i := range a.NumMethod() {
			m := a.Method(i)
			if _, ok := typ.MethodByName(m.Name); !ok {
				t.Fatalf("%v has no %s", typ, m.Name)
			}
		}
	}
	// and Core is the whole adapter: no method of Library outside it
	lt := reflect.TypeOf(coreadapt.Library{})
	for i := range lt.NumMethod() {
		if _, ok := a.MethodByName(lt.Method(i).Name); !ok {
			t.Errorf("coreadapt.Library.%s is not in Core", lt.Method(i).Name)
		}
	}
}

func fixture() *coretest.Library {
	return &coretest.Library{
		Items: []library.Item{
			{ID: 1, Kind: library.KindSeries, Title: "Сёгун", TmdbID: 126308},
			{ID: 2, Kind: library.KindSeason, ParentID: 1, SeasonNo: 1},
			{ID: 3, Kind: library.KindEpisode, ParentID: 2, EpisodeNo: 1},
			{ID: 4, Kind: library.KindEpisode, ParentID: 2, EpisodeNo: 2},
			{ID: 5, Kind: library.KindMovie, Title: "Матрица", OriginalTitle: "The Matrix", TmdbID: 603},
			{ID: 6, Kind: library.KindArtist, Title: "Кино"},
			{ID: 7, Kind: library.KindAlbum, ParentID: 6, Title: "Группа крови"},
			{ID: 8, Kind: library.KindTrack, ParentID: 7, Title: "Группа крови"},
			{ID: 9, Kind: library.KindBook, Title: "Мастер и Маргарита", MetaJSON: `{"authors":["Михаил Булгаков"]}`},
		},
		Files: []library.File{
			{ID: 11, ItemID: 3, Path: "/m/series/Сёгун (2024)/Season 1/S01E01.mkv"},
			{ID: 12, ItemID: 4, Path: "/m/series/Сёгун (2024)/Season 1/S01E02.mkv"},
			{ID: 13, ItemID: 5, Path: "/m/movies/Матрица (1999)/Матрица.mkv"},
			{ID: 14, ItemID: 8, Path: "/m/music/Кино/Группа крови/01.mp3"},
		},
	}
}

func TestFake(t *testing.T) {
	ctx := context.Background()
	l := fixture()
	if it, err := l.ItemByTMDB(ctx, "movie", 603); err != nil || it.ID != 5 {
		t.Fatalf("by tmdb %+v %v", it, err)
	}
	if _, err := l.ItemByTMDB(ctx, "movie", 1); !errors.Is(err, library.ErrNotFound) {
		t.Fatal(err)
	}
	if it, ok, _ := l.FindAlbum(ctx, "кино", "Группа Крови"); !ok || it.ID != 7 {
		t.Fatalf("album %+v", it)
	}
	if it, ok, _ := l.FindBook(ctx, "book", "Михаил Булгаков", "Мастер и Маргарита"); !ok || it.ID != 9 {
		t.Fatalf("book %+v", it)
	}
	if _, ok, _ := l.FindBook(ctx, "book", "Другой", "Мастер и Маргарита"); ok {
		t.Fatal("book by another author")
	}
	if it, ok, _ := l.FindTitle(ctx, "music", 0, "Группа крови", "Кино"); !ok || it.ID != 7 {
		t.Fatalf("title %+v", it)
	}
	if dir, err := l.SeriesFolder(ctx, "/m/series", 126308); err != nil || dir != "Сёгун (2024)" {
		t.Fatalf("folder %q %v", dir, err)
	}
	if have, _ := l.HaveEpisodes(ctx, 126308); !reflect.DeepEqual(have, map[[2]int]bool{{1, 1}: true, {1, 2}: true}) {
		t.Fatalf("have %v", have)
	}
	if ids, _ := l.ItemIDsByTMDB(ctx, "series", []int64{126308, 603}); !reflect.DeepEqual(ids, map[int64]int64{126308: 1}) {
		t.Fatalf("ids %v", ids)
	}
	if dir, err := l.AlbumDir(ctx, 7); err != nil || dir != "/m/music/Кино/Группа крови" {
		t.Fatalf("album dir %q %v", dir, err)
	}
	if ps, _ := l.LibraryPaths(ctx); len(ps) != 4 || !slices.IsSorted(ps) {
		t.Fatalf("paths %v", ps)
	}
	if ok, _ := l.FileExists(ctx, "/m/movies/Матрица (1999)/Матрица.mkv"); !ok {
		t.Fatal("exists")
	}
	if it, ok, _ := l.ItemOfFile(ctx, 12); !ok || it.ID != 4 {
		t.Fatalf("item of file %+v", it)
	}
	if its, _ := l.SearchTitles(ctx, "movie", "the matrix", 0); len(its) != 1 || its[0].ID != 5 {
		t.Fatalf("search %+v", its)
	}
	if fs, _ := l.ItemFiles(ctx, 1); len(fs) != 2 || fs[0].ID != 11 {
		t.Fatalf("files %+v", fs)
	}
	if _, err := l.Item(ctx, 99); !errors.Is(err, library.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestFake_Import(t *testing.T) {
	ctx := context.Background()
	l := fixture()
	l.Missing = map[string]bool{"/t/ep2.mkv": true}
	l.Taken = map[string]bool{"/t/ep3.mkv": true}
	imported, missing, taken, err := l.Import(ctx, []string{"/t/ep1.mkv", "/t/ep2.mkv", "/t/ep3.mkv"}, "abc", 0)
	if err != nil || !slices.Equal(imported, []string{"/t/ep1.mkv"}) || !slices.Equal(missing, []string{"/t/ep2.mkv"}) ||
		!slices.Equal(taken, []string{"/t/ep3.mkv"}) {
		t.Fatalf("%v %v %v %v", imported, missing, taken, err)
	}
	if !reflect.DeepEqual(l.Imports, [][]string{{"/t/ep1.mkv", "/t/ep2.mkv", "/t/ep3.mkv"}}) || !slices.Equal(l.Bound("abc"), []string{"/t/ep1.mkv"}) {
		t.Fatalf("imports %v refs %v", l.Imports, l.Refs)
	}
	if err := l.ClaimRef(ctx, "abc", 2); err != nil || !slices.Equal(l.Owners[2], []string{"/t/ep1.mkv"}) {
		t.Fatalf("owners %v", l.Owners)
	}
}

func TestFake_FilesUnderAndBindRef(t *testing.T) {
	ctx := context.Background()
	l := fixture()
	fs, err := l.FilesUnder(ctx, "/m/series")
	if err != nil || len(fs) != 2 || fs[0].ID != 11 || fs[1].ID != 12 {
		t.Fatalf("under %+v %v", fs, err)
	}
	if fs, _ := l.FilesUnder(ctx, "/m/ser"); len(fs) != 0 {
		t.Fatalf("a name prefix is not a folder: %+v", fs)
	}
	l.Taken = map[string]bool{"/m/movies/Матрица (1999)/Матрица.mkv": true}
	ep1, ep2 := "/m/series/Сёгун (2024)/Season 1/S01E01.mkv", "/m/series/Сёгун (2024)/Season 1/S01E02.mkv"
	missing, taken, err := l.BindRef(ctx, "abc", []string{ep2, ep1, "/nowhere.mkv", "/m/movies/Матрица (1999)/Матрица.mkv"})
	if err != nil || !slices.Equal(missing, []string{"/nowhere.mkv"}) || !slices.Equal(taken, []string{"/m/movies/Матрица (1999)/Матрица.mkv"}) {
		t.Fatalf("%v %v %v", missing, taken, err)
	}
	if !slices.Equal(l.Bound("abc"), []string{ep1, ep2}) {
		t.Fatalf("bound %v", l.Bound("abc"))
	}
	// exactly the paths: the ref's other files lose it; none clears it
	if _, _, err := l.BindRef(ctx, "abc", []string{ep1}); err != nil || !slices.Equal(l.Bound("abc"), []string{ep1}) {
		t.Fatalf("rebind %v %v", l.Bound("abc"), err)
	}
	if _, _, err := l.BindRef(ctx, "abc", nil); err != nil || len(l.Bound("abc")) != 0 {
		t.Fatalf("clear %v %v", l.Bound("abc"), err)
	}
}
