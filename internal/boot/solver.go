package boot

import (
	"context"
	"log"
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
