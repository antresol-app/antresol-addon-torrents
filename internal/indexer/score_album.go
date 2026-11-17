package indexer

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// Album titles that are words of the artist (a self-titled album:
// «Metallica» by Metallica, «Led Zeppelin», «Peter Gabriel») cannot be
// matched by "the name holds the title": the artist alone holds it, so
// «Metallica - 72 Seasons» would be the Black Album. Such a title must be a
// segment of its own besides the artist's.

var (
	// reSegment splits a release name into its parts: "Artist - Album
	// (1991) [FLAC, tracks]" → Artist | Album | 1991 | FLAC | tracks. A
	// slash or hyphen inside a word stays ("AC/DC", "S/T", "Blink-182").
	reSegment = regexp.MustCompile(`\s+[-–—/|]+\s*|[-–—]+\s+|[()\[\]{}«»"“”,:;]`)
	// reNickname: a parenthesised nickname of a self-titled album, «Black
	// Album», «The Blue Album», «Чёрный альбом» (folded).
	reNickname = regexp.MustCompile(`^(?:the )?[\p{L}\d]+ (?:album|альбом)$`)
)

// selfTitledMarks are the parts that say "the album named after the
// artist" (folded).
var selfTitledMarks = map[string]bool{"s t": true, "self titled": true, "selftitled": true}

// sceneName spaces out a scene-style name with no spaces:
// «Metallica-Metallica-1991-FLAC», «Metallica_-_Metallica_(1991)_FLAC»,
// «Metallica.-.Metallica.(1991).FLAC». The hyphens of the artist's own
// words stay («Blink-182-Blink-182-2003-FLAC»).
func sceneName(raw, artist string) string {
	if strings.ContainsAny(strings.TrimSpace(raw), " \t") {
		return raw
	}
	raw = strings.NewReplacer("_", " ", ".", " ").Replace(raw)
	if !strings.Contains(raw, " - ") {
		for _, w := range strings.Fields(artist) {
			if strings.Contains(w, "-") {
				// a Unicode hyphen: not a separator, folded like one
				raw = regexp.MustCompile(`(?i)`+regexp.QuoteMeta(w)).ReplaceAllString(raw, strings.ReplaceAll(w, "-", "\u2010"))
			}
		}
		raw = strings.ReplaceAll(raw, "-", " - ")
	}
	return raw
}

// segments returns the folded, non-empty parts of a release name.
func segments(raw string) []string { return segmentsOf(raw, "") }

// segmentsOf is segments for a release of artist (see sceneName).
func segmentsOf(raw, artist string) []string {
	var out []string
	for _, s := range reSegment.Split(sceneName(raw, artist), -1) {
		if f := foldTitle(s); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// numFold spells the numeral words of a folded title in arabic («led
// zeppelin ii» = «led zeppelin 2»), except a single «i»: the pronoun (a
// trailing «I» after the title is matched by mediaMatches).
func numFold(s string) string {
	words := strings.Fields(s)
	for i, w := range words {
		if n, ok := release.Numeral(w); ok && w != "i" {
			words[i] = n
		}
	}
	return strings.Join(words, " ")
}

// titleInArtist: every word of the work is a word of the artist.
func titleInArtist(work, artist string) bool {
	aw := strings.Fields(artist)
	if len(aw) == 0 {
		return false
	}
	for _, w := range strings.Fields(work) {
		if !slices.Contains(aw, w) {
			return false
		}
	}
	return true
}

// aliasKey drops the article and the word "album" of a nickname: «The
// Black Album» = «Black Album» = «black».
func aliasKey(s string) string {
	s = strings.TrimPrefix(s, "the ")
	s = strings.TrimSuffix(s, " album")
	return strings.TrimSuffix(s, " альбом")
}

// workNumber is the number of a "<work> N" part ("peter gabriel ii" → "2").
func workNumber(seg, work string) (string, bool) {
	rest, ok := strings.CutPrefix(seg, work+" ")
	if !ok {
		return "", false
	}
	return release.Numeral(rest)
}

// hasAlias: a part of one of the aliases is a part of the name — a word
// part as its own part («Security» of «4: Security», «Black Album» = «The
// Black Album»), a number as the work's number ("Peter Gabriel I" for
// «1: Car»).
func hasAlias(segs []string, work string, aliases []string) bool {
	for _, a := range release.UsefulAliases(aliases) {
		for _, p := range segments(a) {
			n, numeral := release.Numeral(p)
			for _, s := range segs {
				if numeral {
					if m, ok := workNumber(s, work); ok && m == n {
						return true
					}
				} else if k := aliasKey(p); k != "" && aliasKey(s) == k {
					return true
				}
			}
		}
	}
	return false
}

// aliasesTell: one of the aliases is a nickname or a number («Blue Album»,
// «2: Scratch»), so a name with another nickname or number is another
// album.
func aliasesTell(aliases []string) bool {
	for _, a := range release.UsefulAliases(aliases) {
		for _, p := range segments(a) {
			if _, n := release.Numeral(p); n || reNickname.MatchString(p) {
				return true
			}
		}
	}
	return false
}

// titleInArtistMatches: the release name names the album whose title is a
// word (or all words) of the artist — one of its aliases, the title once
// more as a part of its own («Metallica - Metallica»; "Peter Gabriel 1" /
// "Led Zeppelin I" for the first one), a self-titled mark («Metallica -
// S/T»), a nickname («Metallica (Black Album)»; when the aliases name one,
// only theirs), or the bare title right before the wanted year
// («Metallica (1991)»). Another album of the artist («72 Seasons», «Led
// Zeppelin II», «Discography») does not match.
func titleInArtistMatches(raw, work, artist, rawArtist string, aliases []string, year int) bool {
	segs := segmentsOf(raw, rawArtist)
	if hasAlias(segs, work, aliases) {
		return true
	}
	count, mark, nick, other := 0, false, false, false
	for _, s := range segs {
		n, numbered := workNumber(s, work)
		switch {
		case s == work:
			count++
		case numbered && n == "1":
			count++
			nick = true // a numbered one: the aliases tell which
		case numbered:
			other = true // «Led Zeppelin II»
		case selfTitledMarks[s]:
			mark = true
		case reNickname.MatchString(s):
			nick = true
		}
	}
	if other || nick && aliasesTell(aliases) {
		return false // another number or nickname than the album's own
	}
	titled := count
	if work == artist && count > 0 {
		titled-- // one of them is the artist
	}
	if titled > 0 || mark || nick {
		return true
	}
	if year > 0 {
		y := strconv.Itoa(year)
		for i := 0; i+1 < len(segs); i++ {
			if (segs[i] == work || segs[i] == artist) && segs[i+1] == y {
				return true
			}
		}
	}
	return false
}

// reissuePenalty demotes a reissue that does not give the original year.
const reissuePenalty = 15

// yearCheck compares the years a music release name gives with the wanted
// one. The wanted year (±1, or a range holding it) anywhere wins:
// «Metallica (1991) [2008, Vertigo]». Otherwise another year excludes the
// release, unless the name marks a reissue («(2011 Remaster)», «(Moroz
// Records) 1996»): that is demoted by reissuePenalty. Years that are words
// of the work or the artist do not count («Van Halen - 1984»).
func yearCheck(raw string, want int, work, artist string, reissue bool) (penalty int, reason string, exclude bool) {
	if want <= 0 {
		return 0, "", false
	}
	words := strings.Fields(foldTitle(work) + " " + foldTitle(artist))
	years, ranges := release.AudioYears(sceneName(raw, artist))
	near, first := false, 0
	for _, y := range years {
		switch {
		case abs(y-want) <= 1:
			near = true
		case slices.Contains(words, strconv.Itoa(y)):
		case first == 0:
			first = y
		}
	}
	for _, r := range ranges {
		if want >= r[0]-1 && want <= r[1]+1 {
			near = true
		} else if first == 0 {
			first = r[0]
		}
	}
	switch {
	case first == 0 || near:
		return 0, "", false
	case reissue:
		return reissuePenalty, fmt.Sprintf("переиздание (%d)", first), false
	}
	return 0, fmt.Sprintf("другой год (%d)", first), true
}
