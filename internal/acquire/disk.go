package acquire

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/disk"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// Disk levels and usage live in internal/disk.
const (
	LevelOK   = disk.LevelOK
	LevelWarn = disk.LevelWarn
	LevelFull = disk.LevelFull
)

// DiskUsage is GET /api/disk and the disk.warning payload.
type DiskUsage = disk.Usage

// LevelOf classifies a usage.
func LevelOf(used, total int64) string { return disk.LevelOf(used, total) }

// CacheEngine is the part of the torrent engine the stream cache needs.
type CacheEngine interface {
	List() []torrent.Status
	// RemoveIfMode removes only if the torrent is still in mode (atomic in
	// the engine): a torrent promoted meanwhile is kept.
	RemoveIfMode(ctx context.Context, infohash, mode string, deleteData bool) error
}

// Disk reads the media disk's fill level and keeps the «Смотреть сейчас»
// cache within its limit. The zero value with Root set works; Engine is
// needed only by PruneStreamCache.
type Disk struct {
	Root   string
	Engine CacheEngine
	Log    *slog.Logger

	// statfs returns total and available bytes; tests replace it.
	statfs func(path string) (total, avail int64, err error)
	// afterSnapshot runs between the cache snapshot and the removals (tests).
	afterSnapshot func()
	// now is time.Now (tests replace it).
	now func() time.Time
	// removed is told of each pruned entry (acquire.New: Service.removed,
	// so the watch inputs of it go too).
	removed func(ih string)
	// dropSuperseded removes a superseded stream once nobody has read it
	// for supersededGrace (acquire.New: Service.dropSuperseded); at is the
	// last use the timer was set for (dropDue re-checks it).
	dropSuperseded func(ih string, at time.Time)

	mu       sync.Mutex
	lastUsed map[string]time.Time
	active   map[string]int
	// superseded: stream torrents another source of the same title replaced
	// while a reader had them open; they go when the last reader is done.
	superseded map[string]bool
	// refs: who watches what through each stream torrent (viewer -> the
	// title or episode asked for), from Watching; in memory only: after a
	// restart a stream is replaced by nobody and goes by the cache limit.
	refs map[string]map[string]StreamItem
	// timers: the pending delayed drop of each superseded stream.
	timers map[string]*time.Timer
	// removing: stream torrents being removed right now (without d.mu
	// held); Use waits for the channel to close.
	removing map[string]chan struct{}
	closed   bool
}

// StreamItem is what a viewer watches through a stream torrent: a movie,
// or one episode of a series (a pack serves many episodes).
type StreamItem struct {
	Kind            string
	TMDBID          int
	Season, Episode int
}

// supersededGrace is how long a superseded stream outlives its last
// reader: the same viewer may open it again (a player's next range
// request, a step back to the old source).
var supersededGrace = 60 * time.Second

// Watching records that viewer watches item through stream torrent ih and
// returns the other stream torrents that viewer watched the same item
// through and nobody else references now: those are replaced. Another
// viewer's stream of the same item is never returned.
func (d *Disk) Watching(ih, viewer string, item StreamItem) (replaced []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.refs == nil {
		d.refs = map[string]map[string]StreamItem{}
	}
	for other, by := range d.refs {
		if other == ih {
			continue
		}
		if it, ok := by[viewer]; ok && it == item {
			delete(by, viewer)
			if len(by) == 0 {
				delete(d.refs, other)
				replaced = append(replaced, other)
			}
		}
	}
	if d.refs[ih] == nil {
		d.refs[ih] = map[string]StreamItem{}
	}
	d.refs[ih][viewer] = item
	sort.Strings(replaced)
	return replaced
}

// Supersede marks stream torrent ih as replaced by another source: it is
// removed supersededGrace after its last reader is done (now, if none),
// unless a viewer watches it again meanwhile.
func (d *Disk) Supersede(ih string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.superseded == nil {
		d.superseded = map[string]bool{}
	}
	d.superseded[ih] = true
	if d.active[ih] <= 0 {
		d.scheduleDropLocked(ih)
	}
}

// Unsupersede takes the mark off ih (it is watched again, or saved).
func (d *Disk) Unsupersede(ih string) {
	d.mu.Lock()
	delete(d.superseded, ih)
	d.stopTimerLocked(ih)
	d.mu.Unlock()
}

// Close stops the pending delayed drops; none is scheduled after it.
func (d *Disk) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	for ih := range d.timers {
		d.stopTimerLocked(ih)
	}
}

func (d *Disk) stopTimerLocked(ih string) {
	if t := d.timers[ih]; t != nil {
		t.Stop()
		delete(d.timers, ih)
	}
}

// scheduleDropLocked calls dropSuperseded after supersededGrace unless ih
// was read again meanwhile (a later Touch) or is no longer superseded; one
// timer per torrent, a newer one replaces it. d.mu is held.
func (d *Disk) scheduleDropLocked(ih string) {
	if d.dropSuperseded == nil || d.closed {
		return
	}
	d.stopTimerLocked(ih)
	at := d.lastUsed[ih]
	var tm *time.Timer
	tm = time.AfterFunc(supersededGrace, func() {
		d.mu.Lock()
		if d.timers[ih] != tm {
			d.mu.Unlock()
			return // stopped or replaced
		}
		delete(d.timers, ih)
		due := d.dueLocked(ih, at)
		d.mu.Unlock()
		if due {
			d.dropSuperseded(ih, at)
		}
	})
	if d.timers == nil {
		d.timers = map[string]*time.Timer{}
	}
	d.timers[ih] = tm
}

// dueLocked: superseded stream ih may go now (nobody reads or watches it,
// nobody touched it since at). d.mu is held.
func (d *Disk) dueLocked(ih string, at time.Time) bool {
	return !d.closed && d.superseded[ih] && d.active[ih] <= 0 && len(d.refs[ih]) == 0 && d.lastUsed[ih].Equal(at)
}

// Usage returns used and total bytes of the file system holding Root.
// Space reserved for root counts as used: Vault cannot write there.
func (d *Disk) Usage() (used, total int64, err error) {
	if d.statfs == nil {
		return disk.Stat(d.Root)
	}
	m := disk.Monitor{Root: d.Root, Statfs: d.statfs}
	u, err := m.Usage()
	return u.Used, u.Total, err
}

// Level is ok/warn/full; an unreadable disk counts as ok (never block on a
// statfs hiccup — writes fail loudly anyway).
func (d *Disk) Level() string {
	used, total, err := d.Usage()
	if err != nil {
		return LevelOK
	}
	return LevelOf(used, total)
}

// Touch marks a stream torrent as just watched (LRU order of the cache).
func (d *Disk) Touch(ih string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.touchLocked(ih)
}

// touchLocked: a use cancels a pending delayed drop (a release of the last
// reader schedules it again). d.mu is held.
func (d *Disk) touchLocked(ih string) {
	if d.lastUsed == nil {
		d.lastUsed = map[string]time.Time{}
	}
	d.lastUsed[ih] = time.Now()
	d.stopTimerLocked(ih)
}

// Use marks a torrent as being read right now; it is never pruned until
// the returned function is called. A removal of ih in progress is waited
// for (the reader then finds no torrent).
func (d *Disk) Use(ih string) (release func()) {
	d.mu.Lock()
	for {
		done := d.removing[ih]
		if done == nil {
			break
		}
		d.mu.Unlock()
		<-done
		d.mu.Lock()
	}
	if d.active == nil {
		d.active = map[string]int{}
	}
	d.active[ih]++
	d.touchLocked(ih)
	d.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.touchLocked(ih)
			if d.active[ih]--; d.active[ih] <= 0 {
				delete(d.active, ih)
				if d.superseded[ih] {
					d.scheduleDropLocked(ih)
				}
			}
		})
	}
}

// keepRecent: a stream torrent read or opened this recently counts as being
// watched (a paused player, Safari between range requests, ffmpeg ahead of
// the viewer hold no reader open) and is never pruned.
const keepRecent = 30 * time.Minute

// errStreamInUse: DropStream kept a stream torrent someone reads (or, for a
// delayed drop, watches or touched again) right now.
var errStreamInUse = errors.New("stream torrent is being read")

// DropStream removes stream torrent ih with its data, as PruneStreamCache
// does, unless a reader has it open (another viewer): errStreamInUse then.
// A torrent no longer in stream mode (promoted to a download) is kept:
// torrent.ErrModeChanged.
func (d *Disk) DropStream(ctx context.Context, ih string) error {
	return d.drop(ctx, ih, nil)
}

// dropDue is DropStream of a superseded stream whose delayed drop was set
// for last use at: it re-checks, in the same critical section as the
// start of the removal, that it is still due (dueLocked).
func (d *Disk) dropDue(ctx context.Context, ih string, at time.Time) error {
	return d.drop(ctx, ih, func() bool { return d.dueLocked(ih, at) })
}

func (d *Disk) drop(ctx context.Context, ih string, due func() bool) error {
	if d.Engine == nil {
		return errStreamInUse
	}
	d.mu.Lock()
	if d.active[ih] > 0 || d.removing[ih] != nil || (due != nil && !due()) {
		d.mu.Unlock()
		return errStreamInUse
	}
	finish := d.beginRemoveLocked(ih)
	d.mu.Unlock()
	err := d.Engine.RemoveIfMode(ctx, ih, torrent.ModeStream, true)
	finish(err == nil || errors.Is(err, torrent.ErrNotFound) || errors.Is(err, torrent.ErrModeChanged))
	return err
}

// beginRemoveLocked marks ih as being removed (Use waits) so the removal
// itself runs without d.mu (a multi-GB directory on a slow disk must not
// stall every other viewer's reads). finish(gone) clears the mark and, when
// the torrent is gone or saved, forgets it. d.mu is held.
func (d *Disk) beginRemoveLocked(ih string) (finish func(gone bool)) {
	if d.removing == nil {
		d.removing = map[string]chan struct{}{}
	}
	done := make(chan struct{})
	d.removing[ih] = done
	return func(gone bool) {
		d.mu.Lock()
		defer d.mu.Unlock()
		delete(d.removing, ih)
		close(done)
		if gone {
			delete(d.lastUsed, ih)
			delete(d.superseded, ih) // gone, or saved: no longer to be dropped
			delete(d.refs, ih)
			d.stopTimerLocked(ih)
		}
	}
}

// storageOffline is why the engine holds its torrents for a missing media
// disk ("" = it is there, or the engine cannot tell).
func (d *Disk) storageOffline() string {
	if o, ok := d.Engine.(offliner); ok {
		return o.Offline()
	}
	return ""
}

// neverGotData: a stream torrent that knows its files and is fine but has
// no data at all (a source opened and left at 0 %). A torrent in error, a
// detached one (Size 0: no metadata, or the media disk was missing at
// start) or one whose data is missing says nothing about its data.
func neverGotData(st torrent.Status) bool {
	return st.Done == 0 && st.Size > 0 && st.Status != torrent.StatusError && st.Error == "" && !st.DataMissing
}

// PruneStreamCache removes stream torrents (with their data), least
// recently watched first, until the cache holds at most maxBytes. Download
// and seed torrents — including promoted ones — are never touched, nor are
// torrents being watched or watched in the last 30 minutes, nor the most
// recently watched one (a single title may be bigger than the whole cache).
// A stream torrent idle that long that never received data (neverGotData)
// goes whatever the limit: it keeps nothing worth a place in «Загрузки».
// Nothing is pruned while the media disk is offline or unreadable: the
// torrents then report no data, and a removal would leave their files on
// the disk without a record.
func (d *Disk) PruneStreamCache(ctx context.Context, maxBytes int64) error {
	if d.Engine == nil {
		return nil
	}
	if msg := d.storageOffline(); msg != "" {
		if d.Log != nil {
			d.Log.Info("stream cache: media disk offline, nothing pruned", "reason", msg)
		}
		return nil
	}
	if _, _, err := d.Usage(); err != nil {
		if d.Log != nil {
			d.Log.Warn("stream cache: media disk unreadable, nothing pruned", "err", err)
		}
		return nil
	}
	now := time.Now()
	if d.now != nil {
		now = d.now()
	}
	type cand struct {
		ih    string
		size  int64
		at    time.Time
		empty bool
	}
	var cands []cand
	var total int64
	newest, newestAt := "", time.Time{}
	d.mu.Lock()
	for _, st := range d.Engine.List() {
		if st.Mode != torrent.ModeStream {
			continue
		}
		total += st.Done
		at := st.AddedAt
		if t, ok := d.lastUsed[st.InfoHash]; ok && t.After(at) {
			at = t
		}
		if newest == "" || at.After(newestAt) {
			newest, newestAt = st.InfoHash, at
		}
		if d.active[st.InfoHash] > 0 || now.Sub(at) < keepRecent {
			continue
		}
		cands = append(cands, cand{st.InfoHash, st.Done, at, neverGotData(st)})
	}
	d.mu.Unlock()
	sort.Slice(cands, func(i, j int) bool { return cands[i].at.Before(cands[j].at) })
	if d.afterSnapshot != nil {
		d.afterSnapshot()
	}
	var errs []error
	for _, c := range cands {
		if !c.empty && (total <= maxBytes || c.ih == newest) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Re-check right before removing: the snapshot may be stale (a
		// playback started, or the torrent was promoted). A new Use waits
		// for the removal and then finds no torrent.
		d.mu.Lock()
		if d.active[c.ih] > 0 || d.removing[c.ih] != nil || d.lastUsed[c.ih].After(c.at) {
			d.mu.Unlock()
			continue
		}
		finish := d.beginRemoveLocked(c.ih)
		d.mu.Unlock()
		err := d.Engine.RemoveIfMode(ctx, c.ih, torrent.ModeStream, true)
		finish(err == nil)
		if err != nil {
			if !errors.Is(err, torrent.ErrNotFound) && !errors.Is(err, torrent.ErrModeChanged) {
				errs = append(errs, err)
			}
			continue
		}
		total -= c.size
		if d.removed != nil {
			d.removed(c.ih)
		}
		if d.Log != nil {
			d.Log.Info("stream cache: removed", "infohash", c.ih, "bytes", c.size)
		}
	}
	if total > maxBytes && d.Log != nil {
		d.Log.Warn("stream cache is over its limit: the titles being watched are kept", "bytes", total, "limit", maxBytes)
	}
	return errors.Join(errs...)
}
