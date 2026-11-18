// Package migrateapi serves the addon's «Переезд» routes under
// /api/admin/migrate (the app reaches them as x/admin/migrate…): the
// status of the old services (Prowlarr, Sonarr, Radarr, qBittorrent), a
// dry run and a run, the history, giving the torrents back to
// qBittorrent and «Перейти на Antresol». The answers have the core's
// /api/migrate shapes (the reused Migrate page reads them as they are).
// admin.Routes mounts the handler inside its own mux, so the admin check
// and the profile's language apply once.
package migrateapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	mediaimport "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/mediaimport/runkit"
	"github.com/antresol-app/antresol-addon-torrents/internal/migrate"
	"github.com/antresol-app/antresol/addonkit"
)

// Deps are what the routes use.
type Deps struct {
	Migrate *migrate.Service
	Log     *slog.Logger
}

// Routes is the handler of /api/admin/migrate and below.
func Routes(d Deps) http.Handler {
	h := &api{d}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/admin/migrate", h.status)
	mux.HandleFunc("GET /api/admin/migrate/state", h.state)
	mux.HandleFunc("POST /api/admin/migrate/run", h.run)
	mux.HandleFunc("GET /api/admin/migrate/reports", h.reports)
	mux.HandleFunc("GET /api/admin/migrate/reports/{id}", h.report)
	mux.HandleFunc("POST /api/admin/migrate/reports/{id}/rollback", h.rollback)
	mux.HandleFunc("GET /api/admin/migrate/switch", h.switchPreview)
	mux.HandleFunc("POST /api/admin/migrate/switch", h.doSwitch)
	mux.HandleFunc("POST /api/admin/migrate/sources/{id}/connect", h.connect)
	mux.HandleFunc("DELETE /api/admin/migrate/sources/{id}/connect", h.forget)
	return mux
}

type api struct{ d Deps }

func (h *api) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.Default()
}

const notScannedMsg = "Медиатека ещё сканируется — подождите"

// writeErr writes {"error": msg} with msg (Russian) in the request's language.
func writeErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	addonkit.WriteError(w, code, i18n.Localize(r.Context(), msg))
}

// decode reads a JSON body (at most 1 MiB); false: a 400 is written.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err == nil {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "Неверный запрос")
		return false
	}
	return true
}

func (h *api) ready(ctx context.Context) (bool, string) {
	ok, reason := h.d.Migrate.Ready(ctx)
	switch {
	case ok:
		return true, ""
	case reason == "":
		return false, notScannedMsg
	}
	return false, reason
}

func (h *api) err(w http.ResponseWriter, r *http.Request, what string, err error) {
	switch {
	case r.Context().Err() != nil:
		return // the client went away
	case errors.Is(err, migrate.ErrNeedsDryRun):
		writeErr(w, r, http.StatusConflict, "Сначала проверьте, что будет перенесено")
	case errors.Is(err, migrate.ErrNotReady):
		// asked again for the reason: ready meanwhile still answers the
		// refusal's own text, never an empty one
		_, reason := h.ready(r.Context())
		if reason == "" {
			reason = notScannedMsg
		}
		writeErr(w, r, http.StatusConflict, reason)
	case errors.Is(err, migrate.ErrBusy):
		writeErr(w, r, http.StatusConflict, "Перенос уже идёт")
	case errors.Is(err, migrate.ErrUnknownSource):
		writeErr(w, r, http.StatusBadRequest, "Неизвестный источник")
	case errors.Is(err, migrate.ErrNoSources):
		writeErr(w, r, http.StatusBadRequest, "Выберите, что перенести")
	case errors.Is(err, migrate.ErrNotFound):
		writeErr(w, r, http.StatusNotFound, "Отчёт не найден")
	case errors.Is(err, migrate.ErrNothingToRollback):
		writeErr(w, r, http.StatusConflict, "В этом отчёте нет перенесённых раздач")
	case errors.Is(err, migrate.ErrRolledBack):
		writeErr(w, r, http.StatusConflict, "Раздачи из этого переноса уже возвращены")
	case errors.Is(err, migrate.ErrNoQBConnection):
		writeErr(w, r, http.StatusConflict, "Чтобы вернуть раздачи, подключите qBittorrent заново: «Подключить» на его карточке (связь не переживает «Забыть» и перезапуск)")
	case errors.Is(err, migrate.ErrArrNotStopped):
		writeErr(w, r, http.StatusBadRequest, "Сначала остановите Sonarr и Radarr")
	case errors.Is(err, migrate.ErrCountChanged):
		writeErr(w, r, http.StatusConflict, "Число загрузок изменилось — проверьте ещё раз")
	case errors.Is(err, migrate.ErrSwitchBlocked):
		var b *migrate.BlockedError
		errors.As(err, &b)
		addonkit.WriteError(w, http.StatusConflict, i18n.T(r.Context(), "Сейчас включать рано — %s", i18n.Localize(r.Context(), b.Reason)))
	case errors.Is(err, migrate.ErrNoFollow):
		writeErr(w, r, http.StatusConflict, "Для слежения нужен ключ TMDB")
	case errors.Is(err, migrate.ErrUnknownRemote):
		writeErr(w, r, http.StatusBadRequest, "Неизвестный источник")
	case errors.Is(err, migrate.ErrMounted):
		writeErr(w, r, http.StatusConflict, "Файлы этого сервиса смонтированы — они и используются")
	default:
		h.log().Error("torrents: "+what, "err", err)
		writeErr(w, r, http.StatusInternalServerError, "Внутренняя ошибка сервера")
	}
}

// migrateState is the part of the status that changes during a run or a
// library scan; reading it touches no old database, so the page polls it.
type migrateState struct {
	Running      *string `json:"running"`
	LibraryReady struct {
		OK     bool   `json:"ok"`
		Reason string `json:"reason"`
	} `json:"libraryReady"`
	State          *migrate.MigrationState `json:"state,omitempty"`
	PendingFollows *int                    `json:"pendingFollows,omitempty"`
}

// migrateStatus is the core's status shape; the addon's sources map no
// profiles, so Users and Profiles are always empty.
type migrateStatus struct {
	Sources   []sourceOut        `json:"sources"`
	Leftovers []mediaimport.Line `json:"leftovers"`
	Users     []struct{}         `json:"users"`
	Profiles  []struct{}         `json:"profiles"`
	migrateState
}

func (h *api) readState(w http.ResponseWriter, r *http.Request) (migrateState, bool) {
	ctx := r.Context()
	var out migrateState
	if id := h.d.Migrate.Running(); id != "" {
		out.Running = &id
	}
	st, err := h.d.Migrate.State(ctx)
	if err != nil {
		h.err(w, r, "migrate state", err)
		return out, false
	}
	n, err := h.d.Migrate.PendingFollows(ctx)
	if err != nil {
		h.err(w, r, "migrate pending follows", err)
		return out, false
	}
	out.State, out.PendingFollows = &st, &n
	out.LibraryReady.OK, out.LibraryReady.Reason = h.ready(ctx)
	out.LibraryReady.Reason = i18n.Localize(ctx, out.LibraryReady.Reason)
	return out, true
}

func (h *api) state(w http.ResponseWriter, r *http.Request) {
	if out, ok := h.readState(w, r); ok {
		addonkit.WriteJSON(w, http.StatusOK, out)
	}
}

// sourceOut is a source's status and, when its files are not mounted, its
// connection over the network (never the key or the password).
type sourceOut struct {
	mediaimport.SourceStatus
	Connect *migrate.RemoteView `json:"connect,omitempty"`
}

func (h *api) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	found := h.d.Migrate.Discover(ctx)
	out := migrateStatus{Sources: make([]sourceOut, len(found)), Leftovers: h.d.Migrate.LeftoverLines(ctx),
		Users: []struct{}{}, Profiles: []struct{}{}}
	for i, st := range found {
		st.Detail = i18n.Localize(ctx, st.Detail)
		st.Error = i18n.Localize(ctx, st.Error)
		out.Sources[i].SourceStatus = st
		if v, ok := h.d.Migrate.Remote(st.ID); ok {
			out.Sources[i].Connect = &v
		}
	}
	out.Leftovers = mediaimport.LocalizeLines(ctx, out.Leftovers)
	st, ok := h.readState(w, r)
	if !ok {
		return
	}
	out.migrateState = st
	addonkit.WriteJSON(w, http.StatusOK, out)
}

// msgNoAdminUID: a real run whose X-Antresol-User has no uid.
const msgNoAdminUID = "Сервер не назвал профиль администратора — перенос не начат"

func (h *api) run(w http.ResponseWriter, r *http.Request) {
	var req mediaimport.RunRequest
	if !decode(w, r, &req) {
		return
	}
	// the admin's id and uid, from X-Antresol-User: the run's AddedBy (its
	// rows go by the uid); a real run without a uid is refused (an
	// older core), never written as nobody's or by an id alone
	var uid string
	if u, ok := addonkit.UserFrom(r); ok {
		req.AdminID, uid = u.ID, u.UID
	}
	if !req.DryRun && uid == "" {
		writeErr(w, r, http.StatusForbidden, msgNoAdminUID)
		return
	}
	id, err := h.d.Migrate.StartAs(r.Context(), req, uid)
	if err != nil {
		h.err(w, r, "migrate run", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusAccepted, map[string]string{"reportId": id})
}

// reportSummary is a report in the history: no lines; Torrents counts the
// qBittorrent torrents a real run moved («Вернуть раздачи в qBittorrent»).
type reportSummary struct {
	*mediaimport.Report
	Torrents int `json:"torrents"`
}

func (h *api) reports(w http.ResponseWriter, r *http.Request) {
	list, err := h.d.Migrate.Reports()
	if err != nil {
		h.err(w, r, "migrate reports", err)
		return
	}
	out := make([]reportSummary, 0, len(list))
	for _, rep := range list {
		sum := reportSummary{Report: rep}
		if !rep.DryRun && rep.RollbackOf == "" {
			sum.Torrents = h.d.Migrate.Torrents(rep.ID)
		}
		out = append(out, sum)
	}
	addonkit.WriteJSON(w, http.StatusOK, out)
}

func (h *api) report(w http.ResponseWriter, r *http.Request) {
	rep, err := h.d.Migrate.Report(r.PathValue("id"))
	if err != nil {
		h.err(w, r, "migrate report", err)
		return
	}
	rep.Localize(r.Context())
	addonkit.WriteJSON(w, http.StatusOK, rep)
}

func (h *api) rollback(w http.ResponseWriter, r *http.Request) {
	id, err := h.d.Migrate.Rollback(r.Context(), r.PathValue("id"))
	if err != nil {
		h.err(w, r, "migrate rollback", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusAccepted, map[string]string{"reportId": id})
}

// switchTimeout bounds «Перейти на Antresol» (a TMDB lookup per follow);
// the switch itself does not stop when the browser leaves halfway.
const switchTimeout = 5 * time.Minute

func (h *api) switchPreview(w http.ResponseWriter, r *http.Request) {
	plan, err := h.d.Migrate.SwitchPreview(r.Context())
	if err != nil {
		h.err(w, r, "migrate switch preview", err)
		return
	}
	plan.Blocked = i18n.Localize(r.Context(), plan.Blocked)
	addonkit.WriteJSON(w, http.StatusOK, plan)
}

func (h *api) doSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm    int  `json:"confirm"`
		ArrStopped bool `json:"arrStopped"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), switchTimeout)
	defer cancel()
	n, err := h.d.Migrate.Switch(ctx, req.Confirm, req.ArrStopped)
	if err != nil {
		h.err(w, r, "migrate switch", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]int{"activated": n})
}

// connectTimeout bounds «Проверить» of a connection (it reads the whole
// list once, to count it).
const connectTimeout = 2 * time.Minute

// connect is «Проверить» on a card whose files are not mounted: the
// address and the key (qBittorrent: the login) are checked and kept in
// memory for the migration; the answer is what was found, or the reason
// in {"error"}. The key and the password are never answered or logged.
func (h *api) connect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		APIKey   string `json:"apiKey"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), connectTimeout)
	defer cancel()
	detail, err := h.d.Migrate.Connect(ctx, id, migrate.Remote{URL: req.URL, APIKey: req.APIKey, Username: req.Username, Password: req.Password})
	var re *migrate.RemoteError
	if errors.As(err, &re) {
		writeErr(w, r, http.StatusBadRequest, re.Msg)
		return
	}
	if err != nil {
		h.err(w, r, "migrate connect", err)
		return
	}
	v, _ := h.d.Migrate.Remote(id)
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"detail": i18n.Localize(r.Context(), detail), "connect": v})
}

// forget drops a kept connection («Забыть»).
func (h *api) forget(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Migrate.Forget(r.PathValue("id")); err != nil {
		h.err(w, r, "migrate forget", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
