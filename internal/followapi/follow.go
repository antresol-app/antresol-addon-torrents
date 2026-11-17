package followapi

import (
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol/addonkit"
)

// «Следить» / «Хочу посмотреть». Any adult profile follows titles (no
// approval; the admin sees who added each download). An adult sees and
// changes only the follows it added; the admin sees and changes all of
// them, the follows of deleted profiles too («Профиль удалён»).
func registerFollow(mux *http.ServeMux, svc *follow.Service) {
	h := &followAPI{svc: svc}
	mux.Handle("GET /api/follow", adults(h.list))
	mux.Handle("POST /api/follow", adults(h.add))
	mux.Handle("GET /api/follow/by-tmdb", adults(h.byTMDB))
	mux.Handle("DELETE /api/follow/{id}", adults(h.remove))
	mux.Handle("PATCH /api/follow/{id}", adults(h.setActive))
	mux.Handle("POST /api/follow/{id}/check", adults(h.check))
}

type followAPI struct{ svc *follow.Service }

func followErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case r.Context().Err() != nil:
		return // the client went away
	case errors.Is(err, follow.ErrInvalid):
		writeErr(w, r, http.StatusBadRequest, "Следить можно только за фильмом или сериалом")
	case errors.Is(err, follow.ErrNotFound):
		writeErr(w, r, http.StatusNotFound, "Не найдено")
	case errors.Is(err, follow.ErrInactive):
		writeErr(w, r, http.StatusConflict, "Слежение выключено")
	case errors.Is(err, follow.ErrUnavailable):
		writeErr(w, r, http.StatusServiceUnavailable, "Нет связи с TMDB. Попробуйте позже")
	default:
		internalErr(w, r, what, err)
	}
}

// mayChange: the admin, or the profile that added the follow, by uid (a
// follow of a deleted profile, no uid, is the admin's alone; a new
// profile with the deleted one's id is another profile).
func mayChange(u addonkit.User, f follow.Follow) bool {
	return u.IsAdmin || mine(u, f)
}

// mine: u added f (by uid, never by id or name).
func mine(u addonkit.User, f follow.Follow) bool {
	return f.UserUID != "" && f.UserUID == u.UID
}

// shown is f as u sees it: «Профиль удалён» is for admins; anyone
// else sees no author of a follow that belongs to nobody. Mine says
// whether u added it, by uid (two profiles may share a name, a deleted
// profile's id may be another's now).
func shown(u addonkit.User, f follow.Follow) follow.Follow {
	if !u.IsAdmin && f.UserID == 0 {
		f.AddedBy = ""
	}
	f.Mine = mine(u, f)
	return f
}

const notYoursMsg = "Это слежение добавил другой профиль"

// owned loads the follow of {id} when this profile may change it; false
// with the error written otherwise.
func (h *followAPI) owned(w http.ResponseWriter, r *http.Request, what string) (follow.Follow, bool) {
	id, ok := idParam(w, r, "id")
	if !ok {
		return follow.Follow{}, false
	}
	f, err := h.svc.Get(r.Context(), id)
	if err != nil {
		followErr(w, r, what, err)
		return f, false
	}
	if !mayChange(user(r), f) {
		writeErr(w, r, http.StatusForbidden, notYoursMsg)
		return f, false
	}
	return f, true
}

// list: the admin's all (orphans included), an adult's own.
func (h *followAPI) list(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.List(r.Context())
	if err != nil {
		internalErr(w, r, "follow list", err)
		return
	}
	u := user(r)
	if !u.IsAdmin {
		mine := items[:0]
		for _, f := range items {
			if mayChange(u, f) {
				mine = append(mine, f)
			}
		}
		items = mine
	}
	for i := range items {
		items[i] = shown(u, items[i])
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// resolutionOK: absent, or one of follow.Resolutions (400 written otherwise).
func resolutionOK(w http.ResponseWriter, r *http.Request, res *string) bool {
	if res != nil && !slices.Contains(follow.Resolutions, *res) {
		writeErr(w, r, http.StatusBadRequest, "Неизвестное качество")
		return false
	}
	return true
}

// add follows a title; resolution («Докачивать новые»' quality) is set on
// a follow this profile may change (another's keeps its own).
func (h *followAPI) add(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind       string  `json:"kind"`
		TMDBID     int     `json:"tmdbId"`
		Resolution *string `json:"resolution"`
	}
	if !decode(w, r, &in) || !resolutionOK(w, r, in.Resolution) {
		return
	}
	u := user(r)
	res := ""
	if in.Resolution != nil {
		res = *in.Resolution
	}
	// a new follow is written with its quality (its first check uses it)
	f, err := h.svc.AddWithResolution(r.Context(), u, in.Kind, in.TMDBID, res)
	if err != nil {
		followErr(w, r, "follow add", err)
		return
	}
	if in.Resolution != nil && *in.Resolution != f.Resolution && mayChange(u, f) {
		if err := h.svc.SetResolution(r.Context(), f.ID, *in.Resolution); err != nil {
			followErr(w, r, "follow resolution", err)
			return
		}
		f.Resolution = *in.Resolution
	}
	addonkit.WriteJSON(w, http.StatusOK, shown(u, f))
}

func (h *followAPI) byTMDB(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.URL.Query().Get("tmdbId"))
	kind := r.URL.Query().Get("kind")
	if err != nil || id <= 0 || (kind != follow.KindSeries && kind != follow.KindMovie) {
		writeErr(w, r, http.StatusBadRequest, "Неизвестный фильм или сериал")
		return
	}
	f, err := h.svc.ByTMDB(r.Context(), kind, id)
	if err != nil {
		followErr(w, r, "follow by tmdb", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, shown(user(r), f))
}

func (h *followAPI) remove(w http.ResponseWriter, r *http.Request) {
	f, ok := h.owned(w, r, "follow remove")
	if !ok {
		return
	}
	if err := h.svc.Remove(r.Context(), f.ID); err != nil {
		followErr(w, r, "follow remove", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *followAPI) setActive(w http.ResponseWriter, r *http.Request) {
	cur, ok := h.owned(w, r, "follow set active")
	if !ok {
		return
	}
	id := cur.ID
	var in struct {
		Active     *bool   `json:"active"`
		Resolution *string `json:"resolution"`
	}
	if !decode(w, r, &in) || !resolutionOK(w, r, in.Resolution) {
		return
	}
	if in.Active == nil && in.Resolution == nil {
		writeErr(w, r, http.StatusBadRequest, "Не указано, включить ли слежение")
		return
	}
	// the quality first: a follow turned on syncs with it (OnFollowed)
	if in.Resolution != nil {
		if err := h.svc.SetResolution(r.Context(), id, *in.Resolution); err != nil {
			followErr(w, r, "follow resolution", err)
			return
		}
	}
	if in.Active != nil {
		if err := h.svc.SetActive(r.Context(), id, *in.Active); err != nil {
			followErr(w, r, "follow set active", err)
			return
		}
	}
	f, err := h.svc.Get(r.Context(), id)
	if err != nil {
		followErr(w, r, "follow get", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, shown(user(r), f))
}

// check starts a check in the background (it may take minutes: the
// searches are spaced): 202, the result comes as the follow-checked event
// (and in GET /api/follow).
func (h *followAPI) check(w http.ResponseWriter, r *http.Request) {
	f, ok := h.owned(w, r, "follow check")
	if !ok {
		return
	}
	if !f.Active {
		followErr(w, r, "follow check", follow.ErrInactive)
		return
	}
	h.svc.Kick(f.ID)
	addonkit.WriteJSON(w, http.StatusAccepted, map[string]bool{"started": true})
}
