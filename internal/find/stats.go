package find

import (
	"net/http"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// POST /api/admin/acquire/files {sourceId, releaseLink, title, artist,
// year, aliases, direct} → the release's files for «Выбрать релиз…».
func (h *handlers) releaseFiles(w http.ResponseWriter, r *http.Request) {
	var req acquire.ReleaseFilesRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := h.Acquire.ReleaseFiles(r.Context(), req)
	if err != nil {
		acquireErr(w, r, "release files", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, res)
}

// SeedingStats is the seeding block of «Статистика»: the torrents seeding
// now (a parked one does not), what was uploaded in all and its ratio to
// what was downloaded.
type SeedingStats struct {
	Count         int     `json:"count"`
	UploadedTotal int64   `json:"uploadedTotal"`
	Ratio         float64 `json:"ratio"`
}

// GET /api/admin/stats → SeedingStats.
func (h *handlers) stats(w http.ResponseWriter, r *http.Request) {
	addonkit.WriteJSON(w, http.StatusOK, seeding(h.Acquire.Torrents()))
}

func seeding(list []acquire.TorrentView) SeedingStats {
	var out SeedingStats
	var down int64
	for _, t := range list {
		if t.Status.Status == torrent.StatusSeeding && !t.Status.Parked {
			out.Count++
		}
		out.UploadedTotal += t.Uploaded
		down += t.Downloaded
	}
	if down > 0 {
		out.Ratio = float64(out.UploadedTotal) / float64(down)
	}
	return out
}
