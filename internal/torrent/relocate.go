package torrent

import (
	"context"
	"path/filepath"
	"strings"

	atorrent "github.com/anacrolix/torrent"
)

// The file browser moves, renames and trashes folders that may hold torrent
// data. A torrent whose files vanish under it would download them again, so
// file operations first ask PlanRelocate what happens to each torrent: the
// ones that can follow the move are re-pointed by BeginRelocate (data kept,
// nothing re-downloaded or re-hashed), the others must be stopped (removed
// from the client, data kept) before the files are touched.

// within reports whether p is dir or lies inside it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// dataRoot is the torrent's own top-level file or folder. e.mu must be held.
func (en *entry) dataRoot() string { return filepath.Join(en.rec.SaveDir, en.rec.Name) }

// follows reports whether en's data can move from from to to with en
// re-pointed: its whole data lies at or inside from, keeps its name, and it
// is not a stream torrent (the stream cache owns those; moving their data
// out would leave it to be pruned). to == "" means the data goes away.
func (en *entry) follows(from, to string) (newSave string, ok bool) {
	if to == "" || en.rec.Mode == ModeStream || en.rec.Name == "" {
		return "", false
	}
	root := en.dataRoot()
	rel, err := filepath.Rel(filepath.Clean(from), root)
	if err != nil {
		return "", false
	}
	newRoot := filepath.Join(to, rel)
	if filepath.Base(newRoot) != en.rec.Name {
		return "", false // the torrent folder itself is renamed
	}
	return filepath.Dir(newRoot), true
}

// PlanRelocate lists the torrents touched when the file or folder from is
// moved to to (to == "": trashed or deleted). follow can be re-pointed with
// BeginRelocate; stop hold data at or inside from, or contain from, and must be
// stopped first.
func (e *Engine) PlanRelocate(from, to string) (follow, stop []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, en := range e.bySeq() {
		if en.removed || en.rec.Name == "" {
			continue
		}
		root := en.dataRoot()
		switch {
		case within(from, root):
			if _, ok := en.follows(from, to); ok {
				follow = append(follow, en.ih)
			} else {
				stop = append(stop, en.ih)
			}
		case within(root, from):
			stop = append(stop, en.ih) // a file or folder inside the torrent
		}
	}
	return follow, stop
}

// Relocation is a relocation begun by BeginRelocate and not yet finished: its
// torrents are dropped from the client and marked moving.
type Relocation struct {
	e        *Engine
	from, to string
	ms       []moved
	done     bool
}

type moved struct {
	en      *entry
	newSave string
	wasOn   bool
	t       *atorrent.Torrent // the handle Begin dropped (nil: none)
}

// Keys lists the infohashes the relocation holds.
func (r *Relocation) Keys() []string {
	keys := make([]string, 0, len(r.ms))
	for _, m := range r.ms {
		keys = append(keys, m.en.ih)
	}
	return keys
}

// BeginRelocate begins a relocation (Finish ends it): the torrents that follow the
// move are marked moving and dropped from the client. Finish must follow.
func (e *Engine) BeginRelocate(ctx context.Context, from, to string) (*Relocation, error) {
	e.mu.Lock()
	select {
	case <-e.closing:
		e.mu.Unlock()
		return nil, ErrClosed
	default:
	}
	var ms []moved
	for _, en := range e.bySeq() {
		if en.removed || !within(from, en.dataRoot()) {
			continue
		}
		ns, ok := en.follows(from, to)
		if !ok {
			continue
		}
		if en.moving {
			e.mu.Unlock()
			return nil, ErrBusy
		}
		ms = append(ms, moved{en: en, newSave: ns, wasOn: en.t != nil, t: en.t})
	}
	var drop []func()
	for _, m := range ms {
		m.en.moving = true
		m.en.uploadOff = false // the handle is dropped; attach decides again
		e.foldCounters(m.en)
		if t := m.en.t; t != nil {
			drop = append(drop, t.Drop)
		}
	}
	e.mu.Unlock()

	for _, d := range drop {
		d()
	}
	return &Relocation{e: e, from: from, to: to, ms: ms}, nil
}

// Finish is the second half: with renamed the torrents are re-pointed at the
// new place, otherwise restored; either way they are attached again and
// moving is cleared. A second call does nothing. It currently never fails.
//
// Nothing attaches a moving torrent, but should a handle other than the one
// Begin dropped be found, it is dropped too, before the re-attach below (the
// client would hand it back, attached at the old place): once en.t is
// overwritten no one could ever park it or stop its upload again. It is
// attached again at the new place (with no-seeding on the next sweep parks it
// again), never left neither parked nor attached.
func (r *Relocation) Finish(ctx context.Context, renamed bool) error {
	e, from, to := r.e, r.from, r.to
	e.mu.Lock()
	if r.done {
		e.mu.Unlock()
		return nil
	}
	r.done = true
	var stray []*atorrent.Torrent
	for i := range r.ms {
		if en := r.ms[i].en; en.t != nil && en.t != r.ms[i].t {
			e.log.Error("torrent attached while moving: dropped", "infohash", en.ih)
			stray = append(stray, en.t)
			en.t, en.applied = nil, nil
			en.parked, en.parkedInfo = false, nil // attached below: the sweep parks it again
			r.ms[i].wasOn = true
		}
	}
	if len(stray) > 0 { // still moving: nothing attaches them meanwhile
		e.mu.Unlock()
		for _, t := range stray {
			t.Drop()
		}
		e.mu.Lock()
	}
	defer e.mu.Unlock()
	for _, m := range r.ms {
		en := m.en
		en.moving = false
		if en.removed {
			continue
		}
		if renamed {
			if td := en.rec.TargetDir; td != "" && within(from, td) {
				if rel, rerr := filepath.Rel(filepath.Clean(from), td); rerr == nil {
					en.rec.TargetDir = filepath.Join(to, rel)
				}
			}
			en.rec.SaveDir = m.newSave
			e.save(en.rec)
		}
		en.t, en.uploadOff = nil, false
		if !m.wasOn {
			continue
		}
		select {
		case <-e.closing:
			continue // the next New attaches it from the database
		default:
		}
		spec, serr := e.specFor(en.rec)
		if serr == nil {
			serr = e.attach(en, spec)
		}
		if serr != nil {
			en.rec.Error = serr.Error()
			e.save(en.rec)
		}
	}
	return nil
}
