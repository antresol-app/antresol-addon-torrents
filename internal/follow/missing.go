package follow

import (
	"fmt"
	"sort"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
)

// Target is something to download: one episode, or a whole season
// (Episode 0).
type Target struct {
	Season, Episode int
}

// label names a target in CheckResult.Queued and state_json.tried:
// "S01E08", "S02".
func (t Target) label() string {
	if t.Episode == 0 {
		return fmt.Sprintf("S%02d", t.Season)
	}
	return fmt.Sprintf("S%02dE%02d", t.Season, t.Episode)
}

// seasonGap is the aired episodes of one season the library lacks.
type seasonGap struct {
	season   int
	missing  []int
	total    int  // episodes of the season
	complete bool // every episode has aired
}

// gaps lists, per regular season, the episodes that aired by today
// (AirDate ≤ today) and are not in have, nor in covered (the title's
// pinned release holds them: its download gets them). An episode without
// a date has not aired, unless its season began over a year ago (old
// shows often lack episode dates). Specials (season 0) are skipped.
func gaps(seasons []tmdb.SeasonDetails, have map[[2]int]bool, today time.Time, covered map[[2]int]bool) []seasonGap {
	day := today.Format("2006-01-02")
	yearAgo := today.AddDate(-1, 0, 0).Format("2006-01-02")
	var out []seasonGap
	for _, s := range seasons {
		if s.Number <= 0 {
			continue
		}
		g := seasonGap{season: s.Number, complete: true}
		old := s.AirDate != "" && s.AirDate <= yearAgo
		for _, e := range s.Episodes {
			if e.Number <= 0 {
				continue
			}
			g.total++
			if (e.AirDate == "" && !old) || e.AirDate > day {
				g.complete = false
				continue
			}
			if k := [2]int{s.Number, e.Number}; !have[k] && !covered[k] {
				g.missing = append(g.missing, e.Number)
			}
		}
		if len(g.missing) == 0 {
			continue
		}
		sort.Ints(g.missing)
		out = append(out, g)
	}
	return out
}

// missingEpisodes is what to download: a season that has fully aired and
// lacks more than half of its episodes is one target (a season pack, as
// «На сервер» downloads series); otherwise each missing aired episode is
// its own target. Episodes in covered are not missing (see gaps).
func missingEpisodes(seasons []tmdb.SeasonDetails, have map[[2]int]bool, today time.Time, covered map[[2]int]bool) []Target {
	var out []Target
	for _, g := range gaps(seasons, have, today, covered) {
		if g.complete && len(g.missing)*2 > g.total {
			out = append(out, Target{Season: g.season})
			continue
		}
		for _, e := range g.missing {
			out = append(out, Target{Season: g.season, Episode: e})
		}
	}
	return out
}

// missingCount is the number of aired episodes the library lacks (and
// covered does not hold).
func missingCount(seasons []tmdb.SeasonDetails, have map[[2]int]bool, today time.Time, covered map[[2]int]bool) int {
	n := 0
	for _, g := range gaps(seasons, have, today, covered) {
		n += len(g.missing)
	}
	return n
}

// latestAir is the latest air date of a target's episodes ("" = none
// known): when it aired only days ago, a missing release is retried sooner.
func latestAir(seasons []tmdb.SeasonDetails, t Target) string {
	latest := ""
	for _, s := range seasons {
		if s.Number != t.Season {
			continue
		}
		for _, e := range s.Episodes {
			if (t.Episode == 0 || e.Number == t.Episode) && e.AirDate > latest {
				latest = e.AirDate
			}
		}
	}
	return latest
}

// importedHave applies Sonarr's choice to a follow imported from it: an
// episode that aired before the import day and Sonarr was not told to get
// (not in Want) counts as had — it is neither downloaded nor counted
// toward a season pack. An undated episode of a season that began before
// the import day is such an episode too (once gaps counts it as aired, a
// year into its season, it still belongs to Sonarr's time; when TMDB later
// dates it after the import, the date decides). Episodes airing from the
// import day on are followed as usual.
func importedHave(seasons []tmdb.SeasonDetails, have map[[2]int]bool, imp ImportState) {
	want := make(map[[2]int]bool, len(imp.Want))
	for _, k := range imp.Want {
		want[k] = true
	}
	day := imp.Baseline.UTC().Format("2006-01-02")
	for _, s := range seasons {
		began := s.AirDate != "" && s.AirDate < day
		for _, e := range s.Episodes {
			k := [2]int{s.Number, e.Number}
			before := e.AirDate != "" && e.AirDate < day || e.AirDate == "" && began
			if before && !want[k] {
				have[k] = true
			}
		}
	}
}
