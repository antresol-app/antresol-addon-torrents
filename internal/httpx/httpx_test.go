package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol/addonkit"
)

// Every kind of acquire error has its status, whichever API answers it.
func TestStatus(t *testing.T) {
	for _, c := range []struct {
		err  error
		want int
	}{
		{acquire.ErrInvalid, http.StatusBadRequest},
		{acquire.ErrNotFound, http.StatusNotFound},
		{acquire.ErrNoRelease, http.StatusNotFound},
		{acquire.ErrUnavailable, http.StatusServiceUnavailable},
		{acquire.ErrConflict, http.StatusConflict},
		{acquire.ErrInProgress, http.StatusConflict},
		{acquire.ErrNoFiles, http.StatusUnprocessableEntity},
		{acquire.ErrSource, http.StatusBadGateway},
		{fmt.Errorf("wrapped: %w", acquire.ErrSource), http.StatusBadGateway},
		{errors.New("other"), http.StatusInternalServerError},
	} {
		if got := Status(c.err); got != c.want {
			t.Errorf("%v: %d, want %d", c.err, got, c.want)
		}
	}
}

func errorOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var v struct{ Error string }
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("%q: %v", w.Body, err)
	}
	return v.Error
}

func TestAcquireErr(t *testing.T) {
	// a service error with a message of its own: its status, its message
	_, userErr := (&acquire.Service{}).Discover(context.Background(), "x")
	if userErr == nil {
		t.Fatal("no user error to test with")
	}
	w := httptest.NewRecorder()
	AcquireErr(w, httptest.NewRequest("GET", "/", nil), "test", "discover", userErr)
	if w.Code != Status(userErr) || errorOf(t, w) == "" || strings.Contains(errorOf(t, w), "Внутренняя") {
		t.Fatalf("user error: %d %s", w.Code, w.Body)
	}
	// anything else: a 500 that tells nothing
	w = httptest.NewRecorder()
	AcquireErr(w, httptest.NewRequest("GET", "/", nil), "test", "discover", errors.New("db: secret path /x"))
	if w.Code != http.StatusInternalServerError || errorOf(t, w) != "Внутренняя ошибка сервера" {
		t.Fatalf("plain error: %d %s", w.Code, w.Body)
	}
	// the client went away: nothing is written
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w = httptest.NewRecorder()
	AcquireErr(w, httptest.NewRequest("GET", "/", nil).WithContext(ctx), "test", "discover", userErr)
	if w.Body.Len() != 0 {
		t.Fatalf("answered a gone client: %s", w.Body)
	}
}

func TestDecode(t *testing.T) {
	var v struct{ A int }
	w := httptest.NewRecorder()
	if !Decode(w, httptest.NewRequest("POST", "/", strings.NewReader(`{"A":3}`)), &v) || v.A != 3 {
		t.Fatalf("a good body: %+v %s", v, w.Body)
	}
	for name, body := range map[string]string{"not json": `{`, "too large": `{"A":1,"pad":"` + strings.Repeat("x", MaxJSON) + `"}`} {
		w = httptest.NewRecorder()
		if Decode(w, httptest.NewRequest("POST", "/", strings.NewReader(body)), &v) || w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

func TestIDParam(t *testing.T) {
	for path, want := range map[string]int64{"5": 5, "0": 0, "-1": 0, "x": 0} {
		r := httptest.NewRequest("GET", "/", nil)
		r.SetPathValue("id", path)
		w := httptest.NewRecorder()
		id, ok := IDParam(w, r, "id")
		if id != want || ok != (want > 0) || (!ok && w.Code != http.StatusBadRequest) {
			t.Errorf("%q: %d %v %d", path, id, ok, w.Code)
		}
	}
}

func TestAdults(t *testing.T) {
	h := Adults(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, c := range []struct {
		user *addonkit.User
		want int
	}{{nil, http.StatusForbidden}, {&addonkit.User{ID: 1, IsKid: true}, http.StatusForbidden}, {&addonkit.User{ID: 1, Lang: "en"}, http.StatusNoContent}} {
		r := httptest.NewRequest("GET", "/", nil)
		if c.user != nil {
			b, _ := json.Marshal(c.user)
			r.Header.Set(addonkit.UserHeader, string(b))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%+v: %d, want %d", c.user, w.Code, c.want)
		}
	}
}
