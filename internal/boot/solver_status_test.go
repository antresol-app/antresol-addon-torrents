package boot

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antresol-app/antresol/addonkit"
)

// helpers.status of FlareSolverr: the address the sources use (the saved
// one, else the server's, else the compose name), never shown when it is
// the admin's; the address saved in «Настройки загрузок» is the one asked.
func TestSolverStatus(t *testing.T) {
	ctx := context.Background()
	saved := ""
	var asked []string
	w := &addonkit.HelperWatch{Helper: addonkit.Helper{Name: "flaresolverr", Image: "img", Port: 8191, Health: "/"},
		Health: &addonkit.HelperHealth{}, Probe: func(context.Context) error { return errors.New("no such host") }}
	s := &SolverStatus{Watch: w, Saved: func(context.Context) (string, error) { return saved, nil },
		Probe: func(_ context.Context, base string) error { asked = append(asked, base); return nil }}

	// nothing set, the helper not found: its compose address, its probe's outcome
	w.Check(ctx)
	st := s.HelpersStatus(ctx).Helpers[0]
	if st.Source != addonkit.HelperNone || st.Address != "http://flaresolverr:8191" || st.OK || len(asked) != 0 {
		t.Fatalf("none: %+v asked %v", st, asked)
	}
	// the address saved in the addon's settings: asked there, not shown
	saved = "http://solver-own:8191"
	st = s.HelpersStatus(ctx).Helpers[0]
	if st.Source != addonkit.HelperSettings || st.Address != "" || !st.OK || len(asked) != 1 || asked[0] != saved {
		t.Fatalf("settings: %+v asked %v", st, asked)
	}
	// fresh: not asked again
	s.HelpersStatus(ctx)
	if len(asked) != 1 {
		t.Fatalf("asked again: %v", asked)
	}
	// the server's variable when nothing is saved
	saved, s.Server = "", "http://srv:8191"
	if st := s.HelpersStatus(ctx).Helpers[0]; st.Source != addonkit.HelperOverride || st.Address != "" || asked[len(asked)-1] != "http://srv:8191" {
		t.Fatalf("server: %+v %v", st, asked)
	}
	// no helper declared and nothing set
	n := &SolverStatus{}
	if st := n.HelpersStatus(ctx).Helpers[0]; st.Problem != addonkit.HelperProblemUnset {
		t.Fatalf("unset: %+v", st)
	}
}

// The hook answers the same over HTTP.
func TestSolverStatusHook(t *testing.T) {
	m := &addonkit.HookMux{}
	(&SolverStatus{Server: "http://srv:8191", Probe: func(context.Context, string) error { return nil }}).Register(m)
	w := httptest.NewRecorder()
	const hook = "helpers.status"
	m.ServeHTTP(w, httptest.NewRequest("POST", "/hooks/"+hook, strings.NewReader(`{}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"source":"override"`) || strings.Contains(w.Body.String(), "srv:8191") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
