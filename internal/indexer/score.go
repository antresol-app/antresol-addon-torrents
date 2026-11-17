package indexer

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/releaseprefs"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// Result is one release found by a source.
type Result = cardigann.Result

// Wanted describes what is being looked for. Titles are all known names
// (TMDB title, original title and alternative titles).
type Wanted struct {
	Titles          []string
	Year            int
	Season, Episode int
	Kind            string // movie | series | music | book | audiobook | comic
	// Artist and Work: see SearchRequest (media kinds).
	Artist, Work string
	Aliases      []string
	// DefaultLang is the language of a release whose name names none: "ru"
	// on a Russian-language tracker.
	DefaultLang string
	// Studios: the favourite voice-over studios, best first — the
	// series' own studios (SearchRequest.Studios) before Prefs.Studios.
	Studios []string
}

// Prefs are the user's release preferences (settings key release_prefs).
type Prefs = releaseprefs.Prefs

// DefaultPrefs: Full HD, Russian dubbing preferred, at least one seeder,
// lossless music.
func DefaultPrefs() Prefs { return releaseprefs.Default() }

// Excluded is the score of a release that must not be offered.
const Excluded = math.MinInt32

var (
	// several seasons named one by one («Seasons 1, 2, 3», «сезоны 1, 2»)
	// or the whole series («Complete Series», «все сезоны», «полный сериал»)
	reSeasonList = regexp.MustCompile(`(?i)(?:\bseasons?|сезон\p{L}*)\s*[:#№]?\s*\d{1,2}\s*(?:,|&|\+|\band\b|и\s)\s*\d{1,2}`)
	reAllSeasons = regexp.MustCompile(`(?i)\bcomplete\s+series\b|все\s+сезоны|полн\p{L}*\s+сериал`)
)

// MultiSeason reports whether a release named title says it holds several
// seasons: a range (SeasonRange), seasons listed one by one, or the whole
// series. A season request then reads the release's files before adding it.
func MultiSeason(title string) bool {
	if lo, hi := SeasonRange(title); hi > lo {
		return true
	}
	return reSeasonList.MatchString(title) || reAllSeasons.MatchString(title)
}

// Score rates a release for want under prefs. Excluded (with the reason)
// for camrips, other titles, years, seasons or episodes, too few seeders or
// a size over the limit. Otherwise higher is better; reasons are short
// Russian notes for the release table.
//
// Music, books, audiobooks and comics are rated by ScoreMedia.
func Score(r Result, info release.Info, want Wanted, p Prefs) (int, []string) {
	if isMediaKind(want.Kind) {
		return ScoreMedia(r, want.Kind, want, p)
	}
	exclude := func(why string) (int, []string) { return Excluded, []string{why} }
	if info.IsCam {
		return exclude("запись с экрана кинотеатра")
	}
	if len(want.Titles) > 0 && !titleMatches(r.Title, info, want.Titles) {
		return exclude("другое название")
	}
	series := want.Kind == "series" || want.Season > 0
	// a later season carries a later year, but never one before the premiere
	if want.Year > 0 && info.Year > 0 && (!series && abs(info.Year-want.Year) > 1 || series && info.Year < want.Year-1) {
		return exclude(fmt.Sprintf("другой год (%d)", info.Year))
	}

	score := 0
	var reasons []string
	if want.Season > 0 {
		switch lo, hi := seasonRange(r.Title, info); {
		case lo == 0:
			score -= 5
			reasons = append(reasons, "сезон не указан")
		case want.Season < lo || want.Season > hi:
			if lo == hi {
				return exclude(fmt.Sprintf("другой сезон (%d)", lo))
			}
			return exclude(fmt.Sprintf("другие сезоны (%d–%d)", lo, hi))
		case lo != hi:
			// a multi-season pack that contains it: only the season's files
			// are downloaded, but a release of that season alone of
			// comparable quality is preferred (20 points ahead)
			score -= 5
			reasons = append(reasons, fmt.Sprintf("сезоны %d–%d", lo, hi))
		case want.Episode == 0:
			score += 15 // exactly this season: the best for a whole-season download
		}
	}
	if want.Episode > 0 && info.Episode > 0 && !info.IsSeasonPack {
		end := max(info.EpisodeEnd, info.Episode)
		if want.Episode < info.Episode || want.Episode > end {
			return exclude(fmt.Sprintf("другая серия (%d)", info.Episode))
		}
	} else if want.Episode > 0 && (info.Episode == 0 || info.IsSeasonPack) {
		reasons = append(reasons, "весь сезон")
	} else if want.Season > 0 && want.Episode == 0 && info.Episode > 0 && !info.IsSeasonPack {
		// a whole season was asked for: one episode or a few are no answer
		if max(info.EpisodeEnd, info.Episode) > info.Episode {
			return exclude("не весь сезон")
		}
		return exclude("одна серия")
	}

	minSeeders := max(p.MinSources, 0)
	if r.Seeders < minSeeders {
		return exclude("нет раздающих")
	}
	// «Максимальный размер фильма»: a season or series pack is legitimately
	// much bigger, so the limit applies to movies only.
	if p.MaxSizeGB > 0 && r.Size > 0 && !series {
		if float64(r.Size) > p.MaxSizeGB*(1<<30) {
			return exclude(fmt.Sprintf("больше %s ГБ", strconv.FormatFloat(p.MaxSizeGB, 'f', -1, 64)))
		}
		score += 5
	}

	maxH := p.MaxHeight
	if maxH <= 0 {
		maxH = 1080
	}
	switch res := info.Resolution; {
	case res == 0:
		score += 10
	case res > maxH:
		score -= 20
		reasons = append(reasons, fmt.Sprintf("%dp — выше нужного", res))
	default:
		score += 50 * res / maxH
		reasons = append(reasons, fmt.Sprintf("%dp", res))
	}

	has := func(a string) bool {
		for _, x := range info.Audio {
			if x == a {
				return true
			}
		}
		return false
	}
	dubW, origW := p.VoiceWeights()
	switch {
	case dubW > 0 && has("Dub"):
		score += 30 * dubW / 10
		reasons = append(reasons, "дубляж")
	case dubW > 0 && has("MVO"):
		score += 20 * dubW / 10
		reasons = append(reasons, "многоголосый перевод")
	case dubW > 0 && has("AVO"):
		score += 10 * dubW / 10
		reasons = append(reasons, "авторский перевод")
	}
	if origW > 0 && has("Original") {
		score += 25 * origW / 10
		reasons = append(reasons, "оригинальная дорожка")
	}
	switch info.Source {
	case "Remux", "BluRay", "WEB-DL":
		score += 5
	}
	// a favourite studio wins clearly, but not with 0–1 seeders or below
	// 720p (a low-quality source without a resolution counts as below)
	if r.Seeders >= 2 && (info.Resolution >= 720 || info.Resolution == 0 && !lowSource[info.Source]) {
		if rank, name := favouriteStudio(r.Title, info, want.Studios); rank >= 0 {
			score += studioBonus(rank)
			reasons = append(reasons, fmt.Sprintf("любимая озвучка: %s", name))
		}
	}

	score, reasons = seedersScore(r, score, reasons)
	return score, reasons
}

// lowSource: sources that are below 720p when a release names no
// resolution (camrips are excluded before).
var lowSource = map[string]bool{"DVDRip": true, "DVDScr": true, "TVRip": true, "SATRip": true, "VHSRip": true}

// studioBonus is the bonus of a favourite studio by its rank (0 = best):
// for every studio of the list a favourite 720p voice-over beats an
// unlisted 1080p, even a full dub, with dozens of seeders; the first still
// beats the second.
func studioBonus(rank int) int { return max(50-2*rank, 40) }

// favouriteStudio returns the rank and name of the best-ranked studio of
// favourites the release names (-1 when none): a known studio parsed from
// its name, or a typed one named as words after the title.
func favouriteStudio(raw string, info release.Info, favourites []string) (int, string) {
	if len(favourites) == 0 {
		return -1, ""
	}
	have := map[string]bool{}
	for _, s := range info.Studios {
		have[release.StudioKey(s)] = true
	}
	tail := ""
	for rank, f := range favourites {
		k := release.StudioKey(f)
		if have[k] {
			return rank, f
		}
		if _, known := release.CanonicalStudio(f); known {
			continue
		}
		if tail == "" {
			tail = release.Tail(raw)
		}
		if release.NamesStudio(tail, f) {
			return rank, f
		}
	}
	return -1, ""
}

// wantedStudios merges studio lists in order (the series' own first),
// each studio once.
func wantedStudios(lists ...[]string) []string { return release.MergeStudios(lists...) }

// seedersScore adds up to 30 points on a log scale of the seeders.
func seedersScore(r Result, score int, reasons []string) (int, []string) {
	if r.Seeders > 0 {
		score += min(30, int(math.Round(10*math.Log10(float64(r.Seeders)+1))))
		reasons = append(reasons, fmt.Sprintf("раздают: %d", r.Seeders))
	}
	return score, reasons
}

// SeasonRange returns the seasons a release named title covers (0, 0 when
// it names none).
func SeasonRange(title string) (int, int) { return release.SeasonRange(title) }

// seasonRange returns the seasons a release covers (0,0 when unknown).
func seasonRange(title string, info release.Info) (int, int) {
	return release.SeasonRangeOf(title, info)
}

// TitleMatches reports whether a release named raw is of one of titles
// (the search's own title check: every name of the release, normalised).
func TitleMatches(raw string, titles []string) bool {
	return titleMatches(raw, release.Parse(raw), titles)
}

// titleMatches compares every name of the release with every wanted title
// after normalisation and folding of diacritics (Shōgun = Shogun, ё = е).
func titleMatches(raw string, info release.Info, titles []string) bool {
	want := map[string]bool{}
	for _, t := range titles {
		if n := foldTitle(t); n != "" {
			want[n] = true
		}
	}
	for _, n := range releaseNames(raw, info) {
		if want[foldTitle(n)] {
			return true
		}
	}
	return false
}

// releaseNames: parsed title and alt title plus every " / " part of the name
// before the first bracket ("Сёгун / Shōgun / Сезон: 1 (2024) …").
func releaseNames(raw string, info release.Info) []string {
	names := []string{info.Title, info.AltTitle}
	head := raw
	if i := strings.IndexAny(head, "([|"); i > 0 {
		head = head[:i]
	}
	for _, part := range strings.Split(head, "/") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		names = append(names, part, release.Parse(part).Title)
	}
	return names
}

func foldTitle(s string) string {
	s = norm.NFD.String(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	// "Fast & Furious" = "Fast and Furious" ("&" is dropped by Normalize)
	return strings.ReplaceAll(release.Normalize(b.String()), " and ", " ")
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
