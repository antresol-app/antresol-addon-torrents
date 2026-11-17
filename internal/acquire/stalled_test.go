package acquire

import (
	"context"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// The stall hint of a title: set only when every active torrent of it is
// stalled (one moving season is headway), in the title-page progress and
// in the pending downloads alike.
func TestTitleStalled(t *testing.T) {
	type tor struct {
		ih, status, stalled string
		season              int
	}
	cases := []struct {
		name string
		tors []tor
		want string
	}{
		{"healthy", []tor{{"a", torrent.StatusDownloading, "", 1}}, ""},
		{"no peers", []tor{{"a", torrent.StatusDownloading, torrent.StalledNoPeers, 1}}, torrent.StalledNoPeers},
		{"magnet without metadata", []tor{{"a", torrent.StatusQueued, torrent.StalledNoMetadata, 1}}, torrent.StalledNoMetadata},
		{"one season moves", []tor{{"a", torrent.StatusDownloading, torrent.StalledNoPeers, 1}, {"b", torrent.StatusDownloading, "", 2}}, ""},
		{"both stalled, no peers wins", []tor{{"a", torrent.StatusQueued, torrent.StalledNoMetadata, 1}, {"b", torrent.StatusDownloading, torrent.StalledNoPeers, 2}}, torrent.StalledNoPeers},
		{"a paused one does not count", []tor{{"a", torrent.StatusDownloading, torrent.StalledNoPeers, 1}, {"b", torrent.StatusPaused, "", 2}}, torrent.StalledNoPeers},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			for _, x := range c.tors {
				e.eng.put(torrent.Status{InfoHash: x.ih, Mode: torrent.ModeDownload, Status: x.status, Stalled: x.stalled, Size: 100, AddedAt: time.Now()})
				if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, paths_json) VALUES (?,?,?,?,?,'["/x"]')`,
					x.ih, "series", 5, "T", x.season); err != nil {
					t.Fatal(err)
				}
			}
			p, ok := e.svc.TitleProgress(context.Background(), "series", 5, 0)
			if !ok || p.Status != "active" || p.Stalled != c.want {
				t.Fatalf("progress %+v %v, want stalled %q", p, ok, c.want)
			}
			for _, v := range e.svc.Torrents() {
				if v.TmdbID != 5 || v.Season == 0 {
					t.Fatalf("torrent view without its title: %+v", v)
				}
			}
			ds := e.svc.LibraryDownloads(context.Background())
			if len(ds) != 1 || ds[0].Stalled != c.want {
				t.Fatalf("downloads %+v, want stalled %q", ds, c.want)
			}
		})
	}
}
