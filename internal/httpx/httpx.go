// Package httpx holds what the addon's API packages (admin, find, followapi,
// migrateapi, watch) share: the JSON error answer, the body and id readers,
// the adults-only gate and the status of an acquire error.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol/addonkit"
)

// MaxJSON bounds a JSON body but the own .torrent upload's.
const MaxJSON = 1 << 20

// WriteErr writes {"error": msg} with msg (Russian) in the request's language.
func WriteErr(w http.ResponseWriter, r *http.Request, code int, msg string) {
	addonkit.WriteError(w, code, i18n.Localize(r.Context(), msg))
}

// InternalErr logs err (as "<pkg>: <what>") and answers 500; nothing when
// the client went away.
func InternalErr(w http.ResponseWriter, r *http.Request, pkg, what string, err error) {
	if r.Context().Err() != nil {
		return
	}
	slog.Warn(pkg+": "+what, "err", err)
	WriteErr(w, r, http.StatusInternalServerError, "Внутренняя ошибка сервера")
}

// Decode reads a JSON body of at most MaxJSON bytes; false: a 400 is written.
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxJSON+1))
	if err == nil && len(body) > MaxJSON {
		err = errors.New("body too large")
	}
	if err == nil {
		err = json.Unmarshal(body, v)
	}
	if err != nil {
		WriteErr(w, r, http.StatusBadRequest, "Неверный запрос")
		return false
	}
	return true
}

// IDParam is the path's {name} as a positive id; false: a 400 is written.
func IDParam(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		WriteErr(w, r, http.StatusBadRequest, "Неверный запрос")
		return 0, false
	}
	return id, true
}

// Adults lets signed-in adult profiles through, in their language.
func Adults(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := addonkit.UserFrom(r)
		if !ok || u.IsKid {
			WriteErr(w, r, http.StatusForbidden, "Недоступно в детском профиле")
			return
		}
		if u.Lang != "" {
			r = r.WithContext(i18n.WithLanguage(r.Context(), u.Lang))
		}
		next.ServeHTTP(w, r)
	})
}

// msgErr is a service's Russian message with the error it came with.
type msgErr struct {
	msg string
	err error
}

func (e msgErr) Error() string { return e.msg }
func (e msgErr) Unwrap() error { return e.err }

// Message is msg (Russian) in the request's language; an *i18n.Error in
// err's chain is rendered whole instead.
func Message(r *http.Request, msg string, err error) string {
	return i18n.Message(r.Context(), msgErr{msg, err})
}

// Status is the HTTP status of an acquire error's kind (500 for others).
func Status(err error) int {
	switch {
	case errors.Is(err, acquire.ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, acquire.ErrNotFound), errors.Is(err, acquire.ErrNoRelease):
		return http.StatusNotFound
	case errors.Is(err, acquire.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, acquire.ErrConflict), errors.Is(err, acquire.ErrInProgress):
		return http.StatusConflict
	case errors.Is(err, acquire.ErrNoFiles):
		return http.StatusUnprocessableEntity
	case errors.Is(err, acquire.ErrSource):
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// AcquireErr answers an acquire service error: its message in the request's
// language with the status of its kind (a «no format» answer also carries
// {"code": "no_format"}); anything else is a logged 500, as "<pkg>: <what>".
func AcquireErr(w http.ResponseWriter, r *http.Request, pkg, what string, err error) {
	if r.Context().Err() != nil {
		return // the client went away
	}
	msg, ok := acquire.UserMessage(err)
	if !ok {
		InternalErr(w, r, pkg, what, err)
		return
	}
	code := Status(err)
	if code >= 500 {
		slog.Warn(pkg+": "+what, "err", err)
	}
	msg = Message(r, msg, err)
	if errors.Is(err, acquire.ErrNoFormat) {
		// «Скачать в другом формате» resends the request with anyFormat
		addonkit.WriteJSON(w, code, map[string]string{"error": msg, "code": "no_format"})
		return
	}
	addonkit.WriteError(w, code, msg)
}
