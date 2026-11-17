package coreadapt_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol/addonkit"
	"github.com/antresol-app/antresol/addonkit/addonkittest"
)

// goldenTime is what the replay answers for the goldens' {time}.
var goldenTime = addonkittest.GoldenTime

// lib is the adapter over the core's recorded answers (replay_test.go;
// {root} stays as recorded).
func lib(t *testing.T) coreadapt.Library {
	t.Helper()
	return coreadapt.Library{C: replay(t)}
}

var ctx = context.Background()

var matrix = library.Item{ID: 1, Kind: "movie", Title: "Матрица", OriginalTitle: "The Matrix", Year: 1999,
	TmdbID: 603, AddedAt: goldenTime, MetaJSON: "{}"}

func TestLibrary_ItemByTMDB(t *testing.T) {
	l := lib(t)
	it, err := l.ItemByTMDB(ctx, "movie", 603)
	if err != nil || !reflect.DeepEqual(it, matrix) {
		t.Fatalf("%+v %v", it, err)
	}
	if _, err := l.ItemByTMDB(ctx, "movie", 1); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestLibrary_FindAlbum(t *testing.T) {
	l := lib(t)
	it, ok, err := l.FindAlbum(ctx, "Кино", "Группа крови")
	if err != nil || !ok || it.ID != 7 || it.Kind != library.KindAlbum || it.ParentID != 6 || it.Title != "Группа крови" ||
		!strings.Contains(it.MetaJSON, `"compilation": false`) {
		t.Fatalf("%+v %v %v", it, ok, err)
	}
	if _, ok, err := l.FindAlbum(ctx, "Кино", "Нет"); ok || err != nil {
		t.Fatalf("unknown: %v %v", ok, err)
	}
}

func TestLibrary_FindBook(t *testing.T) {
	l := lib(t)
	it, ok, err := l.FindBook(ctx, "book", "Михаил Булгаков", "Мастер и Маргарита")
	if err != nil || !ok || it.ID != 10 || it.Kind != library.KindBook || !strings.Contains(it.MetaJSON, "Михаил Булгаков") {
		t.Fatalf("%+v %v %v", it, ok, err)
	}
	if _, ok, err := l.FindBook(ctx, "book", "", "Нет"); ok || err != nil {
		t.Fatalf("unknown: %v %v", ok, err)
	}
}

func TestLibrary_FindTitle(t *testing.T) {
	l := lib(t)
	it, ok, err := l.FindTitle(ctx, "movie", 603, "", "")
	if err != nil || !ok || !reflect.DeepEqual(it, matrix) {
		t.Fatalf("%+v %v %v", it, ok, err)
	}
	if _, ok, err := l.FindTitle(ctx, "movie", 1, "", ""); ok || err != nil {
		t.Fatalf("unknown: %v %v", ok, err)
	}
}

func TestLibrary_SeriesFolder(t *testing.T) {
	l := lib(t)
	// the folder's name under the root, as organize joins it to SeriesDir
	if dir, err := l.SeriesFolder(ctx, "{root}/series", 126308); err != nil || dir != "Сёгун (2024)" {
		t.Fatalf("%q %v", dir, err)
	}
	if _, err := l.SeriesFolder(ctx, "{root}/series", 1); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	for _, c := range [][3]string{{"/m/series", "/m/series/Сёгун (2024)", "Сёгун (2024)"}, {"/m/series/", "/m/series/a/b", "a"}} {
		if got, err := coreadapt.FolderUnder(c[0], c[1]); err != nil || got != c[2] {
			t.Errorf("%v: %q %v", c, got, err)
		}
	}
	for _, p := range []string{"/m/series", "/m/other/x", "/m"} {
		if _, err := coreadapt.FolderUnder("/m/series", p); !errors.Is(err, library.ErrNotFound) {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestLibrary_HaveEpisodes(t *testing.T) {
	l := lib(t)
	have, err := l.HaveEpisodes(ctx, 126308)
	if err != nil || !reflect.DeepEqual(have, map[[2]int]bool{{1, 1}: true, {1, 2}: true}) {
		t.Fatalf("%v %v", have, err)
	}
	if have, err := l.HaveEpisodes(ctx, 1); err != nil || len(have) != 0 {
		t.Fatalf("unknown: %v %v", have, err)
	}
}

func TestLibrary_ItemIDsByTMDB(t *testing.T) {
	l := lib(t)
	got, err := l.ItemIDsByTMDB(ctx, "series", []int64{126308, 603, 7})
	if err != nil || !reflect.DeepEqual(got, map[int64]int64{126308: 2}) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := l.ItemIDsByTMDB(ctx, "series", nil); err != nil || len(got) != 0 {
		t.Fatalf("none: %v %v", got, err)
	}
}

func TestLibrary_AlbumDir(t *testing.T) {
	l := lib(t)
	if dir, err := l.AlbumDir(ctx, 7); err != nil || dir != "{root}/music/Кино/Группа крови" {
		t.Fatalf("%q %v", dir, err)
	}
	if _, err := l.AlbumDir(ctx, 99); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestLibrary_LibraryPaths(t *testing.T) {
	ps, err := lib(t).LibraryPaths(ctx)
	if err != nil || len(ps) != 7 || ps[2] != "{root}/movies/Матрица (1999)/Матрица (1999).mkv" || !slices.IsSorted(ps) {
		t.Fatalf("%v %v", ps, err)
	}
}

func TestLibrary_Import(t *testing.T) {
	imported, missing, taken, err := lib(t).Import(ctx, []string{"{root}/movies/Дюна (2021)"}, "k-0001", 2)
	if err != nil || !slices.Equal(imported, []string{"{root}/movies/Дюна (2021)/Дюна (2021).mkv"}) || len(missing) != 0 || len(taken) != 0 {
		t.Fatalf("%v %v %v %v", imported, missing, taken, err)
	}
}

// The core's missing and taken lists come back as they are, apart from
// what was imported.
func TestLibrary_ImportMissingTaken(t *testing.T) {
	var got struct {
		Paths []string `json:"paths"`
		Ref   string   `json:"ref"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/library/import") {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"files":[{"path":"/m/a.mkv","fileId":1,"itemId":2}],"missing":["/m/b.mkv"],"taken":["/m/c.mkv"]}`)
	}))
	t.Cleanup(srv.Close)
	l := coreadapt.Library{C: addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})}
	imported, missing, taken, err := l.Import(ctx, []string{"/m/a.mkv", "/m/b.mkv", "/m/c.mkv"}, "abc", 0)
	if err != nil || !slices.Equal(imported, []string{"/m/a.mkv"}) || !slices.Equal(missing, []string{"/m/b.mkv"}) || !slices.Equal(taken, []string{"/m/c.mkv"}) {
		t.Fatalf("%v %v %v %v", imported, missing, taken, err)
	}
	if len(got.Paths) != 3 || got.Ref != "abc" {
		t.Fatalf("request %+v", got)
	}
}

func TestLibrary_ClaimRef(t *testing.T) {
	if err := lib(t).ClaimRef(ctx, "k-0001", 2); err != nil {
		t.Fatal(err)
	}
}

func TestLibrary_ClaimPaths(t *testing.T) {
	l := lib(t)
	if err := l.ClaimPaths(ctx, []string{"{root}/movies/Дюна (2021)/Дюна (2021).mkv"}, 2); err != nil {
		t.Fatal(err)
	}
	// none: no call (the replay would answer anything but owners 404)
	if err := (coreadapt.Library{C: addonkit.NewClient(addonkit.Env{CoreURL: "http://127.0.0.1:1"})}).ClaimPaths(ctx, nil, 2); err != nil {
		t.Fatal(err)
	}
}

func TestLibrary_FileExists(t *testing.T) {
	l := lib(t)
	if ok, err := l.FileExists(ctx, "{root}/movies/Матрица (1999)/Матрица (1999).mkv"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
}

func TestLibrary_ItemOfFile(t *testing.T) {
	l := lib(t)
	it, ok, err := l.ItemOfFile(ctx, 1)
	if err != nil || !ok || !reflect.DeepEqual(it, matrix) {
		t.Fatalf("%+v %v %v", it, ok, err)
	}
	if _, ok, err := l.ItemOfFile(ctx, 99); ok || err != nil {
		t.Fatalf("unknown: %v %v", ok, err)
	}
}

func TestLibrary_SeriesAudio(t *testing.T) {
	l := lib(t)
	sa, err := l.SeriesAudio(ctx, 126308)
	if err != nil || len(sa.Files) != 2 || !slices.Equal(sa.Files[0].Titles, []string{"Дубляж", "Оригинал"}) ||
		len(sa.Profiles) != 1 || sa.Profiles[0].UserID != 2 || sa.Profiles[0].AudioTitle != "Оригинал" {
		t.Fatalf("%+v %v", sa, err)
	}
	if sa, err := l.SeriesAudio(ctx, 1); err != nil || len(sa.Files)+len(sa.Profiles) != 0 {
		t.Fatalf("unknown: %+v %v", sa, err)
	}
}

func TestLibrary_SearchTitles(t *testing.T) {
	its, err := lib(t).SearchTitles(ctx, "movie", "матрица", 0)
	if err != nil || len(its) != 1 || !reflect.DeepEqual(its[0], matrix) {
		t.Fatalf("%+v %v", its, err)
	}
}

func TestLibrary_ItemFiles(t *testing.T) {
	l := lib(t)
	fs, err := l.ItemFiles(ctx, 2)
	want0 := library.File{ID: 2, ItemID: 4, Path: "{root}/series/Сёгун (2024)/Season 1/Сёгун.S01E01.mkv", Size: 5,
		Mtime: 1793534400, DurationMs: 3480000}
	if err != nil || len(fs) != 2 || fs[1].ID != 3 || fs[1].ProbeJSON != "" || !strings.Contains(fs[0].ProbeJSON, "Оригинал") {
		t.Fatalf("%+v %v", fs, err)
	}
	fs[0].ProbeJSON = ""
	if fs[0] != want0 {
		t.Fatalf("%+v", fs[0])
	}
	if _, err := l.ItemFiles(ctx, 99); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestLibrary_Item(t *testing.T) {
	l := lib(t)
	if it, err := l.Item(ctx, 1); err != nil || !reflect.DeepEqual(it, matrix) {
		t.Fatalf("%+v %v", it, err)
	}
	if _, err := l.Item(ctx, 99); !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestUsers_Names(t *testing.T) {
	names, err := coreadapt.Users{C: lib(t).C}.Names(ctx)
	if err != nil || !reflect.DeepEqual(names, map[int64]string{1: "Папа", 2: "Маша", 3: "Вася"}) {
		t.Fatalf("%v %v", names, err)
	}
}

func TestLibrary_FilesUnder(t *testing.T) {
	fs, err := lib(t).FilesUnder(ctx, "{root}/movies")
	if err != nil || len(fs) != 2 || fs[0].ID != 7 || fs[0].ItemID != 11 || fs[1].Path != "{root}/movies/Матрица (1999)/Матрица (1999).mkv" ||
		fs[1].DurationMs != 8160000 {
		t.Fatalf("%+v %v", fs, err)
	}
}

// BindRef is one PUT /library/refs/{ref} with exactly the paths; the
// core's missing and taken lists come back.
func TestLibrary_BindRef(t *testing.T) {
	var method, path string
	var got struct {
		Paths []string `json:"paths"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"bound":1,"missing":["/m/b.mkv"],"taken":["/m/c.mkv"]}`)
	}))
	t.Cleanup(srv.Close)
	l := coreadapt.Library{C: addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})}
	ih := strings.Repeat("ab", 20)
	missing, taken, err := l.BindRef(ctx, ih, []string{"/m/a.mkv", "/m/b.mkv", "/m/c.mkv"})
	if err != nil || !slices.Equal(missing, []string{"/m/b.mkv"}) || !slices.Equal(taken, []string{"/m/c.mkv"}) {
		t.Fatalf("%v %v %v", missing, taken, err)
	}
	if method != http.MethodPut || !strings.HasSuffix(path, "/library/refs/"+ih) || len(got.Paths) != 3 {
		t.Fatalf("%s %s %+v", method, path, got)
	}
}

// FileByPath, RefFiles and BindRef over the core's recorded answers.
func TestLibrary_FilesReplayed(t *testing.T) {
	l := lib(t)
	f, ok, err := l.FileByPath(ctx, "{root}/movies/Матрица (1999)/Матрица (1999).mkv")
	if err != nil || !ok || f.ID != 1 || f.ItemID != 1 || f.DurationMs != 8160000 {
		t.Fatalf("by path %+v %v %v", f, ok, err)
	}
	refs, err := l.RefFiles(ctx, "k-0001")
	if err != nil || len(refs) != 1 || refs[0].FileID != 7 || refs[0].Path != "{root}/movies/Дюна (2021)/Дюна (2021).mkv" {
		t.Fatalf("ref files %+v %v", refs, err)
	}
	missing, taken, err := l.BindRef(ctx, "k-0003", []string{"{root}/music/Кино/Группа крови/01 - Группа крови.mp3"})
	if err != nil || len(missing) != 0 || len(taken) != 0 {
		t.Fatalf("bind %v %v %v", missing, taken, err)
	}
}
