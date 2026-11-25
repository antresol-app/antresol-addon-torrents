package watch

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/httpx"
	"github.com/antresol-app/antresol/addonkit"
)

// status is the stream's state for the player's buffer line
// (acquire.StreamStatus; ?file= adds bufferHave/bufferWant).
func (h *handlers) status(w http.ResponseWriter, r *http.Request) {
	file := -1
	if v := r.URL.Query().Get("file"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			file = n
		}
	}
	ih := strings.ToLower(r.PathValue("ih"))
	if !validIH(ih) {
		httpx.WriteErr(w, r, http.StatusNotFound, "Раздача не найдена")
		return
	}
	st, err := h.Acquire.TorrentStatus(ih, file)
	if err != nil {
		acquireErr(w, r, "watch status", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, st)
}
