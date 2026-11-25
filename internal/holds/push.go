package holds

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

const (
	pushEvery = 5 * time.Second
	// fullEvery: a tick whose torrents look as at the last build does no
	// work; this often the set is built and compared anyway (the file
	// browser's roots read again, a change the shapes do not show found).
	fullEvery = time.Minute
)

// Pusher keeps the core's copy of the holds (PUT /files/holds) current:
// PushNow at start (ready waits on it), then at most one push every
// Debounce, when Changed was called or the built set differs from the last
// one pushed (the engine has no change hook: a new download, a finished
// one, a parked one, an organize starting all show up within Debounce, as
// a change of the torrents' shapes). A tick that sees no change builds
// nothing; the set is built and compared in full every fullEvery.
type Pusher struct {
	Src  Source
	Busy func(ih string) bool // being put into the library (acquire.Organizing)
	Put  func(ctx context.Context, h []addonkit.Hold) error
	// FileRoots are /info's fileRoots: holds outside them are left out.
	// With Roots set they are read again before every push (a root
	// missing at start, left out of /info, counts once it is there).
	FileRoots []string
	// MissingRoots are /info's missingRoots: the file browser's roots not
	// on disk (as configured); holds under them are kept in the set.
	MissingRoots []string
	// Roots (optional) reads /info's fileRoots and missingRoots; on an
	// error the push is skipped (retried) and the last known roots stay.
	Roots func(ctx context.Context) (fileRoots, missingRoots []string, err error)
	// Saved (optional) reads the set the core keeps for the addon (GET
	// /files/holds), once, before the first push: a non-empty one counts
	// as the last set accepted, so a first push that would clear it while
	// downloads exist is not sent either. An error skips the push.
	Saved func(ctx context.Context) ([]addonkit.Hold, error)
	// Outside is told the number of holds left out (on the first push and
	// whenever it changes): logged, and shown on the settings page.
	Outside  func(n int)
	Debounce time.Duration // 5 s
	Log      *slog.Logger

	dirty   atomic.Bool
	outside atomic.Int64

	mu      sync.Mutex // one push at a time
	last    []addonkit.Hold
	pushed  bool            // a set was accepted (last)
	seeded  bool            // Saved was read
	coreSet bool            // the core holds a non-empty set for the addon
	refused []addonkit.Hold // the set the core answered 400 to
	told    bool
	toldN   int

	sig   []shape   // the torrents as of the last build
	sigAt time.Time // that build (zero: none yet)

	tick func(d time.Duration) (<-chan time.Time, func()) // tests
	now  func() time.Time                                 // tests; time.Now
}

// PushNow builds and pushes the whole set now.
func (p *Pusher) PushNow(ctx context.Context) error { return p.push(ctx, true) }

// Changed asks for a push at the next tick (the hooks call it after a
// move; nothing is lost when no one does: the tick compares).
func (p *Pusher) Changed() { p.dirty.Store(true) }

// LastOutside is the number of holds the last build left out.
func (p *Pusher) LastOutside() int { return int(p.outside.Load()) }

// Run pushes at every tick what changed, until ctx ends. A failed push is
// retried at the next tick, except a set the core refused (400): that one
// waits for a change (it would be refused again).
func (p *Pusher) Run(ctx context.Context) {
	every := p.Debounce
	if every <= 0 {
		every = pushEvery
	}
	tick := p.tick
	if tick == nil {
		tick = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		}
	}
	c, stop := tick(every)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c:
		}
		force := p.dirty.Swap(false)
		if err := p.push(ctx, force); err != nil && !refused(err) && ctx.Err() == nil {
			p.dirty.Store(true)
		}
	}
}

func (p *Pusher) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// push sends the built set: always with force, else only when it differs
// from the last set pushed (and from a set refused as it is).
func (p *Pusher) push(ctx context.Context, force bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	if f, ok := p.Src.(interface{ Forget() }); ok && force {
		f.Forget() // a forced build lists the files afresh
	}
	list := p.Src.Torrents()
	sig := p.shapes(list)
	if !force && !p.sigAt.IsZero() && now().Sub(p.sigAt) < fullEvery && slices.Equal(sig, p.sig) {
		return nil
	}
	if p.Roots != nil {
		roots, missing, err := p.Roots(ctx)
		if err != nil {
			if ctx.Err() == nil {
				p.log().Error("torrents: read the file browser's roots (the push is retried)", "err", err)
			}
			return fmt.Errorf("holds: file roots: %w", err)
		}
		p.FileRoots, p.MissingRoots = roots, missing
	}
	if !p.seeded && p.Saved != nil {
		saved, err := p.Saved(ctx)
		if err != nil {
			if ctx.Err() == nil {
				p.log().Error("torrents: read the holds the core keeps (the push is retried)", "err", err)
			}
			return fmt.Errorf("holds: saved set: %w", err)
		}
		p.seeded, p.coreSet = true, len(saved) > 0
	}
	h, n := p.build(list)
	p.setOutside(n)
	// a set that shrinks to nothing while downloads exist (a root gone
	// for a moment, a media mount not up yet) would lift the core's
	// protection of all of them: it waits for the next cycle; an engine
	// with no download may clear it
	if len(h) == 0 && p.coreSet {
		if d := downloads(list); d > 0 {
			p.log().Warn(i18n.TL(i18n.RU, "Список защищённых файлов стал пустым при %d загрузках — отправка пропущена", d))
			p.dirty.Store(true) // tried again at the next tick
			if !p.pushed {
				// the first push: the core's saved set protects the data
				// meanwhile, so the addon may turn ready
				p.log().Warn("torrents: ready with the holds the core kept from the last run")
				return nil
			}
			return errShrunk
		}
	}
	p.sig, p.sigAt = sig, now()
	unchanged := p.pushed && reflect.DeepEqual(h, p.last)
	refusedAgain := p.refused != nil && reflect.DeepEqual(h, p.refused)
	if !force && (unchanged || refusedAgain) {
		return nil
	}
	if err := p.Put(ctx, h); err != nil {
		if refused(err) {
			// the core names the path it refused; nothing is dropped:
			// the whole set goes again on the next change
			p.refused = h
			p.log().Error("torrents: the core refused the holds (sent again on the next change)", "holds", len(h), "err", err)
		} else {
			p.sigAt = time.Time{} // retried at the next tick
			if ctx.Err() == nil {
				p.log().Error("torrents: push the holds (retried)", "holds", len(h), "err", err)
			}
		}
		return err
	}
	p.last, p.pushed, p.refused, p.coreSet = h, true, nil, len(h) > 0
	return nil
}

// errShrunk: the set would be empty while downloads exist (not sent).
var errShrunk = errors.New("holds: the set would be empty while downloads exist; not sent")

// downloads counts the torrents with data of their own (stream torrents
// are the stream cache's).
func downloads(list []Info) int {
	n := 0
	for _, in := range list {
		if in.Status.Name != "" && in.Status.SaveDir != "" && in.Status.Mode != torrent.ModeStream {
			n++
		}
	}
	return n
}

// shapes is the torrents' shapes, with whether each is being organized.
func (p *Pusher) shapes(list []Info) []shape {
	out := make([]shape, len(list))
	for i, in := range list {
		out[i] = shapeOf(in.Status)
		out[i].Busy = p.Busy != nil && p.Busy(lower(in.Status.InfoHash))
	}
	return out
}

func (p *Pusher) build(list []Info) ([]addonkit.Hold, int) {
	items := make(map[string]int64, len(list))
	for _, in := range list {
		if in.Status.ItemID != nil {
			items[lower(in.Status.InfoHash)] = *in.Status.ItemID
		}
	}
	return buildIn(list, p.Busy, func(ih string) int64 { return items[ih] }, p.FileRoots, p.MissingRoots)
}

func (p *Pusher) setOutside(n int) {
	p.outside.Store(int64(n))
	if p.told && p.toldN == n {
		return
	}
	p.told, p.toldN = true, n
	if n > 0 {
		p.log().Warn("torrents: downloads outside the file browser's folders: deleting from the library will not remove their data", "count", n)
	}
	if p.Outside != nil {
		p.Outside(n)
	}
}

// refused: the core answered 400 (the set itself is wrong).
func refused(err error) bool {
	var ae *addonkit.APIError
	return errors.As(err, &ae) && ae.Status == http.StatusBadRequest
}
