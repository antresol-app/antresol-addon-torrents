package indexer

import (
	"sort"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

func score(title string, seeders int, size int64, want Wanted, p Prefs) (int, []string) {
	r := Result{Title: title, Seeders: seeders, Size: size, Magnet: "magnet:?xt=urn:btih:x"}
	return Score(r, release.Parse(title), want, p)
}

const gb = 1 << 30

func TestScoreExcludesCam(t *testing.T) {
	want := Wanted{Titles: []string{"Оппенгеймер", "Oppenheimer"}, Year: 2023, Kind: "movie"}
	for _, title := range []string{
		"Оппенгеймер (2023) TS",
		"Oppenheimer.2023.HDCAM.x264",
		"Оппенгеймер / Oppenheimer (2023) CAMRip 720p",
	} {
		s, reasons := score(title, 500, 2*gb, want, DefaultPrefs())
		if s != Excluded {
			t.Errorf("%q: score %d (%v), want excluded", title, s, reasons)
		}
		if len(reasons) == 0 {
			t.Errorf("%q: no reason given", title)
		}
	}
	if s, r := score("Оппенгеймер (2023) WEB-DL 1080p", 50, 8*gb, want, DefaultPrefs()); s == Excluded {
		t.Errorf("normal release excluded: %v", r)
	}
}

func TestScorePrefersDub1080(t *testing.T) {
	want := Wanted{Titles: []string{"Дюна: Часть вторая", "Dune: Part Two"}, Year: 2024, Kind: "movie"}
	titles := []string{
		"Дюна: Часть вторая / Dune: Part Two (2024) WEB-DL 2160p HDR | Дубляж",
		"Дюна: Часть вторая / Dune: Part Two (2024) WEB-DL 1080p | Дубляж",
		"Dune: Part Two (2024) WEB-DL 1080p Original",
		"Дюна: Часть вторая (2024) WEB-DL 720p | Дубляж",
	}
	type scored struct {
		title string
		s     int
	}
	var all []scored
	for _, title := range titles {
		s, reasons := score(title, 100, 10*gb, want, DefaultPrefs())
		if s == Excluded {
			t.Fatalf("%q excluded: %v", title, reasons)
		}
		all = append(all, scored{title, s})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].s > all[j].s })
	if all[0].title != titles[1] {
		t.Fatalf("best = %q, ranking %+v", all[0].title, all)
	}
	// with a 4K screen preference the 2160p dub wins
	p := DefaultPrefs()
	p.MaxHeight = 2160
	s4k, _ := score(titles[0], 100, 10*gb, want, p)
	s1080, _ := score(titles[1], 100, 10*gb, want, p)
	if s4k <= s1080 {
		t.Fatalf("MaxHeight 2160: 2160p %d <= 1080p %d", s4k, s1080)
	}
	// more seeders is better at the same quality, but on a log scale
	few, _ := score(titles[1], 2, 10*gb, want, DefaultPrefs())
	many, _ := score(titles[1], 2000, 10*gb, want, DefaultPrefs())
	if many <= few {
		t.Fatalf("seeders: 2000 -> %d, 2 -> %d", many, few)
	}
	if orig, _ := score(titles[2], 2000, 10*gb, want, DefaultPrefs()); orig >= few {
		t.Fatalf("original with many seeders (%d) beat dub with few (%d)", orig, few)
	}
}

func TestScoreMatchesAltTitle(t *testing.T) {
	want := Wanted{Titles: []string{"Сёгун", "Shogun", "Shōgun"}, Season: 1, Episode: 3, Kind: "series"}
	for _, title := range []string{
		"Shogun.S01.1080p.WEB-DL.x265-NTb",
		"Shōgun.S01E03.1080p.WEB-DL",
		"Сёгун / Shōgun / Сезон: 1 / Серии: 1-10 из 10 (2024) WEB-DL 1080p | MVO",
		"Сегун (1 сезон) WEB-DL 1080p",
	} {
		if s, reasons := score(title, 30, 20*gb, want, DefaultPrefs()); s == Excluded {
			t.Errorf("%q excluded: %v", title, reasons)
		}
	}
	for _, title := range []string{
		"Shogun Assassin (1980) BDRip 1080p",
		"Брат 2 (2000) BDRip 1080p",
	} {
		if s, _ := score(title, 30, 4*gb, want, DefaultPrefs()); s != Excluded {
			t.Errorf("%q not excluded", title)
		}
	}
}

func TestScoreRejectsWrongSeason(t *testing.T) {
	want := Wanted{Titles: []string{"Сёгун", "Shogun"}, Season: 1, Episode: 3, Kind: "series"}
	s, reasons := score("Shogun.S02.1080p.WEB-DL.x265-NTb", 30, 20*gb, want, DefaultPrefs())
	if s != Excluded || !strings.Contains(strings.Join(reasons, " "), "сезон") {
		t.Fatalf("S02 for S01E03: score %d %v", s, reasons)
	}
	if s, r := score("Shogun.S01E05.1080p.WEB-DL", 30, 2*gb, want, DefaultPrefs()); s != Excluded {
		t.Fatalf("E05 for S01E03 not excluded: %v", r)
	}
	if s, r := score("Shogun.S01E03.1080p.WEB-DL", 30, 2*gb, want, DefaultPrefs()); s == Excluded {
		t.Fatalf("exact episode excluded: %v", r)
	}
	if s, r := score("Shogun.S01.1080p.WEB-DL", 30, 20*gb, want, DefaultPrefs()); s == Excluded {
		t.Fatalf("season pack excluded: %v", r)
	}
	// "1-10 серии" is a range of episodes, not seasons
	if s, r := score("Сёгун (1 сезон: 1-10 серии из 10) / Shōgun (2024) WEB-DL 1080p | LostFilm", 30, 20*gb, want, DefaultPrefs()); s == Excluded {
		t.Fatalf("rutor season pack excluded: %v", r)
	}
	if s, r := score("Сёгун / Shōgun / Сезоны: 2-3 (2026) WEB-DL 1080p", 30, 20*gb, want, DefaultPrefs()); s != Excluded {
		t.Fatalf("seasons 2-3 for S01 not excluded: %v", r)
	}
	if s, r := score("Сёгун / Shōgun / Сезоны: 1-2 (2024-2026) WEB-DL 1080p", 30, 20*gb, want, DefaultPrefs()); s == Excluded {
		t.Fatalf("seasons 1-2 for S01 excluded: %v", r)
	}
	// a series with the same name that premiered long before
	want.Year = 2024
	if s, _ := score("Shogun (1980) S01 DVDRip", 30, 8*gb, want, DefaultPrefs()); s != Excluded {
		t.Fatal("Shogun (1980) not excluded for the 2024 series")
	}
	if s, r := score("Shogun.2024.S01E03.1080p.WEB-DL", 30, 2*gb, want, DefaultPrefs()); s == Excluded {
		t.Fatalf("2024 episode excluded: %v", r)
	}
}

func TestScoreYearSizeSeeders(t *testing.T) {
	want := Wanted{Titles: []string{"Брат 2"}, Year: 2000, Kind: "movie"}
	if s, _ := score("Брат 2 (2000) BDRip 1080p", 10, 4*gb, want, DefaultPrefs()); s == Excluded {
		t.Fatal("matching movie excluded")
	}
	if s, _ := score("Брат 2 (2012) BDRip 1080p", 10, 4*gb, want, DefaultPrefs()); s != Excluded {
		t.Fatal("wrong year not excluded")
	}
	if s, _ := score("Брат 2 (2000) BDRip 1080p", 0, 4*gb, want, DefaultPrefs()); s != Excluded {
		t.Fatal("0 seeders not excluded with MinSources 1")
	}
	p := DefaultPrefs()
	p.MaxSizeGB = 3
	if s, _ := score("Брат 2 (2000) BDRip 1080p", 10, 4*gb, want, p); s != Excluded {
		t.Fatal("too large not excluded")
	}
}

// «Сезон N на сервер»: one episode or a part of the season is never picked;
// the season pack wins over a complete-series pack that contains it, and the
// movie size limit does not apply.
func TestScoreSeasonRequestNeedsPack(t *testing.T) {
	want := Wanted{Titles: []string{"Сёгун", "Shogun"}, Season: 1, Kind: "series"}
	p := DefaultPrefs()
	p.MaxSizeGB = 10
	for _, title := range []string{
		"Shogun.S01E03.1080p.WEB-DL",
		"Сёгун / Shogun / Сезон: 1 / Серии: 1-4 из 10 (2024) WEB-DL 1080p",
	} {
		if s, reasons := score(title, 3000, 2*gb, want, p); s != Excluded {
			t.Errorf("%q not excluded (%d %v)", title, s, reasons)
		}
	}
	season, r1 := score("Shogun.S01.1080p.WEB-DL", 20, 40*gb, want, p)
	series, r2 := score("Сёгун / Shogun / Сезоны: 1-3 (2024) WEB-DL 1080p", 20, 90*gb, want, p)
	if season == Excluded || series == Excluded {
		t.Fatalf("packs excluded: %v %v", r1, r2)
	}
	if season <= series {
		t.Errorf("season pack %d should beat the complete-series pack %d", season, series)
	}
	// a movie still honours the size limit
	movie := Wanted{Titles: []string{"Дюна"}, Kind: "movie", Year: 2021}
	if s, _ := score("Дюна (2021) BDRip 1080p", 20, 40*gb, movie, p); s != Excluded {
		t.Errorf("movie over the limit not excluded")
	}
}

// favourite studios: the owner's calibration («почти всегда»)
var (
	studioPrefs = Prefs{MaxHeight: 1080, PreferDub: true, MinSources: 1, Studios: []string{"LostFilm", "Кубик в Кубе"}}
	billions    = Wanted{Titles: []string{"Миллиарды"}, Kind: "series", Season: 3}
)

func TestFavouriteStudioCalibration(t *testing.T) {
	want := billions
	want.Studios = studioPrefs.Studios
	sc := func(title string, seeders int) int {
		s, _ := score(title, seeders, 0, want, studioPrefs)
		return s
	}
	// a favourite 720p beats an unlisted 1080p with dozens of seeders
	for _, seeders := range []int{40, 99} {
		if fav, other := sc("Миллиарды S03 WEB-DL 720p MVO | LostFilm", 5), sc("Миллиарды S03 WEB-DL 1080p MVO | Jaskier", seeders); fav <= other {
			t.Errorf("720p LostFilm %d ≤ 1080p Jaskier (%d seeders) %d", fav, seeders, other)
		}
	}
	// the calibration holds for every studio of a long list, not only the first
	long := Prefs{MaxHeight: 1080, PreferDub: true, MinSources: 1, Studios: []string{"LostFilm", "Кубик в Кубе", "HDRezka Studio",
		"NewStudio", "AlexFilm", "TVShows", "Кураж-Бамбей", "Red Head Sound", "BaibaKo", "Pifagor"}}
	longWant := billions
	longWant.Studios = long.Studios
	// the unlisted rivals: a voice-over and a full dub (Dub scores 10 more than MVO)
	for _, rival := range []string{"Миллиарды S03 WEB-DL 1080p MVO | Jaskier", "Миллиарды S03 WEB-DL 1080p Dub | Jaskier"} {
		other, _ := score(rival, 40, 0, longWant, long)
		for rank, studio := range long.Studios {
			title := "Миллиарды S03 WEB-DL 720p MVO | " + studio
			fav, reasons := score(title, 5, 0, longWant, long)
			if fav <= other || !slicesContains(reasons, "любимая озвучка: "+studio) {
				t.Errorf("rank %d: 720p MVO %s %d ≤ %q (40 seeders) %d %v", rank, studio, fav, rival, other, reasons)
			}
			// 0–1 seeders, below 720p or above the wanted quality: even the first favourite loses
			if low, _ := score("Миллиарды S03 WEB-DL 1080p MVO | "+studio, 1, 0, longWant, long); low >= other {
				t.Errorf("rank %d: 1-seeder %s %d ≥ %q %d", rank, studio, low, rival, other)
			}
			if sd, _ := score("Миллиарды S03 WEBRip 480p MVO | "+studio, 40, 0, longWant, long); sd >= other {
				t.Errorf("rank %d: 480p %s %d ≥ %q %d", rank, studio, sd, rival, other)
			}
			if uhd, _ := score("Миллиарды S03 WEB-DL 2160p MVO | "+studio, 40, 0, longWant, long); uhd >= other {
				t.Errorf("rank %d: 2160p over the max %s %d ≥ %q %d", rank, studio, uhd, rival, other)
			}
		}
	}
	// 0–1 seeders against dozens: the favourite loses
	if fav, other := sc("Миллиарды S03 WEB-DL 1080p MVO | LostFilm", 1), sc("Миллиарды S03 WEB-DL 1080p MVO | Jaskier", 40); fav >= other {
		t.Errorf("1-seeder LostFilm %d ≥ 40-seeder Jaskier %d", fav, other)
	}
	// below 720p: no bonus
	if fav, other := sc("Миллиарды S03 WEBRip 480p MVO | LostFilm", 30), sc("Миллиарды S03 WEB-DL 1080p MVO | Jaskier", 30); fav >= other {
		t.Errorf("480p LostFilm %d ≥ 1080p Jaskier %d", fav, other)
	}
	// rank: the first favourite beats the second at equal quality
	if a, b := sc("Миллиарды S03 1080p MVO | LostFilm", 10), sc("Миллиарды S03 1080p MVO (Кубик в Кубе)", 10); a <= b {
		t.Errorf("rank 0 %d ≤ rank 1 %d", a, b)
	}
	// several studios: the best one counts
	if a, b := sc("Миллиарды S03 1080p MVO (Jaskier, LostFilm)", 10), sc("Миллиарды S03 1080p MVO | LostFilm", 10); a != b {
		t.Errorf("multi-studio %d ≠ single %d", a, b)
	}
	if a, b := sc("Миллиарды S03 1080p Dub + MVO (Кубик в Кубе, LostFilm)", 10), sc("Миллиарды S03 1080p Dub | LostFilm", 10); a != b {
		t.Errorf("multi-studio (second listed is best) %d ≠ single %d", a, b)
	}
	_, reasons := score("Миллиарды S03 1080p MVO | LostFilm", 10, 0, want, studioPrefs)
	if !slicesContains(reasons, "любимая озвучка: LostFilm") {
		t.Errorf("reasons %v", reasons)
	}
	// no studios wanted: nothing changes
	plain := billions
	a, _ := score("Миллиарды S03 1080p MVO | LostFilm", 10, 0, plain, studioPrefs)
	b, _ := score("Миллиарды S03 1080p MVO | Jaskier", 10, 0, plain, studioPrefs)
	if a != b {
		t.Errorf("bonus without Wanted.Studios: %d ≠ %d", a, b)
	}
}

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Above the wanted quality the bonus is given, but it never outweighs the
// penalty: a favourite 2160p (max 1080) does not beat an unlisted 1080p.
func TestFavouriteStudioAboveMaxHeight(t *testing.T) {
	want := billions
	want.Studios = studioPrefs.Studios
	for _, seeders := range []int{5, 40, 99} {
		fav, reasons := score("Миллиарды S03 WEB-DL 2160p MVO | LostFilm", seeders, 0, want, studioPrefs)
		other, _ := score("Миллиарды S03 WEB-DL 1080p MVO | Jaskier", seeders, 0, want, studioPrefs)
		if fav >= other {
			t.Errorf("%d seeders: 2160p LostFilm %d ≥ 1080p Jaskier %d %v", seeders, fav, other, reasons)
		}
	}
}

// A low-quality source without a resolution counts as below 720p: no bonus.
func TestFavouriteStudioLowSourceNoResolution(t *testing.T) {
	want := billions
	want.Studios = studioPrefs.Studios
	other, _ := score("Миллиарды S03 WEB-DL 720p MVO | Jaskier", 10, 0, want, studioPrefs)
	for _, src := range []string{"DVDRip", "DVDScr", "TVRip", "SATRip", "VHSRip"} {
		fav, reasons := score("Миллиарды S03 "+src+" MVO | LostFilm", 10, 0, want, studioPrefs)
		if fav >= other || slicesContains(reasons, "любимая озвучка: LostFilm") {
			t.Errorf("%s favourite %d ≥ unlisted 720p WEB-DL %d %v", src, fav, other, reasons)
		}
	}
	// a good source without a resolution keeps the bonus
	if _, reasons := score("Миллиарды S03 WEB-DL MVO | LostFilm", 10, 0, want, studioPrefs); !slicesContains(reasons, "любимая озвучка: LostFilm") {
		t.Errorf("WEB-DL without resolution lost the bonus: %v", reasons)
	}
}

func TestStudioBonus(t *testing.T) {
	for rank, want := range []int{50, 48, 46, 44, 42, 40, 40, 40, 40, 40, 40} {
		if got := studioBonus(rank); got != want {
			t.Errorf("rank %d: %d, want %d", rank, got, want)
		}
	}
}

func TestFavouriteStudioNeverOverridesExclusions(t *testing.T) {
	want := billions
	want.Studios = studioPrefs.Studios
	for _, title := range []string{
		"Миллиарды S03 CAMRip 720p | LostFilm",
		"Миллиарды S02 WEB-DL 1080p | LostFilm",
	} {
		if s, reasons := score(title, 50, 0, want, studioPrefs); s != Excluded {
			t.Errorf("%q: %d %v, want excluded", title, s, reasons)
		}
	}
	p := studioPrefs
	p.MaxSizeGB = 10
	movie := Wanted{Titles: []string{"Дюна"}, Kind: "movie", Year: 2021, Studios: p.Studios}
	if s, _ := score("Дюна (2021) BDRip 1080p | LostFilm", 50, 40*gb, movie, p); s != Excluded {
		t.Errorf("movie over the size limit not excluded with a favourite studio")
	}
	// a release without a favourite is never excluded for it
	if s, reasons := score("Миллиарды S03 WEB-DL 1080p MVO | Jaskier", 1, 0, want, studioPrefs); s == Excluded {
		t.Errorf("unlisted studio excluded: %v", reasons)
	}
	if s, reasons := score("Миллиарды S03 WEB-DL 1080p MVO", 1, 0, want, studioPrefs); s == Excluded {
		t.Errorf("no studio excluded: %v", reasons)
	}
}

func TestSeriesStudiosRankFirst(t *testing.T) {
	want := billions
	want.Studios = wantedStudios([]string{"Кураж-Бамбей", "lostfilm"}, studioPrefs.Studios)
	if len(want.Studios) != 3 || want.Studios[0] != "Кураж-Бамбей" || want.Studios[1] != "LostFilm" || want.Studios[2] != "Кубик в Кубе" {
		t.Fatalf("merged %v", want.Studios)
	}
	base, _ := score("Миллиарды S03 1080p MVO | Jaskier", 10, 0, want, studioPrefs)
	kb, reasons := score("Миллиарды S03 1080p MVO | Кураж-Бамбей", 10, 0, want, studioPrefs)
	if kb-base != studioBonus(0) {
		t.Errorf("Кураж-Бамбей bonus %d, want %d (%v)", kb-base, studioBonus(0), reasons)
	}
	lf, _ := score("Миллиарды S03 1080p MVO | LostFilm", 10, 0, want, studioPrefs)
	if lf-base != studioBonus(1) {
		t.Errorf("LostFilm bonus %d, want %d", lf-base, studioBonus(1))
	}
}

func TestFreeTextFavouriteStudio(t *testing.T) {
	p := studioPrefs
	p.Studios = []string{"Гоблин"}
	want := Wanted{Titles: []string{"Гоблин"}, Kind: "series", Season: 1, Studios: p.Studios}
	plain, _ := score("Гоблин S01 1080p MVO", 10, 0, want, p)
	named, reasons := score("Гоблин S01 1080p MVO | гоблин", 10, 0, want, p)
	if named-plain != studioBonus(0) || !slicesContains(reasons, "любимая озвучка: Гоблин") {
		t.Errorf("free-text studio: %d vs %d %v", named, plain, reasons)
	}
	other, _ := score("Гоблин S01 1080p MVO | Гоблинский", 10, 0, want, p)
	if other != plain {
		t.Errorf("free-text matched inside a word: %d vs %d", other, plain)
	}
}

// every way a release says it holds several seasons.
func TestMultiSeason(t *testing.T) {
	for title, want := range map[string]bool{
		"Suits Season 1-9 1080p":                             true,
		"Suits Seasons 1-9 Remux":                            true,
		"Suits Seasons 1, 2, 3 1080p":                        true,
		"Suits (2011-2019) Complete Series 1080p":            true,
		"Suits.S01E01-S09E10.1080p":                          true,
		"Форс-мажоры / Suits / 1-9 сезон (2011-2019) BDRip":  true,
		"Форс-мажоры / Suits / Сезоны 1-9 (2011-2019) BDRip": true,
		"Форс-мажоры / Suits / Все сезоны (2011-2019) BDRip": true,
		"Форс-мажоры (полный сериал) 2011-2019 BDRip":        true,
		"Suits - S1-9E1-134 - 2011-2019 Remux":               true,
		"Suits S01 1080p":                                    false,
		"Suits Season 1 1080p":                               false,
		"Форс-мажоры / Suits / Сезон: 1 / Серии: 1-12 из 12": false,
		"Suits.S01E01-E05.1080p":                             false,
		"Suits 1080p":                                        false,
	} {
		if got := MultiSeason(title); got != want {
			t.Errorf("MultiSeason(%q) = %v, want %v", title, got, want)
		}
	}
	if lo, hi := SeasonRange("Suits.S01E01-S09E10.1080p"); lo != 1 || hi != 9 {
		t.Errorf("S01E01-S09E10: %d–%d", lo, hi)
	}
	if lo, hi := SeasonRange("Suits Season 1-9 1080p"); lo != 1 || hi != 9 {
		t.Errorf("Season 1-9: %d–%d", lo, hi)
	}
}

// Live (Suits): «S1-9E1-134» is a complete-series pack, not season 1, and
// a single-season release of comparable quality beats such a pack even
// with more seeders on the pack.
func TestScoreCompactSeasonRangeIsAPack(t *testing.T) {
	const pack = "Suits - S1-9E1-134 - 2011-2019 DVO (NewStudio, TVShows) Blu-Ray Remux 1080p"
	if lo, hi := SeasonRange(pack); lo != 1 || hi != 9 {
		t.Fatalf("SeasonRange(%q) = %d, %d; want 1, 9", pack, lo, hi)
	}
	want := Wanted{Titles: []string{"Форс-мажоры", "Suits"}, Season: 1, Kind: "series"}
	packScore, r1 := score(pack, 60, 1100*gb, want, DefaultPrefs())
	single, r2 := score("Suits S01 DVO (NewStudio) Blu-Ray Remux 1080p", 8, 120*gb, want, DefaultPrefs())
	if packScore == Excluded || single == Excluded {
		t.Fatalf("excluded: %v %v", r1, r2)
	}
	if single <= packScore {
		t.Errorf("single season %d %v should beat the S1-9 pack %d %v", single, r2, packScore, r1)
	}
	want.Season = 10
	if s, r := score(pack, 60, 1100*gb, want, DefaultPrefs()); s != Excluded {
		t.Errorf("S1-9 pack for season 10 not excluded: %v", r)
	}
}
