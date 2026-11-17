package boot

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol/addonkit"
)

// kickRetryEvery is how long a follow's ensure+kick the core did not take
// waits before it is sent again.
const kickRetryEvery = 30 * time.Second

// sweepTimeout bounds the stale-file sweep after a finished download.
var sweepTimeout = 30 * time.Second

// titleKicker is what a follow turned on needs of the core's tracked
// titles (coreadapt.Tracking).
type titleKicker interface {
	EnsureTitle(ctx context.Context, kind string, tmdbID int64, name, poster string) (tracking.Title, error)
	Kick(ctx context.Context, id int64) error
}

// followKicks is follow.Service.OnFollowed: a follow turned on has the
// core track its title (EnsureTitle) and refresh it soon (Kick), so the
// pinned release's missing episodes are looked at at once. It never waits
// on the core: Followed only queues; Run sends. A send the core did not
// take (unreachable, an error) is kept in memory and sent again every
// retry (no persistence; after a restart the core's own round of
// tracking.titles tracks every active follow). A kick answered 404
// (the core does not track that id: it dropped the title meanwhile) makes
// a new ensure at once, not a retry of the same kick.
type followKicks struct {
	core  titleKicker
	every func() time.Duration // read at each retry (tests shorten it)
	log   func() *slog.Logger

	mu      sync.Mutex
	pending map[followKey]follow.Follow
	wake    chan struct{}
}

type followKey struct {
	kind string
	tmdb int
}

func newFollowKicks(core titleKicker, every func() time.Duration, log func() *slog.Logger) *followKicks {
	return &followKicks{core: core, every: every, log: log,
		pending: map[followKey]follow.Follow{}, wake: make(chan struct{}, 1)}
}

// Followed queues f's title for an ensure+kick (OnFollowed).
func (k *followKicks) Followed(_ context.Context, f follow.Follow) {
	k.mu.Lock()
	k.pending[followKey{f.Kind, f.TMDBID}] = f
	k.mu.Unlock()
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

// Run sends the queued ensure+kicks until ctx ends: at once when woken,
// the ones the core did not take again every retry.
func (k *followKicks) Run(ctx context.Context) {
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-k.wake:
		case <-retry:
		}
		retry = nil
		if !k.sendAll(ctx) && ctx.Err() == nil {
			retry = time.After(k.every())
		}
	}
}

// sendAll sends every queued follow; false when one is left queued.
func (k *followKicks) sendAll(ctx context.Context) bool {
	k.mu.Lock()
	batch := k.pending
	k.pending = map[followKey]follow.Follow{}
	k.mu.Unlock()
	ok := true
	for key, f := range batch {
		err := k.send(ctx, f)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return true
		}
		k.log().Warn("torrents: followed title not sent to the core (retried)", "kind", f.Kind, "tmdb", f.TMDBID, "err", err, "in", k.every())
		k.mu.Lock()
		if _, newer := k.pending[key]; !newer {
			k.pending[key] = f
		}
		k.mu.Unlock()
		ok = false
	}
	return ok
}

// send is one ensure+kick; a kick of an id the core no longer tracks
// (404) ensures the title again and kicks the new id.
func (k *followKicks) send(ctx context.Context, f follow.Follow) error {
	for range 2 {
		t, err := k.core.EnsureTitle(ctx, f.Kind, int64(f.TMDBID), f.Title, f.Poster)
		if err != nil {
			return err
		}
		err = k.core.Kick(ctx, t.ID)
		if !addonkit.IsNotFound(err) {
			return err
		}
	}
	return errKickedUnknown
}

// errKickedUnknown: the core answered 404 to the kick of a title it had
// just ensured, twice.
var errKickedUnknown = errors.New("boot: the core does not track the title it just ensured")

// sweepStale is the finish callback's hook for the stale files of a
// renamed pinned re-upload: after a download that did not fail, the pins
// of its release delete their old files (pinned.Service.SweepStale) in the
// background, under sweepTimeout on the run context (acquire never waits
// on it); an error is logged. The holds go out again after it.
func (s *Services) sweepStale(f acquire.Finished) {
	if f.Failed || s.sweep == nil {
		return
	}
	run := s.runCtx
	if run == nil {
		run = context.Background()
	}
	go func() {
		ctx, cancel := context.WithTimeout(run, sweepTimeout)
		defer cancel()
		if err := s.sweep(ctx, f.Key); err != nil && run.Err() == nil {
			s.log().Warn("torrents: old files of a replaced pinned release", "key", f.Key, "err", err)
		}
		s.holdsChanged()
	}()
}

// holdsChanged calls HoldsChanged as it is now (main sets it after Build:
// the holds pusher is made from the services).
func (s *Services) holdsChanged() {
	if s.HoldsChanged != nil {
		s.HoldsChanged()
	}
}
