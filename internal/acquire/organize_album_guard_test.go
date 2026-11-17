package acquire

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/musicbrainz"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
)

// cueSheet is a CUE sheet of one album with one track.
func cueSheet(performer, title, date, file string) string {
	return "REM DATE " + date + "\nPERFORMER \"" + performer + "\"\nTITLE \"" + title + "\"\nFILE \"" + file + "\" WAVE\n  TRACK 01 AUDIO\n    TITLE \"One\"\n    INDEX 01 00:00:00\n"
}

// guardJob is a music torrent of one album: folder/01.flac and
// folder/album.cue (folder "" = files at the torrent top).
func guardJob(t *testing.T, folder, performer, cueTitle, date string) OrganizeJob {
	t.Helper()
	root := t.TempDir()
	tor := filepath.Join(root, "torrents/music")
	dir := ""
	if folder != "" {
		dir = folder + "/"
	}
	files := torrentFiles(t, tor, dir+"01.flac", dir+"album.cue")
	writeFile(t, tor, dir+"album.cue", cueSheet(performer, cueTitle, date, "01.flac"))
	return mediaJob(root, KindMusic, files)
}

func TestOrganizeRefusesAnotherAlbum(t *testing.T) {
	cases := []struct {
		name, artist, title, folder, performer, cue, date, want string
	}{
		{"72 seasons", "Metallica", "Metallica", "Metallica - 72 Seasons (2023)", "Metallica", "Metallica - 72 Seasons", "2023", "72 Seasons (2023)"},
		{"colon prefix", "Metallica", "Metallica", "Metallica", "Metallica", "Metallica: 72 Seasons", "2023", "72 Seasons (2023)"},
		{"slash prefix", "Metallica", "Metallica", "Metallica", "Metallica", "Metallica / 72 Seasons", "2023", "72 Seasons (2023)"},
		{"load", "Metallica", "Metallica", "", "", "Load", "1996", "Load (1996)"},
		{"lz ii for lz", "Led Zeppelin", "Led Zeppelin", "Led Zeppelin", "Led Zeppelin", "Led Zeppelin II", "1969", "Led Zeppelin II (1969)"},
		{"lz for lz ii", "Led Zeppelin", "Led Zeppelin II", "Led Zeppelin", "Led Zeppelin", "Led Zeppelin", "1969", "Led Zeppelin (1969)"},
		{"another russian album", "Кино", "Группа крови", "Кино", "Кино", "Звезда по имени Солнце", "1989", "Звезда по имени Солнце (1989)"},
		{"another album, disc 2", "Metallica", "Metallica", "Metallica", "Metallica", "72 Seasons (Disc 2)", "2023", "72 Seasons (2023)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := guardJob(t, c.folder, c.performer, c.cue, c.date)
			j.Title, j.Artist, j.Year = c.title, c.artist, 1991
			// an automatic pick named like the torrent is no evidence
			j.Release = c.artist + " - " + c.cue + " (" + c.date + ") [FLAC]"
			paths, err := organize.Organize(context.Background(), j)
			if err == nil || err.Error() != "Скачан другой альбом: "+c.want {
				t.Fatalf("err %v, paths %v", err, paths)
			}
			if _, err := os.Stat(j.MusicDir); !os.IsNotExist(err) {
				t.Fatalf("music folder created: %v", err)
			}
			for _, f := range j.Files { // the torrent keeps its files (and seeds)
				if _, err := os.Stat(f.FullPath); err != nil {
					t.Fatal(err)
				}
			}
			// «Разложить всё равно»
			j.Force = true
			if _, err := organize.Organize(context.Background(), j); err != nil {
				t.Fatalf("forced: %v", err)
			}
		})
	}
}

// The review's probes: none of these is strong evidence of another album.
func TestOrganizeAlbumGuardAcceptsTheAlbum(t *testing.T) {
	cases := []struct {
		name, artist, title, folder, performer, cue, date, release string
		aliases                                                    []string
	}{
		{"remaster date", "Metallica", "Metallica", "Metallica", "Metallica", "Metallica", "2021", "", nil},
		{"remaster date at the top", "Metallica", "Metallica", "", "Metallica", "Metallica", "2021", "", nil},
		{"the wall 2011", "Pink Floyd", "The Wall", "Pink Floyd - The Wall", "Pink Floyd", "The Wall", "2011", "", nil},
		{"experience edition", "Pink Floyd", "The Wall", "The Wall (Experience Edition)", "Pink Floyd", "The Wall (Experience Edition)", "2011", "", nil},
		{"moroz 1996", "Кино", "Группа крови", "Кино - Группа крови (Moroz Records 1996)", "Кино", "Группа крови", "1996", "", nil},
		{"kino translit with artist", "Кино", "Группа крови", "Kino", "Kino", "Kino - Gruppa krovi", "1988", "", nil},
		{"kino translit", "Кино", "Группа крови", "Kino", "Kino", "Gruppa krovi", "1988", "", nil},
		{"kino translation", "Кино", "Группа крови", "Kino", "Kino", "Blood Type", "1988", "", nil},
		{"black album cue", "Metallica", "Metallica", "Metallica", "Metallica", "The Black Album", "1991", "", nil},
		{"and vs ampersand", "Simon & Garfunkel", "Parsley, Sage, Rosemary and Thyme", "", "Simon & Garfunkel", "Parsley, Sage, Rosemary & Thyme", "1966", "", nil},
		{"apostrophe and dot", "The Beatles", "Sgt. Pepper's Lonely Hearts Club Band", "", "The Beatles", "Sgt Peppers Lonely Hearts Club Band", "1967", "", nil},
		{"diacritics", "Sigur Rós", "Ágætis byrjun", "", "Sigur Ros", "Agaetis Byrjun", "1999", "", nil},
		{"va abbreviated", "Various Artists", "Now That's What I Call Music! 100", "", "Various Artists", "Now 100", "2018", "", nil},
		{"va translated", "Various Artists", "Лучшие песни 80-х", "", "VA", "Best of 80s", "2010", "", nil},
		{"remastered title", "Metallica", "Metallica", "", "Metallica", "Metallica (Remastered 2021)", "2021", "", nil},
		{"named by the hand-picked release", "Metallica", "Metallica", "", "Metallica", "Live Shit", "1993", "Metallica - Live Shit: Binge & Purge (1993)", nil},
		{"disc in brackets", "Metallica", "Metallica", "", "Metallica", "Metallica (Disc 1)", "2021", "", nil},
		{"cd number", "Metallica", "Metallica", "", "Metallica", "Metallica CD 2", "2021", "", nil},
		{"artist and cd", "Metallica", "Metallica", "", "Metallica", "Metallica - CD 1", "2021", "", nil},
		{"edition and disc", "Led Zeppelin", "Led Zeppelin", "", "Led Zeppelin", "Led Zeppelin (Deluxe Edition) [Disc 2]", "2014", "", nil},
		{"nickname and disc", "Weezer", "Weezer", "", "Weezer", "Weezer (Blue Album) Disc 2", "2004", "", []string{"Blue Album"}},
		{"numbered and disc", "Led Zeppelin", "Led Zeppelin II", "", "Led Zeppelin", "Led Zeppelin II (Disc 1)", "2014", "", nil},
		{"russian disc", "Кино", "Группа крови", "", "Кино", "Группа крови - Диск 2", "2012", "", nil},
		{"remaster suffix", "Pink Floyd", "The Wall", "", "Pink Floyd", "The Wall - 2011 Remastered Version", "2011", "", nil},
		{"named by an alias", "Weezer", "Weezer", "", "Weezer", "The Blue Album", "1994", "", []string{"Blue Album"}},
		{"another artist's sheet", "Metallica", "Metallica", "", "Megadeth", "Rust in Peace", "1990", "", nil},
		{"edition sharing a word", "Pink Floyd", "The Wall", "", "Pink Floyd", "The Wall Live 1980-81: Is There Anybody Out There?", "2000", "", nil},
		{"no title", "Metallica", "Metallica", "Metallica - Metallica", "Metallica", "", "2023", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			j := guardJob(t, c.folder, c.performer, c.cue, c.date)
			j.Title, j.Artist, j.Year, j.Release, j.Aliases, j.HandPicked = c.title, c.artist, 1991, c.release, c.aliases, c.release != ""
			paths, err := organize.Organize(context.Background(), j)
			if err != nil {
				t.Fatal(err)
			}
			if len(paths) == 0 {
				t.Fatal("nothing placed")
			}
		})
	}
}

// A real two-disc layout: CD1/ and CD2/, each with its own sheet.
func TestOrganizeAlbumGuardMultiDisc(t *testing.T) {
	for _, c := range [][3]string{
		{"Metallica", "Metallica", "Metallica (Disc 1)"}, {"Metallica", "Metallica", "Metallica [CD1]"},
		{"Metallica", "Metallica", "Metallica - CD 1"}, {"Led Zeppelin", "Led Zeppelin II", "Led Zeppelin II (Disc 1)"},
		{"Pink Floyd", "The Wall", "The Wall (Disc 1)"},
	} {
		t.Run(c[2], func(t *testing.T) {
			root := t.TempDir()
			tor := filepath.Join(root, "torrents/music")
			d := c[0] + " - " + c[1] + "/"
			files := torrentFiles(t, tor, d+"CD1/01.flac", d+"CD1/a.cue", d+"CD2/01.flac", d+"CD2/a.cue")
			writeFile(t, tor, d+"CD1/a.cue", cueSheet(c[0], c[2], "2021", "01.flac"))
			writeFile(t, tor, d+"CD2/a.cue", cueSheet(c[0], strings.Replace(c[2], "1", "2", 1), "2021", "01.flac"))
			j := mediaJob(root, KindMusic, files)
			j.Title, j.Artist = c[1], c[0]
			paths, err := organize.Organize(context.Background(), j)
			if err != nil || len(paths) != 4 {
				t.Fatalf("%v %v", paths, err)
			}
		})
	}
}

func TestForceOrganizeSkipsTheAlbumCheck(t *testing.T) {
	e := newEnv(t)
	const name = "Metallica - Metallica (1991) [FLAC]"
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(name, "https://t/1", 1, 50)}}
	tor := filepath.Join(e.root, "torrents", "music")
	e.eng.nextFiles = torrentFiles(t, tor, "M/01.flac", "M/album.cue")
	writeFile(t, tor, "M/album.cue", cueSheet("Metallica", "72 Seasons", "2023", "01.flac"))
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindMusic, Title: "Metallica", Artist: "Metallica", Year: 1991,
		Aliases: []string{"live", "The Black Album"}, Mode: ModeDownload, UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.svc.acquisition(context.Background(), res.InfoHash)
	if err != nil || a.Meta.Release != name || !slices.Equal(a.Meta.Aliases, []string{"The Black Album"}) {
		t.Fatalf("meta %+v %v", a.Meta, err)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); a.Error != "Скачан другой альбом: 72 Seasons (2023)" || a.OrganizedAt.Valid {
		t.Fatalf("after download %+v", a)
	}
	if err := e.svc.RetryOrganize(context.Background(), res.InfoHash); err != nil {
		t.Fatal(err)
	}
	e.svc.wait()
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); a.OrganizedAt.Valid {
		t.Fatal("retry skipped the check")
	}
	if err := e.svc.ForceOrganize(context.Background(), res.InfoHash); err != nil {
		t.Fatal(err)
	}
	e.svc.wait()
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); !a.OrganizedAt.Valid || a.Error != "" {
		t.Fatalf("forced %+v", a)
	}
	if _, err := os.Stat(filepath.Join(e.root, "media", "music", "Metallica", "Metallica (1991)", "01.flac")); err != nil {
		t.Fatal(err)
	}
}

func TestMusicRequestCarriesYearAndAliases(t *testing.T) {
	e := newEnv(t)
	e.mb.groups = []musicbrainz.ReleaseGroup{{ID: "mb-w", Title: "Weezer", Artist: "Weezer", Year: 1994, Disambiguation: "compilation",
		Aliases: []string{"Blue Album", "live"}}}
	cs, err := e.svc.DiscoverMedia(context.Background(), KindMusic, "weezer")
	// the disambiguation is a note («compilation»), not a name; generic aliases go
	if err != nil || len(cs) != 1 || !slices.Equal(cs[0].Aliases, []string{"Blue Album"}) {
		t.Fatalf("candidates %+v %v", cs, err)
	}
	if _, err := e.svc.Releases(context.Background(), ReleaseRequest{Kind: KindMusic, Title: "Weezer", Artist: "Weezer", Year: 1994, Aliases: cs[0].Aliases}); err != nil {
		t.Fatal(err)
	}
	// an artist alone has no year to check; a book never does
	if _, err := e.svc.Releases(context.Background(), ReleaseRequest{Kind: KindMusic, Artist: "Weezer", Year: 1994}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Releases(context.Background(), ReleaseRequest{Kind: KindBook, Title: "Солярис", Artist: "Лем", Year: 1961}); err != nil {
		t.Fatal(err)
	}
	q := e.idx.requests
	if q[0].Year != 1994 || !slices.Equal(q[0].Aliases, []string{"Blue Album"}) || q[1].Year != 0 || q[2].Year != 0 {
		t.Fatalf("requests %+v", q)
	}
}

func TestHandPickedReleaseIsKept(t *testing.T) {
	e := newEnv(t)
	e.eng.nextFiles = torrentFiles(t, filepath.Join(e.root, "torrents", "music"), "M/01.flac")
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindMusic, Title: "Metallica", Artist: "Metallica", Mode: ModeDownload,
		ReleaseLink: "https://t/9", SourceID: 1, ReleaseTitle: "Metallica - Live Shit: Binge & Purge (1993)", UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	a, err := e.svc.acquisition(context.Background(), res.InfoHash)
	if err != nil || !a.Meta.HandPicked || a.Meta.Release != "Metallica - Live Shit: Binge & Purge (1993)" {
		t.Fatalf("meta %+v %v", a.Meta, err)
	}
}

// otherAlbumTorrent: a hand-picked release for «Metallica» whose CUE
// sheet names «72 Seasons» (files 01.flac, 02.flac, album.cue).
func otherAlbumTorrent(t *testing.T, e *env) {
	t.Helper()
	selectionTorrent(t, e, "M", "01.flac", "02.flac", "album.cue")
	writeFile(t, filepath.Join(e.root, "torrents", "music"), "M/album.cue", cueSheet("Metallica", "72 Seasons", "2023", "01.flac"))
	previewFiles(t, e, ReleaseFilesRequest{SourceID: 3, ReleaseLink: "https://t/1", Title: "Metallica", Artist: "Metallica", Year: 1991})
}

func metallicaPick(files ...int) AcquireRequest {
	return AcquireRequest{Kind: KindMusic, Title: "Metallica", Artist: "Metallica", Year: 1991, Mode: ModeDownload,
		ReleaseLink: "https://t/1", SourceID: 3, ReleaseTitle: "Metallica - Metallica (1991) [FLAC]", UserID: 1, Files: files}
}

func TestOrganizePickedSkipsOtherAlbumCheck(t *testing.T) {
	e := newEnv(t)
	otherAlbumTorrent(t, e)
	res, err := e.svc.Acquire(context.Background(), metallicaPick(0, 2)) // 02.flac unchecked
	if err != nil {
		t.Fatal(err)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); !a.OrganizedAt.Valid || a.Error != "" || !a.Meta.Picked {
		t.Fatalf("not organized: %+v", a)
	}
	dir := filepath.Join(e.root, "media", "music", "Metallica", "Metallica (1991)")
	if _, err := os.Stat(filepath.Join(dir, "01.flac")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "02.flac")); !os.IsNotExist(err) {
		t.Fatalf("unselected track placed: %v", err)
	}

	// the job itself: Picked skips the check
	j := guardJob(t, "Metallica", "Metallica", "72 Seasons", "2023")
	j.Title, j.Artist, j.Year, j.Picked, j.RequestedRoot = "Metallica", "Metallica", 1991, true, "Metallica"
	if paths, err := organize.Organize(context.Background(), j); err != nil || len(paths) != 2 {
		t.Fatalf("%v %v", paths, err)
	}
}

func TestOrganizeUntouchedSingleAlbumKeepsCheck(t *testing.T) {
	e := newEnv(t)
	otherAlbumTorrent(t, e)
	res, err := e.svc.Acquire(context.Background(), metallicaPick(0, 1, 2)) // every file, as the preview checked them
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); a.Meta.Picked || a.Meta.RequestedRoot != "M" {
		t.Fatalf("meta %+v", a.Meta)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); a.Error != "Скачан другой альбом: 72 Seasons (2023)" || a.OrganizedAt.Valid {
		t.Fatalf("after download %+v", a)
	}
	if _, err := os.Stat(filepath.Join(e.root, "media", "music")); !os.IsNotExist(err) {
		t.Fatalf("music folder created: %v", err)
	}
}

// An album the library has already, in a folder named unlike the torrent's:
// the selected tracks go into that folder (disc folders kept), with no
// cover, CUE sheet or second album folder, and no other-album check.
func TestOrganizeIntoExistingAlbumDir(t *testing.T) {
	root := t.TempDir()
	tor := filepath.Join(root, "torrents/music")
	const folder = "Кино - Группа крови (Remastered)"
	files := torrentFiles(t, tor, folder+"/05 - Бошетунмай.flac", folder+"/cover.jpg", folder+"/album.cue", folder+"/CD2/01 - Бонус.flac")
	writeFile(t, tor, folder+"/album.cue", cueSheet("Кино", "Звезда по имени Солнце", "1989", "05 - Бошетунмай.flac"))
	j := mediaJob(root, KindMusic, files)
	j.Title, j.Artist, j.Year = "Группа крови", "Кино", 1988
	j.Picked, j.RequestedRoot = true, folder
	dir := filepath.Join(j.MusicDir, "Кино", "Группа крови")
	writeFile(t, dir, "01 - Группа крови.flac", "old")
	j.AlbumDir = dir
	paths, err := organize.Organize(context.Background(), j)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "05 - Бошетунмай.flac"), filepath.Join(dir, "CD2", "01 - Бонус.flac")}
	slices.Sort(paths)
	slices.Sort(want)
	if !slices.Equal(paths, want) {
		t.Fatalf("paths %v, want %v", paths, want)
	}
	entries, err := os.ReadDir(filepath.Join(j.MusicDir, "Кино"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "Группа крови" {
		t.Fatalf("artist folder: %v %v", entries, err)
	}
	for _, n := range []string{"cover.jpg", "album.cue"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Fatalf("%s placed: %v", n, err)
		}
	}
	// without AlbumDir the same torrent is refused as another album
	j.AlbumDir, j.Picked = "", false
	if _, err := organize.Organize(context.Background(), j); err == nil || !strings.HasPrefix(err.Error(), "Скачан другой альбом") {
		t.Fatalf("guard without AlbumDir: %v", err)
	}
}

// Into an album the library has, only tracks of the requested album's
// folder: another album of the torrent never lands there, and a torrent
// without the requested album places nothing.
func TestOrganizeIntoExistingAlbumDirOnlyRequestedRoot(t *testing.T) {
	root := t.TempDir()
	tor := filepath.Join(root, "torrents/music")
	files := torrentFiles(t, tor, "Кино - Дискография/1988 - Группа крови/05 - Бошетунмай.flac", "Кино - Дискография/1990 - Live/05 - Бошетунмай.flac")
	j := mediaJob(root, KindMusic, files)
	j.Title, j.Artist, j.Year = "Группа крови", "Кино", 1988
	j.Picked, j.RequestedRoot = true, "Кино - Дискография/1988 - Группа крови"
	dir := filepath.Join(j.MusicDir, "Кино", "Группа крови")
	j.AlbumDir = dir
	paths, err := organize.Organize(context.Background(), j)
	if err != nil || !slices.Equal(paths, []string{filepath.Join(dir, "05 - Бошетунмай.flac")}) {
		t.Fatalf("paths %v %v", paths, err)
	}
	j.RequestedRoot = ""
	if paths, err := organize.Organize(context.Background(), j); err == nil || len(paths) != 0 {
		t.Fatalf("no requested root: %v %v", paths, err)
	}
}
