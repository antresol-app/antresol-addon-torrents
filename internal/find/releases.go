package find

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol/addonkit"
)

// GET /api/discover?q=[&kind=music|book|audiobook|comic]: TMDB movies and
// series (in the profile's language) without kind, else the kind's catalog.
func (h *handlers) discover(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var cs []acquire.Candidate
	var err error
	if kind := q.Get("kind"); kind != "" {
		cs, err = h.Acquire.DiscoverMedia(r.Context(), kind, q.Get("q"))
	} else {
		ctx := tmdb.WithLanguage(r.Context(), i18n.TMDB(i18n.FromContext(r.Context())))
		cs, err = h.Acquire.Discover(ctx, q.Get("q"))
	}
	if err != nil {
		acquireErr(w, r, "discover", err)
		return
	}
	if cs == nil {
		cs = []acquire.Candidate{}
	}
	addonkit.WriteJSON(w, http.StatusOK, cs)
}

func queryInt(r *http.Request, name string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(name))
	return n
}

// GET /api/releases?…[&stream=1]: the releases of a title, best first.
// With stream=1 the answer is NDJSON, one releaseLine per line: the
// releases of the sources that answered so far and the sources still
// pending (done=false), then the final set (done=true). An error before
// the first line is the usual JSON error with its status; one after it is
// a last line {"error", "done": true}.
func (h *handlers) releases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := acquire.ReleaseRequest{
		TMDBID: queryInt(r, "tmdbId"), Kind: q.Get("kind"),
		Season: queryInt(r, "season"), Episode: queryInt(r, "episode"),
		ExtID: q.Get("extId"), Title: q.Get("title"), Artist: q.Get("artist"),
		Year: queryInt(r, "year"), Aliases: q["alias"],
	}
	mine := h.myLanguages(r)
	if q.Get("stream") != "1" {
		resp, err := h.Acquire.Releases(r.Context(), req, nil)
		if err != nil {
			acquireErr(w, r, "releases", err)
			return
		}
		addonkit.WriteJSON(w, http.StatusOK, mineResponse(resp, mine))
		return
	}
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	started := false
	send := func(line releaseLine) {
		if !started {
			started = true
			hd := w.Header()
			hd.Set("Content-Type", "application/x-ndjson")
			hd.Set("Cache-Control", "no-cache")
			hd.Set("X-Accel-Buffering", "no") // a proxy in front: do not buffer
			w.WriteHeader(http.StatusOK)
		}
		if enc.Encode(line) == nil {
			_ = rc.Flush()
		}
	}
	// the progress calls are serialized by the search and end before it returns
	resp, err := h.Acquire.Releases(r.Context(), req, func(p indexer.Progress) {
		send(releaseLine{SearchResponse: mineResponse(p.SearchResponse, mine), Pending: p.Pending})
	})
	switch {
	case err != nil && !started:
		acquireErr(w, r, "releases", err)
	case err != nil:
		// the sources were asked already: only an ended context fails now
		if r.Context().Err() == nil {
			slog.Warn("find: releases", "err", err)
			send(releaseLine{Error: i18n.Localize(r.Context(), "Что-то пошло не так"), Done: true})
		}
	default:
		send(releaseLine{SearchResponse: mineResponse(resp, mine), Pending: []indexer.PendingSource{}, Done: true})
	}
}

// releaseLine is a line of the /api/releases stream.
type releaseLine struct {
	indexer.SearchResponse
	Pending []indexer.PendingSource `json:"pending"`
	Done    bool                    `json:"done"`
	Error   string                  `json:"error,omitempty"`
}

// mineResponse is resp with each release's subtitles cut to «Мои языки».
func mineResponse(resp indexer.SearchResponse, mine []string) indexer.SearchResponse {
	for _, list := range [][]indexer.ScoredRelease{resp.Releases, resp.Rejected} {
		for i := range list {
			list[i].Info.Subs = mineSubs(list[i].Info.Subs, mine)
		}
	}
	return resp
}

// mineSubs keeps the subtitle languages of «Мои языки» (a release may name
// ten); "sub" (subtitles of no named language) stays.
func mineSubs(subs, mine []string) []string {
	if len(mine) == 0 {
		return subs
	}
	want := map[string]bool{}
	for _, l := range mine {
		want[langBadge(l)] = true
	}
	var out []string
	for _, s := range subs {
		if s == "sub" || want[langBadge(s)] {
			out = append(out, s)
		}
	}
	return out
}

// langCodes and langBadge: a language as its two-letter badge, as the
// core's library badges read them ("rus", "ru-RU" → "RU").
var langCodes = map[string]string{
	"rus": "RU", "ru": "RU", "eng": "EN", "en": "EN", "ukr": "UK", "uk": "UK", "deu": "DE", "ger": "DE", "de": "DE",
	"fra": "FR", "fre": "FR", "fr": "FR", "spa": "ES", "es": "ES", "ita": "IT", "it": "IT", "jpn": "JA", "ja": "JA",
	"kor": "KO", "ko": "KO", "zho": "ZH", "chi": "ZH", "zh": "ZH", "pol": "PL", "pl": "PL", "por": "PT", "pt": "PT",
}

func langBadge(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if i := strings.IndexAny(l, "-_"); i > 0 {
		l = l[:i]
	}
	if c, ok := langCodes[l]; ok {
		return c
	}
	if len(l) == 2 {
		return strings.ToUpper(l)
	}
	return ""
}
