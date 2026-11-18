package find

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol/addonkit"
)

// approveTimeout bounds approving the kids' requests after an admin's add.
const approveTimeout = 10 * time.Second

// POST /api/acquire: the add. Any adult picks a release; the file lists
// of the track step and a download onto a full disk stay admin-only. The
// owner is the profile (organize's import owner). After an admin's
// download, the kids' requests of it are approved.
func (h *handlers) acquire(w http.ResponseWriter, r *http.Request) {
	var req acquire.AcquireRequest
	if !decode(w, r, &req) {
		return
	}
	switch req.Quality {
	case "", "720p", "1080p", "2160p":
		req.Resolution = req.Quality
	default:
		writeErr(w, r, http.StatusBadRequest, "Неизвестное качество")
		return
	}
	u := user(r)
	req.UserID, req.UserUID = u.ID, u.UID
	req.Force = req.Force && u.IsAdmin // only an admin may download onto a full disk
	if !u.IsAdmin {
		req.Files = nil
		req.CheckLink = true
	}
	res, err := h.Acquire.Acquire(r.Context(), req)
	if err != nil {
		acquireErr(w, r, "acquire", err)
		return
	}
	if h.Added != nil {
		h.Added()
	}
	if req.Replace && req.Mode == acquire.ModeDownload && (req.Kind == acquire.KindMovie || req.Kind == acquire.KindSeries) {
		// «Выбрать другую раздачу»: the old unfinished download goes once the new one is in
		if _, err := h.Acquire.CancelTitle(r.Context(), req.Kind, req.TMDBID, req.Season, res.InfoHash); err != nil {
			slog.Warn("find: replace download", "kind", req.Kind, "tmdbId", req.TMDBID, "err", err)
		}
	}
	if u.IsAdmin && req.Mode == acquire.ModeDownload && h.Approve != nil {
		// «Одобрить» of «Запросы» is this download (from there or from anywhere)
		// (the download runs whatever the tab does next: so does the approve)
		a := addonkit.ApproveRequest{AdminID: u.ID, Kind: req.Kind, TMDBID: req.TMDBID, ExtID: req.ExtID, Title: req.Title, Artist: req.Artist}
		actx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), approveTimeout)
		_, err := h.Approve(actx, a)
		cancel()
		if err != nil {
			slog.Warn("find: approve requests", "kind", req.Kind, "tmdbId", req.TMDBID, "extId", req.ExtID, "err", err)
		}
	}
	res.Note = i18n.Localize(r.Context(), res.Note)
	addonkit.WriteJSON(w, http.StatusOK, res)
}

// titleProgress is a title's progress as the route answers it: the hint
// line in the profile's language beside the service's fields; the
// tracker's own words (trackerError) go to admins only.
type titleProgress struct {
	acquire.TitleProgress
	Hint string `json:"hint,omitempty"`
}

// GET /api/acquire/progress?kind=movie|series&tmdbId=N[&season=S] → the
// title's download progress (200), or 204 when nothing is downloading.
func (h *handlers) titleProgress(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tmdbID, err1 := strconv.Atoi(q.Get("tmdbId"))
	season := 0
	var err2 error
	if v := q.Get("season"); v != "" {
		season, err2 = strconv.Atoi(v)
	}
	kind := q.Get("kind")
	if err1 != nil || err2 != nil || tmdbID <= 0 || season < 0 || (kind != acquire.KindMovie && kind != acquire.KindSeries) {
		writeErr(w, r, http.StatusBadRequest, "Неизвестный фильм или сериал")
		return
	}
	p, ok := h.Acquire.TitleProgress(r.Context(), kind, tmdbID, season)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, progressFor(r, p))
}

// progressFor is p as the profile of r sees it: the hint in its language,
// the source's own words for an admin only.
func progressFor(r *http.Request, p acquire.TitleProgress) titleProgress {
	out := titleProgress{TitleProgress: p}
	if p.Status == "active" {
		if hint := acquire.StallHint(p.Stalled, p.Liveness, p.TrackerErrorKind); hint != nil {
			out.Hint = hint.RU
			if i18n.FromContext(r.Context()) == i18n.EN {
				out.Hint = hint.EN
			}
		}
	}
	if !user(r).IsAdmin {
		out.TrackerError = ""
	}
	return out
}

// maxBatch is the most titles one batch asks about (a search's cards).
const maxBatch = 100

// GET /api/acquire/progress/batch?t=kind:tmdbId[:season]&t=… → {"<t>":
// progress} of the titles with something downloading (a search's cards
// in one request, each as /api/acquire/progress answers it).
func (h *handlers) titlesProgress(w http.ResponseWriter, r *http.Request) {
	ts := r.URL.Query()["t"]
	if len(ts) == 0 || len(ts) > maxBatch {
		writeErr(w, r, http.StatusBadRequest, "Неизвестный фильм или сериал")
		return
	}
	byKey := map[acquire.TitleKey]string{}
	keys := make([]acquire.TitleKey, 0, len(ts))
	for _, t := range ts {
		k, ok := parseTitleKey(t)
		if !ok {
			writeErr(w, r, http.StatusBadRequest, "Неизвестный фильм или сериал")
			return
		}
		if _, seen := byKey[k]; !seen {
			byKey[k] = t
			keys = append(keys, k)
		}
	}
	out := map[string]titleProgress{}
	for k, p := range h.Acquire.TitlesProgress(r.Context(), keys) {
		if t, ok := byKey[k]; ok {
			out[t] = progressFor(r, p)
		}
	}
	addonkit.WriteJSON(w, http.StatusOK, out)
}

// parseTitleKey reads "kind:tmdbId" or "kind:tmdbId:season" (a movie or a series).
func parseTitleKey(t string) (acquire.TitleKey, bool) {
	parts := strings.Split(t, ":")
	if len(parts) < 2 || len(parts) > 3 || (parts[0] != acquire.KindMovie && parts[0] != acquire.KindSeries) {
		return acquire.TitleKey{}, false
	}
	id, err := strconv.Atoi(parts[1])
	if err != nil || id <= 0 {
		return acquire.TitleKey{}, false
	}
	season := 0
	if len(parts) == 3 {
		if season, err = strconv.Atoi(parts[2]); err != nil || season < 0 {
			return acquire.TitleKey{}, false
		}
	}
	return acquire.TitleKey{Kind: parts[0], TMDBID: id, Season: season}, true
}

// POST /api/acquire/cancel {kind, tmdbId, season} → {cancelled}: stops and
// deletes a title's unfinished downloads (whoever may start one may stop
// it; finished downloads stay).
func (h *handlers) cancelTitle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind   string `json:"kind"`
		TMDBID int    `json:"tmdbId"`
		Season int    `json:"season"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.TMDBID <= 0 || in.Season < 0 || (in.Kind != acquire.KindMovie && in.Kind != acquire.KindSeries) {
		writeErr(w, r, http.StatusBadRequest, "Неизвестный фильм или сериал")
		return
	}
	n, err := h.Acquire.CancelTitle(r.Context(), in.Kind, in.TMDBID, in.Season, "")
	if err != nil {
		acquireErr(w, r, "cancel download", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]int{"cancelled": n})
}

// POST /api/torrents/{ih}/promote [{force}] → {status}; status is
// waiting_space when the disk is full (force: admin only).
func (h *handlers) promote(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Force bool `json:"force"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}
	st, err := h.Acquire.Promote(r.Context(), r.PathValue("ih"), in.Force && user(r).IsAdmin)
	if err != nil {
		acquireErr(w, r, "promote", err)
		return
	}
	if h.Added != nil {
		h.Added() // a stream became a download: its hold
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]string{"status": st})
}
