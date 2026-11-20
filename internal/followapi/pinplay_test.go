package followapi

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol/addonkit"
)

// pinDeps are the env's deps as the pin routes have them, for the calls
// that do not go through HTTP.
func (e *env) pinDeps() Deps {
	return Deps{Pins: e.pins, Acquire: e.player, Library: e.lib,
		Watch: func(_ context.Context, u addonkit.User, ih string, idx int) (string, error) {
			e.watches = append(e.watches, watched{u.ID, ih, idx})
			return "in-" + strconv.Itoa(len(e.watches)), nil
		},
		Added: func() { e.added++ }}
}

// PlayPinned is the pinned release's «Смотреть» without the HTTP: no pin
// (or nothing tracked) is found=false and starts nothing; a pin streams
// exactly as POST /pinned/{kind}/{tmdbId}/play does.
func TestPlayPinned(t *testing.T) {
	e := newEnv(t)
	d := e.pinDeps()
	mama := addonkit.User{ID: 2, UID: "uid-2", Name: "Мама", Lang: "ru"}
	ctx := context.Background()
	if id, found, err := d.PlayPinned(ctx, mama, "series", 100, 1, 2); id != "" || found || err != nil {
		t.Fatalf("untracked title: %q %v %v", id, found, err)
	}
	e.pin(e.do("POST", "/api/pinned/series/100", mamaU, `{"sourceId":3,"link":"https://tracker.test/dl/1"}`))
	e.pin(e.do("POST", "/api/pinned/movie/6", mamaU, `{"sourceId":3,"link":"https://tracker.test/dl/1"}`))
	e.do("DELETE", "/api/pinned/movie/6", mamaU, "")
	if id, found, err := d.PlayPinned(ctx, mama, "movie", 6, 0, 0); id != "" || found || err != nil {
		t.Fatalf("tracked, not pinned: %q %v %v", id, found, err)
	}
	if len(e.player.plays) != 0 {
		t.Fatalf("started without a pin: %+v", e.player.plays)
	}

	id, found, err := d.PlayPinned(ctx, mama, "series", 100, 1, 2)
	if id != "in-1" || !found || err != nil {
		t.Fatalf("pinned: %q %v %v", id, found, err)
	}
	c := e.player.plays[0]
	if c.Mode != acquire.ModeStream || c.ReleaseLink != "https://tracker.test/dl/1" || c.SourceID != 3 || c.Season != 1 || c.Episode != 2 ||
		c.Kind != acquire.KindSeries || c.TMDBID != 100 || c.UserID != 2 || !c.CheckLink {
		t.Fatalf("request %+v", c)
	}
	if want := (watched{2, "abcdef0123456789abcdef0123456789abcdef01", 1}); len(e.watches) != 1 || e.watches[0] != want {
		t.Fatalf("watch %+v", e.watches)
	}
	if e.added != 1 || e.acq.n() != 0 {
		t.Fatalf("holds changes %d, follow acquisitions %d", e.added, e.acq.n())
	}

	// a profile with a rating limit does not reach a title without a known
	// certification: as if it had no pin
	teen := mama
	teen.MaxRating = "12+"
	if id, found, err := d.PlayPinned(ctx, teen, "series", 100, 1, 2); id != "" || found || err != nil {
		t.Fatalf("hidden: %q %v %v", id, found, err)
	}
	if len(e.player.plays) != 1 {
		t.Fatalf("a hidden title was started: %+v", e.player.plays)
	}

	// the watch's error comes back with found=true (the pin was there)
	d.Watch = func(context.Context, addonkit.User, string, int) (string, error) { return "", errors.New("no input") }
	if id, found, err := d.PlayPinned(ctx, mama, "series", 100, 1, 1); id != "" || !found || err == nil || err.Error() != "no input" {
		t.Fatalf("watch error: %q %v %v", id, found, err)
	}

	// nothing to play with: no pins service, or no way to watch
	if _, found, err := (Deps{}).PlayPinned(ctx, mama, "series", 100, 1, 1); found || err != nil {
		t.Fatalf("empty deps: %v %v", found, err)
	}
	d.Watch = nil
	if _, found, err := d.PlayPinned(ctx, mama, "series", 100, 1, 1); found || err != nil {
		t.Fatalf("no watch: %v %v", found, err)
	}
}
