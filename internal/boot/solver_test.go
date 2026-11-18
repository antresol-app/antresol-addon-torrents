package boot

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/antresol-app/antresol/addonkit"
)

type fakeSolverSources struct {
	mu      sync.Mutex
	set     []string
	recheck int
}

func (f *fakeSolverSources) SetFlareSolverr(u string) {
	f.mu.Lock()
	f.set = append(f.set, u)
	f.mu.Unlock()
}
func (f *fakeSolverSources) RecheckFailed(context.Context) {
	f.mu.Lock()
	f.recheck++
	f.mu.Unlock()
}

func solverHelper(up *bool) *addonkit.HelperWatch {
	return &addonkit.HelperWatch{Helper: addonkit.Helper{Name: "flaresolverr", Image: "img", Port: 8191, Health: "/"},
		Probe: func(context.Context) error {
			if *up {
				return nil
			}
			return errors.New("down")
		}}
}

// TestSolverWatch_UsesTheHelperWhenNothingIsSet: with no saved address and
// no server variable the helper's compose address is used once it
// answers (and the failed sources are checked again), and dropped when it
// goes away.
func TestSolverWatch_UsesTheHelperWhenNothingIsSet(t *testing.T) {
	up := false
	src := &fakeSolverSources{}
	w := NewSolverWatch(solverHelper(&up), src, func(context.Context) (string, error) { return "", nil }, "")
	ctx := context.Background()
	w.Check(ctx)
	if len(src.set) != 0 {
		t.Fatalf("set %v while not found", src.set)
	}
	up = true
	w.Check(ctx)
	if len(src.set) != 1 || src.set[0] != "http://flaresolverr:8191" || src.recheck != 1 {
		t.Fatalf("found: set %v, rechecks %d", src.set, src.recheck)
	}
	w.Check(ctx) // no change: nothing again
	if len(src.set) != 1 {
		t.Fatalf("set again: %v", src.set)
	}
	up = false
	w.Check(ctx)
	if len(src.set) != 2 || src.set[1] != "" || src.recheck != 1 {
		t.Fatalf("lost: set %v, rechecks %d", src.set, src.recheck)
	}
}

// TestSolverWatch_TheAdminsAddressWins: a saved address or the server's
// variable is never replaced by the helper.
func TestSolverWatch_TheAdminsAddressWins(t *testing.T) {
	for name, c := range map[string]struct{ saved, server string }{
		"saved":  {"http://192.168.50.20:8191", ""},
		"server": {"", "http://192.168.50.21:8191"},
	} {
		t.Run(name, func(t *testing.T) {
			up := true
			src := &fakeSolverSources{}
			w := NewSolverWatch(solverHelper(&up), src, func(context.Context) (string, error) { return c.saved, nil }, c.server)
			w.Check(context.Background())
			if len(src.set) != 0 || src.recheck != 0 {
				t.Fatalf("set %v, rechecks %d", src.set, src.recheck)
			}
		})
	}
	// a saved address that cannot be read: nothing is changed
	up := true
	src := &fakeSolverSources{}
	w := NewSolverWatch(solverHelper(&up), src, func(context.Context) (string, error) { return "", errors.New("db") }, "")
	w.Check(context.Background())
	if len(src.set) != 0 {
		t.Fatalf("set %v on a read error", src.set)
	}
}
