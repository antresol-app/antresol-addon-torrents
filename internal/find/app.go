package find

// The app protocol (the core's apps reach it as /api/addons/torrents/x/app…):
// what the addon's web part shows on a title page — «Смотреть сейчас» and
// «Скачать» of a title that is not in the library, and the search for a
// title to add — described as data, so a native app draws the same buttons
// without knowing this addon. The routes are thin: the finding, the add,
// the progress and the watch are the services behind the web routes.
//
//	GET  /api/app                   the ways and the search this addon offers (404: none)
//	GET  /api/app/search?q=         titles to add or watch ({titles})
//	POST /api/app/step              one step of a way's choice ({title, rows, note, empty})
//	POST /api/app/run               start a way ({input}, {done}, {pick})
//	GET  /api/app/state?kind&tmdbId[&season]   the ways' progress ({ways})
//	POST /api/app/cancel            stop a way's download ({done})
//
// A row's ref is opaque to the app: it carries what the next step or the run
// needs (the season, the episode, the quality, the release's link), so
// every call stands alone. Every label is in the profile's language.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol/addonkit"
)

// appVersion is the protocol's version (AppInfo.v).
const appVersion = 1

// The ways' ids: opaque to the app, which only hands them back.
const (
	wayWatch = "watch"
	wayKeep  = "keep"
)

// maxAppRows bounds the releases one step lists (the best come first).
const maxAppRows = 40

// AppDeps are what the app routes need beside the web routes' Deps.
type AppDeps struct {
	Deps
	// HasSources says an enabled source exists (nil: yes); without one the
	// addon offers the app nothing.
	HasSources func(ctx context.Context) (bool, error)
	// HasMetadata says TMDB can be asked (nil: yes).
	HasMetadata func() bool
	// Seasons are a series' seasons with their episode counts (specials
	// left out).
	Seasons func(ctx context.Context, tmdbID int) ([]tmdb.SeasonCount, error)
	// PlayPinned streams the title's pinned release as the profile
	// (followapi.Deps.PlayPinned); found false: there is none. nil: the
	// server's own pick always.
	PlayPinned func(ctx context.Context, u addonkit.User, kind string, tmdbID int64, season, episode int) (id string, found bool, err error)
	// Watch registers a started stream's file as the profile's input
	// (watch.Deps.Watch), and WatchError writes its errors.
	Watch      func(ctx context.Context, u addonkit.User, ih string, idx int) (string, error)
	WatchError func(w http.ResponseWriter, r *http.Request, what string, err error)
}

// RegisterApp adds the app routes to mux (the profiles' /api/ routes).
func RegisterApp(mux *http.ServeMux, d AppDeps) {
	h := &appHandlers{handlers: handlers{d.Deps}, d: d}
	mux.Handle("GET /api/app", adults(h.offered(h.info)))
	mux.Handle("GET /api/app/search", adults(h.offered(h.search)))
	mux.Handle("POST /api/app/step", adults(h.offered(h.step)))
	mux.Handle("POST /api/app/run", adults(h.offered(h.run)))
	// a download that runs can be watched and stopped whatever the sources
	// do now
	mux.Handle("GET /api/app/state", adults(http.HandlerFunc(h.state)))
	mux.Handle("POST /api/app/cancel", adults(http.HandlerFunc(h.cancel)))
}

type appHandlers struct {
	handlers
	d AppDeps
}

// offered answers 404 while there is nothing to offer: no TMDB, or no
// enabled source.
func (h *appHandlers) offered(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.d.HasMetadata != nil && !h.d.HasMetadata() {
			http.NotFound(w, r)
			return
		}
		if h.d.HasSources != nil {
			ok, err := h.d.HasSources(r.Context())
			if err != nil {
				slog.Warn("find: app sources", "err", err)
				writeErr(w, r, http.StatusInternalServerError, "Внутренняя ошибка сервера")
				return
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
		}
		next(w, r)
	}
}

// tr is key in the profile's language.
func tr(r *http.Request, key string, args ...any) string { return i18n.T(r.Context(), key, args...) }

// --- the description ---

type appWayInfo struct {
	ID       string   `json:"id"`
	Label    string   `json:"label"`
	Icon     string   `json:"icon"`
	Role     string   `json:"role"`
	Kinds    []string `json:"kinds"`
	Scope    string   `json:"scope"`
	Episodes bool     `json:"episodes"`
}

type appSearchInfo struct {
	Label string `json:"label"`
}

type appInfo struct {
	V      int            `json:"v"`
	Ways   []appWayInfo   `json:"ways"`
	Search *appSearchInfo `json:"search,omitempty"`
}

// GET /api/app.
func (h *appHandlers) info(w http.ResponseWriter, r *http.Request) {
	kinds := []string{acquire.KindMovie, acquire.KindSeries}
	addonkit.WriteJSON(w, http.StatusOK, appInfo{
		V: appVersion,
		Ways: []appWayInfo{
			{ID: wayWatch, Label: tr(r, "Смотреть сейчас"), Icon: "play", Role: "watch", Kinds: kinds, Scope: "outside", Episodes: true},
			{ID: wayKeep, Label: tr(r, "Скачать"), Icon: "download", Role: "keep", Kinds: kinds, Scope: "outside", Episodes: false},
		},
		Search: &appSearchInfo{Label: tr(r, "Найти и добавить")},
	})
}

// --- the search ---

type appTitle struct {
	Kind          string `json:"kind"`
	TMDBID        int    `json:"tmdbId"`
	Title         string `json:"title"`
	Year          int    `json:"year,omitempty"`
	Poster        string `json:"poster,omitempty"`
	Overview      string `json:"overview,omitempty"`
	InLibrary     bool   `json:"inLibrary"`
	LibraryItemID *int64 `json:"libraryItemId,omitempty"`
}

// GET /api/app/search?q=: the movies and series TMDB knows by that name, in
// the profile's language.
func (h *appHandlers) search(w http.ResponseWriter, r *http.Request) {
	ctx := tmdb.WithLanguage(r.Context(), i18n.TMDB(i18n.FromContext(r.Context())))
	cs, err := h.Acquire.Discover(ctx, r.URL.Query().Get("q"))
	if err != nil {
		acquireErr(w, r, "app search", err)
		return
	}
	out := make([]appTitle, 0, len(cs))
	for _, c := range cs {
		if c.Kind != acquire.KindMovie && c.Kind != acquire.KindSeries {
			continue
		}
		t := appTitle{Kind: c.Kind, TMDBID: c.TMDBID, Title: c.Title, Year: c.Year, Poster: c.Poster, Overview: c.Overview,
			InLibrary: c.InLibrary}
		if c.InLibrary {
			t.LibraryItemID = c.LibraryItemID
		}
		out = append(out, t)
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"titles": out})
}

// --- the refs ---

// Stages a ref leads past: the season, the episode or the quality was
// chosen, or the release to start (a best pick has no link).
const (
	refSeason  = "s"
	refEpisode = "e"
	refQuality = "q"
	refRelease = "r"
	refBest    = "b"
)

// appRef is a row's ref: what the choice so far came to.
type appRef struct {
	K   string `json:"k"`
	S   int    `json:"s,omitempty"`
	E   int    `json:"e,omitempty"`
	Q   string `json:"q,omitempty"`
	L   string `json:"l,omitempty"`
	Src int64  `json:"src,omitempty"`
}

// maxRef bounds a ref (a magnet link is a few hundred bytes).
const maxRef = 16 << 10

func (a appRef) encode() string {
	b, _ := json.Marshal(a)
	return base64.RawURLEncoding.EncodeToString(b)
}

// qualityOK: a quality the add takes ("" is any).
func qualityOK(q string) bool { return q == "" || q == "720p" || q == "1080p" || q == "2160p" }

// parseRef reads a ref; nil for none. ok false: it is not one of ours.
func parseRef(s string) (ref *appRef, ok bool) {
	if s == "" {
		return nil, true
	}
	if len(s) > maxRef {
		return nil, false
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	var a appRef
	if json.Unmarshal(b, &a) != nil {
		return nil, false
	}
	switch a.K {
	case refSeason, refEpisode, refQuality, refRelease, refBest:
	default:
		return nil, false
	}
	if a.S < 0 || a.E < 0 || a.S > 10_000 || a.E > 100_000 || !qualityOK(a.Q) || a.Src < 0 {
		return nil, false
	}
	return &a, true
}

// --- the request ---

// appCall is the body of step and run.
type appCall struct {
	Way     string `json:"way"`
	Kind    string `json:"kind"`
	TMDBID  int    `json:"tmdbId"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Ref     string `json:"ref"`
	Quick   bool   `json:"quick"`
}

// readCall reads and checks a step or run body; false: the answer is
// written.
func (h *appHandlers) readCall(w http.ResponseWriter, r *http.Request) (appCall, *appRef, bool) {
	var in appCall
	if !decode(w, r, &in) {
		return in, nil, false
	}
	if in.Way != wayWatch && in.Way != wayKeep {
		writeErr(w, r, http.StatusNotFound, "Не найдено") // the way is gone: the app asks again what there is
		return in, nil, false
	}
	if in.TMDBID <= 0 || in.Season < 0 || in.Episode < 0 || (in.Kind != acquire.KindMovie && in.Kind != acquire.KindSeries) {
		writeErr(w, r, http.StatusBadRequest, "Неизвестный фильм или сериал")
		return in, nil, false
	}
	if in.Kind == acquire.KindMovie {
		in.Season, in.Episode = 0, 0
	}
	ref, ok := parseRef(in.Ref)
	if !ok {
		writeErr(w, r, http.StatusBadRequest, "Неверный запрос")
		return in, nil, false
	}
	return in, ref, true
}

// --- the steps ---

type appRow struct {
	Ref     string   `json:"ref"`
	Label   string   `json:"label"`
	Detail  string   `json:"detail,omitempty"`
	Badges  []string `json:"badges,omitempty"`
	Current bool     `json:"current"`
	Final   bool     `json:"final"`
}

type appStep struct {
	Title string   `json:"title"`
	Rows  []appRow `json:"rows"`
	Note  string   `json:"note,omitempty"`
	Empty string   `json:"empty,omitempty"`
}

// POST /api/app/step: a series asks its season (and, to watch, its
// episode) unless the call names them; then the way's own step: the quality
// of a download, and the releases.
func (h *appHandlers) step(w http.ResponseWriter, r *http.Request) {
	in, ref, ok := h.readCall(w, r)
	if !ok {
		return
	}
	series := in.Kind == acquire.KindSeries
	season, episode, quality, chosen := in.Season, in.Episode, "", false
	if ref != nil {
		switch ref.K {
		case refSeason:
			season, episode = ref.S, 0
		case refEpisode:
			season, episode = ref.S, ref.E
		case refQuality:
			season, quality, chosen = ref.S, ref.Q, true
		default: // a release is a final row: it has no step
			writeErr(w, r, http.StatusBadRequest, "Неверный запрос")
			return
		}
	}
	if in.Way == wayKeep {
		episode = 0 // a download is of a season
	}
	if series && season == 0 {
		h.seasonStep(w, r, in)
		return
	}
	if in.Way == wayWatch && series && episode == 0 {
		h.episodeStep(w, r, in, season)
		return
	}
	if in.Way == wayKeep && !chosen {
		h.qualityStep(w, r, season)
		return
	}
	h.releaseStep(w, r, in, season, episode, quality)
}

// seasonsOf are the series' seasons; false: the error is written.
func (h *appHandlers) seasonsOf(w http.ResponseWriter, r *http.Request, tmdbID int) ([]tmdb.SeasonCount, bool) {
	if h.d.Seasons == nil {
		return nil, true
	}
	seasons, err := h.d.Seasons(r.Context(), tmdbID)
	if err != nil {
		acquireErr(w, r, "app seasons", err)
		return nil, false
	}
	return seasons, true
}

func (h *appHandlers) seasonStep(w http.ResponseWriter, r *http.Request, in appCall) {
	seasons, ok := h.seasonsOf(w, r, in.TMDBID)
	if !ok {
		return
	}
	rows := make([]appRow, 0, len(seasons))
	for _, s := range seasons {
		row := appRow{Ref: appRef{K: refSeason, S: s.Number}.encode(), Label: tr(r, "Сезон %d", s.Number)}
		if s.Episodes > 0 {
			row.Detail = i18n.Plural(r.Context(), s.Episodes, "серия", "серии", "серий")
		}
		rows = append(rows, row)
	}
	addonkit.WriteJSON(w, http.StatusOK, appStep{Title: tr(r, "Сезон"), Rows: rows, Empty: tr(r, "Сезонов не нашлось")})
}

func (h *appHandlers) episodeStep(w http.ResponseWriter, r *http.Request, in appCall, season int) {
	seasons, ok := h.seasonsOf(w, r, in.TMDBID)
	if !ok {
		return
	}
	rows := []appRow{}
	for _, s := range seasons {
		if s.Number != season {
			continue
		}
		for n := 1; n <= s.Episodes; n++ {
			rows = append(rows, appRow{Ref: appRef{K: refEpisode, S: season, E: n}.encode(), Label: tr(r, "Серия %d", n)})
		}
	}
	addonkit.WriteJSON(w, http.StatusOK, appStep{Title: tr(r, "Серия"), Rows: rows, Empty: tr(r, "Серий не нашлось")})
}

// The download's qualities: any (the server's own pick), then each.
var appQualities = []string{"", "720p", "1080p", "2160p"}

func (h *appHandlers) qualityStep(w http.ResponseWriter, r *http.Request, season int) {
	rows := make([]appRow, 0, len(appQualities))
	for _, q := range appQualities {
		label := q
		switch q {
		case "":
			label = tr(r, "Любое")
		case "2160p":
			label = "4K"
		}
		rows = append(rows, appRow{Ref: appRef{K: refQuality, S: season, Q: q}.encode(), Label: label})
	}
	addonkit.WriteJSON(w, http.StatusOK, appStep{Title: tr(r, "Качество"), Rows: rows,
		Note: tr(r, "Antresol выберет лучшую раздачу в этом качестве, а если такой нет — лучшую в другом.")})
}

func (h *appHandlers) releaseStep(w http.ResponseWriter, r *http.Request, in appCall, season, episode int, quality string) {
	mine := h.myLanguages(r)
	// a list for a phone's step: the slow sources are not waited for
	resp, err := h.Acquire.Releases(indexer.WithSettle(r.Context(), indexer.SettleGrace, indexer.SettleCap),
		acquire.ReleaseRequest{TMDBID: in.TMDBID, Kind: in.Kind, Season: season, Episode: episode, Langs: mine}, nil)
	if err != nil {
		acquireErr(w, r, "app releases", err)
		return
	}
	resp = mineResponse(resp, mine)
	rows := []appRow{}
	if in.Way == wayKeep {
		// the server's pick in the quality asked, as «Скачать» of the web page
		detail := tr(r, "Любое качество")
		if quality != "" {
			detail = qualityLabel(quality)
		}
		rows = append(rows, appRow{Ref: appRef{K: refBest, S: season, Q: quality}.encode(), Label: tr(r, "Лучшая раздача"),
			Detail: detail, Final: true})
	}
	for _, rel := range resp.Releases {
		if len(rows) >= maxAppRows {
			break
		}
		link := rel.Link
		if link == "" {
			link = rel.Magnet
		}
		if link == "" {
			continue // nothing to start it by
		}
		rows = append(rows, releaseRow(r, rel.Info.Resolution, rel.Info.HDR, rel.Info.Codec, rel.Info.Source, rel.Info.IsSeasonPack,
			rel.Info.Audio, rel.Info.Subs, rel.Info.Studios, rel.Size, rel.Seeders, rel.SourceName, rel.Title,
			appRef{K: refRelease, S: season, E: episode, Q: quality, L: link, Src: rel.SourceID}))
	}
	note := ""
	if in.Way == wayWatch {
		note = tr(r, "Лучшие — сверху. Antresol учитывает качество, озвучку и сколько людей раздают.")
	}
	if n := failedNames(resp.Failed); n != "" {
		note = strings.TrimSpace(note + " " + tr(r, "Не ответили: %s. Остальные источники проверены.", n))
	}
	addonkit.WriteJSON(w, http.StatusOK, appStep{Title: tr(r, "Раздачи"), Rows: rows, Note: note,
		Empty: tr(r, "Подходящих раздач не нашлось.")})
}

func failedNames(failed []indexer.SourceError) string {
	names := make([]string, 0, len(failed))
	for _, f := range failed {
		if f.Name != "" {
			names = append(names, f.Name)
		}
	}
	return strings.Join(names, ", ")
}

// qualityLabel is a download quality as the picker names it.
func qualityLabel(q string) string {
	if q == "2160p" {
		return "4K"
	}
	return q
}

// releaseRow is a release as a row: the label says what it is (quality,
// codec, size), the second line who has it and how many share it, the badges
// the rest (the audio, the subtitles, a whole season).
func releaseRow(r *http.Request, resolution int, hdr bool, codec, source string, seasonPack bool, audio, subs, studios []string,
	size int64, seeders int, sourceName, title string, ref appRef) appRow {
	var parts []string
	switch {
	case resolution >= 2160:
		parts = append(parts, "4K")
	case resolution > 0:
		parts = append(parts, strconv.Itoa(resolution)+"p")
	}
	if hdr {
		parts = append(parts, "HDR")
	}
	if codec != "" {
		parts = append(parts, codec)
	}
	if size > 0 {
		parts = append(parts, sizeText(r, size))
	}
	label := strings.Join(parts, " · ")
	if label == "" {
		label = truncate(title, 80)
	}
	var detail []string
	if len(studios) > 0 {
		detail = append(detail, strings.Join(studios[:min(len(studios), 2)], ", "))
	}
	if sourceName != "" {
		detail = append(detail, sourceName)
	}
	detail = append(detail, tr(r, "Раздают: %d", seeders))
	var badges []string
	if source != "" {
		badges = append(badges, source)
	}
	if seasonPack {
		badges = append(badges, tr(r, "весь сезон"))
	}
	badges = append(badges, audio...)
	if b := subsBadge(r, subs); b != "" {
		badges = append(badges, b)
	}
	return appRow{Ref: ref.encode(), Label: label, Detail: strings.Join(detail, " · "), Badges: badges, Final: true}
}

// subsBadge is «Субтитры: RU, EN» («Субтитры» when named without a
// language); "" without subtitles.
func subsBadge(r *http.Request, subs []string) string {
	var langs []string
	for _, s := range subs {
		if s != "sub" {
			langs = append(langs, strings.ToUpper(s))
		}
	}
	switch {
	case len(langs) > 0:
		return tr(r, "Субтитры: %s", strings.Join(langs, ", "))
	case len(subs) > 0:
		return tr(r, "Субтитры")
	}
	return ""
}

func truncate(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n-1]) + "…"
}

// sizeText is a size as people read it: «12,3 ГБ», "12.3 GB".
func sizeText(r *http.Request, n int64) string {
	units := []string{"Б", "КБ", "МБ", "ГБ", "ТБ"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	num := strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
	if v >= 100 || i == 0 {
		num = strconv.FormatFloat(math.Round(v), 'f', 0, 64)
	}
	if i18n.FromContext(r.Context()) == i18n.RU {
		num = strings.Replace(num, ".", ",", 1)
	}
	return num + " " + tr(r, units[i])
}

// --- the run ---

// POST /api/app/run: «Смотреть сейчас» streams a release (the pinned one,
// else the server's pick, when none was chosen) and answers the core's
// input of its file; «Скачать» downloads one to the server. «quick» starts
// without a choice, or says {"pick": true} when it needs one.
func (h *appHandlers) run(w http.ResponseWriter, r *http.Request) {
	in, ref, ok := h.readCall(w, r)
	if !ok {
		return
	}
	var link string
	var src int64
	season, episode, quality := in.Season, in.Episode, ""
	if ref != nil {
		if ref.K != refRelease && ref.K != refBest {
			writeErr(w, r, http.StatusBadRequest, "Неверный запрос") // a step's row is not a start
			return
		}
		season, episode, quality, link, src = ref.S, ref.E, ref.Q, ref.L, ref.Src
		if in.Kind == acquire.KindMovie {
			season, episode = 0, 0
		}
	} else if !in.Quick {
		writeErr(w, r, http.StatusBadRequest, "Неверный запрос")
		return
	}
	if in.Kind == acquire.KindSeries && (season == 0 || (in.Way == wayWatch && episode == 0)) {
		// a series needs its season (and, to watch, its episode) first
		addonkit.WriteJSON(w, http.StatusOK, map[string]bool{"pick": true})
		return
	}
	if in.Way == wayKeep {
		h.runKeep(w, r, in, season, quality, link, src)
		return
	}
	h.runWatch(w, r, in, season, episode, link, src)
}

func (h *appHandlers) runWatch(w http.ResponseWriter, r *http.Request, in appCall, season, episode int, link string, src int64) {
	if h.d.Watch == nil {
		http.NotFound(w, r)
		return
	}
	u := user(r)
	ctx := r.Context()
	if link == "" && h.d.PlayPinned != nil {
		id, found, err := h.d.PlayPinned(ctx, u, in.Kind, int64(in.TMDBID), season, episode)
		if err != nil {
			h.watchErr(w, r, err)
			return
		}
		if found {
			addonkit.WriteJSON(w, http.StatusOK, map[string]string{"input": id})
			return
		}
	}
	actx := ctx
	if link == "" {
		// the server's pick: it starts with the best release of the sources
		// that answered first, not after the slowest one
		actx = indexer.WithSettle(ctx, indexer.SettleGrace, indexer.SettleCap)
	}
	res, err := h.acquireAs(actx, u, h.myLanguages(r), acquire.AcquireRequest{TMDBID: in.TMDBID, Kind: in.Kind,
		Season: season, Episode: episode, Mode: acquire.ModeStream, ReleaseLink: link, SourceID: src})
	if err != nil {
		acquireErr(w, r, "app watch", err)
		return
	}
	id, err := h.d.Watch(ctx, u, strings.ToLower(res.InfoHash), res.FileIndex)
	if err != nil {
		h.watchErr(w, r, err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]string{"input": id})
}

// watchErr writes an error of the watch or the pinned start.
func (h *appHandlers) watchErr(w http.ResponseWriter, r *http.Request, err error) {
	if h.d.WatchError != nil {
		h.d.WatchError(w, r, "app watch", err)
		return
	}
	acquireErr(w, r, "app watch", err)
}

func (h *appHandlers) runKeep(w http.ResponseWriter, r *http.Request, in appCall, season int, quality, link string, src int64) {
	res, err := h.acquireAs(r.Context(), user(r), h.myLanguages(r), acquire.AcquireRequest{TMDBID: in.TMDBID, Kind: in.Kind,
		Season: season, Mode: acquire.ModeDownload, ReleaseLink: link, SourceID: src, Quality: quality, Resolution: quality})
	if err != nil {
		acquireErr(w, r, "app download", err)
		return
	}
	msg := tr(r, "Загрузка началась")
	if in.Kind == acquire.KindSeries && season > 0 {
		msg = tr(r, "Загрузка сезона %d началась", season)
	}
	if res.Status == "waiting_space" {
		msg = tr(r, "Ждёт места на диске: загрузка начнётся, когда освободится место.")
	}
	if note := i18n.Localize(r.Context(), res.Note); note != "" {
		msg += " " + note
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]string{"done": msg})
}

// --- the state ---

type appWayState struct {
	Active   bool     `json:"active"`
	Fraction *float64 `json:"fraction,omitempty"`
	Text     string   `json:"text,omitempty"`
	Cancel   bool     `json:"cancel,omitempty"`
}

// titleOf reads kind, tmdbId and season of the query; false: 400 written.
func titleOf(w http.ResponseWriter, r *http.Request) (kind string, tmdbID, season int, ok bool) {
	q := r.URL.Query()
	kind = q.Get("kind")
	tmdbID, err1 := strconv.Atoi(q.Get("tmdbId"))
	var err2 error
	if v := q.Get("season"); v != "" {
		season, err2 = strconv.Atoi(v)
	}
	if err1 != nil || err2 != nil || tmdbID <= 0 || season < 0 || (kind != acquire.KindMovie && kind != acquire.KindSeries) {
		writeErr(w, r, http.StatusBadRequest, "Неизвестный фильм или сериал")
		return "", 0, 0, false
	}
	return kind, tmdbID, season, true
}

// GET /api/app/state?kind&tmdbId[&season]: the download's ring: how far it
// is, and that it can be stopped.
func (h *appHandlers) state(w http.ResponseWriter, r *http.Request) {
	kind, tmdbID, season, ok := titleOf(w, r)
	if !ok {
		return
	}
	st := appWayState{}
	if p, found := h.Acquire.TitleProgress(r.Context(), kind, tmdbID, season); found {
		if p.Size > 0 {
			f := math.Min(math.Max(float64(p.Done)/float64(p.Size), 0), 1)
			st.Fraction = &f
		}
		switch p.Status {
		case "active":
			st.Active, st.Cancel = true, true
			if st.Fraction != nil {
				st.Text = tr(r, "Скачано %d%%", int(*st.Fraction*100))
			}
		case "paused":
			st.Active, st.Cancel = true, true
			st.Text = tr(r, "Приостановлено")
		case "done":
			st.Text = tr(r, "Скачано")
		}
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"ways": map[string]appWayState{wayKeep: st}})
}

// POST /api/app/cancel {way, kind, tmdbId[, season]}: the title's unfinished
// downloads stop (finished ones stay).
func (h *appHandlers) cancel(w http.ResponseWriter, r *http.Request) {
	in, _, ok := h.readCall(w, r)
	if !ok {
		return
	}
	if in.Way != wayKeep {
		writeErr(w, r, http.StatusNotFound, "Не найдено")
		return
	}
	n, err := h.Acquire.CancelTitle(r.Context(), in.Kind, in.TMDBID, in.Season, "")
	if err != nil {
		acquireErr(w, r, "app cancel", err)
		return
	}
	msg := tr(r, "Загрузка отменена")
	if n == 0 {
		msg = tr(r, "Нечего отменять")
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]string{"done": msg})
}
