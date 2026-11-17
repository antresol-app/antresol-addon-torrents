package acquire

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/musicbrainz"
)

// Ported from the core's legacy discover_music_test (GET /api/discover
// ?kind=music, now find's route over DiscoverMedia): a song title finds
// the album it is on, the famous recording first, and says which song
// (MatchedTrack); MusicBrainz answers from the recorded «sad but true»
// fixtures.
func TestDiscoverMusicSongTitle(t *testing.T) {
	read := func(f string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", "musicbrainz", f))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	groups, recs := read("release-groups-sad-but-true.json"), read("recordings-sad-but-true.json")
	mb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ws/2/recording" {
			w.Write(recs)
			return
		}
		w.Write(groups)
	}))
	defer mb.Close()
	e := newEnv(t)
	e.svc.o.MusicBrainz = &musicbrainz.Client{BaseURL: mb.URL + "/ws/2", CoverBase: "https://caa.test", Limiter: &metadata.Limiter{}}
	cs, err := e.svc.DiscoverMedia(context.Background(), KindMusic, "sad but true")
	if err != nil {
		t.Fatal(err)
	}
	at := -1
	for i, c := range cs {
		if c.Title == "Metallica" && c.Subtitle == "Metallica" {
			at = i
		} else if c.MatchedTrack != "" {
			t.Errorf("matchedTrack on %+v", c)
		}
	}
	if at != 0 {
		t.Fatalf("Metallica at %d: %+v", at, cs)
	}
	c := cs[0]
	if c.MatchedTrack != "Sad but True" || c.Year != 1991 || c.Kind != KindMusic || c.ExtID == "" ||
		c.Poster != "https://caa.test/release-group/"+c.ExtID+"/front-500" {
		t.Fatalf("%+v", c)
	}
}
