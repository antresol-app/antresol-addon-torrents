package followapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/pinned"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// maxOwnTorrent bounds an own .torrent file (a long series' metafile is a
// few MB at most): the decoded file, as before the move.
const maxOwnTorrent = 8 << 20

// maxOwnBody bounds the own route's JSON body, read before decoding: the
// file's base64 (4/3 of maxOwnTorrent, ≈ 10.7 MiB) and the rest of the
// JSON. The core's proxy sets no body limit of its own.
const maxOwnBody = 12 << 20

// User-facing messages of the pin routes (catalog keys).
const (
	msgPinNoInput    = "Укажите magnet-ссылку, ссылку на раздачу или торрент-файл"
	msgPinNotTorrent = "Это не торрент-файл"
	msgPinNotPinned  = "Раздача не закреплена"
	msgPinNoEpisodes = "Укажите серии для загрузки"
	msgPinNoSuchEps  = "В раздаче нет этих серий"
	msgPinTooLarge   = "Торрент-файл слишком большой"
	msgBadRequest    = "Неверный запрос"
)

// The household's pinned release of a title. Any adult profile pins,
// unpins, refreshes and accepts a suggestion (as with follows). A new pin,
// an accepted suggestion and a refresh that changed the release have the
// core rescan the title soon (its kick): its sources, the feed and a
// follow's missing episodes follow the new release without waiting for
// the next round. /play is «Смотреть» of an episode from the pin: a
// stream, registered as the profile's input; /download downloads
// given episodes of it to the server.
func registerPinned(mux *http.ServeMux, p *pinnedAPI) {
	const base = "/api/pinned/{kind}/{tmdbId}"
	mux.Handle("GET "+base, adults(p.get))
	mux.Handle("POST "+base, adults(p.pinRelease))
	mux.Handle("DELETE "+base, adults(p.unpin))
	mux.Handle("POST "+base+"/own", adults(p.own))
	mux.Handle("POST "+base+"/refresh", adults(p.refresh))
	mux.Handle("POST "+base+"/accept-suggestion", adults(p.accept))
	if p.Watch != nil {
		mux.Handle("POST "+base+"/play", adults(p.playEpisode))
	}
	mux.Handle("POST "+base+"/download", adults(p.downloadEpisodes))
}

type pinnedAPI struct{ Deps }

func (p *pinnedAPI) tracking() pinned.Tracking { return p.Pins.Tracking }

// kick asks the core to rescan the title soon; a failure is logged (the
// title's next round comes anyway).
func (p *pinnedAPI) kick(ctx context.Context, id int64) {
	if err := p.tracking().Kick(ctx, id); err != nil && ctx.Err() == nil {
		slog.Warn("followapi: pinned title kick", "title", id, "err", err)
	}
}

type suggestionOut struct {
	ReleaseTitle string    `json:"releaseTitle"`
	Episodes     int       `json:"episodes"`
	Found        time.Time `json:"found"`
}

type pinOut struct {
	SourceID     string `json:"sourceId"`
	Link         string `json:"link"`
	DetailsURL   string `json:"details"`
	ReleaseTitle string `json:"releaseTitle"`
	InfoHash     string `json:"infoHash"`
	Quality      string `json:"quality"`
	// Episodes: how many episodes the release holds (a film: 0).
	Episodes    int        `json:"episodes"`
	PinnedAt    time.Time  `json:"pinnedAt"`
	LastChecked *time.Time `json:"lastChecked"`
	// LastError: why the last refresh kept the pin, in the request's
	// language ("" = fine).
	LastError  string         `json:"lastError"`
	Suggestion *suggestionOut `json:"suggestion"`
}

func pinJSON(ctx context.Context, kind string, p pinned.Pin) *pinOut {
	o := &pinOut{SourceID: p.SourceID, Link: p.Link, DetailsURL: p.DetailsURL, ReleaseTitle: p.ReleaseTitle, InfoHash: p.InfoHash,
		Quality: p.Quality, PinnedAt: p.PinnedAt, LastError: i18n.Localize(ctx, p.LastError)}
	if kind == "series" {
		o.Episodes = len(pinned.EpisodesOf(p.Files, 1))
	}
	if !p.LastChecked.IsZero() {
		lc := p.LastChecked
		o.LastChecked = &lc
	}
	if sg := p.Suggestion; sg != nil {
		o.Suggestion = &suggestionOut{ReleaseTitle: sg.ReleaseTitle, Episodes: sg.Episodes, Found: sg.Found}
	}
	return o
}

// pinErr maps the pin service's errors: its own (catalog keys) are the
// person's to fix; a source or the network that did not give the release
// is a 502; the rest is logged.
func pinErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	var ie *i18n.Error
	switch {
	case errors.Is(err, pinned.ErrNotPinned):
		writeErr(w, r, http.StatusNotFound, msgPinNotPinned)
	case errors.As(err, &ie):
		unprocessable(w, r, err)
	case errors.Is(err, indexer.ErrNotFound):
		writeErr(w, r, http.StatusNotFound, "Источник не найден")
	default:
		if _, ok := acquire.UserMessage(err); ok {
			acquireErr(w, r, what, err)
			return
		}
		slog.Warn("followapi: "+what, "err", err)
		writeErr(w, r, http.StatusBadGateway, "Не удалось получить раздачу — проверьте ссылку")
	}
}

// unprocessable writes a person's mistake (a catalog key or an error
// whose text is one) in the request's language.
func unprocessable(w http.ResponseWriter, r *http.Request, err error) {
	addonkit.WriteError(w, http.StatusUnprocessableEntity, i18n.Message(r.Context(), err))
}

// titleParams reads {kind} and {tmdbId}; false: 400 written.
func titleParams(w http.ResponseWriter, r *http.Request) (string, int64, bool) {
	kind := r.PathValue("kind")
	id, err := strconv.ParseInt(r.PathValue("tmdbId"), 10, 64)
	if kind != "series" && kind != "movie" {
		writeErr(w, r, http.StatusBadRequest, "Неизвестный раздел")
		return "", 0, false
	}
	if err != nil || id <= 0 {
		writeErr(w, r, http.StatusBadRequest, "Неверный id")
		return "", 0, false
	}
	return kind, id, true
}

// lookup is {kind}/{tmdbId}: its tracked title at the core (found false:
// nobody tracks it yet) once the profile may see it (titleVisible; 404
// when hidden).
func (p *pinnedAPI) lookup(w http.ResponseWriter, r *http.Request) (kind string, t tracking.Title, found, ok bool) {
	kind, tmdbID, ok := titleParams(w, r)
	if !ok {
		return "", t, false, false
	}
	t, found, err := p.tracking().TitleByTMDB(r.Context(), kind, tmdbID)
	if err != nil {
		internalErr(w, r, "pinned title", err)
		return "", t, false, false
	}
	if !found {
		t = tracking.Title{Kind: kind, TMDBID: tmdbID} // gated as it would be once made
	}
	ok, err = titleVisible(r.Context(), p.Library, user(r), t)
	if err != nil {
		internalErr(w, r, "pinned gate", err)
		return "", t, false, false
	}
	if !ok {
		writeErr(w, r, http.StatusNotFound, "Не найдено") // a hidden title is not told apart
		return "", t, false, false
	}
	return kind, t, found, true
}

// ensure is lookup's title, made at the core when absent (name and poster
// from the page, for a title off the library). Callers check their input
// first: a refused pin leaves no tracked title behind.
func (p *pinnedAPI) ensure(w http.ResponseWriter, r *http.Request, name, poster string) (string, tracking.Title, bool) {
	kind, t, found, ok := p.lookup(w, r)
	if !ok {
		return "", t, false
	}
	if found {
		return kind, t, true
	}
	t, err := p.tracking().EnsureTitle(r.Context(), kind, t.TMDBID, strings.TrimSpace(name), poster)
	if err != nil {
		internalErr(w, r, "pinned title", err)
		return "", t, false
	}
	return kind, t, true
}

// checkLink: link may be pinned for source sourceID (a magnet for any);
// false with the error written.
func (p *pinnedAPI) checkLink(w http.ResponseWriter, r *http.Request, sourceID int64, link string) bool {
	if strings.HasPrefix(strings.ToLower(link), "magnet:") {
		return true
	}
	if sourceID <= 0 {
		writeErr(w, r, http.StatusBadRequest, "Источник не найден")
		return false
	}
	if err := p.Pins.Indexer.CheckReleaseLink(r.Context(), sourceID, link); err != nil {
		if errors.Is(err, indexer.ErrNotFound) {
			pinErr(w, r, "pin link", err)
		} else {
			unprocessable(w, r, err)
		}
		return false
	}
	return true
}

// ownLinkKnown: a pasted topic URL is on the site of an enabled source
// (PinOwn tries those); false with ErrNoSource written.
func (p *pinnedAPI) ownLinkKnown(w http.ResponseWriter, r *http.Request, link string) bool {
	srcs, err := p.Pins.Indexer.Sources(r.Context())
	if err != nil {
		internalErr(w, r, "pin sources", err)
		return false
	}
	for _, s := range srcs {
		if s.Enabled && p.Pins.Indexer.CheckReleaseLink(r.Context(), s.ID, link) == nil {
			return true
		}
	}
	unprocessable(w, r, pinned.ErrNoSource)
	return false
}

func (p *pinnedAPI) get(w http.ResponseWriter, r *http.Request) {
	kind, t, found, ok := p.lookup(w, r)
	if !ok {
		return
	}
	var out *pinOut
	if found {
		pin, ok, err := p.Pins.ByTitle(r.Context(), t.ID)
		if err != nil {
			internalErr(w, r, "pinned get", err)
			return
		}
		if ok {
			out = pinJSON(r.Context(), kind, pin)
		}
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"pin": out})
}

func (p *pinnedAPI) pinRelease(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SourceID     int64  `json:"sourceId"`
		Link         string `json:"link"`
		Details      string `json:"details"`
		ReleaseTitle string `json:"releaseTitle"`
		Name         string `json:"name"`
		Poster       string `json:"poster"` // TMDB path
	}
	if !decode(w, r, &in) {
		return
	}
	in.Link = strings.TrimSpace(in.Link)
	if in.Link == "" {
		writeErr(w, r, http.StatusBadRequest, msgPinNoInput)
		return
	}
	if !p.checkLink(w, r, in.SourceID, in.Link) {
		return
	}
	kind, t, ok := p.ensure(w, r, in.Name, in.Poster)
	if !ok {
		return
	}
	src := ""
	if in.SourceID > 0 {
		src = strconv.FormatInt(in.SourceID, 10)
	}
	pin, err := p.Pins.PinRelease(r.Context(), t.ID, src, in.Link, in.Details, in.ReleaseTitle, user(r))
	if err != nil {
		pinErr(w, r, "pin release", err)
		return
	}
	p.kick(r.Context(), t.ID)
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"pin": pinJSON(r.Context(), kind, pin)})
}

// ownFile is the own route's .torrent file: the base64 of the JSON's
// "file", at most maxOwnTorrent decoded (checked from the base64
// length first, then on the bytes). ok false: 400 written.
func ownFile(w http.ResponseWriter, r *http.Request, b64 string) ([]byte, bool) {
	b64 = strings.TrimSpace(b64)
	if base64.RawStdEncoding.DecodedLen(len(strings.TrimRight(b64, "="))) > maxOwnTorrent {
		writeErr(w, r, http.StatusBadRequest, msgPinTooLarge)
		return nil, false
	}
	file, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		file, err = base64.RawStdEncoding.DecodeString(b64)
	}
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, msgBadRequest)
		return nil, false
	}
	if len(file) > maxOwnTorrent {
		writeErr(w, r, http.StatusBadRequest, msgPinTooLarge)
		return nil, false
	}
	return file, true
}

// own: JSON {input} (a magnet or a topic URL) or {file} (a .torrent file
// as base64), with optional name and poster. The proxy forwards JSON
// only: a multipart form is refused.
func (p *pinnedAPI) own(w http.ResponseWriter, r *http.Request) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); strings.HasPrefix(mt, "multipart/") {
		writeErr(w, r, http.StatusBadRequest, msgBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOwnBody+1))
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, msgBadRequest)
		return
	}
	if len(body) > maxOwnBody {
		writeErr(w, r, http.StatusBadRequest, msgPinTooLarge)
		return
	}
	var in struct {
		Input  string `json:"input"`
		File   string `json:"file"`
		Name   string `json:"name"`
		Poster string `json:"poster"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, r, http.StatusBadRequest, msgBadRequest)
		return
	}
	var file []byte
	if in.File != "" {
		var ok bool
		if file, ok = ownFile(w, r, in.File); !ok {
			return
		}
	}
	input := strings.TrimSpace(in.Input)
	// the input first: a refused one leaves no tracked title behind
	switch {
	case in.File != "":
		if _, _, _, err := torrent.ParseFiles(file); err != nil {
			addonkit.WriteError(w, http.StatusUnprocessableEntity, i18n.Localize(r.Context(), msgPinNotTorrent))
			return
		}
	case input == "":
		addonkit.WriteError(w, http.StatusUnprocessableEntity, i18n.Localize(r.Context(), msgPinNoInput))
		return
	case !strings.HasPrefix(strings.ToLower(input), "magnet:"):
		if !p.ownLinkKnown(w, r, input) {
			return
		}
	}
	kind, t, ok := p.ensure(w, r, in.Name, in.Poster)
	if !ok {
		return
	}
	pin, err := p.Pins.PinOwn(r.Context(), t.ID, input, file, user(r))
	if err != nil {
		pinErr(w, r, "pin own", err)
		return
	}
	p.kick(r.Context(), t.ID)
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"pin": pinJSON(r.Context(), kind, pin)})
}

func (p *pinnedAPI) unpin(w http.ResponseWriter, r *http.Request) {
	_, t, found, ok := p.lookup(w, r)
	if !ok {
		return
	}
	if found {
		if err := p.Pins.Unpin(r.Context(), t.ID); err != nil {
			internalErr(w, r, "unpin", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// pinOf is the title's pin; ok false (404 written) when it has none.
func (p *pinnedAPI) pinOf(w http.ResponseWriter, r *http.Request) (string, tracking.Title, pinned.Pin, bool) {
	kind, t, found, ok := p.lookup(w, r)
	if !ok {
		return "", t, pinned.Pin{}, false
	}
	if !found {
		writeErr(w, r, http.StatusNotFound, msgPinNotPinned)
		return "", t, pinned.Pin{}, false
	}
	pin, has, err := p.Pins.ByTitle(r.Context(), t.ID)
	if err != nil {
		internalErr(w, r, "pinned get", err)
		return "", t, pin, false
	}
	if !has {
		writeErr(w, r, http.StatusNotFound, msgPinNotPinned)
		return "", t, pin, false
	}
	return kind, t, pin, true
}

// refresh fetches the pinned topic again now. A failed refresh that this
// call recorded on the pin (its reason in lastError) is answered with the
// pin; any other failure (nothing recorded: a failed write, the client
// gone) is an error.
func (p *pinnedAPI) refresh(w http.ResponseWriter, r *http.Request) {
	kind, t, before, ok := p.pinOf(w, r)
	if !ok {
		return
	}
	changed, rerr := p.Pins.Refresh(r.Context(), before)
	if r.Context().Err() != nil {
		return
	}
	now, found, err := p.Pins.ByTitle(r.Context(), t.ID)
	if err == nil && !found {
		err = pinned.ErrNotPinned
	}
	if err != nil {
		if rerr != nil {
			err = rerr
		}
		pinErr(w, r, "pin refresh", err)
		return
	}
	// Refresh stores the Russian text of the error it returns: the same
	// text on the row is this call's record (a repeat of the last one too)
	recorded := rerr != nil && now.LastError != "" && now.LastError == i18n.MessageL(i18n.RU, rerr)
	if rerr != nil && !recorded {
		pinErr(w, r, "pin refresh", rerr)
		return
	}
	if changed {
		p.kick(r.Context(), t.ID)
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"pin": pinJSON(r.Context(), kind, now), "changed": changed})
}

func (p *pinnedAPI) accept(w http.ResponseWriter, r *http.Request) {
	kind, t, _, ok := p.pinOf(w, r)
	if !ok {
		return
	}
	pin, err := p.Pins.AcceptSuggestion(r.Context(), t.ID)
	if err != nil {
		pinErr(w, r, "accept suggestion", err)
		return
	}
	p.kick(r.Context(), t.ID)
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"pin": pinJSON(r.Context(), kind, pin)})
}

// pinLink is the link to acquire the pin by and its source: the pin's own
// link (a magnet of its infohash for an own .torrent file); a magnet needs
// no source, any other link its source's id.
func pinLink(pin pinned.Pin) (link string, src int64, err error) {
	link = pin.Link
	if link == "" {
		link = "magnet:?xt=urn:btih:" + pin.InfoHash
	}
	if !strings.HasPrefix(strings.ToLower(link), "magnet:") {
		src, err = strconv.ParseInt(pin.SourceID, 10, 64)
	}
	return link, src, err
}

// streamRequest is the request that streams {season, episode} (a film:
// neither) from the pin for u: the pin's own link (never a download).
func streamRequest(u addonkit.User, kind string, t tracking.Title, pin pinned.Pin, season, episode int) (acquire.AcquireRequest, error) {
	link, src, err := pinLink(pin)
	if err != nil {
		return acquire.AcquireRequest{}, err
	}
	req := acquire.AcquireRequest{TMDBID: int(t.TMDBID), Kind: acquire.KindSeries, Mode: acquire.ModeStream,
		ReleaseLink: link, SourceID: src, UserID: u.ID, UserUID: u.UID, CheckLink: !u.IsAdmin}
	if kind == "movie" {
		req.Kind = acquire.KindMovie
	} else {
		req.Season, req.Episode = season, episode
	}
	return req, nil
}

// playEpisode streams {season, episode} (a film: neither) from the pin,
// as «Смотреть» of a release row does (never a follow, never a download),
// and answers the core input of the stream's file: {inputId}.
func (p *pinnedAPI) playEpisode(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Season  int `json:"season"`
		Episode int `json:"episode"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	kind, t, pin, ok := p.pinOf(w, r)
	if !ok {
		return
	}
	u := user(r)
	req, err := streamRequest(u, kind, t, pin, in.Season, in.Episode)
	if err != nil {
		internalErr(w, r, "pinned play source", err)
		return
	}
	out, err := p.Acquire.Acquire(r.Context(), req)
	if err != nil {
		acquireErr(w, r, "pinned play", err)
		return
	}
	p.added()
	id, err := p.Watch(r.Context(), u, strings.ToLower(out.InfoHash), out.FileIndex)
	if err != nil {
		if p.WatchError != nil {
			p.WatchError(w, r, "pinned play", err)
		} else {
			acquireErr(w, r, "pinned play", err)
		}
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]string{"inputId": id})
}

// downloadEpisodes downloads only the given episodes of a season from the
// pin (their video and external tracks, chosen as the scanner does:
// EpisodesOf), to the server. 409 when the pin holds none of them.
func (p *pinnedAPI) downloadEpisodes(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Season   int   `json:"season"`
		Episodes []int `json:"episodes"`
	}
	if !decode(w, r, &in) {
		return
	}
	kind, t, pin, ok := p.pinOf(w, r)
	if !ok {
		return
	}
	if kind != "series" || len(in.Episodes) == 0 {
		writeErr(w, r, http.StatusUnprocessableEntity, msgPinNoEpisodes)
		return
	}
	eps, err := p.tracking().Episodes(r.Context(), t.ID)
	if err != nil {
		internalErr(w, r, "pinned download episodes", err)
		return
	}
	byEp := pinned.EpisodesOf(pin.Files, pinned.DefaultSeason(eps))
	var files []int
	for _, n := range in.Episodes {
		files = append(files, byEp[[2]int{in.Season, n}]...)
	}
	slices.Sort(files)
	files = slices.Compact(files)
	if len(files) == 0 {
		writeErr(w, r, http.StatusConflict, msgPinNoSuchEps)
		return
	}
	u := user(r)
	link, src, err := pinLink(pin)
	if err != nil {
		internalErr(w, r, "pinned download source", err)
		return
	}
	req := acquire.AcquireRequest{TMDBID: int(t.TMDBID), Kind: acquire.KindSeries, Season: in.Season,
		ReleaseLink: link, SourceID: src, UserID: u.ID, UserUID: u.UID, CheckLink: !u.IsAdmin}
	out, err := p.Acquire.AcquirePinnedFiles(r.Context(), pin.InfoHash, req, files)
	if err != nil {
		acquireErr(w, r, "pinned download", err)
		return
	}
	p.added()
	out.Note = i18n.Localize(r.Context(), out.Note)
	addonkit.WriteJSON(w, http.StatusOK, out)
}
