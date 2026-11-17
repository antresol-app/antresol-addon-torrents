package acquire

import (
	"context"
	"reflect"
	"testing"
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
