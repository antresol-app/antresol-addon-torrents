package followapi

import (
	"errors"
	"net/http"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/httpx"
	"github.com/antresol-app/antresol/addonkit"
)

// «Найти озвучку…» of a film or series page and the player's audio menu:
// one search of the sources per GET; a file list only for a candidate the
// user expands (/files) or downloads, at most five a search. Adults only
// (the kid refusal is new here: legacy mounted these inside the core's
// nonKid group).
func registerVoiceOvers(mux *http.ServeMux, v *voiceAPI) {
	const base = "/api/items/{id}/voiceovers"
	mux.Handle("GET "+base, adults(v.find))
	mux.Handle("POST "+base, adults(v.download))
	mux.Handle("POST "+base+"/files", adults(v.preview))
	mux.Handle("GET "+base+"/downloads", adults(v.downloads))
}

type voiceAPI struct{ Deps }

// item is the library item of {id} when the profile may see it; false:
// 404 (or the error) written.
func (v *voiceAPI) item(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := httpx.IDParam(w, r, "id")
	if !ok {
		return 0, false
	}
	it, err := v.Library.Item(r.Context(), id)
	if err != nil {
		if errors.Is(err, library.ErrNotFound) {
			httpx.WriteErr(w, r, http.StatusNotFound, "Не найдено")
		} else {
			httpx.InternalErr(w, r, "followapi", "voice-over item", err)
		}
		return 0, false
	}
	// an item over the profile's rating limit is not told apart from none
	ok, err = itemVisible(r.Context(), v.Library, user(r), it)
	if err != nil {
		httpx.InternalErr(w, r, "followapi", "voice-over gate", err)
		return 0, false
	}
	if !ok {
		httpx.WriteErr(w, r, http.StatusNotFound, "Не найдено")
		return 0, false
	}
	return id, true
}

func (v *voiceAPI) find(w http.ResponseWriter, r *http.Request) {
	id, ok := v.item(w, r)
	if !ok {
		return
	}
	res, err := v.Acquire.FindVoiceOvers(r.Context(), id)
	if err != nil {
		httpx.AcquireErr(w, r, "followapi", "voice-overs", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, res)
}

// download starts the download of a candidate ({"id"}) of the item's last
// search; force (admin): even when the disk is full.
func (v *voiceAPI) download(w http.ResponseWriter, r *http.Request) {
	id, ok := v.item(w, r)
	if !ok {
		return
	}
	var req acquire.VoiceOverRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	u := user(r)
	req.ItemID, req.UserID, req.UserUID = id, u.ID, u.UID
	req.Force = req.Force && u.IsAdmin
	res, err := v.Acquire.DownloadVoiceOver(r.Context(), req)
	if err != nil {
		httpx.AcquireErr(w, r, "followapi", "voice-over download", err)
		return
	}
	v.added()
	addonkit.WriteJSON(w, http.StatusOK, res)
}

// preview is a candidate's file list as the episodes it covers.
func (v *voiceAPI) preview(w http.ResponseWriter, r *http.Request) {
	id, ok := v.item(w, r)
	if !ok {
		return
	}
	var req acquire.VoiceOverRequest
	if !httpx.Decode(w, r, &req) {
		return
	}
	res, err := v.Acquire.PreviewVoiceOver(r.Context(), id, req.ID)
	if err != nil {
		httpx.AcquireErr(w, r, "followapi", "voice-over files", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, res)
}

// downloads: the item's voice-over downloads and their episodes.
func (v *voiceAPI) downloads(w http.ResponseWriter, r *http.Request) {
	id, ok := v.item(w, r)
	if !ok {
		return
	}
	res, err := v.Acquire.VoiceOverDownloads(r.Context(), id)
	if err != nil {
		httpx.InternalErr(w, r, "followapi", "voice-over downloads", err)
		return
	}
	if res == nil {
		res = []acquire.VoiceOverDownload{}
	}
	addonkit.WriteJSON(w, http.StatusOK, res)
}
