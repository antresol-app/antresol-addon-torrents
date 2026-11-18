package acquire

import (
	"context"
	"reflect"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

func TestPendingAcquisitions(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	db := e.svc.o.DB
	for _, q := range []string{
		// queued download of S01E08, a season pack, an organized movie, a
		// stream (never organized), a row whose torrent is gone, a range
		// picked for E03, a season pack picked for E05, a queued movie
		`INSERT INTO torrents (infohash, name, mode, save_dir, release_json) VALUES ('aa','Shogun.S01E08.1080p','download','/t',NULL),
			('bb','Shogun.S02.1080p','seed','/t',NULL),('cc','Brat','download','/t',NULL),('dd','x','stream','/t',NULL),
			('ff','x','download','/t','{"title":"Сёгун / Shogun [S03E03-E08 из 10]"}'),('gg','Shogun.S04.WEB-DL.1080p','download','/t','{"title":"bad'),
			('hh','Dune','download','/t',NULL)`,
		`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, episode) VALUES ('aa','series',42,'Сёгун',1,8),('bb','series',42,'Сёгун',2,0),
			('dd','series',42,'Сёгун',1,9),('ee','movie',7,'Дюна',0,0),('ff','series',42,'Сёгун',3,3),('gg','series',42,'Сёгун',4,5),('hh','movie',7,'Дюна',0,0)`,
		`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, organized_at) VALUES ('cc','movie',8,'Брат','2024-01-01T00:00:00Z')`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	got, err := e.svc.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []PendingAcquisition{{Kind: "series", TMDBID: 42, Season: 1, Episode: 8, InfoHash: "aa"}, {Kind: "series", TMDBID: 42, Season: 2, InfoHash: "bb"},
		{Kind: "series", TMDBID: 42, Season: 3, Episode: 3, EpisodeEnd: 8, InfoHash: "ff"}, {Kind: "series", TMDBID: 42, Season: 4, InfoHash: "gg"},
		{Kind: "movie", TMDBID: 7, InfoHash: "hh"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// pendingOf is the rows of Pending for infohash ih, as {season, episode,
// end}.
func pendingOf(t *testing.T, e *env, ih string) [][3]int {
	t.Helper()
	got, err := e.svc.Pending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out [][3]int
	for _, p := range got {
		if p.InfoHash == ih {
			out = append(out, [3]int{p.Season, p.Episode, p.EpisodeEnd})
		}
	}
	return out
}

func execAll(t *testing.T, e *env, qs ...string) {
	t.Helper()
	for _, q := range qs {
		if _, err := e.svc.o.DB.ExecContext(context.Background(), q); err != nil {
			t.Fatal(err)
		}
	}
}

// threeSeasons is a pack of S01–S03, 2 episodes each, in season folders
// (plus a sample and a text file, which cover nothing).
func threeSeasons() []torrent.FileInfo {
	var fs []torrent.FileInfo
	for _, p := range []string{"Show.S01-S03/Season 1/01.mkv", "Show.S01-S03/Season 1/02.mkv",
		"Show.S01-S03/Season 2/Show.S02E01.mkv", "Show.S01-S03/Season 2/Show.S02E02.mkv",
		"Show.S01-S03/S03/01.mkv", "Show.S01-S03/S03/02.mkv", "Show.S01-S03/readme.txt"} {
		fs = append(fs, torrent.FileInfo{Index: len(fs), Path: p, Size: 1 << 20})
	}
	return fs
}

// A pack of several seasons downloading whole covers each episode its
// files hold, not only the season it was asked for.
func TestPendingMultiSeasonPackFiles(t *testing.T) {
	e := newEnv(t)
	e.eng.put(torrent.Status{InfoHash: "pk", Mode: ModeDownload, Status: torrent.StatusDownloading}).files = threeSeasons()
	execAll(t, e, `INSERT INTO torrents (infohash, name, mode, save_dir) VALUES ('pk','Show.S01-S03.1080p','download','/t')`,
		`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, episode) VALUES ('pk','series',42,'Show',1,0)`)
	want := [][3]int{{1, 1, 1}, {1, 2, 2}, {2, 1, 1}, {2, 2, 2}, {3, 1, 1}, {3, 2, 2}}
	if got := pendingOf(t, e, "pk"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Files picked by hand cover only themselves; a pack downloading some of
// its seasons covers each season asked for and not placed yet, whole.
func TestPendingPackSelection(t *testing.T) {
	e := newEnv(t)
	for _, ih := range []string{"pick", "pk"} {
		h := e.eng.put(torrent.Status{InfoHash: ih, Mode: ModeDownload, Status: torrent.StatusDownloading})
		h.files, h.selection = threeSeasons(), []int{0, 1, 2}
	}
	execAll(t, e, `INSERT INTO torrents (infohash, name, mode, save_dir) VALUES ('pick','Show.S01-S03.1080p','download','/t'),('pk','Show.S01-S03.1080p','download','/t')`,
		`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, episode, meta_json) VALUES ('pick','series',42,'Show',0,0,'{"picked":true}'),
			('pk','series',42,'Show',1,0,'{"seasons":[1,2],"organized":[1]}')`)
	if got, want := pendingOf(t, e, "pick"), [][3]int{{1, 1, 1}, {1, 2, 2}, {2, 1, 1}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("picked: got %v, want %v", got, want)
	}
	if got, want := pendingOf(t, e, "pk"), [][3]int{{2, 0, 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pack: got %v, want %v", got, want)
	}
}

// Before the file list comes, a pack added whole covers each season its
// name says; a single season pack its season, as before.
func TestPendingSeasonRangeBeforeFiles(t *testing.T) {
	e := newEnv(t)
	execAll(t, e, `INSERT INTO torrents (infohash, name, mode, save_dir, release_json) VALUES ('pk','x','download','/t','{"title":"Сериал / Show [S01-S03] WEB-DL 1080p"}'),
			('one','Show.S04.1080p','download','/t',NULL)`,
		`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, episode) VALUES ('pk','series',42,'Show',0,0),('one','series',42,'Show',4,0)`)
	if got, want := pendingOf(t, e, "pk"), [][3]int{{1, 0, 0}, {2, 0, 0}, {3, 0, 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pack: got %v, want %v", got, want)
	}
	if got, want := pendingOf(t, e, "one"), [][3]int{{4, 0, 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("single season: got %v, want %v", got, want)
	}
}

// A stream of a pack, a pack whose torrent was removed and one whose
// organize failed for good cover nothing; a failed attempt with retries
// left still covers.
func TestPendingPackNotCovering(t *testing.T) {
	e := newEnv(t)
	for _, ih := range []string{"st", "gone", "fail", "retry"} {
		e.eng.put(torrent.Status{InfoHash: ih, Mode: ModeDownload}).files = threeSeasons()
	}
	execAll(t, e, `INSERT INTO torrents (infohash, name, mode, save_dir) VALUES ('st','Show.S01-S03','stream','/t'),
			('fail','Show.S01-S03','download','/t'),('retry','Show.S01-S03','download','/t')`,
		`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, episode, organize_error, organize_attempts) VALUES
			('st','series',42,'Show',1,0,'',0),('gone','series',42,'Show',1,0,'',0),
			('fail','series',42,'Show',1,0,'Не удалось',3),('retry','series',42,'Show',1,0,'Не удалось',1)`)
	for _, ih := range []string{"st", "gone", "fail"} {
		if got := pendingOf(t, e, ih); got != nil {
			t.Fatalf("%s covers %v", ih, got)
		}
	}
	if got := pendingOf(t, e, "retry"); len(got) != 6 {
		t.Fatalf("retry covers %v", got)
	}
}
