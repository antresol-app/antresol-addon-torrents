package indexer

import (
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

func TestParseTracks(t *testing.T) {
	for _, c := range []struct {
		name   string
		tracks int
		langs  []string
	}{
		{"Avatar.Extended.2009.BDRip.1080p.Rus", 1, []string{"ru"}},
		{"Аватар / Avatar (2009) BDRip 1080p | 2xRus, Eng", 3, []string{"ru", "en"}},
		{"Avatar.2009.Extended.BDRip.1080p.2xRus.Eng", 3, []string{"ru", "en"}},
		{"Аватар / Avatar [2009, BDRip 1080p] Dub + MVO + Eng + Sub Rus, Eng", 3, []string{"ru", "en"}},
		{"Avatar.2009.1080p.BluRay.DUAL.x264", 2, nil},
		{"Avatar 2009 1080p BluRay Dual Audio", 2, nil},
		{"Avatar.2009.MULTi.1080p.BluRay.x264", 2, nil},
		{"Аватар (2009) BDRip 1080p | Rus, Eng", 2, []string{"ru", "en"}},
		{"Аватар (2009) BDRip 1080p | RUS/ENG", 2, []string{"ru", "en"}},
		{"Аватар (2009) BDRip 1080p | D | Sub Eng", 1, []string{"ru"}},
		{"Аватар (2009) BDRip 1080p | Дубляж | Original", 2, []string{"ru", "orig"}},
		{"Аватар (2009) BDRip 1080p | 3 x MVO, Ukr", 4, []string{"ru", "uk"}},
		// a title word is not an audio language
		{"Английский пациент / The English Patient (1996) BDRip 1080p | MVO", 1, []string{"ru"}},
		{"Avatar.2009.1080p.WEB-DL", 0, nil},
	} {
		got := parseTracks(c.name, release.Parse(c.name))
		if got.Tracks != c.tracks || !slices.Equal(got.Langs, c.langs) {
			t.Errorf("%q: %+v, want %d tracks %v", c.name, got, c.tracks, c.langs)
		}
	}
}

func TestPageTracks(t *testing.T) {
	page := `Качество: BDRip 1080p
Аудио 1: Русский (AC3, 6 ch, 448 Kbps) Дублированный
Аудио 2: Русский (AC3, 6 ch) Многоголосый закадровый
Аудио #3: English (DTS, 6 ch, 1536 Kbps)
Субтитры: русские (полные, форсированные), английские`
	tr, subs := pageTracks(page)
	if tr.Tracks != 3 || !slices.Equal(tr.Langs, []string{"ru", "en"}) || !slices.Equal(subs, []string{"ru", "en"}) {
		t.Fatalf("tracks %+v subs %v", tr, subs)
	}
	if _, subs := pageTracks("Аудио 1: Русский\nСубтитры: нет"); subs != nil {
		t.Fatalf("«Субтитры: нет» read as subtitles: %v", subs)
	}
	if _, subs := pageTracks("Субтитры: есть"); !slices.Equal(subs, []string{"sub"}) {
		t.Fatalf("subtitles of no named language: %v", subs)
	}
	// the topic keeps them
	if got := parseTopic(page); got.Tracks.Tracks != 3 || got.empty() || !slices.Equal(got.Subs, []string{"ru", "en"}) {
		t.Fatalf("topic %+v", got)
	}
}

func TestExtrasScore(t *testing.T) {
	r := Result{Seeders: 30}
	ru := Wanted{SubLangs: []string{"ru", "en"}}
	for _, c := range []struct {
		name   string
		tracks int
		subs   []string
		want   Wanted
		seeds  int
		bonus  int
	}{
		{"one track, no subs", 1, nil, ru, 30, 0},
		{"two tracks", 2, nil, ru, 30, 6},
		{"three tracks", 3, nil, ru, 30, 12},
		{"five tracks: capped", 5, nil, ru, 30, 15},
		{"subs of the profile", 1, []string{"en"}, ru, 30, 8},
		{"subs of another language", 1, []string{"de"}, ru, 30, 0},
		{"tracks and subs", 3, []string{"ru"}, ru, 30, 20},
		// a stream with few seeders: the extras count in proportion
		{"stream, 5 seeders", 3, []string{"ru"}, Wanted{SubLangs: ru.SubLangs, Stream: true}, 5, 10},
		{"stream, 0 seeders", 3, []string{"ru"}, Wanted{SubLangs: ru.SubLangs, Stream: true}, 0, 0},
		{"stream, 40 seeders", 3, []string{"ru"}, Wanted{SubLangs: ru.SubLangs, Stream: true}, 40, 20},
	} {
		r.Seeders = c.seeds
		got, why := extrasScore(r, release.Info{Subs: c.subs}, tracksInfo{Tracks: c.tracks}, c.want)
		if got != c.bonus {
			t.Errorf("%s: %d (%v), want %d", c.name, got, why, c.bonus)
		}
	}
	_, why := extrasScore(Result{Seeders: 30}, release.Info{Subs: []string{"en", "ru"}}, tracksInfo{Tracks: 3}, ru)
	if !slices.Equal(why, []string{"звуковых дорожек: 3", "субтитры: ru, en"}) {
		t.Fatalf("reasons %v", why)
	}
}

// 200 seeders start a stream no better than 50.
func TestSeedersSaturate(t *testing.T) {
	at := func(n int) int { s, _ := seedersScore(Result{Seeders: n}, 0, nil); return s }
	if at(200) != at(50) || at(5000) != at(50) || at(50) <= at(10) || at(10) <= at(1) {
		t.Fatalf("1:%d 10:%d 50:%d 200:%d", at(1), at(10), at(50), at(200))
	}
}

// The case that started it: «Смотреть сейчас» of Avatar picked a
// Russian-only BDRip with the most seeders over releases with the original
// track and subtitles. Now a release with more tracks and the profile's
// subtitles wins while it has enough seeders; a far better-seeded one does
// not beat it by seeders alone.
func TestRankingPrefersMultiTrack(t *testing.T) {
	want := Wanted{Titles: []string{"Аватар", "Avatar", "Avatar Extended"}, Year: 2009, Kind: "movie", SubLangs: []string{"ru", "en"}, Stream: true}
	p := DefaultPrefs()
	type cand struct {
		title string
		seeds int
	}
	cands := []cand{
		{"Avatar.Extended.2009.BDRip.1080p.Rus", 200},
		{"Аватар / Avatar (2009) BDRip 1080p | Dub, Eng | Sub Rus, Eng", 45},
		{"Аватар / Avatar (2009) BDRip 1080p | 2xRus, Eng", 60},
		{"Аватар / Avatar (2009) BDRip 1080p | 2xRus, Eng | Sub Rus, Eng", 2}, // too few to start
	}
	type scored struct {
		title string
		seeds int
		s     int
		why   []string
	}
	var all []scored
	for _, c := range cands {
		r := Result{Title: c.title, Seeders: c.seeds, Size: 10 * gb, Magnet: "magnet:?xt=urn:btih:x"}
		info := release.Parse(c.title)
		s, why := Score(r, info, want, p)
		if s == Excluded {
			t.Fatalf("%q excluded: %v", c.title, why)
		}
		b, more := extrasScore(r, info, parseTracks(c.title, info), want)
		all = append(all, scored{c.title, c.seeds, s + b, append(why, more...)})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].s > all[j].s })
	if all[0].seeds != 45 || !strings.Contains(all[0].title, "Sub Rus, Eng") {
		t.Fatalf("best %+v\nranking %+v", all[0], all)
	}
	rank := map[int]int{} // seeders → place
	for i, a := range all {
		rank[a.seeds] = i
	}
	// the 2-seeder's extras hardly count: the same tracks with 60 seeders win
	if rank[2] < rank[60] {
		t.Fatalf("a 2-seeder multi-track release ranked above a well-seeded one: %+v", all)
	}
	// the Russian-only release is behind the multi-track ones with enough seeders
	if rank[200] < rank[60] || rank[200] < rank[45] {
		t.Fatalf("the Russian-only release by seeders alone: %+v", all)
	}
}
