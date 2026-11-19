package acquire

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// TMDB's search matches whole words, and the last word by its beginning
// too, but forgives no typo: «Джентельмены» finds nothing, «Джент» finds
// «Джентльмены». discoverFuzzy asks again by the beginnings of the query's
// longest words (where a typo is least likely) and keeps the titles each of
// whose query words is within a few edits of one of the title's words — the
// library's search does the same (core: internal/library/fuzzy.go).

// fuzzyWords is how many of the query's longest words are asked by their
// beginnings: each costs a movie and a TV search, only when the query as
// typed found nothing.
const fuzzyWords = 2

// discoverFuzzy is Discover's second look; nothing when the query has no
// word long enough to carry a typo. A Cyrillic query is compared with the
// Russian titles (TMDB answers in the profile's language otherwise, and
// «The Gentlemen» is no misspelt «Джентльмены»).
func discoverFuzzy(ctx context.Context, m Metadata, q string) (movies, shows []tmdb.Result) {
	query := strings.Fields(release.Normalize(q))
	prefixes := fuzzyPrefixes(query)
	if len(prefixes) == 0 {
		return nil, nil
	}
	if hasCyrillic(q) && !strings.HasPrefix(tmdb.ContextLanguage(ctx), "ru") {
		ctx = tmdb.WithLanguage(ctx, "ru-RU")
	}
	type found struct{ movies, shows []tmdb.Result }
	res := make([]found, len(prefixes))
	var wg sync.WaitGroup
	for i, p := range prefixes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mv, sh, _ := searchBoth(ctx, m, p) // a failed look is just no candidates
			res[i] = found{mv, sh}
		}()
	}
	wg.Wait()
	var allM, allS []tmdb.Result
	for _, f := range res {
		allM = append(allM, f.movies...)
		allS = append(allS, f.shows...)
	}
	return fuzzyKeep(query, allM), fuzzyKeep(query, allS)
}

// fuzzyPrefixes are the beginnings of the query's longest words that can
// carry a typo (two fifths of the word, at least three letters), longest
// word first.
func fuzzyPrefixes(query []string) []string {
	words := make([]string, 0, len(query))
	for _, w := range query {
		if fuzzyEdits(len([]rune(w))) > 0 {
			words = append(words, w)
		}
	}
	sort.SliceStable(words, func(i, j int) bool { return len([]rune(words[i])) > len([]rune(words[j])) })
	var out []string
	for _, w := range words {
		r := []rune(w)
		p := string(r[:max(3, (2*len(r)+4)/5)])
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
		if len(out) == fuzzyWords {
			break
		}
	}
	return out
}

// fuzzyKeep is the results that match the query words (by title or
// original title), once each, fewest edits first, TMDB's order on ties.
func fuzzyKeep(query []string, rs []tmdb.Result) []tmdb.Result {
	type hit struct {
		r     tmdb.Result
		score int
	}
	var hits []hit
	seen := map[int]bool{}
	for _, r := range rs {
		if seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		best, ok := fuzzyScore(query, release.Normalize(r.Title))
		if s, ok2 := fuzzyScore(query, release.Normalize(r.OriginalTitle)); ok2 && (!ok || s < best) {
			best, ok = s, true
		}
		if ok {
			hits = append(hits, hit{r, best})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score < hits[j].score })
	out := make([]tmdb.Result, len(hits))
	for i, h := range hits {
		out[i] = h.r
	}
	return out
}

func hasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

// fuzzyEdits is how many edits a query word of n letters may carry: none
// for short words (too many neighbours), one up to seven letters, two from
// eight.
func fuzzyEdits(n int) int {
	switch {
	case n < 4:
		return 0
	case n < 8:
		return 1
	default:
		return 2
	}
}

// fuzzyScore is the edits a title needs to answer the query words (each
// must match some word of the title, or the beginning of one); ok is false
// when one does not. Both sides are normalized (release.Normalize).
func fuzzyScore(query []string, title string) (score int, ok bool) {
	words := strings.Fields(title)
	if len(words) == 0 || len(query) == 0 {
		return 0, false
	}
	for _, q := range query {
		qr := []rune(q)
		limit := fuzzyEdits(len(qr))
		best := limit + 1
		for _, w := range words {
			if d := wordEdits(qr, []rune(w), limit); d < best {
				best = d
				if d == 0 {
					break
				}
			}
		}
		if best > limit {
			return 0, false
		}
		score += best
	}
	return score, true
}

// wordEdits is the edits between query word q and title word w, or a
// prefix of w at least q's length (the word still being typed); above
// limit it is limit+1.
func wordEdits(q, w []rune, limit int) int {
	best := limit + 1
	for n := len(q) - limit; n <= len(q)+limit; n++ {
		if n < 1 || n > len(w) || (n < len(w) && n < len(q)) {
			continue
		}
		if d := osa(q, w[:n], best-1); d < best {
			best = d
		}
	}
	return best
}

// osa is the optimal-string-alignment distance (insertions, deletions,
// substitutions, swaps of neighbours) between a and b, cut short above
// limit (it then answers limit+1).
func osa(a, b []rune, limit int) int {
	if d := len(a) - len(b); d > limit || -d > limit {
		return limit + 1
	}
	prev2 := make([]int, len(b)+1)
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		low := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			v := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				v = min(v, prev2[j-2]+1)
			}
			cur[j] = v
			low = min(low, v)
		}
		if low > limit {
			return limit + 1
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return min(prev[len(b)], limit+1)
}
