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
	// for supersededGrace (acquire.New: Service.dropSuperseded).
	dropSuperseded func(ih string)

	mu       sync.Mutex
	lastUsed map[string]time.Time
	active   map[string]int
	// superseded: stream torrents another source of the same title replaced
	// while a reader had them open; they go when the last reader is done.
	superseded map[string]bool
}

// supersededGrace is how long a superseded stream outlives its last
// reader: the same viewer may open it again (a player's next range
// request, a step back to the old source).
var supersededGrace = 60 * time.Second

// Supersede marks stream torrent ih as replaced by another source: it is
// removed supersededGrace after its last reader is done (now, if none).
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
	d.mu.Unlock()
}

// scheduleDropLocked calls dropSuperseded after supersededGrace unless ih
// was read again meanwhile (a later Touch) or is no longer superseded.
// d.mu is held.
func (d *Disk) scheduleDropLocked(ih string) {
	if d.dropSuperseded == nil {
		return
	}
	at := d.lastUsed[ih]
	time.AfterFunc(supersededGrace, func() {
		d.mu.Lock()
		due := d.superseded[ih] && d.active[ih] <= 0 && d.lastUsed[ih].Equal(at)
		d.mu.Unlock()
		if due {
			d.dropSuperseded(ih)
		}
	})
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
	if d.lastUsed == nil {
		d.lastUsed = map[string]time.Time{}
	}
	d.lastUsed[ih] = time.Now()
}

// Use marks a torrent as being read right now; it is never pruned until
// the returned function is called.
func (d *Disk) Use(ih string) (release func()) {
	d.mu.Lock()
	if d.active == nil {
		d.active = map[string]int{}
	}
	d.active[ih]++
	d.mu.Unlock()
	d.Touch(ih)
	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			if d.lastUsed == nil {
				d.lastUsed = map[string]time.Time{}
			}
			d.lastUsed[ih] = time.Now()
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

// errStreamInUse: DropStream kept a stream torrent someone reads right now.
var errStreamInUse = errors.New("stream torrent is being read")

// DropStream removes stream torrent ih with its data, as PruneStreamCache
// does, unless a reader has it open (another viewer): errStreamInUse then.
// A torrent no longer in stream mode (promoted to a download) is kept:
// torrent.ErrModeChanged.
func (d *Disk) DropStream(ctx context.Context, ih string) error {
	if d.Engine == nil {
		return errStreamInUse
	}
	// d.mu is held over the removal so a new Use waits for it and then
	// finds no torrent (as in PruneStreamCache).
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active[ih] > 0 {
		return errStreamInUse
	}
	err := d.Engine.RemoveIfMode(ctx, ih, torrent.ModeStream, true)
	if err == nil || errors.Is(err, torrent.ErrNotFound) || errors.Is(err, torrent.ErrModeChanged) {
		delete(d.lastUsed, ih)
		delete(d.superseded, ih) // gone, or saved: no longer to be dropped
	}
	return err
}

// PruneStreamCache removes stream torrents (with their data), least
// recently watched first, until the cache holds at most maxBytes. Download
// and seed torrents — including promoted ones — are never touched, nor are
// torrents being watched or watched in the last 30 minutes, nor the most
// recently watched one (a single title may be bigger than the whole cache).
// A stream torrent idle that long that holds no data at all (a source
// opened and left at 0 %) goes whatever the limit: it keeps nothing worth
// a place in «Загрузки».
func (d *Disk) PruneStreamCache(ctx context.Context, maxBytes int64) error {
	if d.Engine == nil {
		return nil
	}
	now := time.Now()
	if d.now != nil {
		now = d.now()
	}
	type cand struct {
		ih   string
		size int64
		at   time.Time
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
		cands = append(cands, cand{st.InfoHash, st.Done, at})
	}
	d.mu.Unlock()
	sort.Slice(cands, func(i, j int) bool { return cands[i].at.Before(cands[j].at) })
	if d.afterSnapshot != nil {
		d.afterSnapshot()
	}
	var errs []error
	for _, c := range cands {
		if c.size > 0 && (total <= maxBytes || c.ih == newest) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Re-check right before removing: the snapshot may be stale (a
		// playback started, or the torrent was promoted). d.mu is held over
		// the removal so a new Use waits for it and then finds no torrent.
		d.mu.Lock()
		if d.active[c.ih] > 0 || d.lastUsed[c.ih].After(c.at) {
			d.mu.Unlock()
			continue
		}
		err := d.Engine.RemoveIfMode(ctx, c.ih, torrent.ModeStream, true)
		if err == nil {
			delete(d.lastUsed, c.ih)
			delete(d.superseded, c.ih)
		}
		d.mu.Unlock()
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
