package pinned

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

type fakeSearcher struct {
	releases []indexer.ScoredRelease
	reqs     []indexer.SearchRequest
	at       []time.Time
}

func (f *fakeSearcher) Search(_ context.Context, q indexer.SearchRequest) (indexer.SearchResponse, error) {
	f.reqs = append(f.reqs, q)
	f.at = append(f.at, time.Now())
	return indexer.SearchResponse{Releases: f.releases}, nil
}

func rel(title, link string, score int) indexer.ScoredRelease {
	return indexer.ScoredRelease{Result: cardigann.Result{Title: title, Link: link}, SourceID: 3, SourceName: "Tracker", Score: score}
}

func newTorrentScanner(e *env, f *fakeSearcher, top int) *torrentScanner {
	sc := NewTorrentScanner(nil, e.svc, top).(*torrentScanner)
	sc.search, sc.pause = f, 0
	return sc
}

// season1 is a season of n aired episodes.
func season1(n int) []tracking.Episode {
	var eps []tracking.Episode
	for i := 1; i <= n; i++ {
		eps = append(eps, tracking.Episode{Season: 1, Number: i, AirDate: "2026-10-01"})
	}
	return eps
}

func TestTorrentScannerSeasonPackCoversSeason(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	eps := []tracking.Episode{{Season: 1, Number: 1, AirDate: "2025-01-01"},
		{Season: 2, Number: 1, AirDate: "2026-10-01"}, {Season: 2, Number: 2, AirDate: "2026-10-08"},
		{Season: 2, Number: 3, AirDate: "2026-12-01"}} // S02E03 airs later
	f := &fakeSearcher{releases: []indexer.ScoredRelease{
		rel("Show.S02.1080p.WEB-DL-GRP", "https://tracker.test/dl/pack", 50),
		rel("Show.S02E01-E02.720p.HDTV-XX", "https://tracker.test/dl/two", 40),
		{Result: cardigann.Result{Title: "Show S02E02 2160p", Magnet: "magnet:?xt=urn:btih:" + strings.Repeat("a", 40)}, SourceID: 4, SourceName: "Other", Score: 30},
	}}
	sc := newTorrentScanner(e, f, 0)
	if sc.Kind() != "torrent" {
		t.Fatal(sc.Kind())
	}
	got, err := sc.Scan(ctx, tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}, eps)
	if err != nil {
		t.Fatal(err)
	}
	want := "S2E1 Tracker https://tracker.test/dl/pack 1080p []; S2E2 Tracker https://tracker.test/dl/pack 1080p []; " +
		"S2E1 Tracker https://tracker.test/dl/two 720p []; S2E2 Tracker https://tracker.test/dl/two 720p []; " +
		"S2E2 Other magnet:?xt=urn:btih:" + strings.Repeat("a", 40) + " 2160p []"
	if srcKeys(got) != want {
		t.Fatalf("sources\n got %s\nwant %s", srcKeys(got), want)
	}
	if len(f.reqs) != 1 || f.reqs[0].Text != "Show" || f.reqs[0].Kind != "series" || f.reqs[0].Season != 2 {
		t.Fatalf("requests %+v", f.reqs)
	}
}

func TestTorrentScannerKeepsTop(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	var rs []indexer.ScoredRelease
	for i := range 8 {
		rs = append(rs, rel(fmt.Sprintf("Show.S01E%02d.1080p-GRP", i+1), fmt.Sprintf("https://tracker.test/dl/%d", i), i))
	}
	f := &fakeSearcher{releases: rs}
	got, err := newTorrentScanner(e, f, 0).Scan(ctx, tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}, season1(8))
	if err != nil {
		t.Fatal(err)
	}
	// the 5 best by score: E04..E08
	if len(got) != 5 || got[0].Episode != 8 || got[4].Episode != 4 {
		t.Fatalf("sources %s", srcKeys(got))
	}
}

func TestTorrentScannerMovie(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	f := &fakeSearcher{releases: []indexer.ScoredRelease{rel("Film.2024.1080p.WEB-DL-GRP", "https://tracker.test/dl/f", 9)}}
	got, err := newTorrentScanner(e, f, 0).Scan(ctx, tracking.Title{ID: e.title.ID, Kind: "movie", Name: "Film"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if srcKeys(got) != "S0E0 Tracker https://tracker.test/dl/f 1080p []" || f.reqs[0].Kind != "movie" || f.reqs[0].Season != 0 {
		t.Fatalf("sources %s, req %+v", srcKeys(got), f.reqs)
	}
}

const pinMagnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"

// pinMagnetSeven pins a magnet-only release of S01E01–07 (a bitmagnet find).
func pinMagnetSeven(t *testing.T, e *env, name string) Pin {
	t.Helper()
	var files []string
	for i := 1; i <= 7; i++ {
		files = append(files, fmt.Sprintf("Show.S01E%02d.1080p.WEB-DL-GRP.mkv", i))
	}
	e.eng.magnets[pinMagnet] = metafile(t, name, files...)
	p, err := e.svc.PinOwn(context.Background(), e.title.ID, pinMagnet, nil, by(e.users[0]))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTorrentScannerSuggestsForMagnetPin(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	p := pinMagnetSeven(t, e, "Show.S01.1080p.WEB-DL-GRP")
	f := &fakeSearcher{releases: []indexer.ScoredRelease{
		rel("Show.S01E01-08.1080p.WEB-DL-GRP", "https://tracker.test/dl/8", 10),
		rel("Show.S01E01-06.1080p.WEB-DL-GRP", "https://tracker.test/dl/6", 20), // fewer: no
	}}
	if _, err := newTorrentScanner(e, f, 0).Scan(ctx, tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}, season1(8)); err != nil {
		t.Fatal(err)
	}
	got, _, _ := e.svc.ByTitle(ctx, e.title.ID)
	sg := got.Suggestion
	if sg == nil || sg.Episodes != 8 || sg.Link != "https://tracker.test/dl/8" || sg.SourceID != "3" ||
		sg.ReleaseTitle != "Show.S01E01-08.1080p.WEB-DL-GRP" || !sg.Found.Equal(e.now) {
		t.Fatalf("suggestion %+v", sg)
	}
	if got.InfoHash != p.InfoHash || got.Link != p.Link || len(got.Files) != 7 || len(e.eng.replaced) != 0 {
		t.Fatalf("pin changed %+v", got)
	}
}

func TestTorrentScannerNoSuggestionOnOtherGroup(t *testing.T) {
	for name, title := range map[string]string{
		"group":      "Show.S01E01-08.1080p.WEB-DL-OTHER",
		"resolution": "Show.S01E01-08.720p.WEB-DL-GRP",
		"title":      "Another.Show.S01E01-08.1080p.WEB-DL-GRP",
		"same count": "Show.S01E01-07.1080p.WEB-DL-GRP",
	} {
		t.Run(name, func(t *testing.T) {
			e, ctx := newEnv(t), context.Background()
			pinMagnetSeven(t, e, "Show.S01.1080p.WEB-DL-GRP")
			f := &fakeSearcher{releases: []indexer.ScoredRelease{rel(title, "https://tracker.test/dl/x", 10)}}
			if _, err := newTorrentScanner(e, f, 0).Scan(ctx, tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}, season1(8)); err != nil {
				t.Fatal(err)
			}
			if got, _, _ := e.svc.ByTitle(ctx, e.title.ID); got.Suggestion != nil {
				t.Fatalf("suggestion %+v", got.Suggestion)
			}
		})
	}
}

func TestTorrentScannerNoSuggestionForTrackerPin(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	pinTwo(t, e) // a tracker topic: refreshed by its own source, never suggested over
	f := &fakeSearcher{releases: []indexer.ScoredRelease{rel("Show.S01E01-08.1080p.WEB-DL.HDRezka", "https://tracker.test/dl/8", 10)}}
	if _, err := newTorrentScanner(e, f, 0).Scan(ctx, tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}, season1(8)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := e.svc.ByTitle(ctx, e.title.ID); got.Suggestion != nil {
		t.Fatalf("suggestion %+v", got.Suggestion)
	}
}

func TestTorrentScannerDaily(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	f := &fakeSearcher{releases: []indexer.ScoredRelease{rel("Show.S01E01.1080p-GRP", "https://tracker.test/dl/1", 1)}}
	ti := tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}
	sc := newTorrentScanner(e, f, 0)
	got, err := sc.Scan(ctx, ti, season1(1))
	if err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	e.track.UpsertSources(ti.ID, "torrent", got, e.now)
	e.now = e.now.Add(23 * time.Hour)
	if _, err := sc.Scan(ctx, ti, season1(1)); !errors.Is(err, ErrUnchanged) || len(f.reqs) != 1 {
		t.Fatalf("err %v, %d searches", err, len(f.reqs))
	}
	// a new scanner (a restart) reads the cadence from the rows' last_seen
	if _, err := newTorrentScanner(e, f, 0).Scan(ctx, ti, season1(1)); !errors.Is(err, ErrUnchanged) || len(f.reqs) != 1 {
		t.Fatalf("after restart: err %v, %d searches", err, len(f.reqs))
	}
	e.now = e.now.Add(2 * time.Hour)
	if got, err := sc.Scan(ctx, ti, season1(1)); err != nil || len(got) != 1 || len(f.reqs) != 2 {
		t.Fatalf("after a day: %v %v, %d searches", got, err, len(f.reqs))
	}

	// a title with nothing found is not searched again within the day either
	f.releases = nil
	other, err := e.track.EnsureTitle(ctx, "series", 200, "Other", "")
	if err != nil {
		t.Fatal(err)
	}
	ot := tracking.Title{ID: other.ID, Kind: "series", Name: "Other"}
	if got, err := sc.Scan(ctx, ot, season1(1)); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	if _, err := sc.Scan(ctx, ot, season1(1)); !errors.Is(err, ErrUnchanged) || len(f.reqs) != 3 {
		t.Fatalf("empty title: err %v, %d searches", err, len(f.reqs))
	}
}

func TestTorrentScannerKeepsOlderSeasons(t *testing.T) {
	ctx := context.Background()
	eps := []tracking.Episode{{Season: 1, Number: 1, AirDate: "2025-01-01"}, {Season: 2, Number: 1, AirDate: "2026-10-01"},
		{Season: 2, Number: 2, AirDate: "2026-10-08"}}

	// torrent: season 2 is searched; the season-1 row stays, an old
	// season-2 row the search no longer finds goes
	e := newEnv(t)
	ti := tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}
	prev := []tracking.Source{{Season: 1, Episode: 1, Ref: "https://tracker.test/dl/s1", Provider: "Tracker", Quality: "720p"},
		{Season: 2, Episode: 1, Ref: "https://tracker.test/dl/gone", Provider: "Tracker"}}
	e.track.UpsertSources(ti.ID, "torrent", prev, e.now.Add(-48*time.Hour))
	fs := &fakeSearcher{releases: []indexer.ScoredRelease{rel("Show.S02.1080p.WEB-DL-GRP", "https://tracker.test/dl/pack", 5)}}
	got, err := newTorrentScanner(e, fs, 0).Scan(ctx, ti, eps)
	if err != nil {
		t.Fatal(err)
	}
	if want := "S2E1 Tracker https://tracker.test/dl/pack 1080p []; S2E2 Tracker https://tracker.test/dl/pack 1080p []; " +
		"S1E1 Tracker https://tracker.test/dl/s1 720p []"; srcKeys(got) != want {
		t.Fatalf("torrent\n got %s\nwant %s", srcKeys(got), want)
	}
}

func TestTorrentScannerSuggestsForBracketGroup(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	var files []string
	for i := 1; i <= 7; i++ {
		files = append(files, fmt.Sprintf("[Group] Show - %02d (1080p).mkv", i))
	}
	e.eng.magnets[pinMagnet] = metafile(t, "[Group] Show (01-07) (1080p)", files...)
	if _, err := e.svc.PinOwn(ctx, e.title.ID, pinMagnet, nil, by(e.users[0])); err != nil {
		t.Fatal(err)
	}
	f := &fakeSearcher{releases: []indexer.ScoredRelease{
		rel("[Other] Show - 01-09 [1080p]", "https://tracker.test/dl/other", 30), // another group
		rel("[Group] Show - 01-08 [720p]", "https://tracker.test/dl/720", 20),    // another resolution
		rel("[Group] Show - 01-08 [1080p]", "https://tracker.test/dl/8", 10),
	}}
	got, err := newTorrentScanner(e, f, 0).Scan(ctx, tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}, season1(9))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 9+8+8 {
		t.Fatalf("sources %s", srcKeys(got))
	}
	p, _, _ := e.svc.ByTitle(ctx, e.title.ID)
	if p.Suggestion == nil || p.Suggestion.Link != "https://tracker.test/dl/8" || p.Suggestion.Episodes != 8 || len(p.Files) != 7 {
		t.Fatalf("pin %+v, suggestion %+v", p, p.Suggestion)
	}
}

func TestTorrentScannerNoSuggestionAcrossSeasons(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	pinMagnetSeven(t, e, "Show.S01.1080p.WEB-DL-GRP")
	eps := season1(8)
	for i := 1; i <= 8; i++ {
		eps = append(eps, tracking.Episode{Season: 2, Number: i, AirDate: "2026-10-01"})
	}
	// the same family, 8 > 7 episodes, but of season 2: no update of S01
	f := &fakeSearcher{releases: []indexer.ScoredRelease{rel("Show.S02E01-08.1080p.WEB-DL-GRP", "https://tracker.test/dl/s2", 10)}}
	if _, err := newTorrentScanner(e, f, 0).Scan(ctx, tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}, eps); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := e.svc.ByTitle(ctx, e.title.ID); got.Suggestion != nil {
		t.Fatalf("suggestion %+v", got.Suggestion)
	}
}

func TestTorrentScannerTopSkipsLinklessAndUnlisted(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	f := &fakeSearcher{releases: []indexer.ScoredRelease{
		rel("Show.S01E01.1080p-GRP", "", 9), // no link nor magnet: not one of the top
		rel("[Group] Show - 03-30 [1080p]", "https://tracker.test/dl/abs", 5),
	}}
	got, err := newTorrentScanner(e, f, 1).Scan(ctx, tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}, season1(4))
	if err != nil {
		t.Fatal(err)
	}
	// absolute numbers past the season's list are left out
	if want := "S1E3 Tracker https://tracker.test/dl/abs 1080p []; S1E4 Tracker https://tracker.test/dl/abs 1080p []"; srcKeys(got) != want {
		t.Fatalf("sources\n got %s\nwant %s", srcKeys(got), want)
	}
}

// Searches are spaced by the scanner's pause (10 s by default, as follow
// spaces its own), across titles: a round of due titles never sends them
// back to back. A ctx ending during the wait ends the scan.
func TestTorrentScannerSpacesSearches(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	if d := NewTorrentScanner(nil, e.svc, 0).(*torrentScanner).pause; d != 10*time.Second {
		t.Fatalf("default pause %v", d)
	}
	f := &fakeSearcher{}
	sc := newTorrentScanner(e, f, 0)
	sc.pause = 300 * time.Millisecond
	other, err := e.track.EnsureTitle(ctx, "series", 101, "Other", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, ti := range []tracking.Title{{ID: e.title.ID, Kind: "series", Name: "Show"}, {ID: other.ID, Kind: "series", Name: "Other"}} {
		if _, err := sc.Scan(ctx, ti, season1(1)); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.at) != 2 || f.at[1].Sub(f.at[0]) < 300*time.Millisecond {
		t.Fatalf("searches at %v", f.at)
	}
	third, _ := e.track.EnsureTitle(ctx, "series", 102, "Third", "")
	sc.pause = time.Hour
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := sc.Scan(cctx, tracking.Title{ID: third.ID, Kind: "series", Name: "Third"}, season1(1)); !errors.Is(err, context.DeadlineExceeded) || len(f.at) != 2 {
		t.Fatalf("err %v, %d searches", err, len(f.at))
	}
}

func srcKeys(srcs []tracking.Source) string {
	var out []string
	for _, s := range srcs {
		out = append(out, fmt.Sprintf("S%dE%d %s %s %s %v", s.Season, s.Episode, s.Provider, s.Ref, s.Quality, s.Labels))
	}
	return strings.Join(out, "; ")
}

// TestTorrentScanner_Unchanged: a title searched within the day answers
// the addon's own ErrUnchanged (the hook maps it to Unchanged), read from
// the last seen of the torrent rows the core holds; the scanner is the
// addon's Scanner.
func TestTorrentScanner_Unchanged(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	var sc Scanner = newTorrentScanner(e, &fakeSearcher{}, 0)
	ti := tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}
	e.track.UpsertSources(ti.ID, "torrent", []tracking.Source{{Season: 1, Episode: 1, Ref: "https://tracker.test/dl/1"}}, e.now.Add(-time.Hour))
	if _, err := sc.Scan(ctx, ti, season1(1)); !errors.Is(err, ErrUnchanged) {
		t.Fatalf("searched within the day: %v", err)
	}
	// the core unreachable: an error, not Unchanged
	e.track.Err = errors.New("core down")
	if _, err := sc.Scan(ctx, ti, season1(1)); err == nil || errors.Is(err, ErrUnchanged) {
		t.Fatalf("core down: %v", err)
	}
}
