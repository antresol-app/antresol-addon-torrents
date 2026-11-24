package cardigann

import (
	"context"
	"errors"
	"time"
)

// ErrSolverBusy comes with ErrCloudflare when a request gave up waiting for
// FlareSolverr's single slot (see SolverGate): nothing was sent, and nothing
// is known about the site or about FlareSolverr's health.
var ErrSolverBusy = errors.New("FlareSolverr занят другим запросом")

// FlareSolverr's time limits (variables for tests).
var (
	// solverMaxTimeout is the maxTimeout sent with every request:
	// FlareSolverr gives up on a challenge itself after this long.
	solverMaxTimeout = 60 * time.Second
	// solverCallTimeout bounds the HTTP call to it: the maxTimeout and the
	// time it needs to start and to stop its browser around the challenge.
	solverCallTimeout = solverMaxTimeout + 30*time.Second
)

// SolverGate lets one request at a time through to FlareSolverr. Every
// request starts a browser there and a request it is asked for is never
// cancelled when the asker leaves, so several at once only make it slower
// until the host runs out of memory. Share one gate between all the
// runners of a process (Runner.SolverGate); a runner given none uses a
// gate of its own.
type SolverGate struct{ slot chan struct{} }

// NewSolverGate makes a gate with its one slot free.
func NewSolverGate() *SolverGate { return &SolverGate{slot: make(chan struct{}, 1)} }

// acquire takes the slot, waiting for it while ctx lasts (no more than
// ctx: a waiter never queues past its own deadline). A slot that came free
// just as ctx ended is let go again: nothing is sent for an asker that has
// gone.
func (g *SolverGate) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case g.slot <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-g.slot
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *SolverGate) release() { <-g.slot }
