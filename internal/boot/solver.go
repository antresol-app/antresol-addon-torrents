package boot

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// solverSources is what the watch changes of the sources manager
// (*indexer.Manager).
type solverSources interface {
	SetFlareSolverr(url string)
	RecheckFailed(ctx context.Context)
}

// SolverWatch finds the FlareSolverr helper service the manifest declares
// by its compose name, for when the admin gave no address: the one saved
// in «Настройки загрузок» and the server's
// ANTRESOL_TORRENTS_FLARESOLVERR_URL both win over it. Found, the sources
// use it and the failed ones are checked again; gone, they go without.
type SolverWatch struct {
	*addonkit.HelperWatch
	sources solverSources
	saved   func(ctx context.Context) (string, error)
	server  string
}

// NewSolverWatch watches w for sources; saved reads the address saved in
// the settings, server is the server's variable.
func NewSolverWatch(w *addonkit.HelperWatch, sources solverSources, saved func(ctx context.Context) (string, error), server string) *SolverWatch {
	return &SolverWatch{HelperWatch: w, sources: sources, saved: saved, server: server}
}

// Check probes once and applies a change.
func (s *SolverWatch) Check(ctx context.Context) bool {
	before := s.HelperWatch.Found()
	found := s.HelperWatch.Check(ctx)
	if found != before {
		s.apply(ctx, found)
	}
	return found
}

// Run checks at once, then every 30 s until ctx ends (a boot loop).
func (s *SolverWatch) Run(ctx context.Context) {
	s.Check(ctx)
	every := s.Every
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Check(ctx)
		}
	}
}

func (s *SolverWatch) apply(ctx context.Context, found bool) {
	if s.server != "" {
		return
	}
	saved, err := s.saved(ctx)
	if err != nil || saved != "" {
		return
	}
	log.Printf("torrents: FlareSolverr at %s: found %v", s.Helper.Address(), found)
	s.sources.SetFlareSolverr(s.HelperWatch.Address())
	if found {
		s.sources.RecheckFailed(ctx)
	}
}

// SolverStatus answers helpers.status for FlareSolverr: the address the
// sources go to — the one saved in «Настройки загрузок», else the server's
// ANTRESOL_TORRENTS_FLARESOLVERR_URL, else the helper on its compose name
// — and whether it answers (asked at most every 2 minutes, 10 s).
type SolverStatus struct {
	// Watch is the helper on its compose name (nil: none declared).
	Watch *addonkit.HelperWatch
	// Saved reads the address saved in the settings; Server is the
	// server's variable.
	Saved  func(ctx context.Context) (string, error)
	Server string
	// Probe asks an address (nil: addonkit.ProbeAddress, "/").
	Probe func(ctx context.Context, base string) error

	mu     sync.Mutex
	addr   string
	health *addonkit.HelperHealth
}

// healthOf is the rolling health of addr (a new one when it changed).
func (s *SolverStatus) healthOf(addr string) *addonkit.HelperHealth {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.health == nil || s.addr != addr {
		s.addr, s.health = addr, &addonkit.HelperHealth{}
	}
	return s.health
}

// HelpersStatus is the hook's answer; never an address the admin gave.
func (s *SolverStatus) HelpersStatus(ctx context.Context) addonkit.HelpersStatus {
	name := "flaresolverr"
	if s.Watch != nil {
		name = s.Watch.Helper.Name
	}
	st := addonkit.HelperState{Name: name, Source: addonkit.HelperNone}
	addr := ""
	if s.Saved != nil {
		if u, err := s.Saved(ctx); err == nil && u != "" {
			addr, st.Source = u, addonkit.HelperSettings
		}
	}
	if addr == "" && s.Server != "" {
		addr, st.Source = s.Server, addonkit.HelperOverride
	}
	if addr == "" {
		if s.Watch == nil {
			st.Problem = addonkit.HelperProblemUnset
			return addonkit.HelpersStatus{Helpers: []addonkit.HelperState{st}}
		}
		// the compose name: the watch's own probes (every 30 s)
		st.Address = s.Watch.Helper.Address()
		if s.Watch.Found() {
			st.Source = addonkit.HelperCompose
		}
		return addonkit.HelpersStatus{Helpers: []addonkit.HelperState{s.Watch.Health.State(ctx, st, nil)}}
	}
	probe := s.Probe
	if probe == nil {
		probe = func(ctx context.Context, base string) error { return addonkit.ProbeAddress(ctx, nil, base, "/", false) }
	}
	st = s.healthOf(addr).State(ctx, st, func(ctx context.Context) error { return probe(ctx, addr) })
	return addonkit.HelpersStatus{Helpers: []addonkit.HelperState{st}}
}

// Register answers helpers.status on m.
func (s *SolverStatus) Register(m *addonkit.HookMux) {
	m.Handle("helpers.status", func(ctx context.Context, _ *addonkit.User, _ json.RawMessage) (any, error) {
		return s.HelpersStatus(ctx), nil
	})
}
