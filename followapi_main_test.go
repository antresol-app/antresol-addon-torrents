package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/find"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/followapi"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/watch"
	"github.com/antresol-app/antresol/addonkit"
)

// TestFollowAPI_NotUnderAdmin: the follow routes are on the profiles'
// mux: an adult's GET /api/follow reaches the handler there (main mounts
// that mux at /api/, the admins' at /api/admin/).
func TestFollowAPI_NotUnderAdmin(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mux := profileRoutes(watch.Deps{}, find.Deps{}, followapi.Deps{Follows: &follow.Service{DB: db}})
	for _, c := range []struct {
		path string
		code int
	}{{"/api/follow", http.StatusOK}, {"/api/admin/follow", http.StatusNotFound}} {
		r := httptest.NewRequest(http.MethodGet, c.path, nil)
		r.Header.Set(addonkit.UserHeader, `{"id":2,"name":"b","isAdmin":false,"isKid":false,"lang":"ru"}`)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != c.code || (c.code == http.StatusOK && strings.TrimSpace(w.Body.String()) != `{"items":[]}`) {
			t.Fatalf("GET %s: %d %s", c.path, w.Code, w.Body)
		}
	}
}
