package acquire

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol/addonkit"
)

// seriesFixture fills the library with a series: its episode files'
// audio (SeriesAudio) and the voices profiles chose.
type seriesFixture struct {
	t      *testing.T
	e      *env
	tmdbID int64
	files  int
}

func newSeriesFixture(e *env, tmdbID int) *seriesFixture {
	e.t.Helper()
	if e.lib.Audio == nil {
		e.lib.Audio = map[int64]addonkit.SeriesAudio{}
	}
	return &seriesFixture{t: e.t, e: e, tmdbID: int64(tmdbID)}
}

func (f *seriesFixture) exec(q string, args ...any) int64 {
	f.t.Helper()
	res, err := f.e.svc.o.DB.Exec(q, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// file adds an episode file of the series tmdbID whose audio tracks are
// titled titles (the core lists them by season, episode, file).
func (f *seriesFixture) file(tmdbID int64, season, ep int, titles ...string) {
	sa := f.e.lib.Audio[tmdbID]
	sa.Files = append(sa.Files, addonkit.EpisodeAudio{Season: season, Episode: ep, Titles: append([]string{}, titles...)})
	sort.SliceStable(sa.Files, func(a, b int) bool {
		if sa.Files[a].Season != sa.Files[b].Season {
			return sa.Files[a].Season < sa.Files[b].Season
		}
		return sa.Files[a].Episode < sa.Files[b].Episode
	})
	f.e.lib.Audio[tmdbID] = sa
	f.files++
}

// episodes adds n episode files of a season whose audio tracks are titled
// titles.
func (f *seriesFixture) episodes(season, n int, titles ...string) {
	f.t.Helper()
	for ep := 1; ep <= n; ep++ {
		f.file(f.tmdbID, season, ep, titles...)
	}
}

// chose records a profile's voice for the series (the latest first).
func (f *seriesFixture) chose(userID int64, title string) {
	sa := f.e.lib.Audio[f.tmdbID]
	sa.Profiles = append(sa.Profiles, addonkit.ProfileVoice{UserID: userID, AudioTitle: title})
	f.e.lib.Audio[f.tmdbID] = sa
}

func (f *seriesFixture) acquisition(ih string, tmdbID, season int, releaseTitle, created string) {
	f.t.Helper()
	rel, _ := json.Marshal(map[string]any{"title": releaseTitle})
	f.exec(`INSERT INTO torrents (infohash, name, mode, save_dir, release_json) VALUES (?, 'x', 'download', '/t', ?)`, ih, string(rel))
	f.exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, created_at) VALUES (?, 'series', ?, 'Миллиарды', ?, ?)`, ih, tmdbID, season, created)
}

func TestSeriesStudiosFromFiles(t *testing.T) {
	e := newEnv(t)
	f := newSeriesFixture(e, 1405)
	f.episodes(1, 3, "LostFilm", "English")
	f.episodes(2, 2, "MVO | Кураж-Бамбей", "Original")
	// an acquisition of another studio counts after the files
	f.acquisition("aa", 1405, 3, "Миллиарды / Billions S03 1080p | Jaskier", "2026-10-01T00:00:00Z")
	got := e.svc.seriesStudios(context.Background(), 1405)
	if want := []string{"Кураж-Бамбей", "LostFilm", "Jaskier"}; !slices.Equal(got, want) {
		t.Fatalf("studios %v, want %v", got, want)
	}
}

// Episodes under the series itself (no season items) count as well as
// those under its seasons; only the latest two seasons with files are read.
func TestSeriesStudiosEpisodesDirectAndUnderSeasons(t *testing.T) {
	e := newEnv(t)
	f := newSeriesFixture(e, 1405)
	// season 3 lies directly under the series (no season item)
	for ep := 1; ep <= 2; ep++ {
		f.file(1405, 3, ep, "Кураж-Бамбей")
	}
	f.episodes(2, 2, "LostFilm")
	f.episodes(1, 3, "NewStudio") // the third season back: not read
	// a broken probe row and an episode of another series change nothing
	f.file(1405, 3, 9)
	f.file(999, 4, 1, "Jaskier")
	if got, want := e.svc.seriesStudios(context.Background(), 1405), []string{"Кураж-Бамбей", "LostFilm"}; !slices.Equal(got, want) {
		t.Fatalf("studios %v, want %v", got, want)
	}
}

func TestSeriesStudiosMoreEpisodesFirstWithinASeason(t *testing.T) {
	e := newEnv(t)
	f := newSeriesFixture(e, 1405)
	for i, title := range []string{"NewStudio", "LostFilm", "LostFilm", "LostFilm.TV"} {
		f.file(1405, 1, i+1, title)
	}
	if got, want := e.svc.seriesStudios(context.Background(), 1405), []string{"LostFilm", "NewStudio"}; !slices.Equal(got, want) {
		t.Fatalf("studios %v, want %v", got, want)
	}
}

func TestSeriesStudiosFromAcquisitions(t *testing.T) {
	e := newEnv(t)
	f := newSeriesFixture(e, 1405)
	f.acquisition("aa", 1405, 1, "Миллиарды / Billions S01 1080p | LostFilm", "2026-09-01T00:00:00Z")
	f.acquisition("bb", 1405, 2, "Миллиарды / Billions S02 1080p MVO (NewStudio)", "2026-10-01T00:00:00Z")
	f.acquisition("cc", 999, 1, "Другое S01 1080p | Jaskier", "2026-10-02T00:00:00Z")
	if got, want := e.svc.seriesStudios(context.Background(), 1405), []string{"NewStudio", "LostFilm"}; !slices.Equal(got, want) {
		t.Fatalf("studios %v, want %v", got, want)
	}
}

func TestSeriesStudiosFromProfileChoices(t *testing.T) {
	e := newEnv(t)
	f := newSeriesFixture(e, 1405)
	f.chose(5, "Кубик в кубе")
	if got, want := e.svc.seriesStudios(context.Background(), 1405), []string{"Кубик в Кубе"}; !slices.Equal(got, want) {
		t.Fatalf("studios %v, want %v", got, want)
	}
	// the files' studio comes before a profile's choice
	f.episodes(1, 1, "LostFilm")
	if got, want := e.svc.seriesStudios(context.Background(), 1405), []string{"LostFilm", "Кубик в Кубе"}; !slices.Equal(got, want) {
		t.Fatalf("studios %v, want %v", got, want)
	}
}

func TestSeriesStudiosNone(t *testing.T) {
	e := newEnv(t)
	if got := e.svc.seriesStudios(context.Background(), 1405); got != nil {
		t.Fatalf("unknown series: %v", got)
	}
	f := newSeriesFixture(e, 1405)
	f.episodes(1, 2, "English", "Original")
	if got := e.svc.seriesStudios(context.Background(), 1405); got != nil {
		t.Fatalf("no studio tracks: %v", got)
	}
	// a database that fails to answer: no hint, no panic
	e.svc.o.DB.Close()
	if got := e.svc.seriesStudios(context.Background(), 1405); got != nil {
		t.Fatalf("closed db: %v", got)
	}
}

func TestReleasesPassSeriesStudios(t *testing.T) {
	e := newEnv(t)
	f := newSeriesFixture(e, 1405)
	f.episodes(1, 2, "LostFilm")
	if _, err := e.svc.Releases(context.Background(), ReleaseRequest{Kind: KindSeries, TMDBID: 1405, Season: 2}); err != nil {
		t.Fatal(err)
	}
	if got := e.idx.requests[0].Studios; !slices.Equal(got, []string{"LostFilm"}) {
		t.Fatalf("series request studios %v", got)
	}
	// a series without files: no hint
	if _, err := e.svc.Releases(context.Background(), ReleaseRequest{Kind: KindSeries, TMDBID: 77, Season: 1}); err != nil {
		t.Fatal(err)
	}
	if got := e.idx.requests[1].Studios; got != nil {
		t.Fatalf("new series studios %v", got)
	}
	// a movie never gets one (even with the same TMDB id)
	if _, err := e.svc.Releases(context.Background(), ReleaseRequest{Kind: KindMovie, TMDBID: 1405}); err != nil {
		t.Fatal(err)
	}
	if got := e.idx.requests[2].Studios; got != nil {
		t.Fatalf("movie studios %v", got)
	}
}

func TestAcquireSeriesPassesSeriesStudios(t *testing.T) {
	e := newEnv(t)
	f := newSeriesFixture(e, 1405)
	f.episodes(1, 2, "Кураж-Бамбей")
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Сёгун S02 1080p", "https://t/s2", 1, 50)}}
	if _, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 1405, Kind: KindSeries, Season: 2, Mode: ModeDownload, UserID: 1}); err != nil {
		t.Fatal(err)
	}
	if got := e.idx.requests[0].Studios; !slices.Equal(got, []string{"Кураж-Бамбей"}) {
		t.Fatalf("automatic pick studios %v", got)
	}
	sr, err := e.svc.searchFor(context.Background(), AcquireRequest{TMDBID: 1405, Kind: KindSeries, Season: 2})
	if err != nil || !slices.Equal(sr.Studios, []string{"Кураж-Бамбей"}) {
		t.Fatalf("replacement search studios %v (%v)", sr.Studios, err)
	}
}

// The core's SeriesAudio with two profiles' voices: the files' studios
// first (latest season, more episodes), then the releases', then the
// profiles' choices in the core's order (the latest first).
func TestSeriesStudios_FromSeriesAudio(t *testing.T) {
	e := newEnv(t)
	f := newSeriesFixture(e, 1405)
	f.episodes(1, 3, "LostFilm", "English")
	f.episodes(2, 2, "MVO | Кураж-Бамбей", "Original")
	f.acquisition("aa", 1405, 3, "Миллиарды / Billions S03 1080p | Jaskier", "2026-10-01T00:00:00Z")
	f.chose(5, "Кубик в кубе")
	f.chose(7, "NewStudio")
	f.chose(9, "LostFilm") // already the files': once
	got := e.svc.seriesStudios(context.Background(), 1405)
	if want := []string{"Кураж-Бамбей", "LostFilm", "Jaskier", "Кубик в Кубе", "NewStudio"}; !slices.Equal(got, want) {
		t.Fatalf("studios %v, want %v", got, want)
	}
}
