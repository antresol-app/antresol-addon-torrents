package follow

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
)

var today = time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

// season builds season n with aired episodes (dates before today) and
// future ones (after today).
func season(n, aired, future int) tmdb.SeasonDetails {
	s := tmdb.SeasonDetails{Number: n}
	for i := 1; i <= aired; i++ {
		s.Episodes = append(s.Episodes, tmdb.Episode{Number: i, AirDate: fmt.Sprintf("2024-01-%02d", i)})
	}
	for i := aired + 1; i <= aired+future; i++ {
		s.Episodes = append(s.Episodes, tmdb.Episode{Number: i, AirDate: fmt.Sprintf("2024-07-%02d", i)})
	}
	return s
}

func haveEps(s int, eps ...int) map[[2]int]bool {
	m := map[[2]int]bool{}
	for _, e := range eps {
		m[[2]int{s, e}] = true
	}
	return m
}

func TestMissingAiredEpisodes(t *testing.T) {
	got := missingEpisodes([]tmdb.SeasonDetails{season(1, 10, 0), season(2, 0, 8)}, haveEps(1, 1, 2, 3, 4, 5, 6, 7), today, nil)
	want := []Target{{1, 8}, {1, 9}, {1, 10}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPrefersSeasonPack(t *testing.T) {
	got := missingEpisodes([]tmdb.SeasonDetails{season(1, 10, 0)}, haveEps(1, 1, 2), today, nil)
	if want := []Target{{1, 0}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNoPackForAiringSeason(t *testing.T) {
	got := missingEpisodes([]tmdb.SeasonDetails{season(2, 4, 4)}, map[[2]int]bool{}, today, nil)
	if want := []Target{{2, 1}, {2, 2}, {2, 3}, {2, 4}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestMissingSkipsSpecialsAndUndated(t *testing.T) {
	sp := season(0, 3, 0)
	undated := tmdb.SeasonDetails{Number: 3, Episodes: []tmdb.Episode{{Number: 1}, {Number: 2, AirDate: "2024-06-01"}}}
	got := missingEpisodes([]tmdb.SeasonDetails{sp, undated}, map[[2]int]bool{}, today, nil)
	// season 3 is not fully aired (episode 1 has no date): no pack; the
	// episode airing today counts
	if want := []Target{{3, 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestUndatedEpisodesOfOldSeasonAired(t *testing.T) {
	old := tmdb.SeasonDetails{Number: 1, AirDate: "2001-09-01", Episodes: []tmdb.Episode{{Number: 1}, {Number: 2}}}
	got := missingEpisodes([]tmdb.SeasonDetails{old}, haveEps(1, 1), today, nil)
	if want := []Target{{1, 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}
