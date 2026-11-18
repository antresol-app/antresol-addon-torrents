package admin

import (
	"errors"
	"log"
	"net/http"
	"slices"

	"github.com/antresol-app/antresol-addon-torrents/internal/boot"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/lang"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/releaseprefs"
	"github.com/antresol-app/antresol/addonkit"
)

// settings: «Настройки загрузок» — the release preferences and «Раздача и
// место». «Не раздавать» has its own route and changes only through
// NoSeed.Set; the general save never touches it.
type settings struct{ Deps }

// settingsView is GET /api/admin/settings and the answer of its PUT.
type settingsView struct {
	UploadLimited  bool    `json:"uploadLimited"`
	UploadLimitKBs int64   `json:"uploadLimitKBs"` // KB/s while UploadLimited
	StreamCacheGB  float64 `json:"streamCacheGB"`  // «Кеш просмотра»
	// NoSeeding is the effective «Не раздавать» (forced or the setting);
	// NoSeedingForced: ANTRESOL_TORRENTS_NO_SEEDING=on, the row is locked.
	NoSeeding       bool `json:"noSeeding"`
	NoSeedingForced bool `json:"noSeedingForced"`
	Quality         int  `json:"quality"` // 720 | 1080 | 2160
	// Voice is the first of Voices (older clients send only it).
	Voice  string   `json:"voice"`
	Voices []string `json:"voices"` // «Озвучка» in order: dub and/or original
	// Languages: the order the language choices are offered in. The
	// core's «Языки» are not visible to an addon: the profile's language
	// first, then the catalog's.
	Languages      []string `json:"languages"`
	BookLangs      []string `json:"bookLangs"`
	AudiobookLangs []string `json:"audiobookLangs"`
	ComicLangs     []string `json:"comicLangs"`
	MaxSizeGB      float64  `json:"maxSizeGB"` // 0 = no limit
	PreferLossless bool     `json:"preferLossless"`
	MusicFormat    string   `json:"musicFormat"`
	MP3Bitrate     string   `json:"mp3Bitrate"`
	// OutsideFileRoots: holds outside the file browser's roots.
	OutsideFileRoots *int `json:"outsideFileRoots,omitempty"`
	// FlareSolverrURL: the address saved here ("" = none; its own PUT
	// writes it); FlareSolverrServer: the server's, used while none is saved.
	FlareSolverrURL    string `json:"flareSolverrUrl"`
	FlareSolverrServer string `json:"flareSolverrServer"`
}

// settingsIn is a PUT's body: a field left out keeps its value; noSeeding*,
// languages, outsideFileRoots and the FlareSolverr fields (their own PUT)
// are not set here and are ignored.
type settingsIn struct {
	UploadLimited  *bool     `json:"uploadLimited"`
	UploadLimitKBs *int64    `json:"uploadLimitKBs"`
	StreamCacheGB  *float64  `json:"streamCacheGB"`
	Quality        *int      `json:"quality"`
	Voice          *string   `json:"voice"`
	Voices         *[]string `json:"voices"`
	BookLangs      *[]string `json:"bookLangs"`
	AudiobookLangs *[]string `json:"audiobookLangs"`
	ComicLangs     *[]string `json:"comicLangs"`
	MaxSizeGB      *float64  `json:"maxSizeGB"`
	PreferLossless *bool     `json:"preferLossless"`
	MusicFormat    *string   `json:"musicFormat"`
	MP3Bitrate     *string   `json:"mp3Bitrate"`
}

// order is the language order of the request's profile: its «Языки»
// (Deps.Languages), else its interface language, first; then the
// other known languages (lang.Codes), so a list ordered by it (the books'
// languages) keeps every language it holds.
func (s *settings) order(r *http.Request) []string {
	var first []string
	if s.Languages != nil {
		first = s.Languages(r.Context(), userID(r))
	}
	if len(first) == 0 {
		if l := i18n.FromContext(r.Context()); lang.Known(l) {
			first = []string{l}
		}
	}
	out := []string{}
	for _, c := range append(slices.Clone(first), lang.Codes...) {
		if lang.Known(c) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

func (s *settings) load(r *http.Request) (settingsView, releaseprefs.Prefs, error) {
	ctx := r.Context()
	var v settingsView
	p := releaseprefs.Default()
	if s.Sources != nil {
		var err error
		if p, err = s.Sources.Prefs(ctx); err != nil {
			return v, p, err
		}
	}
	var err error
	if v.UploadLimited, v.UploadLimitKBs, err = s.Settings.Upload(ctx); err != nil {
		return v, p, err
	}
	if v.StreamCacheGB, err = s.Settings.StreamCacheGB(ctx, s.StreamCacheDefaultGB); err != nil {
		return v, p, err
	}
	// an unreadable setting reads as on (it never seeds): shown on
	on, nerr := s.NoSeed.Effective(ctx)
	if nerr != nil {
		log.Printf("admin: no-seeding setting: %v", nerr)
	}
	v.NoSeeding, v.NoSeedingForced = on, s.NoSeed.Forced()
	v.Languages = s.order(r)
	v.Quality, v.MaxSizeGB, v.PreferLossless = p.MaxHeight, p.MaxSizeGB, p.PreferLossless
	v.MusicFormat, v.MP3Bitrate = p.MusicFormatOrDefault(), p.MP3BitrateOrDefault()
	v.PreferLossless = v.MusicFormat == releaseprefs.MusicLossless
	v.Voices = p.Voices
	if len(v.Voices) == 0 {
		v.Voices = []string{"dub"}
		if p.PreferOriginal || !p.PreferDub {
			v.Voices = []string{"original"}
		}
	}
	v.Voice = v.Voices[0]
	v.BookLangs = lang.Ordered(p.MediaLangs("book"), v.Languages)
	v.AudiobookLangs = lang.Ordered(p.MediaLangs("audiobook"), v.Languages)
	v.ComicLangs = lang.Ordered(p.MediaLangs("comic"), v.Languages)
	if v.FlareSolverrURL, err = s.Settings.FlareSolverrURL(ctx); err != nil {
		return v, p, err
	}
	v.FlareSolverrServer = s.FlareSolverrServer
	if s.Outside != nil {
		n := s.Outside()
		v.OutsideFileRoots = &n
	}
	return v, p, nil
}

func (s *settings) get(w http.ResponseWriter, r *http.Request) {
	v, _, err := s.load(r)
	if err != nil {
		internalErr(w, r, "settings", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, v)
}

// validate returns a Russian message for an invalid value ("" = fine).
func (v settingsView) validate() string {
	switch {
	case v.Quality != 720 && v.Quality != 1080 && v.Quality != 2160:
		return "Выберите качество: 720p, 1080p или 4K"
	case v.Voice != "dub" && v.Voice != "original":
		return "Выберите озвучку: русская или оригинал"
	case v.MaxSizeGB < 0 || v.MaxSizeGB > 1000:
		return "Размер фильма — от 0 до 1000 ГБ"
	case v.StreamCacheGB < 1 || v.StreamCacheGB > 10000:
		return "Кеш просмотра — от 1 ГБ"
	case v.UploadLimited && (v.UploadLimitKBs < 1 || v.UploadLimitKBs > 10_000_000):
		return "Укажите скорость отдачи больше нуля"
	case !lang.Valid(v.BookLangs, 0) || !lang.Valid(v.AudiobookLangs, 0) || !lang.Valid(v.ComicLangs, 0):
		return "Неизвестный язык"
	case !validVoices(v.Voices):
		return "Выберите озвучку: русская или оригинал"
	case !releaseprefs.ValidMusicFormat(v.MusicFormat):
		return "Неизвестный формат музыки"
	case !releaseprefs.ValidMP3Bitrate(v.MP3Bitrate):
		return "Неизвестный битрейт"
	}
	return ""
}

func validVoices(l []string) bool {
	if len(l) < 1 || len(l) > 2 {
		return false
	}
	for i, x := range l {
		if x != "dub" && x != "original" || i > 0 && x == l[0] {
			return false
		}
	}
	return true
}

// PUT /api/admin/settings: the fields sent change, the rest keep; the
// answer is the whole settings object. The upload limit and the stream
// cache apply at once. «Не раздавать» is not touched.
func (s *settings) put(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	v, p, err := s.load(r)
	if err != nil {
		internalErr(w, r, "settings", err)
		return
	}
	var in settingsIn
	if !decode(w, r, &in) {
		return
	}
	set := func(dst *bool, src *bool) {
		if src != nil {
			*dst = *src
		}
	}
	set(&v.UploadLimited, in.UploadLimited)
	if in.UploadLimitKBs != nil {
		v.UploadLimitKBs = *in.UploadLimitKBs
	}
	if in.StreamCacheGB != nil {
		v.StreamCacheGB = *in.StreamCacheGB
	}
	if in.Quality != nil {
		v.Quality = *in.Quality
	}
	if in.MaxSizeGB != nil {
		v.MaxSizeGB = *in.MaxSizeGB
	}
	for _, f := range []struct {
		dst *[]string
		src *[]string
	}{{&v.BookLangs, in.BookLangs}, {&v.AudiobookLangs, in.AudiobookLangs}, {&v.ComicLangs, in.ComicLangs}} {
		if f.src != nil {
			*f.dst = *f.src
		}
	}
	// «Озвучка»: the list, else an older client's one choice, else as stored
	switch {
	case in.Voices != nil:
		v.Voices = *in.Voices
	case in.Voice != nil:
		v.Voices = []string{*in.Voice}
	}
	v.Voice = ""
	if len(v.Voices) > 0 {
		v.Voice = v.Voices[0]
	}
	// «Формат загрузки»: as sent; a client that knows only «без потерь»
	// switches lossless ↔ mp3; «Битрейт» as sent, else kept
	switch {
	case in.MusicFormat != nil:
		v.MusicFormat = *in.MusicFormat
	case in.PreferLossless != nil && *in.PreferLossless:
		v.MusicFormat = releaseprefs.MusicLossless
	case in.PreferLossless != nil && v.MusicFormat == releaseprefs.MusicLossless:
		v.MusicFormat = releaseprefs.MusicMP3
	}
	if in.MP3Bitrate != nil {
		v.MP3Bitrate = *in.MP3Bitrate
	}
	if msg := v.validate(); msg != "" {
		writeErr(w, r, http.StatusBadRequest, msg)
		return
	}
	v.BookLangs = lang.Ordered(v.BookLangs, v.Languages)
	v.AudiobookLangs = lang.Ordered(v.AudiobookLangs, v.Languages)
	v.ComicLangs = lang.Ordered(v.ComicLangs, v.Languages)

	if s.Sources != nil {
		p.MaxHeight, p.MaxSizeGB = v.Quality, v.MaxSizeGB
		p.Voices = v.Voices
		p.PreferDub, p.PreferOriginal = lang.Rank(v.Voices, "dub") >= 0, lang.Rank(v.Voices, "original") >= 0
		p.BookLangs, p.AudiobookLangs, p.ComicLangs = v.BookLangs, v.AudiobookLangs, v.ComicLangs
		p.MusicFormat, p.MP3Bitrate = v.MusicFormat, v.MP3Bitrate
		p.PreferLossless = p.MusicFormat == releaseprefs.MusicLossless
		if err := s.Sources.SavePrefs(ctx, p); err != nil {
			addonkit.WriteError(w, http.StatusBadRequest, i18n.Message(ctx, err))
			return
		}
	}
	if err := s.Settings.SetUpload(ctx, v.UploadLimited, v.UploadLimitKBs); err != nil {
		internalErr(w, r, "settings save", err)
		return
	}
	if err := s.Settings.SetStreamCacheGB(ctx, v.StreamCacheGB); err != nil {
		internalErr(w, r, "settings save", err)
		return
	}
	if s.Upload != nil {
		bps := int64(0)
		if v.UploadLimited && v.UploadLimitKBs > 0 {
			bps = v.UploadLimitKBs * 1024
		}
		s.Upload.SetUploadLimit(bps)
	}
	if s.Acquire != nil {
		s.Acquire.SetStreamCacheBytes(int64(v.StreamCacheGB * (1 << 30)))
	}
	out, _, err := s.load(r)
	if err != nil {
		internalErr(w, r, "settings", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, out)
}

// PUT /api/admin/settings/no-seeding {"on": bool} → 204 once the state
// file and the engine follow (NoSeed.Set); forced by the server → 409
// «Задано на сервере», nothing changes.
func (s *settings) putNoSeeding(w http.ResponseWriter, r *http.Request) {
	var in struct {
		On *bool `json:"on"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.On == nil {
		writeErr(w, r, http.StatusBadRequest, "Неверный запрос")
		return
	}
	if err := s.NoSeed.Set(r.Context(), *in.On); err != nil {
		if errors.Is(err, boot.ErrForced) {
			writeErr(w, r, http.StatusConflict, "Задано на сервере")
			return
		}
		internalErr(w, r, "no-seeding", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
