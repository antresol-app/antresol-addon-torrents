package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// FlareSolverr in «Настройки загрузок»: the address the sources behind
// Cloudflare go through. Saved only once it answers as FlareSolverr; the
// saved address wins over the server's ANTRESOL_TORRENTS_FLARESOLVERR_URL,
// and clearing it falls back to that one.

// flareSolverrView is the answer of PUT /api/admin/settings/flaresolverr.
type flareSolverrView struct {
	URL     string `json:"url"`               // as saved ("" = none saved)
	Version string `json:"version,omitempty"` // what the address answered
}

// pingTimeout bounds the check of an address.
const pingTimeout = 10 * time.Second

// normalizeSolverURL trims the address and checks it is an http(s) URL
// with a host; "" stays "" (no address).
func normalizeSolverURL(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", true
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false
	}
	return strings.TrimRight(s, "/"), true
}

// pingSolver asks base for FlareSolverr's index (GET /: {"msg":
// "FlareSolverr is ready!", "version": …}); the answer is the version, or a
// Russian message of what went wrong.
func pingSolver(ctx context.Context, client *http.Client, base string) (version, problem string) {
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return "", "Неверный адрес"
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", "FlareSolverr не ответил за 10 секунд. Проверьте адрес и что он запущен."
		}
		return "", "Не удалось подключиться к FlareSolverr. Проверьте адрес и что он запущен."
	}
	defer resp.Body.Close()
	var out struct {
		Msg     string `json:"msg"`
		Version string `json:"version"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &out) != nil || !strings.Contains(strings.ToLower(out.Msg), "flaresolverr") {
		return "", "По этому адресу отвечает не FlareSolverr."
	}
	return out.Version, ""
}

// PUT /api/admin/settings/flaresolverr {"url"} → the saved address and
// FlareSolverr's version; "" clears it. 422 with a message when the address
// is not FlareSolverr or does not answer: nothing is saved. Once saved, the
// enabled sources whose check failed are checked again in the background.
func (s *settings) putFlareSolverr(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL *string `json:"url"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.URL == nil {
		writeErr(w, r, http.StatusBadRequest, "Неверный запрос")
		return
	}
	u, ok := normalizeSolverURL(*in.URL)
	if !ok {
		writeErr(w, r, http.StatusUnprocessableEntity, "Укажите адрес вида http://flaresolverr:8191")
		return
	}
	var out flareSolverrView
	if u != "" {
		client := s.SolverHTTP
		if client == nil {
			client = http.DefaultClient
		}
		v, problem := pingSolver(r.Context(), client, u)
		if problem != "" {
			writeErr(w, r, http.StatusUnprocessableEntity, problem)
			return
		}
		out.Version = v
	}
	if err := s.Settings.SetFlareSolverrURL(r.Context(), u); err != nil {
		internalErr(w, r, "flaresolverr", err)
		return
	}
	out.URL = u
	if s.Sources != nil {
		effective := u
		if effective == "" {
			effective = s.FlareSolverrServer
		}
		s.Sources.SetFlareSolverr(effective)
		go s.Sources.RecheckFailed(context.WithoutCancel(r.Context()))
	}
	addonkit.WriteJSON(w, http.StatusOK, out)
}
