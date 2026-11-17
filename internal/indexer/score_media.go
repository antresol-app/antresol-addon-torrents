package indexer

import (
	langpkg "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/lang"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// isMediaKind: the kinds ScoreMedia rates.
func isMediaKind(kind string) bool {
	switch kind {
	case "music", "book", "audiobook", "comic":
		return true
	}
	return false
}

// mediaWant is the wanted work (album, book, series; "" = everything of
// the artist) and artist (or author) of a media search. A Wanted without
// them (built by hand) wants its first title.
func mediaWant(want Wanted) (work, artist string) {
	work, artist = strings.TrimSpace(want.Work), strings.TrimSpace(want.Artist)
	if work == "" && artist == "" && len(want.Titles) > 0 {
		work = strings.TrimSpace(want.Titles[0])
	}
	return work, artist
}

// nameStopwords are never the word that tells one artist from another
// ("The Beatles" vs "The Rolling Stones").
var nameStopwords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "of": true, "feat": true, "ft": true, "der": true, "die": true,
	"das": true, "le": true, "la": true, "les": true, "el": true, "los": true, "и": true, "da": true, "de": true,
}

// mediaMatches: the release name holds the whole work (when one is wanted)
// and, when an artist is known, one of the artist's words of 3+ letters
// that is not an article or stopword (all of its words when it has none:
// "AC/DC"). Names are compared folded (ё = е, no diacritics, no
// punctuation).
//
// A work whose words are all words of the artist (a self-titled album,
// «Metallica» by Metallica) is not found in the artist's name: see
// titleInArtistMatches.
func mediaMatches(kind, raw, work, artist string, aliases []string, year int) bool {
	name := " " + foldTitle(raw) + " "
	has := func(w string) bool { return strings.Contains(name, " "+w+" ") }
	w := foldTitle(work)
	hasWork := func() bool {
		if has(w) || strings.Contains(" "+numFold(strings.TrimSpace(name))+" ", " "+numFold(w)+" ") {
			return true
		}
		// a trailing "I" / "1" after the title words («Led Zeppelin I»);
		// never a bare «1» (the pronoun «I» is not «The Beatles 1»)
		if i := strings.LastIndex(w, " "); i > 0 && (w[i+1:] == "i" || w[i+1:] == "1") {
			return has(w[:i]+" i") || has(w[:i]+" 1")
		}
		return false
	}
	switch {
	case kind == "music" && w != "" && titleInArtist(w, foldTitle(artist)):
		if !titleInArtistMatches(raw, w, foldTitle(artist), artist, aliases, year) {
			return false
		}
	case w != "" && !hasWork() && !hasAlias(segments(raw), w, aliases) && !bookAlias(kind, has, aliases):
		return false
	case w == "" && strings.TrimSpace(artist) == "":
		return false // nothing wanted
	}
	words := strings.Fields(foldTitle(artist))
	if len(words) == 0 {
		return true
	}
	var long, plain []string
	for _, w := range words {
		if nameStopwords[w] {
			continue
		}
		plain = append(plain, w)
		if utf8.RuneCountInString(w) >= 3 {
			long = append(long, w)
		}
	}
	if len(long) == 0 {
		if len(plain) == 0 {
			plain = words // "The The"
		}
		for _, w := range plain {
			if !has(w) {
				return false
			}
		}
		return true
	}
	for _, w := range long {
		if has(w) {
			return true
		}
	}
	if kind == "book" || kind == "audiobook" {
		// a translation names the author in the other alphabet: «Робертс»
		// for «Roberts» (a book found by its Russian title, or the reverse)
		fields := strings.Fields(name)
		if nameSkeletonsMeet(long, fields) || w != "" && shortSkeletonsMeet(long, fields) {
			return true
		}
		// an English audiobook often names the work alone («Harry Potter
		// and the Philosopher's Stone [Audio Book]»): a long title with an
		// audiobook mark is enough
		if kind != "audiobook" || w == "" || !audiobookMarked(raw) {
			return false
		}
		if len(strings.Fields(w)) >= 3 && hasWork() {
			return true
		}
		for _, a := range release.UsefulAliases(aliases) {
			if f := foldTitle(a); len(strings.Fields(f)) >= 3 && has(f) {
				return true
			}
		}
		return false
	}
	return false
}

// reAudiobookMark: words and formats only an audio release has.
var reAudiobookMark = regexp.MustCompile(`(?i)аудиокниг|аудиоспектакл|audio\s?book|unabridged|narrated|\bчита(ет|ют)\b|\bчтец|kbps|кбит|(^|[^\pL\pN])(mp3|m4b|m4a|aac|flac|ogg|opus)($|[^\pL\pN])`)

func audiobookMarked(name string) bool { return reAudiobookMark.MatchString(name) }

// reGame: a PC or console game («Metro 2033 (2010) PC | RePack»).
var reGame = regexp.MustCompile(`(?i)(^|[^\pL\pN])(pc|repack|steam-rip|xbox|ps[345]|nintendo switch)($|[^\pL\pN])`)

// notAudiobook: why a release found for an audiobook is something else —
// a film or series (resolution, WEB-DL…), a game or an e-book — or "" when
// it may be one. Russian aggregators (JacRed, RuTor, BigFANGroup) ignore
// the category and answer a title with its film and game too, and the
// film names the author's first name («Мастер и Маргарита (Михаил Локшин)»).
func notAudiobook(r Result) string {
	if audiobookMarked(r.Title) {
		return ""
	}
	if info := release.Parse(r.Title); info.Resolution > 0 || info.Source != "" {
		return "видео, не аудиокнига"
	}
	if reGame.MatchString(r.Title) {
		return "игра, не аудиокнига"
	}
	if len(release.ParseBook(r.Title).Formats) > 0 {
		return "электронная книга, не аудиокнига"
	}
	if len(r.Category) > 0 {
		for _, c := range r.Category {
			if c/1000 != 2 && c/1000 != 4 && c/1000 != 5 && c/1000 != 6 {
				return ""
			}
		}
		return "раздел трекера — не аудиокниги"
	}
	return ""
}

// bookAlias: a book or audiobook named by another of its titles (the
// original «Shantaram» of «Шантарам», or the reverse) anywhere in the name.
func bookAlias(kind string, has func(string) bool, aliases []string) bool {
	if kind != "book" && kind != "audiobook" {
		return false
	}
	for _, a := range release.UsefulAliases(aliases) {
		if f := foldTitle(a); f != "" && has(f) {
			return true
		}
	}
	return false
}

// ScoreMedia rates a music, book, audiobook or comic release: Excluded
// (with the reason) for another title, formats the readers do not open
// (DJVU, DOC, a PDF manga) or too few seeders; otherwise the format, a
// discography (−20 when one album is wanted, +10 when the whole artist is),
// the language of books and manga (manga: Russian +10, English +5; an
// unnamed one is the tracker's own) and the seeders. The size limit is a
// movie setting and does not apply.
func ScoreMedia(r Result, kind string, want Wanted, p Prefs) (int, []string) {
	exclude := func(why string) (int, []string) { return Excluded, []string{why} }
	work, artist := mediaWant(want)
	if len(want.Titles) > 0 && !mediaMatches(kind, r.Title, work, artist, want.Aliases, want.Year) {
		return exclude("другое название")
	}
	score := 0
	var reasons []string
	switch kind {
	case "music":
		a := release.ParseAudio(r.Title)
		penalty, why, other := yearCheck(r.Title, want.Year, work, artist, a.Reissue)
		if other {
			return exclude(why)
		}
		if penalty > 0 {
			score -= penalty
			reasons = append(reasons, why)
		}
		switch {
		case work == "" && a.Discography:
			score += 10 // the artist was asked for: all of it
			reasons = append(reasons, "дискография")
		case a.Discography && !release.ParseAudio(work).Discography:
			// it holds the album (its name says so), but a single album is better
			score -= 20
			reasons = append(reasons, "дискография — альбом в её составе")
		}
		points, format := musicFormatScore(a, p)
		score += points
		reasons = append(reasons, format...)
		if a.Image {
			reasons = append(reasons, "образ + CUE")
		}
	case "audiobook":
		if why := notAudiobook(r); why != "" {
			return exclude(why)
		}
		switch f := release.ParseAudio(r.Title).Format; f {
		case "M4B":
			score += 20
			reasons = append(reasons, "M4B")
		case "MP3", "AAC", "FLAC":
			score += 10
			reasons = append(reasons, f)
		}
	case "book", "comic":
		b := release.ParseBook(r.Title)
		points := map[string]int{"EPUB": 30, "FB2": 20, "MOBI": 15, "AZW3": 15, "PDF": 10}
		unreadable := "Формат не открывается в Antresol"
		if kind == "comic" {
			// the comics reader opens archives only (no PDF)
			points = map[string]int{"CBZ": 20, "CBR": 15, "CB7": 15}
			unreadable = "Формат не открывается в читалке комиксов"
		}
		best, bestFormat := 0, ""
		for _, f := range b.Formats {
			if points[f] > best {
				best, bestFormat = points[f], f
			}
		}
		if len(b.Formats) > 0 && best == 0 {
			return exclude(unreadable)
		}
		if best > 0 {
			score += best
			reasons = append(reasons, bestFormat)
		}
		lang := b.Lang
		if kind == "book" {
			lang = release.BookLang(r.Title) // the language badge's (ScoredRelease.Lang)
		}
		if kind == "comic" && lang == "" {
			lang = want.DefaultLang
		}
		// the viewer's languages for this kind, best first
		if bonus := langpkg.Bonus(langpkg.Rank(p.MediaLangs(kind), lang)); bonus > 0 {
			score += bonus
			reasons = append(reasons, langpkg.In(lang))
		}
	}
	if r.Seeders < max(p.MinSources, 0) {
		return exclude("нет раздающих")
	}
	return seedersScore(r, score, reasons)
}
