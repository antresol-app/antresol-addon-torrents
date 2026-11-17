package acquire

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// «Выбрать треки…» of albums the library has (an earlier pick of some of
// their songs): each picked album folder of the release whose album the
// library has goes into that album's folder (AlbumDirs), so the album
// grows instead of getting a second copy; the others get a new folder.
// When the library has the requested album but the release has no folder
// of it, the result says the songs make a separate album.
func TestPickedTracksJoinLibraryAlbum(t *testing.T) {
	ctx := context.Background()
	const dir, dir2 = "/music/Кино/Группа крови (1988)", "/music/Кино/Звезда по имени Солнце (1989)"
	const r1, r2 = "Кино - Дискография/1988 - Группа крови", "Кино - Дискография/1989 - Звезда по имени Солнце"
	paths := []string{
		"1988 - Группа крови/01 - Группа крови.flac", "1988 - Группа крови/02 - Закрой за мной дверь.flac", "1988 - Группа крови/cover.jpg",
		"1989 - Звезда по имени Солнце/01 - Звезда по имени Солнце.flac",
	}
	cases := []struct {
		name  string
		owned []string
		title string
		files []int
		want  map[string]string
		noted bool
	}{
		{"owned, one song", []string{"Группа крови"}, "Группа крови", []int{1}, map[string]string{r1: dir}, false},
		{"owned, song and cover", []string{"Группа крови"}, "Группа крови", []int{1, 2}, map[string]string{r1: dir}, false},
		{"new album", nil, "Группа крови", []int{1}, nil, false},
		{"owned, songs of two albums", []string{"Группа крови"}, "Группа крови", []int{1, 3}, map[string]string{r1: dir}, false},
		{"both owned", []string{"Группа крови", "Звезда по имени Солнце"}, "Группа крови", []int{1, 3}, map[string]string{r1: dir, r2: dir2}, false},
		{"owned, a song of another album", []string{"Группа крови"}, "Группа крови", []int{3}, nil, false},
		{"owned, no folder of it", []string{"Ночь"}, "Ночь", []int{1}, nil, true},
	}
	ids := map[string]int64{"Группа крови": 42, "Звезда по имени Солнце": 43, "Ночь": 44}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.svc.o.Library = albumDirLib{fakeLib: e.lib, dirs: map[int64]string{42: dir, 43: dir2, 44: "/music/Кино/Ночь (1986)"}}
			e.lib.albums = map[string]library.Item{}
			for _, o := range c.owned {
				e.lib.albums["Кино|"+o] = library.Item{ID: ids[o], Kind: library.KindAlbum}
			}
			mf := planRelease(t, e, linkA, "Кино - Дискография", paths...)
			e.eng.nextIH = metafileIH(t, mf)
			res, err := e.svc.Acquire(ctx, AcquireRequest{Kind: KindMusic, Mode: ModeDownload, ExtID: "rg-kino", Title: c.title, Artist: "Кино",
				Year: 1988, ReleaseLink: linkA, SourceID: 1, ReleaseTitle: "Кино - Дискография", Files: c.files, UserID: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(e.eng.added) != 1 || !slices.Equal(e.eng.added[0].Files, c.files) {
				t.Fatalf("added %+v", e.eng.added)
			}
			a, err := e.svc.acquisition(ctx, res.InfoHash)
			if err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(a.Meta.AlbumDirs, c.want) || a.Meta.AlbumDir != "" || !a.Meta.Picked {
				t.Fatalf("meta %+v, want albumDirs %v", a.Meta, c.want)
			}
			if c.noted != (res.Note == fmt.Sprintf(msgNoAlbumFolder, c.title)) || !c.noted && res.Note != "" {
				t.Fatalf("note %q", res.Note)
			}
		})
	}
}

// A discography with two albums the library has: each album folder's songs
// go into its own library folder (no cover); a third album gets a new one.
func TestOrganizeIntoAlbumDirsPerFolder(t *testing.T) {
	root := t.TempDir()
	tor := filepath.Join(root, "torrents/music")
	const a1, a2, a3 = "Кино/1988 - Группа крови", "Кино/1989 - Звезда по имени Солнце", "Кино/1986 - Ночь"
	files := torrentFiles(t, tor, a1+"/02 - Закрой за мной дверь.flac", a1+"/cover.jpg", a2+"/02 - Невесёлая песня.flac", a3+"/01 - Ночь.flac")
	j := mediaJob(root, KindMusic, files)
	j.Title, j.Artist, j.Year = "Группа крови", "Кино", 1988
	j.Picked, j.RequestedRoot = true, a1
	d1, d2 := filepath.Join(root, "lib/Группа крови"), filepath.Join(root, "lib/Звезда")
	j.AlbumDirs = map[string]string{a1: d1, a2: d2}
	paths, err := organize.Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(d1, "02 - Закрой за мной дверь.flac"), filepath.Join(d2, "02 - Невесёлая песня.flac"),
		filepath.Join(j.MusicDir, "Кино", "Ночь (1986)", "01 - Ночь.flac")}
	slices.Sort(paths)
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
}

// The same song from another release («03. Война.mp3» next to «03 - Война.flac»)
// is not added twice; a number-only name counts by its disc and number.
// The same title is the same song only on the same disc (when both names
// have one), and a version («(Demo)», «(Live)», « - Remix») is another
// recording; a lone version word is no title.
func TestAlbumSongsHasDiscAndVersion(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"CD1/01 - Intro.flac", "05 - Песня.flac", "06 - Звезда - Live.flac", "07 - Ночь.flac"} {
		writeFile(t, dir, n, "x")
	}
	have := organize.AlbumSongsIn(dir)
	for rel, want := range map[string]bool{
		"CD1/01. Intro.mp3":         true,  // same disc
		"CD2/01. Intro.mp3":         false, // another disc's «Intro»
		"01. Intro.mp3":             true,  // no disc on this side: the title decides
		"05. Песня (Demo).mp3":      false,
		"05. Песня (Live).mp3":      false,
		"05. Песня.mp3":             true,
		"08. Пачка - Live.mp3":      false, // shares only «Live» with «Звезда - Live»
		"07. Ночь (Remastered).mp3": true,  // an edition, the same recording
	} {
		if got := have.Has(rel); got != want {
			t.Errorf("%s: %v, want %v", rel, got, want)
		}
	}
}

// A song pick that grows the download while the first pick's import still
// runs (waiting on the library scan): that import, finishing later, does
// not mark the larger selection imported; the next reconcile imports the
// added songs once the selection is complete.
func TestGrowWhileFirstImportRuns(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ih := kinoInEngine(t, e)
	if _, err := e.svc.Acquire(ctx, kinoPick(0)); err != nil {
		t.Fatal(err)
	}
	inScan, release := make(chan struct{}), make(chan struct{})
	first := true
	e.lib.scanned = func() {
		if first {
			first = false
			close(inScan)
			<-release
		}
	}
	now := time.Now()
	setDone := func(done *time.Time) {
		e.eng.mu.Lock()
		st := e.eng.st[ih]
		st.Mode, st.CompletedAt = ModeDownload, done
		e.eng.st[ih] = st
		e.eng.mu.Unlock()
	}
	setDone(&now)
	e.svc.startOrganize(ih, false)
	<-inScan
	// the first import waits on the scan; the pick grows the download
	if _, err := e.svc.Acquire(ctx, kinoPick(2)); err != nil {
		t.Fatal(err)
	}
	setDone(nil) // the added song downloads
	close(release)
	e.svc.wg.Wait()
	if a, _ := e.svc.acquisition(ctx, ih); a.OrganizedAt.Valid {
		t.Fatalf("the old import marked the grown selection imported: %+v", a)
	}
	album := filepath.Join(e.root, "media/music/Кино/Группа крови (1988)")
	if _, err := os.Stat(filepath.Join(album, "03 - Война.flac")); !os.IsNotExist(err) {
		t.Fatalf("added song imported before it completed: %v", err)
	}
	// still downloading: reconcile leaves it
	e.svc.reconcile(ctx)
	e.svc.wg.Wait()
	if a, _ := e.svc.acquisition(ctx, ih); a.OrganizedAt.Valid {
		t.Fatal("imported while downloading")
	}
	// complete: reconcile imports the added song
	later := time.Now()
	setDone(&later)
	e.svc.reconcile(ctx)
	e.svc.wg.Wait()
	a, _ := e.svc.acquisition(ctx, ih)
	if !a.OrganizedAt.Valid {
		t.Fatalf("not imported after completion: %+v", a)
	}
	if _, err := os.Stat(filepath.Join(album, "03 - Война.flac")); err != nil {
		t.Fatalf("added song: %v", err)
	}
}

func TestOrganizeIntoAlbumDirSkipsSameSong(t *testing.T) {
	root := t.TempDir()
	tor := filepath.Join(root, "torrents/music")
	const folder = "Кино - Группа крови (2012) [MP3]"
	files := torrentFiles(t, tor, folder+"/03. Война.mp3", folder+"/04. Спокойная ночь.mp3", folder+"/CD2/05.mp3", folder+"/CD2/06.mp3")
	j := mediaJob(root, KindMusic, files)
	j.Title, j.Artist, j.Year = "Группа крови", "Кино", 1988
	j.Picked, j.RequestedRoot = true, folder
	dir := filepath.Join(j.MusicDir, "Кино", "Группа крови (1988)")
	writeFile(t, dir, "03 - Война.flac", "flac")
	writeFile(t, dir, "CD2/05.flac", "flac")
	j.AlbumDir = dir
	paths, err := organize.Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "04. Спокойная ночь.mp3"), filepath.Join(dir, "CD2", "06.mp3")}
	slices.Sort(paths)
	if !slices.Equal(paths, want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
}

// The track step marks the songs the library's album has already
// (another release's copies): «Уже в медиатеке».
func TestReleaseFilesMarksSongsInLibrary(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.root, "media/music/Кино/Группа крови (1988)")
	writeFile(t, dir, "03 - Война.flac", "flac")
	e.svc.o.Library = albumDirLib{fakeLib: e.lib, dirs: map[int64]string{42: dir}}
	e.lib.albums = map[string]library.Item{"Кино|Группа крови": {ID: 42, Kind: library.KindAlbum}}
	mf := testMetafile(t, "Кино - Группа крови (2012) [MP3]", "02. Закрой за мной дверь.mp3", "03. Война.mp3", "cover.jpg")
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	res := previewFiles(t, e, groupaKrovi)
	for _, f := range res.Files {
		if f.InLibrary != strings.HasSuffix(f.Path, "Война.mp3") {
			t.Errorf("%+v", f)
		}
	}
}

// Into an album the library has, a track whose name the album holds
// already (another copy of that song) is left out: no «01 - … (2).flac».
func TestOrganizeIntoAlbumDirSkipsTracksItHas(t *testing.T) {
	root := t.TempDir()
	tor := filepath.Join(root, "torrents/music")
	const folder = "Кино - Группа крови (1988)"
	files := torrentFiles(t, tor, folder+"/01 - Группа крови.flac", folder+"/02 - Закрой за мной дверь.flac")
	j := mediaJob(root, KindMusic, files)
	j.Title, j.Artist, j.Year = "Группа крови", "Кино", 1988
	j.Picked, j.RequestedRoot = true, folder
	dir := filepath.Join(j.MusicDir, "Кино", "Группа крови (1988)")
	writeFile(t, dir, "01 - Группа крови.flac", "the first pick's copy")
	j.AlbumDir = dir
	want := []string{filepath.Join(dir, "02 - Закрой за мной дверь.flac")}
	paths, err := organize.Organize(context.Background(), j)
	if err != nil || !slices.Equal(paths, want) {
		t.Fatalf("paths %v %v", paths, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("album folder %v", entries)
	}
	// a re-run of the same organize keeps what it placed
	paths, err = organize.Organize(context.Background(), j)
	if err != nil || !slices.Equal(paths, want) {
		t.Fatalf("re-run: %v %v", paths, err)
	}
}

// «Загрузки» counts the songs of a partial album: tracks only, no cover
// or CUE sheet; a whole album says nothing.
func TestTorrentsCountPickedTracks(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := planRelease(t, e, linkA, "Кино - Группа крови (1988)",
		"01 - Группа крови.flac", "02 - Закрой за мной дверь.flac", "03 - Война.flac", "cover.jpg", "album.cue")
	e.eng.nextIH = metafileIH(t, mf)
	_, _, entries, err := torrent.ParseFiles(mf)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range entries {
		e.eng.nextFiles = append(e.eng.nextFiles, torrent.FileInfo{Index: f.Index, Path: f.Path, Size: f.Size})
	}
	res, err := e.svc.Acquire(ctx, AcquireRequest{Kind: KindMusic, Mode: ModeDownload, ExtID: "rg-kino", Title: "Группа крови", Artist: "Кино",
		Year: 1988, ReleaseLink: linkA, SourceID: 1, Files: []int{1, 2, 3}, UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	setFiles := func(n int) {
		e.eng.mu.Lock()
		st := e.eng.st[res.InfoHash]
		st.SelectedFiles, st.TotalFiles = n, 5
		e.eng.st[res.InfoHash] = st
		e.eng.mu.Unlock()
	}
	view := func() TorrentView {
		for _, x := range e.svc.Torrents() {
			if x.InfoHash == res.InfoHash {
				return x
			}
		}
		t.Fatal("not listed")
		return TorrentView{}
	}
	setFiles(3)
	if v := view(); v.SelectedTracks != 2 || v.TotalTracks != 3 {
		t.Fatalf("tracks %d of %d", v.SelectedTracks, v.TotalTracks)
	}
	setFiles(5) // the whole album: no count
	if v := view(); v.SelectedTracks != 0 || v.TotalTracks != 0 {
		t.Fatalf("whole album: %+v", v)
	}
}

const kinoRelease = "Кино - Группа крови (1988)"

// kinoInEngine: the source answers linkA with a 3-song release (plus a
// cover) whose files exist under the torrents folder, as a downloaded
// torrent's would; the engine adds it as nextIH.
func kinoInEngine(t *testing.T, e *env) string {
	t.Helper()
	songs := []string{"01 - Группа крови.flac", "02 - Закрой за мной дверь.flac", "03 - Война.flac", "cover.jpg"}
	mf := planRelease(t, e, linkA, kinoRelease, songs...)
	e.eng.nextIH = metafileIH(t, mf)
	rels := make([]string, len(songs))
	for i, s := range songs {
		rels[i] = kinoRelease + "/" + s
	}
	e.eng.nextFiles = torrentFiles(t, filepath.Join(e.root, "torrents/music"), rels...)
	return e.eng.nextIH
}

func kinoPick(files ...int) AcquireRequest {
	return AcquireRequest{Kind: KindMusic, Mode: ModeDownload, ExtID: "rg-kino", Title: "Группа крови", Artist: "Кино", Year: 1988,
		ReleaseLink: linkA, SourceID: 1, ReleaseTitle: kinoRelease, Files: files, UserID: 1}
}

// The track step of a release «Загрузки» has with some songs: the file
// list (from the download itself), those songs marked as downloading; a
// release downloaded whole says so instead.
func TestReleaseFilesOfPartialDownload(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ih := kinoInEngine(t, e)
	if _, err := e.svc.Acquire(ctx, kinoPick(1)); err != nil {
		t.Fatal(err)
	}
	fetchesBefore := len(e.idx.downloads)
	req := ReleaseFilesRequest{SourceID: 1, ReleaseLink: linkA, Title: "Группа крови", Artist: "Кино", Year: 1988, Song: "Война"}
	res, err := e.svc.ReleaseFiles(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added || !res.InDownload || res.InfoHash != ih || len(res.Files) != 4 {
		t.Fatalf("%+v", res)
	}
	for _, f := range res.Files {
		if f.Downloading != (f.Index == 1) {
			t.Errorf("downloading %+v", f)
		}
		if f.Matched != (f.Index == 2) {
			t.Errorf("matched %+v", f)
		}
	}
	if len(e.idx.downloads) != fetchesBefore+1 && len(e.idx.downloads) != fetchesBefore {
		t.Fatalf("downloads %v", e.idx.downloads)
	}

	// the whole release in «Загрузки»: nothing left to pick
	e2 := newEnv(t)
	kinoInEngine(t, e2)
	if _, err := e2.svc.Acquire(ctx, AcquireRequest{Kind: KindMusic, Mode: ModeDownload, ExtID: "rg-kino", Title: "Группа крови", Artist: "Кино",
		Year: 1988, ReleaseLink: linkA, SourceID: 1, UserID: 1}); err != nil {
		t.Fatal(err)
	}
	res, err = e2.svc.ReleaseFiles(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Added || res.InDownload || res.Message != msgReleaseWhole || len(res.Files) != 0 {
		t.Fatalf("whole: %+v", res)
	}
}

// A second pick of the same release adds its songs to the download that
// has the first ones: one download, a larger selection, organized again
// into the same album folder when it completes (the songs placed before
// stay, no second copy).
func TestPickMoreTracksOfSameRelease(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ih := kinoInEngine(t, e)
	if _, err := e.svc.Acquire(ctx, kinoPick(0, 3)); err != nil {
		t.Fatal(err)
	}
	e.svc.organize(ctx, ih, false, false)
	album := filepath.Join(e.root, "media/music/Кино/Группа крови (1988)")
	a, err := e.svc.acquisition(ctx, ih)
	if err != nil || !a.OrganizedAt.Valid {
		t.Fatalf("first organize: %+v %v", a, err)
	}
	if v := e.svc.Torrents(); len(v) != 1 {
		t.Fatalf("views %+v", v)
	}
	e.eng.mu.Lock()
	st := e.eng.st[ih]
	st.SelectedFiles, st.TotalFiles = 2, 4
	e.eng.st[ih] = st
	e.eng.mu.Unlock()
	if v := e.svc.Torrents()[0]; v.SelectedTracks != 1 || v.TotalTracks != 3 {
		t.Fatalf("first counts %d of %d", v.SelectedTracks, v.TotalTracks)
	}

	res, err := e.svc.Acquire(ctx, kinoPick(2))
	if err != nil {
		t.Fatal(err)
	}
	if res.InfoHash != ih || !res.Existing {
		t.Fatalf("result %+v", res)
	}
	if len(e.eng.added) != 1 || len(e.eng.grown) != 1 || !slices.Equal(e.eng.grown[0], []int{2}) {
		t.Fatalf("added %d, grown %v", len(e.eng.added), e.eng.grown)
	}
	a, err = e.svc.acquisition(ctx, ih)
	if err != nil || a.OrganizedAt.Valid || a.Attempts != 0 || a.Error != "" {
		t.Fatalf("not reopened: %+v %v", a, err)
	}
	// «Загрузки»: still one download, the count follows the selection
	e.eng.mu.Lock()
	st.SelectedFiles = 3
	e.eng.st[ih] = st
	e.eng.mu.Unlock()
	if v := e.svc.Torrents(); len(v) != 1 || v[0].SelectedTracks != 2 || v[0].TotalTracks != 3 {
		t.Fatalf("views %+v", v)
	}

	e.svc.organize(ctx, ih, false, false)
	entries, err := os.ReadDir(album)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, en := range entries {
		names = append(names, en.Name())
	}
	if want := []string{"01 - Группа крови.flac", "03 - Война.flac", "cover.jpg"}; !slices.Equal(names, want) {
		t.Fatalf("album folder %v, want %v", names, want)
	}
	if dirs, _ := os.ReadDir(filepath.Join(e.root, "media/music/Кино")); len(dirs) != 1 {
		t.Fatalf("artist folder %v", dirs)
	}
	if a, _ := e.svc.acquisition(ctx, ih); !a.OrganizedAt.Valid {
		t.Fatalf("second organize: %+v", a)
	}

	// songs it has already: nothing changes, nothing reopened
	if _, err := e.svc.Acquire(ctx, kinoPick(0)); err != nil {
		t.Fatal(err)
	}
	if a, _ := e.svc.acquisition(ctx, ih); !a.OrganizedAt.Valid {
		t.Fatalf("reopened for nothing: %+v", a)
	}
	// a pick without a song is refused as before
	if _, err := e.svc.Acquire(ctx, kinoPick(3)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cover only: %v", err)
	}
}

// A metadata wait that runs out names no number of seconds (the acquire
// path's deadline began before its search); a torrent closed meanwhile
// says that instead.
func TestFileListWaitMessages(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{{torrent.ErrNoInfo, msgFileListTimeout}, {torrent.ErrInfoClosed, msgFileListClosed}, {torrent.ErrClosed, msgFileListClosed}} {
		e := newEnv(t)
		e.eng.fetchInfo = func(context.Context, string) ([]byte, error) { return nil, c.err }
		req := groupaKrovi
		req.ReleaseLink = "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)
		_, err := e.svc.ReleaseFiles(context.Background(), req)
		if msg, _ := UserMessage(err); msg != c.want {
			t.Errorf("%v: %q", c.err, msg)
		}
	}
	if strings.ContainsAny(msgFileListTimeout, "0123456789") {
		t.Fatalf("a number in %q", msgFileListTimeout)
	}
}
