package indexer

import (
	"unicode/utf8"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// nameSkeleton is the consonant skeleton of a folded name word, the same
// for its Latin and Cyrillic spellings: «roberts» and «робертс» are
// «rbrts», «tolkien» and «толкин» «tlkn», «pratchett» and «пратчетт»
// «prCt». Vowels, «h», «w» and «y» are dropped (Rowling = Роулинг) and
// doubled consonants are one.
func nameSkeleton(word string) string { return skeleton(word, false) }

// skeleton: keepW spells «w» as «v» (Weir = Вейер) instead of dropping it
// (Rowling = Роулинг).
func skeleton(word string, keepW bool) string { return release.Skeleton(word, keepW) }

// nameSkeletonsMeet: one of the author's words (folded, 3+ letters) has
// the skeleton of one of the release name's words, in either alphabet; a
// skeleton of fewer than 3 sounds tells nothing and never matches.
func nameSkeletonsMeet(author, name []string) bool {
	have := map[string]bool{}
	for _, w := range name {
		if k := nameSkeleton(w); utf8.RuneCountInString(k) >= 3 {
			have[k] = true
		}
	}
	for _, w := range author {
		if k := nameSkeleton(w); utf8.RuneCountInString(k) >= 3 && have[k] {
			return true
		}
	}
	return false
}

// shortSkeletonsMeet: a short surname of two sounds whose «w» reads «v»
// («weir» and «вейер» are «vr»). Two sounds tell little, so it is asked
// only once the release already holds the work.
func shortSkeletonsMeet(author, name []string) bool {
	have := map[string]bool{}
	for _, w := range name {
		if k := skeleton(w, true); utf8.RuneCountInString(k) == 2 {
			have[k] = true
		}
	}
	for _, w := range author {
		if k := skeleton(w, true); utf8.RuneCountInString(k) == 2 && have[k] {
			return true
		}
	}
	return false
}
