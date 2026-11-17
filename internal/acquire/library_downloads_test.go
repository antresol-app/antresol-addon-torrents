package acquire

import (
	"context"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

func TestLibraryDownloads(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	justNow := now.UTC().Add(-time.Minute).Format(time.RFC3339)
	longAgo := now.UTC().Add(-time.Hour).Format(time.RFC3339)
	// a series already in the library (a new season downloading)
	e.lib.items[42] = library.Item{ID: 900, Kind: "series", TmdbID: 42}
	e.lib.albums = map[string]library.Item{}
	e.lib.books = map[string]library.Item{}
	type tor struct {
		ih, mode, status string
		done, size       int64
		age              int // minutes
		kind             string
		tmdb, season     int
		title, meta      string
		organized        any
		err              string
	}
	for _, x := range []tor{
		{"m1", torrent.ModeDownload, torrent.StatusDownloading, 40, 100, 1, "movie", 7, 0, "Дюна", `{}`, nil, ""},
		{"s1", torrent.ModeDownload, torrent.StatusPaused, 10, 50, 3, "series", 42, 2, "Сёгун", `{}`, nil, ""},
		{"s2", torrent.ModeDownload, torrent.StatusDownloading, 5, 50, 2, "series", 42, 3, "Сёгун", `{}`, nil, ""},
		{"st", torrent.ModeStream, torrent.StatusDownloading, 1, 100, 1, "movie", 8, 0, "Стрим", `{}`, nil, ""},
		{"old", torrent.ModeDownload, torrent.StatusSeeding, 100, 100, 600, "movie", 9, 0, "Брат", `{}`, longAgo, ""},
		{"org", torrent.ModeDownload, torrent.StatusSeeding, 100, 100, 5, "music", 0, 0, "Группа крови", `{"artist":"Кино"}`, justNow, ""},
		{"bad", torrent.ModeDownload, torrent.StatusSeeding, 100, 100, 4, "book", 0, 0, "Мастер и Маргарита", `{"author":"Булгаков"}`, nil, "Не получилось"},
		{"ws", torrent.ModeDownload, torrent.StatusWaitingSpace, 0, 100, 0, "audiobook", 0, 0, "Дюна", `{"author":"Герберт"}`, nil, ""},
	} {
		e.eng.put(torrent.Status{InfoHash: x.ih, Mode: x.mode, Status: x.status, Done: x.done, Size: x.size,
			AddedAt: now.Add(-time.Duration(x.age) * time.Minute)})
		if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, meta_json, organized_at, organize_error, paths_json, poster)
			VALUES (?,?,?,?,?,?,?,?,'["/x"]',?)`, x.ih, x.kind, x.tmdb, x.title, x.season, x.meta, x.organized, x.err, x.ih+".jpg"); err != nil {
			t.Fatal(err)
		}
	}
	got := e.svc.LibraryDownloads(ctx)
	type row struct {
		key, status, creator string
		item                 int64
		done, size           int64
	}
	var rows []row
	for _, d := range got {
		rows = append(rows, row{d.Key, d.Status, d.Creator, d.ItemID, d.Done, d.Size})
	}
	want := []row{
		{"audiobook:|герберт|дюна", "waiting", "Герберт", 0, 0, 100},
		{"movie:7", "active", "", 0, 40, 100},
		// two seasons summed; downloading wins over paused; in the library
		{"series:42", "active", "", 900, 15, 100},
		{"book:|булгаков|мастер и маргарита", "error", "Булгаков", 0, 100, 100},
		// organized a minute ago, not scanned yet
		{"music:кино||группа крови", "organizing", "Кино", 0, 100, 100},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %+v\nwant %+v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d: got %+v\nwant %+v", i, rows[i], want[i])
		}
	}
	if got[1].Poster != "m1.jpg" || got[1].Title != "Дюна" || got[1].TMDBID != 7 {
		t.Fatalf("movie %+v", got[1])
	}

	// the scan indexed the album: its card is the library's now
	e.lib.albums["Кино|Группа крови"] = library.Item{ID: 5, Kind: "album"}
	for _, d := range e.svc.LibraryDownloads(ctx) {
		if d.Kind == KindMusic {
			t.Fatalf("organized album still listed: %+v", d)
		}
	}
}
