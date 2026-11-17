// Package followapi is the profiles' «Следить», pinned releases and «Найти
// озвучку…»: /api/follow…, /api/pinned/{kind}/{tmdbId}… and
// /api/items/{id}/voiceovers…, shapes as the core served them before the
// move (legacy follow_handlers, pinned_handlers, voiceovers_handlers).
// Every route is for adult profiles (kids are refused here too, as
// defence in depth: the core keeps them from the proxy); none is
// admin-only, the admins' differences stay inside the handlers (all
// follows, no link check, «force»). main mounts them on the profiles'
// /api/ mux, served once the addon is ready.
package followapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/pinned"
	"github.com/antresol-app/antresol/addonkit"
)

// Acquire is what the pin and voice-over routes use of acquire.Service.
type Acquire interface {
	Acquire(ctx context.Context, req acquire.AcquireRequest) (acquire.AcquireResult, error)
	AcquirePinnedFiles(ctx context.Context, ih string, req acquire.AcquireRequest, files []int) (acquire.AcquireResult, error)
	FindVoiceOvers(ctx context.Context, itemID int64) (acquire.VoiceOvers, error)
	DownloadVoiceOver(ctx context.Context, req acquire.VoiceOverRequest) (acquire.VoiceOverStarted, error)
	PreviewVoiceOver(ctx context.Context, itemID int64, id string) (acquire.VoiceOverPreview, error)
	VoiceOverDownloads(ctx context.Context, itemID int64) ([]acquire.VoiceOverDownload, error)
}

// Library is what the routes read of the core's library
// (coreadapt.Library).
type Library interface {
	Item(ctx context.Context, id int64) (library.Item, error)
	ItemByTMDB(ctx context.Context, kind string, tmdbID int64) (library.Item, error)
}

var _ Library = coreadapt.Library{}

// Deps are what the routes need.
type Deps struct {
	// Follows: nil when the core has no TMDB (no follow service): the
	// follow routes are not mounted, as before the move.
	Follows *follow.Service
	// Pins: nil, no pin routes. Its Tracking is the core's tracked titles.
	Pins    *pinned.Service
	Acquire Acquire
	// Library is the core's library: the voice-over routes' item, and the
	// rating gate of both families (a title's library item).
	Library Library
	// Watch registers a started stream's file as the profile's input and
	// returns its id (watch.Deps.Watch); its errors go to WatchError.
	Watch      func(ctx context.Context, u addonkit.User, ih string, idx int) (string, error)
	WatchError func(w http.ResponseWriter, r *http.Request, what string, err error)
	// Added (optional): a download or stream was added (the holds go out
	// at once).
	Added func()
}

func (d Deps) added() {
	if d.Added != nil {
		d.Added()
	}
}

// Register adds the routes to mux (the profiles' /api/ routes).
func Register(mux *http.ServeMux, d Deps) {
	if d.Follows != nil {
		registerFollow(mux, d.Follows)
	}
	if d.Pins != nil && d.Pins.Tracking != nil && d.Acquire != nil && d.Library != nil {
		registerPinned(mux, &pinnedAPI{d})
	}
	if d.Acquire != nil && d.Library != nil {
		registerVoiceOvers(mux, &voiceAPI{d})
	}
}

// adults lets signed-in adult profiles through, in their language.
func adults(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := addonkit.UserFrom(r)
		if !ok || u.IsKid {
			writeErr(w, r, http.StatusForbidden, "Недоступно в детском профиле")
			return
		}
		if u.Lang != "" {
			r = r.WithContext(i18n.WithLanguage(r.Context(), u.Lang))
		}
		h(w, r)
	})
}

// user is the request's profile (adults let only those through).
func user(r *http.Request) addonkit.User {
	if u, ok := addonkit.UserFrom(r); ok {
		return *u
	}
	return addonkit.User{}
}

// writeErr writes {"error": msg} (Russian) in the request's language.
func writeErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	addonkit.WriteError(w, code, i18n.Localize(r.Context(), msg))
}

// internalErr logs err and answers 500 (nothing when the client went).
func internalErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.Warn("followapi: "+what, "err", err)
	writeErr(w, r, http.StatusInternalServerError, "Внутренняя ошибка сервера")
}

// maxJSON bounds a JSON body but the own .torrent upload's.
const maxJSON = 1 << 20

// decode reads a JSON body of at most limit bytes; false: a 400 is
// written.
func decodeN(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err == nil && int64(len(body)) > limit {
		err = errors.New("body too large")
	}
	if err == nil {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		writeErr(w, r, http.StatusBadRequest, "Неверный запрос")
		return false
	}
	return true
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool { return decodeN(w, r, maxJSON, v) }

// idParam is the path's {name} as a positive id; false: 400 written.
func idParam(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, r, http.StatusBadRequest, "Неверный id")
		return 0, false
	}
	return id, true
}

// msgErr is a service's Russian message with the error it came with.
type msgErr struct {
	msg string
	err error
}

func (e msgErr) Error() string { return e.msg }
func (e msgErr) Unwrap() error { return e.err }

// acquireErr writes an acquire error: its message in the request's
// language with the status of its kind; anything else is a 500.
func acquireErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		return
	}
	msg, ok := acquire.UserMessage(err)
	if !ok {
		internalErr(w, r, what, err)
		return
	}
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, acquire.ErrInvalid):
		code = http.StatusBadRequest
	case errors.Is(err, acquire.ErrNotFound), errors.Is(err, acquire.ErrNoRelease):
		code = http.StatusNotFound
	case errors.Is(err, acquire.ErrUnavailable):
		code = http.StatusServiceUnavailable
	case errors.Is(err, acquire.ErrConflict), errors.Is(err, acquire.ErrInProgress):
		code = http.StatusConflict
	case errors.Is(err, acquire.ErrNoFiles):
		code = http.StatusUnprocessableEntity
	case errors.Is(err, acquire.ErrSource):
		code = http.StatusBadGateway
	}
	if code >= 500 {
		slog.Warn("followapi: "+what, "err", err)
	}
	addonkit.WriteError(w, code, i18n.Message(r.Context(), msgErr{msg, err}))
}
