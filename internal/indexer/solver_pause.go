package indexer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// solverPause is what is held against a source whose requests FlareSolverr
// failed (it was unreachable, took too long or did not get through the
// challenge): until when its searches are not sent, and how many such
// failures there were in a row. Each costs a browser on FlareSolverr's host
// for up to a minute, and a wave of searches (a round of tracked titles
// that fall due together) would otherwise ask again at every title.
type solverPause struct {
	until time.Time
	fails int
}

// solverPausedError is the answer of a search of a source that is paused
// for a failed FlareSolverr: a Cloudflare error that says until when. cause
// is the failure that started the pause (nil: an earlier one).
type solverPausedError struct {
	until time.Time
	cause error
}

func (e *solverPausedError) Error() string {
	return fmt.Sprintf("%s: FlareSolverr failed, the source is paused until %s", cardigann.ErrCloudflare, e.until.Format("15:04"))
}

func (e *solverPausedError) Unwrap() []error {
	if e.cause != nil {
		return []error{cardigann.ErrCloudflare, e.cause}
	}
	return []error{cardigann.ErrCloudflare}
}

// solverPausedUntil: source id's searches are not sent until the returned
// time. The note outlives the pause (for the count of failures in a row)
// until an answer or wake ends it.
func (m *Manager) solverPausedUntil(id int64) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.paused[id]
	if !ok || !m.now().Before(p.until) {
		return time.Time{}, false
	}
	return p.until, true
}

// solverOutcome is what runner r of source id heard from FlareSolverr
// (cardigann.Runner.OnSolve). A solved page ends the run of failures. A
// failure (an ErrCloudflare) is one more in the run and pauses the source
// for cardigann.BackoffAfter of the count; while the source is paused it
// changes nothing (a request let through all the same, the admin's test or a
// download, is no new failure). It runs when FlareSolverr answers, also
// when the search that asked has given up long before: the source's status
// then says why, where its search saw only a timeout. A runner the source
// has left (new settings) is not heard.
func (m *Manager) solverOutcome(id int64, r *cardigann.Runner, err error) {
	if err != nil && !errors.Is(err, cardigann.ErrCloudflare) {
		return
	}
	m.mu.Lock()
	if c, ok := m.runners[id]; !ok || c.runner != r {
		m.mu.Unlock()
		return
	}
	if err == nil {
		delete(m.paused, id)
		m.mu.Unlock()
		return
	}
	if m.paused == nil {
		m.paused = map[int64]solverPause{}
	}
	p := m.paused[id]
	if m.now().Before(p.until) {
		// paused already: a request that was let through all the same (the
		// admin's test, a download) is no new failure in a row
		m.mu.Unlock()
		return
	}
	p.fails++
	p.until = m.now().Add(cardigann.BackoffAfter(p.fails))
	m.paused[id] = p
	m.mu.Unlock()
	m.log.Info("indexer: FlareSolverr failed, source paused", "id", id, "fails", p.fails, "until", p.until.Format(time.RFC3339), "err", cardigann.RedactText(err.Error()))
	m.setStatus(context.Background(), id, StatusError, humanError(&solverPausedError{until: p.until, cause: err}))
}

// pausedFailure is err as the failure of source id when it left the source
// paused (see solverOutcome): a solverPausedError that says until when; any
// other error, or one that paused nothing, comes back as it is.
func (m *Manager) pausedFailure(id int64, err error) error {
	if !errors.Is(err, cardigann.ErrCloudflare) || errors.Is(err, cardigann.ErrSolverBusy) {
		return err
	}
	until, ok := m.solverPausedUntil(id)
	if !ok {
		return err
	}
	return &solverPausedError{until: until, cause: err}
}
