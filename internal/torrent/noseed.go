package torrent

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// «Не раздавать» (EngineConfig.NoSeeding, SetNoSeeding).
//
// Refusing to upload is not enough: anacrolix goes on announcing a torrent
// with DisallowDataUpload to its trackers and the DHT, so the server stays
// listed in the swarm. So a finished torrent is dropped from the client and
// "parked": no announce, no connection, no port use. It is still a finished
// torrent to everyone else: Status says seeding (with Parked), its files come
// from the metafile, its data is read straight from the disk. It comes back
// only for what needs the swarm: more files selected, a move out of the
// stream cache, or the setting turned off.
//
// A torrent an open reader is using is not dropped (that would break the
// stream); it uploads nothing and is parked when the reader closes.
//
// A torrent not finished yet must stay in the swarm to download, but it
// uploads nothing either: a peer asking for a piece we already have gets
// none, at any stage (attach, setHold, sweepNoSeeding).

// SetNoSeeding applies the setting at once to every torrent.
func (e *Engine) SetNoSeeding(on bool) {
	if e.cfg.Off {
		return
	}
	e.mu.Lock()
	e.noSeed = on
	e.mu.Unlock()
	e.sweepNoSeeding()
}

// NoSeeding reports whether «Не раздавать» is in force (nothing is
// uploaded to anyone).
func (e *Engine) NoSeeding() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.noSeed
}

// sweepNoSeeding brings every torrent in line with the setting (monitor, every
// tick, and SetNoSeeding).
func (e *Engine) sweepNoSeeding() {
	var drops []*atorrent.Torrent
	e.mu.Lock()
	select {
	case <-e.closing:
		e.mu.Unlock()
		return
	default:
	}
	for _, en := range e.bySeq() {
		switch {
		case !e.noSeed:
			if en.parked && !en.moving { // a moving one: next tick, after Finish
				e.unpark(en)
			} else if en.uploadOff && en.t != nil {
				en.uploadOff = false
				if en.hold != StatusPaused {
					en.t.AllowDataUpload()
				}
			}
		case e.mayPark(en):
			if en.readers.Load() > 0 {
				if !en.uploadOff {
					en.uploadOff = true
					en.t.DisallowDataUpload()
				}
				continue
			}
			drops = append(drops, en.t)
			e.park(en)
		default:
			// attached and not finished (downloading, moving, failed...):
			// it stays in the swarm to download, but gives nothing, as an
			// in-progress client would by default (anacrolix uploads what
			// it has to anyone who asks)
			if en.t != nil && !en.uploadOff {
				en.uploadOff = true
				en.t.DisallowDataUpload()
			}
		}
	}
	e.mu.Unlock()
	for _, t := range drops {
		t.Drop()
	}
}

// mayPark: en is attached and finished, with nothing about to change. e.mu
// must be held.
func (e *Engine) mayPark(en *entry) bool {
	t := en.t
	if t == nil || en.parked || en.moving || en.removed || en.relocating || en.dupOf != "" || en.rec.Error != "" ||
		en.rec.CompletedAt == nil || en.applied != t || e.needsMove(en) || t.Info() == nil {
		return false
	}
	if en.rec.Files != nil {
		return selectionComplete(t, en.rec.Files)
	}
	return t.Complete().Bool()
}

// park detaches en from the client; the caller drops the torrent off e.mu.
// e.mu must be held.
func (e *Engine) park(en *entry) {
	en.parkedInfo = en.t.Info()
	e.foldCounters(en)
	en.baseUp, en.baseDown = en.rec.Uploaded, en.rec.Downloaded
	en.parked, en.uploadOff = true, false
	en.t, en.applied = nil, nil
	en.upRate, en.downRate = 0, 0
	en.stalled = ""
	en.rec.Status = e.statusOf(en).Status
	e.save(en.rec)
	e.log.Info("finished torrent left the swarm: seeding is off", "infohash", en.ih)
}

// parkRestored parks a stored finished torrent instead of attaching it, so a
// restart never shows it to the swarm. e.mu must be held.
func (e *Engine) parkRestored(en *entry, spec *atorrent.TorrentSpec) bool {
	r := en.rec
	if !e.noSeed || r.CompletedAt == nil || r.Error != "" || spec.InfoBytes == nil || e.needsMove(en) ||
		(r.Status != StatusSeeding && r.Status != StatusPaused) {
		return false
	}
	info, err := (&metainfo.MetaInfo{InfoBytes: spec.InfoBytes}).UnmarshalInfo()
	if err != nil {
		return false
	}
	en.parkedInfo = &info
	en.parked = true
	en.baseUp, en.baseDown = r.Uploaded, r.Downloaded
	en.lastUp, en.lastDown = r.Uploaded, r.Downloaded
	return true
}

// unpark attaches en again; its watcher takes it from there (a selection grown
// meanwhile is downloaded, nothing else is). e.mu must be held.
func (e *Engine) unpark(en *entry) {
	en.parked, en.parkedInfo = false, nil
	spec, err := e.specFor(en.rec)
	if err == nil {
		// files deleted meanwhile: their pieces are downloaded again, not
		// trusted from the stored state
		e.reconcile(en, spec, false)
		err = e.attach(en, spec)
	}
	if err != nil {
		en.rec.Error = err.Error()
		e.log.Error("attach parked torrent", "infohash", en.ih, "err", err)
		e.save(en.rec)
	}
}

// parkedStatus fills what statusOf takes from the live torrent. e.mu held.
func (en *entry) parkedStatus(s *Status) {
	s.Parked = true
	all := en.parkedInfo.UpvertedFiles()
	s.TotalFiles = len(all)
	for i, f := range all {
		if isSelected(en.rec.Files, i) {
			s.Size += f.Length
			s.SelectedFiles++
		}
	}
	s.Done = s.Size
}

// parkedFiles is Files of a parked torrent (nil: it is not parked).
func (en *entry) parkedFiles() []FileInfo {
	en.e.mu.Lock()
	defer en.e.mu.Unlock()
	if !en.parked {
		return nil
	}
	all := en.parkedInfo.UpvertedFiles()
	out := make([]FileInfo, len(all))
	for i, f := range all {
		p := parkedPath(en.parkedInfo, f)
		sel := isSelected(en.rec.Files, i)
		out[i] = FileInfo{Index: i, Path: p, FullPath: filepath.Join(en.rec.SaveDir, filepath.FromSlash(p)), Size: f.Length, Selected: sel}
		if sel {
			out[i].Progress = 1
		}
	}
	return out
}

// parkedPath is the path anacrolix's File.Path gives the file.
func parkedPath(info *metainfo.Info, f metainfo.FileInfo) string {
	return strings.Join(append([]string{info.BestName()}, f.BestPath()...), "/")
}

// parkedRead opens a file of a parked torrent from the disk. handled=false: it
// was not parked or the file was never downloaded (the torrent is attached
// again, to fetch it): the caller goes on with a live reader.
func (en *entry) parkedRead(ctx context.Context, fileIndex int) (r io.ReadSeekCloser, handled bool, err error) {
	e := en.e
	e.mu.Lock()
	defer e.mu.Unlock()
	if !en.parked || en.removed {
		return nil, false, nil
	}
	if en.moving {
		return nil, true, ErrBusy
	}
	if err := en.readBlocked(); err != nil {
		return nil, true, err
	}
	all := en.parkedInfo.UpvertedFiles()
	if fileIndex < 0 || fileIndex >= len(all) {
		return nil, true, os.ErrNotExist
	}
	if !isSelected(en.rec.Files, fileIndex) {
		e.unpark(en)
		return nil, false, nil
	}
	p := filepath.Join(en.rec.SaveDir, filepath.FromSlash(parkedPath(en.parkedInfo, all[fileIndex])))
	f, err := os.Open(p)
	if err != nil {
		return nil, true, err
	}
	return &parkedReader{File: f, ctx: ctx}, true, nil
}

// parkedReader reads a finished file from the disk, within its context like a
// live reader.
type parkedReader struct {
	*os.File
	ctx context.Context
}

func (r *parkedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.File.Read(p)
}
