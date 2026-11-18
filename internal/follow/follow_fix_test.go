package follow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
)

func (e *env) check(id int64) CheckResult {
	e.t.Helper()
	res, err := e.svc.Check(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// A file "S01E01-E02" is one library episode; both count as had.
func TestMultiEpisodeFileCounts(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	for i, name := range []string{"Сёгун - S01E01-E02.mkv", "Сёгун - S01E03.mkv", "Сёгун - S01E04.mkv", "Сёгун - S01E05.mkv",
		"Сёгун - S01E06.mkv", "Сёгун - S01E07.mkv", "Сёгун - S01E08.mkv", "Сёгун - S01E09.mkv"} {
		ep := i + 1
		if i > 0 {
			ep = i + 2
		}
		e.episodeFile(42, 1, ep, filepath.Join(dir, name))
	}
	f := e.add("series", 42)
	if res := e.check(f.ID); !reflect.DeepEqual(res.Queued, []string{"S01E10"}) || res.Missing != 1 {
		t.Fatalf("%+v %v", res, e.acq.requests())
	}
}

// A pending range release covers all its episodes, not only the one asked for.
func TestPendingRangeCovers(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2)
	e.acq.pending = []acquire.PendingAcquisition{{Kind: "series", TMDBID: 42, Season: 1, Episode: 3, EpisodeEnd: 9, InfoHash: "x"}}
	f := e.add("series", 42)
	if res := e.check(f.ID); !reflect.DeepEqual(res.Queued, []string{"S01E10"}) {
		t.Fatalf("%+v %v", res, e.acq.requests())
	}
}

// Downloaded (queued, no longer pending) yet still missing: wait 7 days.
func TestQueuedButGoneDeferredWeek(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	f := e.add("series", 42)
	if res := e.check(f.ID); !reflect.DeepEqual(res.Queued, []string{"S01E10"}) {
		t.Fatalf("%+v", res)
	}
	e.now = e.now.Add(2 * time.Hour) // organized, but E10 did not show up
	if res := e.check(f.ID); len(res.Queued) != 0 || len(e.acq.requests()) != 1 {
		t.Fatalf("searched again: %+v %v", res, e.acq.requests())
	}
	if g := e.get(f.ID); !strings.Contains(g.LastError, "не появилось в медиатеке") {
		t.Fatalf("note %q", g.LastError)
	}
	e.now = e.now.Add(6 * 24 * time.Hour)
	if e.check(f.ID); len(e.acq.requests()) != 1 {
		t.Fatal("retried within a week")
	}
	e.now = e.now.Add(25 * time.Hour)
	if res := e.check(f.ID); !reflect.DeepEqual(res.Queued, []string{"S01E10"}) || len(e.acq.requests()) != 2 {
		t.Fatalf("not retried after a week: %+v", res)
	}
}

// An unmatched library title blocks only until the enricher tried it.
func TestUnmatchedBlocksOnlyUntilEnricherTried(t *testing.T) {
	e := newEnv(t)
	e.episodes(0, 1, 1)
	f := e.add("series", 42)
	if e.check(f.ID); len(e.acq.requests()) != 0 {
		t.Fatal("not blocked")
	}
	for i, it := range e.lib.Items {
		if it.Kind == library.KindSeries {
			e.lib.Items[i].MetaJSON = `{"tmdb_at":"2024-05-01T00:00:00Z","tmdb_miss":true}`
		}
	}
	if res := e.check(f.ID); !reflect.DeepEqual(res.Queued, []string{"S01"}) {
		t.Fatalf("still blocked: %+v", res)
	}
	if g := e.get(f.ID); !strings.Contains(g.LastError, "без данных TMDB") {
		t.Fatalf("no note: %q", g.LastError)
	}
}

func TestKickOnAddChecksAtOnce(t *testing.T) {
	old := startDelay
	startDelay = time.Hour
	t.Cleanup(func() { startDelay = old })
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	ch, stop := e.bus.Subscribe()
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.svc.Run(ctx) }()
	defer func() { cancel(); <-done }()
	f := e.add("series", 42)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if c, ok := ev.Data.(Checked); ok && ev.Type == EventFollowChecked {
				if c.FollowID != f.ID || !reflect.DeepEqual(c.Queued, []string{"S01E10"}) || len(e.acq.requests()) != 1 {
					t.Fatalf("%+v", c)
				}
				// adding again is no new follow: no second check
				e.add("series", 42)
				e.svc.kickMu.Lock()
				n := len(e.svc.kicked)
				e.svc.kickMu.Unlock()
				if n != 0 {
					t.Fatalf("kicked again: %d", n)
				}
				return
			}
		case <-deadline:
			t.Fatal("not checked after add")
		}
	}
}

func TestFreshEpisodeRetriedSooner(t *testing.T) {
	e := newEnv(t)
	s1 := season(1, 9, 0)
	s1.Episodes = append(s1.Episodes, tmdb.Episode{Number: 10, AirDate: today.Format("2006-01-02")})
	e.tm.seasons[1] = s1
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	e.acq.err = acquire.ErrNoRelease
	f := e.add("series", 42)
	e.check(f.ID)
	e.now = e.now.Add(7 * time.Hour)
	if e.check(f.ID); len(e.acq.requests()) != 2 {
		t.Fatalf("fresh episode not retried after 7 h: %v", e.acq.requests())
	}
}

func TestSourcesFailedNoBackoff(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	e.acq.err = fmt.Errorf("%w: %w", acquire.ErrNoRelease, acquire.ErrSourcesFailed)
	f := e.add("series", 42)
	e.check(f.ID)
	e.now = e.now.Add(time.Hour)
	if e.check(f.ID); len(e.acq.requests()) != 2 {
		t.Fatalf("backed off on a source outage: %v", e.acq.requests())
	}
}

func TestOneFailureDoesNotStopOthers(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7)
	e.acq.errFor = map[[2]int]error{{1, 8}: errors.New("Не удалось открыть раздачу")}
	f := e.add("series", 42)
	res := e.check(f.ID)
	if !reflect.DeepEqual(res.Queued, []string{"S01E09", "S01E10"}) || len(e.acq.requests()) != 3 {
		t.Fatalf("%+v %v", res, e.acq.requests())
	}
	if g := e.get(f.ID); !strings.Contains(g.LastError, "S01E08: Не удалось открыть раздачу") {
		t.Fatalf("note %q", g.LastError)
	}
}

func TestCapPerCheck(t *testing.T) {
	e := newEnv(t)
	e.tm.tv.Seasons = []tmdb.SeasonCount{{Number: 1, Episodes: 12}}
	e.tm.seasons[1] = season(1, 8, 4) // airing: per episode
	s := e.tm.seasons[1]
	for i := range s.Episodes {
		s.Episodes[i].AirDate = "2024-01-01"
	}
	s.Episodes = append(s.Episodes, tmdb.Episode{Number: 13, AirDate: "2025-01-01"}) // not aired: no pack
	e.tm.seasons[1] = s
	f := e.add("series", 42)
	res := e.check(f.ID)
	if len(res.Queued) != maxPerCheck || res.Missing != 12 {
		t.Fatalf("%+v", res)
	}
	if g := e.get(f.ID); !strings.Contains(g.LastError, "Ещё 2 — в следующей проверке") {
		t.Fatalf("note %q", g.LastError)
	}
}

func TestPauseSpacesSearches(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7)
	e.svc.Pause = 50 * time.Millisecond
	f := e.add("series", 42)
	start := time.Now()
	e.check(f.ID)
	if d := time.Since(start); d < 100*time.Millisecond || len(e.acq.requests()) != 3 {
		t.Fatalf("3 searches in %v", d)
	}
}

func TestChecksSerialized(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	e.acq.delay = 20 * time.Millisecond
	a := e.add("series", 42)
	b := e.add("series", 43)
	var wg sync.WaitGroup
	for _, id := range []int64{a.ID, b.ID, a.ID} {
		wg.Add(1)
		go func() { defer wg.Done(); e.svc.Check(context.Background(), id) }()
	}
	wg.Wait()
	if e.acq.maxActive != 1 {
		t.Fatalf("%d checks at once", e.acq.maxActive)
	}
}

func TestTMDBErrorForOneDoesNotEndRound(t *testing.T) {
	e := newEnv(t)
	e.episodes(43, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	bad := e.add("series", 42)
	good := e.add("series", 43)
	e.tm.tvErr = map[int]error{42: fmt.Errorf("tmdb /tv/42: %w", &tmdb.StatusError{Code: 502})}
	if err := e.svc.CheckAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g := e.get(good.ID); g.LastChecked == nil || len(e.acq.requests()) != 1 {
		t.Fatalf("round ended at a 502: %+v", g)
	}
	if g := e.get(bad.ID); g.LastError == "" || g.LastError == msgOffline {
		t.Fatalf("bad %+v", g)
	}
}

func TestDiskErrorBlocks(t *testing.T) {
	e := newEnv(t)
	e.acq.diskErr = errors.New("statfs: input/output error")
	f := e.add("series", 42)
	if e.check(f.ID); len(e.acq.requests()) != 0 || e.get(f.ID).LastError == "" {
		t.Fatal("queued with an unreadable disk")
	}
}

func TestBlockedKeepsMissing(t *testing.T) {
	e := newEnv(t)
	f := e.add("series", 42)
	e.acq.disk = acquire.DiskUsage{Level: acquire.LevelFull}
	e.check(f.ID) // missing 10 counted, nothing queued
	e.episodes(0, 1, 1)
	if res := e.check(f.ID); res.Missing != 10 || e.get(f.ID).Missing != 10 {
		t.Fatalf("missing reset: %+v", res)
	}
}

// Kicks coalesce per follow and none is dropped: 20 kicks of one follow
// give one check; 40 new follows are all checked.
func TestKicksCoalesce(t *testing.T) {
	old := startDelay
	startDelay = time.Hour
	t.Cleanup(func() { startDelay = old })
	e := newEnv(t)
	e.tm.movie.ReleaseDate = "2030-01-01" // not out: a check queues nothing
	ch, stop := e.bus.Subscribe()
	defer stop()
	var ids []int64
	for i := 1; i <= 40; i++ {
		ids = append(ids, e.add("movie", i).ID) // each add kicks
	}
	for range 20 {
		e.svc.Kick(ids[0])
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.svc.Run(ctx) }()
	defer func() { cancel(); <-done }()
	checks := map[int64]int{}
	total := 0
	deadline := time.After(10 * time.Second)
	quiet := time.NewTimer(time.Hour)
	for {
		select {
		case ev := <-ch:
			if c, ok := ev.Data.(Checked); ok && ev.Type == EventFollowChecked {
				checks[c.FollowID]++
				total++
				if total == len(ids) {
					quiet.Reset(300 * time.Millisecond) // nothing more may come
				}
			}
		case <-quiet.C:
			for _, id := range ids {
				if checks[id] != 1 {
					t.Fatalf("follow %d checked %d times (%v)", id, checks[id], checks)
				}
			}
			return
		case <-deadline:
			t.Fatalf("checked %d of %d", total, len(ids))
		}
	}
}

// the unmatched check asks the core's title search once per
// normalised title (title, original title, aliases) and keeps only
// top-level items with no TMDB id within a year of the title's; an item
// not looked up yet wins over one TMDB found nothing for.
func TestUnmatchedOverTitleSearch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		items   []library.Item
		blocked bool
	}{
		{"original title, a year off", []library.Item{{Kind: "series", OriginalTitle: "Shōgun", Year: 2025}}, true},
		{"two years off", []library.Item{{Kind: "series", Title: "Сёгун", Year: 2022}}, false},
		{"no year", []library.Item{{Kind: "series", Title: "Сёгун"}}, true},
		{"matched to TMDB", []library.Item{{Kind: "series", Title: "Сёгун", TmdbID: 7}}, false},
		{"another kind", []library.Item{{Kind: "movie", Title: "Сёгун"}}, false},
		{"tried, then untried", []library.Item{{Kind: "series", Title: "Сёгун", MetaJSON: `{"tmdb_at":"2024-05-01T00:00:00Z"}`},
			{Kind: "series", OriginalTitle: "Shōgun"}}, true},
		{"tried only", []library.Item{{Kind: "series", Title: "Сёгун", MetaJSON: `{"tmdb_at":"2024-05-01T00:00:00Z"}`}}, false},
	} {
		e := newEnv(t)
		for _, it := range tc.items {
			e.item(it)
		}
		f := e.add("series", 42)
		e.check(f.ID)
		if blocked := len(e.acq.requests()) == 0; blocked != tc.blocked {
			t.Errorf("%s: blocked %v, note %q", tc.name, blocked, e.get(f.ID).LastError)
		}
	}
}

// threeSeasonShow: TMDB has seasons 1–3 of 10 aired episodes each.
func (e *env) threeSeasonShow() {
	e.tm.tv.Seasons = []tmdb.SeasonCount{{Number: 1, Episodes: 10}, {Number: 2, Episodes: 10}, {Number: 3, Episodes: 10}}
	e.tm.seasons = map[int]tmdb.SeasonDetails{1: season(1, 10, 0), 2: season(2, 10, 0), 3: season(3, 10, 0)}
}

// A pack of S01–S03 downloading (Pending: each episode its files hold,
// the S02 files lacking E05) leaves the follow nothing but S02E05.
func TestPendingMultiSeasonPackCovers(t *testing.T) {
	e := newEnv(t)
	e.threeSeasonShow()
	for s := 1; s <= 3; s++ {
		for ep := 1; ep <= 10; ep++ {
			if s == 2 && ep == 5 {
				continue
			}
			e.acq.pending = append(e.acq.pending, acquire.PendingAcquisition{Kind: "series", TMDBID: 42, Season: s, Episode: ep, EpisodeEnd: ep, InfoHash: "pk"})
		}
	}
	f := e.add("series", 42)
	if res := e.check(f.ID); !reflect.DeepEqual(res.Queued, []string{"S02E05"}) || res.Missing != 1 {
		t.Fatalf("%+v %v", res, e.acq.requests())
	}
}

// Before its file list, a pack of seasons (one row per season) and a
// whole-series download (season 0) cover their seasons.
func TestPendingSeasonsBeforeFilesCover(t *testing.T) {
	for name, pending := range map[string][]acquire.PendingAcquisition{
		"range": {{Kind: "series", TMDBID: 42, Season: 1, InfoHash: "pk"}, {Kind: "series", TMDBID: 42, Season: 2, InfoHash: "pk"},
			{Kind: "series", TMDBID: 42, Season: 3, InfoHash: "pk"}},
		"whole": {{Kind: "series", TMDBID: 42, InfoHash: "pk"}},
	} {
		e := newEnv(t)
		e.threeSeasonShow()
		e.acq.pending = pending
		f := e.add("series", 42)
		if res := e.check(f.ID); len(res.Queued) != 0 || len(e.acq.requests()) != 0 {
			t.Fatalf("%s: %+v %v", name, res, e.acq.requests())
		}
	}
}

// Pack files numbered absolutely (S02E11–E20 for a 10-episode season) do
// not make the season look complete: its real episodes are still looked
// for.
func TestPendingMisnumberedPackNotComplete(t *testing.T) {
	e := newEnv(t)
	e.threeSeasonShow()
	for s := 1; s <= 3; s++ {
		for ep := 1; ep <= 10; ep++ {
			if s == 2 {
				ep += 10
			}
			e.acq.pending = append(e.acq.pending, acquire.PendingAcquisition{Kind: "series", TMDBID: 42, Season: s, Episode: ep, EpisodeEnd: ep, InfoHash: "pk"})
			if s == 2 {
				ep -= 10
			}
		}
	}
	f := e.add("series", 42)
	e.check(f.ID)
	if len(e.acq.requests()) == 0 {
		t.Fatal("season 2 counted as complete from mis-numbered pack files")
	}
}
