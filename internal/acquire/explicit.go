package acquire

import (
	"strings"
	"unicode"
)

// explicitStems: words (and their beginnings) that mark a catalog entry as
// adult content in an Open Library subject or a MusicBrainz disambiguation,
// in English and Russian. Matched at the start of a word, so «sexual» and
// «эротика» count but «Essex» does not. A small explicit list, on purpose
// cautious: a kid profile loses a few harmless results rather than see one
// that is not.
var explicitStems = []string{
	"erotic", "porn", "sexual", "explicit", "bdsm", "hentai", "smut", "fetish", "nsfw", "xxx",
	"эрот", "порн", "секс", "откровенн", "хентай", "фетиш",
}

// explicitWords are matched as whole words («sex», not «sextet»).
var explicitWords = map[string]bool{"sex": true, "sexy": true}

// explicitPhrases are matched anywhere (case folded).
var explicitPhrases = []string{"18+", "adult fiction", "adult content", "для взрослых", "romance, erotic", "erotic romance"}

// ExplicitSubjects says whether any of the texts (subjects, a
// disambiguation, secondary types) marks adult content.
func ExplicitSubjects(texts []string) bool {
	for _, s := range texts {
		s = strings.ToLower(s)
		if s == "" {
			continue
		}
		for _, p := range explicitPhrases {
			if strings.Contains(s, p) {
				return true
			}
		}
		for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if explicitWords[w] {
				return true
			}
			for _, st := range explicitStems {
				if strings.HasPrefix(w, st) {
					return true
				}
			}
		}
	}
	return false
}
