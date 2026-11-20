package acquire

import (
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// The player's «Статистика»: the release's title and source name (from
// the stored release), upload rate and all-time upload, connected peers —
// never a tracker address.
func TestTorrentStatusForStats(t *testing.T) {
	e := newEnv(t)
	ih := strings.Repeat("d", 40)
	e.eng.put(torrent.Status{InfoHash: ih, Name: "avatar_2009_bdrip", Mode: ModeStream, Status: torrent.StatusDownloading,
		Size: 1000, Done: 250, DownRate: 300, UpRate: 7, Uploaded: 42, Peers: 9, Seeds: 4})
	rel := `{"title":"Avatar.Extended.2009.BDRip.1080p.2xRus.Eng","sourceName":"RuTracker","link":"https://tracker.example/dl?passkey=secret"}`
	if _, err := e.svc.o.DB.Exec(`INSERT INTO torrents (infohash, name, mode, save_dir, release_json) VALUES (?, 'avatar_2009_bdrip', 'stream', '/t', ?)`, ih, rel); err != nil {
		t.Fatal(err)
	}
	st, err := e.svc.TorrentStatus(ih, -1)
	if err != nil {
		t.Fatal(err)
	}
	want := StreamStatus{Status: torrent.StatusDownloading, Mode: ModeStream, Size: 1000, Done: 250, DownRate: 300, UpRate: 7, Uploaded: 42,
		Peers: 9, Seeds: 4, Name: "Avatar.Extended.2009.BDRip.1080p.2xRus.Eng", SourceName: "RuTracker"}
	if st != want {
		t.Fatalf("status %+v\nwant   %+v", st, want)
	}

	// no stored release: the torrent's own name, no source
	other := strings.Repeat("e", 40)
	e.eng.put(torrent.Status{InfoHash: other, Name: "Some.Torrent", Mode: ModeStream, Status: torrent.StatusDownloading})
	if st, _ := e.svc.TorrentStatus(other, -1); st.Name != "Some.Torrent" || st.SourceName != "" {
		t.Fatalf("no release: %+v", st)
	}
}

func TestSafeSourceName(t *testing.T) {
	for in, want := range map[string]string{
		"RuTracker":                          "RuTracker",
		" Kinozal ":                          "Kinozal",
		"https://rutracker.org/forum":        "",
		"tracker.example/announce?passkey=1": "",
		"user@host":                          "",
		"":                                   "",
	} {
		if got := safeSourceName(in); got != want {
			t.Errorf("safeSourceName(%q) = %q, want %q", in, got, want)
		}
	}
}
