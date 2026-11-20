package indexer

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// What a release says of its audio tracks beyond the voice-over kinds
// release.Parse finds: how many tracks there are and in which languages
// («2xRus», «Dub + MVO + Eng», «DUAL», «Multi», «Rus, Eng», a page's
// «Аудио 1:… Аудио 2:…»). A release with the original track and a voice-over
// suits more of the household than one with a single Russian track.

// tracksInfo is the audio tracks of a release (0 = unknown).
type tracksInfo struct {
	Tracks int
	// Langs: the languages of the audio named ("ru", "en", "uk", …;
	// "orig" for an original track of an unnamed language).
	Langs []string
}

func (t *tracksInfo) addLang(l string) {
	if l != "" && !slices.Contains(t.Langs, l) {
		t.Langs = append(t.Langs, l)
	}
}

// audioLangWords are the language words of a release's audio, lower case.
var audioLangWords = map[string]string{
	"rus": "ru", "russian": "ru", "рус": "ru", "русский": "ru",
	"eng": "en", "english": "en", "англ": "en", "английский": "en",
	"ukr": "uk", "ukrainian": "uk", "укр": "uk", "украинский": "uk",
	"ger": "de", "deu": "de", "german": "de",
	"fre": "fr", "fra": "fr", "french": "fr",
	"ita": "it", "italian": "it", "spa": "es", "spanish": "es",
	"jap": "ja", "jpn": "ja", "japanese": "ja", "kor": "ko", "korean": "ko",
	"chi": "zh", "chn": "zh", "chinese": "zh",
}

// voiceKinds are a Russian voice-over's kinds: each one named is a track.
var voiceKinds = map[string]bool{"dub": true, "mvo": true, "dvo": true, "avo": true}

var (
	// «2xRus», «3 x MVO», «2хRus» (a Cyrillic х)
	reTracksN = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])(\d)\s*[xх×]\s*(rus|eng|ukr|dub|mvo|dvo|avo|sub)\b`)
	reDual    = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])dual(?:[\s._-]*audio)?(?:$|[^\p{L}\p{N}])`)
	reMulti   = regexp.MustCompile(`(?i)(?:^|[^\p{L}\p{N}])multi(?:[\s._-]*audio)?(?:$|[^\p{L}\p{N}])`)
	reWord    = regexp.MustCompile(`[\p{L}\p{N}]+`)
	reSubWord = regexp.MustCompile(`(?i)^(sub|subs|subtitles|субтитры|сабы|engsub|russub|hardsub|softsub)$`)
)

// parseTracks reads the audio tracks a release name names (only after the
// title, where release.Parse finds the technical part).
func parseTracks(raw string, info release.Info) tracksInfo {
	var t tracksInfo
	// after the names («Английский пациент / The English Patient») and the year
	tail := raw
	for _, name := range []string{info.Title, info.AltTitle} {
		if name == "" {
			continue
		}
		if i := strings.Index(tail, name); i >= 0 {
			tail = tail[i+len(name):]
		}
	}
	if info.Year > 0 {
		if i := strings.Index(tail, strconv.Itoa(info.Year)); i >= 0 {
			tail = tail[i+4:]
		}
	}
	explicit := 0
	counted := map[string]bool{} // languages «Nx…» counted already
	for _, m := range reTracksN.FindAllStringSubmatch(tail, -1) {
		kind := strings.ToLower(m[2])
		if kind == "sub" {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		explicit += n
		l := "ru"
		switch kind {
		case "eng":
			l = "en"
		case "ukr":
			l = "uk"
		}
		counted[l] = true
		t.addLang(l)
	}
	// the language words, not those of the subtitles («Sub Rus, Eng»)
	spans := reWord.FindAllStringIndex(tail, -1)
	words := make([]string, len(spans))
	for i, sp := range spans {
		words[i] = tail[sp[0]:sp[1]]
	}
	// «Eng Sub», «Rus.Subs»: a language word right before a subtitle marker
	// (only a space, a dot or a dash between) is the subtitles'
	subsNext := func(i int) bool {
		if i+1 >= len(words) || !reSubWord.MatchString(strings.ToLower(words[i+1])) {
			return false
		}
		return strings.Trim(tail[spans[i][1]:spans[i+1][0]], " ._-") == ""
	}
	inSubs := false
	kinds := map[string]bool{}
	for i, w := range words {
		lw := strings.ToLower(w)
		if reSubWord.MatchString(lw) {
			inSubs = true
			continue
		}
		lang, isLang := audioLangWords[lw]
		if !isLang {
			inSubs = false
			if voiceKinds[lw] {
				kinds[lw] = true
				t.addLang("ru")
			}
			continue
		}
		if inSubs || subsNext(i) {
			continue
		}
		t.addLang(lang)
	}
	for _, a := range info.Audio {
		switch a {
		case "Dub", "MVO", "AVO":
			kinds[strings.ToLower(a)] = true
			t.addLang("ru")
		case "Original":
			if !slices.Contains(t.Langs, "en") {
				t.addLang("orig")
			}
		}
	}
	// the Russian voice-overs are one track each, every other language one;
	// «Nx…» counts its own, the languages it did not name come on top
	others, uncounted := 0, 0
	for _, l := range t.Langs {
		if l == "ru" {
			continue
		}
		others++
		if !counted[l] {
			uncounted++
		}
	}
	russian := len(kinds)
	if russian == 0 && slices.Contains(t.Langs, "ru") {
		russian = 1
	}
	t.Tracks = russian + others
	if explicit > 0 {
		t.Tracks = max(t.Tracks, explicit+uncounted)
	}
	if reDual.MatchString(tail) || reMulti.MatchString(tail) {
		t.Tracks = max(t.Tracks, 2)
	}
	return t
}

// mergeTracks is the name's tracks with what the release page said.
func mergeTracks(t tracksInfo, page tracksInfo) tracksInfo {
	t.Tracks = max(t.Tracks, page.Tracks)
	for _, l := range page.Langs {
		t.addLang(l)
	}
	return t
}

// subLangWords are the language words of a page's «Субтитры:» line.
var subLangWords = []struct{ word, lang string }{
	{"рус", "ru"}, {"russian", "ru"}, {"rus", "ru"},
	{"англ", "en"}, {"english", "en"}, {"eng", "en"},
	{"укр", "uk"}, {"ukr", "uk"}, {"ukrainian", "uk"},
	{"нем", "de"}, {"german", "de"}, {"франц", "fr"}, {"french", "fr"},
	{"испан", "es"}, {"spanish", "es"}, {"итал", "it"}, {"italian", "it"},
	{"япон", "ja"}, {"japanese", "ja"}, {"корей", "ko"}, {"korean", "ko"},
	{"китай", "zh"}, {"chinese", "zh"},
}

var (
	// «Аудио 1:», «Аудио #2:», «Audio 3:», «Аудиодорожка 2:», «Перевод 2:»
	reAudioNLine = regexp.MustCompile(`(?im)^[\s*•·\-]*(?:аудио|audio|звук|аудиодорожк\p{L}*|дорожк\p{L}*|перевод)\s*#?\s*(\d{1,2})\s*[:：]\s*(.*)$`)
	reSubsLine   = regexp.MustCompile(`(?im)^[\s*•·\-]*(?:субтитры|subtitles|subs)\s*[:：]\s*(.+)$`)
	reNoSubs     = regexp.MustCompile(`(?i)^\s*(?:нет|отсутствуют|no|none)(?:$|[^\p{L}])`)
)

// pageTracks reads a release page's audio lines (one per track) and its
// «Субтитры:» line.
func pageTracks(text string) (tracksInfo, []string) {
	var t tracksInfo
	seen := map[string]bool{}
	for _, m := range reAudioNLine.FindAllStringSubmatch(text, 20) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		low := strings.ToLower(m[2])
		switch {
		case strings.Contains(low, "англ") || strings.Contains(low, "english") || strings.Contains(low, "eng"):
			t.addLang("en")
		case strings.Contains(low, "укр") || strings.Contains(low, "ukr"):
			t.addLang("uk")
		case strings.Contains(low, "оригинал") || strings.Contains(low, "original"):
			t.addLang("orig")
		case strings.Contains(low, "рус") || strings.Contains(low, "дубл") || strings.Contains(low, "голос") || strings.Contains(low, "авторск"):
			t.addLang("ru")
		}
	}
	t.Tracks = len(seen)
	var subs []string
	if m := reSubsLine.FindStringSubmatch(text); m != nil && !reNoSubs.MatchString(m[1]) {
		low := strings.ToLower(m[1])
		for _, w := range subLangWords {
			if strings.Contains(low, w.word) && !slices.Contains(subs, w.lang) {
				subs = append(subs, w.lang)
			}
		}
		if len(subs) == 0 {
			subs = []string{"sub"}
		}
	}
	return t, subs
}
