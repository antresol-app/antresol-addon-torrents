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
// challenge): until when its background searches are not sent, and how many
// such failures there were in a row. Each costs a browser on FlareSolverr's
// host for up to a minute, and a wave of searches (a round of tracked titles
// that fall due together) would otherwise ask again at every title. The
// pause holds only those searches nobody waits on (see Background): a search
// a person started still tries the source.
type solverPause struct {
	until time.Time
	fails int
}

// solverPausedError is the answer of a background search of a source that
// is paused for a failed FlareSolverr: a Cloudflare error that says until
// when automatic checks resume. cause is the failure that started the pause
// (nil: an earlier one).
type solverPausedError struct {
	until time.Time
	cause error
}

func (e *solverPausedError) Error() string {
	return fmt.Sprintf("%s: FlareSolverr failed, automatic checks of the source are paused until %s", cardigann.ErrCloudflare, e.until.Format("15:04"))
}

func (e *solverPausedError) Unwrap() []error {
	if e.cause != nil {
		return []error{cardigann.ErrCloudflare, e.cause}
	}
	return []error{cardigann.ErrCloudflare}
}

// solverPausedUntil: source id's background searches are not sent until
// the returned time. The note outlives the pause (for the count of failures
// in a row) until an answer or wake ends it.
func (m *Manager) solverPausedUntil(id int64) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.paused[id]
	if !ok || !m.now().Before(p.until) {
		return time.Time{}, false
	}
	return p.until, true
}

// clearSolverPause ends the pause of source id and its run of failures: it
// answered a search (a person's too: the pause is only a guess that it
// would not).
func (m *Manager) clearSolverPause(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.paused, id)
}

// solverOutcome is what runner r of source id heard from FlareSolverr
// (cardigann.Runner.OnSolve) for the request of ctx's asker. A solved page
// ends the run of failures, whoever asked. A failure (an ErrCloudflare) of
// a background request is one more in the run and pauses the source for
// cardigann.BackoffAfter of the count; while the source is paused it
// changes nothing (a request let through all the same, a download, is no
// new failure). A failure of a request a person started (a search, the
// admin's test) never starts or lengthens a pause: only the automatic
// checks that the pause would hold back count. It runs when FlareSolverr
// answers, also when the search that asked has given up long before: the
// source's status then says why, where its search saw only a timeout. A
// runner the source has left (new settings) is not heard.
func (m *Manager) solverOutcome(ctx context.Context, id int64, r *cardigann.Runner, err error) {
	if err != nil && (!errors.Is(err, cardigann.ErrCloudflare) || !IsBackground(ctx)) {
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
		// paused already: a request that was let through all the same (a
		// download) is no new failure in a row
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

// recordFailure stores err as the status of source id after a search or a
// test failed, and returns the error to report. A background search that
// left the source paused says until when automatic checks resume. A
// person's search or test reports the failure itself (it just tried, a time
// to come back is not what it asks) and, when the source is paused all the
// same, leaves the status as the pause set it: the pause is what holds for
// the checks that follow.
func (m *Manager) recordFailure(ctx context.Context, id int64, err error) error {
	if IsBackground(ctx) {
		err = m.pausedFailure(id, err)
	} else if _, paused := m.solverPausedUntil(id); paused && errors.Is(err, cardigann.ErrCloudflare) {
		return err
	}
	m.setStatus(ctx, id, StatusError, humanError(err))
	return err
}
