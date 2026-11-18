package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antresol-app/antresol/addonkit"
)

// fakeSolver answers GET / as FlareSolverr does (or as something else).
func fakeSolver(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestFlareSolverrSavedOnlyWhenItAnswers(t *testing.T) {
	e := newEnv(t, false, false)
	ok := fakeSolver(t, `{"msg":"FlareSolverr is ready!","version":"3.3.21"}`)

	// something else at the address: refused, nothing saved
	other := fakeSolver(t, `{"status":"ok"}`)
	w := e.do("PUT", "/api/admin/settings/flaresolverr", `{"url":"`+other+`"}`, &admin)
	if w.Code != http.StatusUnprocessableEntity || errorOf(w) != "По этому адресу отвечает не FlareSolverr." {
		t.Fatalf("not FlareSolverr: %d %s", w.Code, w.Body)
	}
	if u, _ := e.st.FlareSolverrURL(context.Background()); u != "" || e.src.FlareSolverr() != "" {
		t.Fatalf("saved after a refusal: %q %q", u, e.src.FlareSolverr())
	}
	// nothing listening
	w = e.do("PUT", "/api/admin/settings/flaresolverr", `{"url":"http://127.0.0.1:1"}`, &admin)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(errorOf(w), "Не удалось подключиться к FlareSolverr") {
		t.Fatalf("unreachable: %d %s", w.Code, w.Body)
	}
	// not an address
	w = e.do("PUT", "/api/admin/settings/flaresolverr", `{"url":"ftp://x"}`, &admin)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad scheme: %d", w.Code)
	}

	// FlareSolverr: saved (a trailing slash dropped), the searches use it, GET shows it
	w = e.do("PUT", "/api/admin/settings/flaresolverr", `{"url":" `+ok+`/ "}`, &admin)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"version":"3.3.21"`) {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if e.src.FlareSolverr() != ok {
		t.Fatalf("manager uses %q, want %q", e.src.FlareSolverr(), ok)
	}
	w = e.do("GET", "/api/admin/settings", "", &admin)
	if !strings.Contains(w.Body.String(), `"flareSolverrUrl":"`+ok+`"`) {
		t.Fatalf("GET settings: %s", w.Body)
	}
	// a general settings save leaves it alone
	e.do("PUT", "/api/admin/settings", `{"flareSolverrUrl":"http://other:1"}`, &admin)
	if u, _ := e.st.FlareSolverrURL(context.Background()); u != ok {
		t.Fatalf("general save changed it to %q", u)
	}
	// an adult may not
	if w := e.do("PUT", "/api/admin/settings/flaresolverr", `{"url":""}`, &adult); w.Code != http.StatusForbidden {
		t.Fatalf("adult: %d", w.Code)
	}
}

func TestFlareSolverrClearedFallsBackToTheServers(t *testing.T) {
	e := newEnv(t, false, false)
	e.h = Routes(Deps{Acquire: e.acq, Sources: e.src, Settings: e.st, NoSeed: e.ns, Upload: e.eng,
		StreamCacheDefaultGB: 50, FlareSolverrServer: "http://server-solver:8191"})
	if err := e.st.SetFlareSolverrURL(context.Background(), "http://saved:8191"); err != nil {
		t.Fatal(err)
	}
	w := e.do("PUT", "/api/admin/settings/flaresolverr", `{"url":""}`, &admin)
	if w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	if u, _ := e.st.FlareSolverrURL(context.Background()); u != "" {
		t.Fatalf("still saved: %q", u)
	}
	if e.src.FlareSolverr() != "http://server-solver:8191" {
		t.Fatalf("manager uses %q", e.src.FlareSolverr())
	}
	w = e.do("GET", "/api/admin/settings", "", &admin)
	if !strings.Contains(w.Body.String(), `"flareSolverrServer":"http://server-solver:8191"`) {
		t.Fatalf("GET settings: %s", w.Body)
	}
}

// TestFlareSolverrHelper: with nothing saved and no server address the
// settings show the helper service (found: its compose address; not: the
// block to add), and clearing the field falls back to the found helper.
func TestFlareSolverrHelper(t *testing.T) {
	up := false
	hw := &addonkit.HelperWatch{Helper: addonkit.Helper{Name: "flaresolverr", Image: "img:1", Port: 8191, Health: "/"},
		Probe: func(context.Context) error {
			if up {
				return nil
			}
			return errors.New("down")
		}}
	e := newEnv(t, false, false)
	e.h = Routes(Deps{Acquire: e.acq, Sources: e.src, Settings: e.st, NoSeed: e.ns, Upload: e.eng,
		StreamCacheDefaultGB: 50, FlareSolverrHelper: hw})
	helperOf := func() addonkit.HelperStatus {
		var v struct {
			FlareSolverrHelper *addonkit.HelperStatus `json:"flareSolverrHelper"`
		}
		w := e.do("GET", "/api/admin/settings", "", &admin)
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil || v.FlareSolverrHelper == nil {
			t.Fatalf("GET settings: %d %s", w.Code, w.Body)
		}
		return *v.FlareSolverrHelper
	}
	if st := helperOf(); st.Source != addonkit.HelperNone || !strings.Contains(st.Compose, "image: img:1") {
		t.Fatalf("not found: %+v", st)
	}
	hw.Check(context.Background())
	if st := helperOf(); st.Source != addonkit.HelperNone {
		t.Fatalf("still not found: %+v", st)
	}
	up = true
	hw.Check(context.Background())
	if st := helperOf(); st.Source != addonkit.HelperCompose || st.Address != "http://flaresolverr:8191" || st.Compose != "" {
		t.Fatalf("found: %+v", st)
	}
	// clearing the field: the helper again
	if w := e.do("PUT", "/api/admin/settings/flaresolverr", `{"url":""}`, &admin); w.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	if e.src.FlareSolverr() != "http://flaresolverr:8191" {
		t.Fatalf("manager uses %q", e.src.FlareSolverr())
	}
	// a saved address: the helper is the admin's
	if err := e.st.SetFlareSolverrURL(context.Background(), "http://192.168.50.20:8191"); err != nil {
		t.Fatal(err)
	}
	if st := helperOf(); st.Source != addonkit.HelperOverride || st.Address != "" {
		t.Fatalf("saved: %+v", st)
	}
}

func TestNormalizeSolverURL(t *testing.T) {
	for in, want := range map[string]string{
		"flaresolverr:8191":     "http://flaresolverr:8191",
		"http://10.0.0.5:8191/": "http://10.0.0.5:8191",
		"https://fs.example/":   "https://fs.example",
		"":                      "",
	} {
		if got, ok := normalizeSolverURL(in); !ok || got != want {
			t.Errorf("%q → %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"ftp://x", "http://", "http://x/?a=1"} {
		if _, ok := normalizeSolverURL(bad); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}
