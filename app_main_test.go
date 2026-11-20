package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/find"
	"github.com/antresol-app/antresol-addon-torrents/internal/followapi"
	"github.com/antresol-app/antresol-addon-torrents/internal/watch"
	"github.com/antresol-app/antresol/addonkit"
)

// TestAppRoutes_OnProfilesMux: the app protocol's routes join the
// profiles' mux (main adds them to profileRoutes' answer) without a pattern
// of the watch, find, follow or pin routes clashing with them (the mux
// panics on a clash), and they are not under the admins' /api/admin/.
func TestAppRoutes_OnProfilesMux(t *testing.T) {
	mux := profileRoutes(watch.Deps{}, find.Deps{}, followapi.Deps{})
	find.RegisterApp(mux, find.AppDeps{
		HasSources:  func(context.Context) (bool, error) { return true, nil },
		HasMetadata: func() bool { return true },
	})
	for _, c := range []struct {
		path string
		code int
	}{{"/api/app", http.StatusOK}, {"/api/admin/app", http.StatusNotFound}} {
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		r.Header.Set(addonkit.UserHeader, `{"id":2,"name":"b","isAdmin":false,"isKid":false,"lang":"ru"}`)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != c.code || (c.code == http.StatusOK && !strings.Contains(w.Body.String(), `"id":"watch"`)) {
			t.Fatalf("GET %s: %d %s", c.path, w.Code, w.Body)
		}
	}
}
