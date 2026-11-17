package torrent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// Per-file selection: AddOptions.Files names the files to download (indexes
// in Info.UpvertedFiles order, which is also t.Files() order). nil is the
// whole torrent and keeps every behaviour of a plain add. A selection is
// fixed at add time (record.Files, persisted as files_json).
//
// Only hash-verified pieces count: a selection is complete when every piece
// of every selected file has PieceState.Complete, which anacrolix sets only
// after the piece passed its hash check. Pieces shared with unselected
// neighbours (boundary pieces) are downloaded and verified with the selected
// file, so those neighbours exist on disk as sparse partial files; they must
// stay there while the torrent lives. anacrolix itself never reports a
// partial torrent complete, so trackers never get event=completed for it.

var errSelectionNeedsInfo = errors.New("torrent: a file selection needs the metafile")

// normaliseSelection validates files against infoBytes and returns it
// sorted and de-duplicated; a selection naming every file becomes nil.
func normaliseSelection(files []int, infoBytes []byte) ([]int, error) {
	if files == nil {
		return nil, nil
	}
	if infoBytes == nil {
		return nil, errSelectionNeedsInfo
	}
	if len(files) == 0 {
		return nil, errors.New("torrent: empty file selection")
	}
	inf, err := (&metainfo.MetaInfo{InfoBytes: infoBytes}).UnmarshalInfo()
	if err != nil {
		return nil, fmt.Errorf("torrent file: %w", err)
	}
	n := len(inf.UpvertedFiles())
	out := slices.Clone(files)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, i := range out {
		if i < 0 || i >= n {
			return nil, fmt.Errorf("torrent: no file %d (the torrent has %d)", i, n)
		}
	}
	if len(out) == n {
		return nil, nil
	}
	return out, nil
}

// AddFiles grows a torrent's selection by files (engine indexes): the
// union is downloaded, files done stay as they are (their pieces are
// verified already), and OnComplete fires again once the larger selection
// is complete. A whole torrent (no selection) is left alone; a union that
// names every file makes it whole. Files already selected change nothing.
func (e *Engine) AddFiles(ctx context.Context, infohash string, files []int) error {
	if len(files) == 0 {
		return errors.New("torrent: empty file selection")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	en, err := e.entry(infohash)
	if err != nil {
		return err
	}
	if en.removed {
		return ErrNotFound
	}
	if en.rec.Files == nil {
		return nil // every file is downloaded already
	}
	if en.moving && en.parked {
		// unparking now would leave a handle at the old place (a plain
		// move attaches nothing here: its re-attach takes the selection)
		return ErrBusy
	}
	var info []byte
	t := en.t
	if t != nil && t.Info() != nil {
		info = t.Metainfo().InfoBytes
	} else if mi, err := metainfo.LoadFromFile(e.metafilePath(en.ih)); err == nil {
		info = mi.InfoBytes
	}
	sel, err := normaliseSelection(append(slices.Clone(en.rec.Files), files...), info)
	if err != nil {
		return err
	}
	if sel != nil && slices.Equal(sel, en.rec.Files) {
		return nil
	}
	if err := e.st.updateFiles(ctx, en.ih, sel); err != nil {
		return err
	}
	// the watcher of a complete selection is gone: a new one waits for the
	// larger selection (a running one reads the selection at each tick)
	watcherGone := en.rec.CompletedAt != nil
	en.rec.Files, en.rec.CompletedAt = sel, nil
	if en.parked {
		e.unpark(en) // its watcher waits for the larger selection
	}
	// while moving, t is the handle the move dropped: the move's re-attach
	// applies the larger selection and starts the watcher at the new place
	// (one started now could complete early, with the save dir being left)
	if t != nil && en.applied == t && !en.moving {
		e.applyWanted(en, t)
		if watcherGone {
			e.wg.Add(1)
			go func() {
				defer e.wg.Done()
				e.watchSelection(en, t)
			}()
		}
	}
	en.rec.Status = e.statusOf(en).Status
	e.save(en.rec)
	return nil
}

// RemoveFiles shrinks a torrent's selection by files (engine indexes; a
// whole torrent becomes a selection of the others): they are no longer
// downloaded, and the data of those that are only the torrent's is
// deleted (truncated to 0 bytes; the empty file stays). A dropped file
// keeps its data when any of these holds: it shares a piece with a file
// still selected (that piece is verified across both, see the package
// doc), another torrent uses the path, the torrent is not a download, the
// file has another hard link (e.g. placed in the library), or mayEmpty
// (the caller: e.g. no library row names the path; nil = none may be
// emptied) says no. The pieces of emptied data are forgotten, so
// selecting the files again downloads them afresh. Files not selected
// change nothing; leaving nothing selected is refused (remove the torrent
// instead).
func (e *Engine) RemoveFiles(ctx context.Context, infohash string, files []int, mayEmpty func(path string) bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	en, err := e.entry(infohash)
	if err != nil {
		return err
	}
	if en.removed {
		return ErrNotFound
	}
	if en.moving {
		return ErrBusy
	}
	t := en.t
	var infoBytes []byte
	if t != nil && t.Info() != nil {
		infoBytes = t.Metainfo().InfoBytes
	} else if mi, err := metainfo.LoadFromFile(e.metafilePath(en.ih)); err == nil {
		infoBytes = mi.InfoBytes
	}
	if infoBytes == nil {
		return errSelectionNeedsInfo
	}
	inf, err := (&metainfo.MetaInfo{InfoBytes: infoBytes}).UnmarshalInfo()
	if err != nil {
		return fmt.Errorf("torrent file: %w", err)
	}
	all := inf.UpvertedFiles()
	drop := map[int]bool{}
	for _, i := range files {
		if i < 0 || i >= len(all) {
			return fmt.Errorf("torrent: no file %d (the torrent has %d)", i, len(all))
		}
		if isSelected(en.rec.Files, i) {
			drop[i] = true
		}
	}
	if len(drop) == 0 {
		return nil
	}
	var keep []int
	for i := range all {
		if isSelected(en.rec.Files, i) && !drop[i] {
			keep = append(keep, i)
		}
	}
	if len(keep) == 0 {
		return errors.New("torrent: no file would be left selected")
	}
	if err := e.st.updateFiles(ctx, en.ih, keep); err != nil {
		return err
	}
	en.rec.Files = keep
	if t != nil && en.applied == t {
		e.applyWanted(en, t)
	}
	en.rec.Status = e.statusOf(en).Status
	e.save(en.rec)

	// The dropped files' own data: pieces no kept file touches.
	type span struct{ first, end int } // pieces [first, end)
	spans := make([]span, len(all))
	var off int64
	for i, f := range all {
		if f.Length > 0 {
			spans[i] = span{int(off / inf.PieceLength), int((off+f.Length-1)/inf.PieceLength) + 1}
		}
		off += f.Length
	}
	kept := map[int]bool{}
	for _, i := range keep {
		for p := spans[i].first; p < spans[i].end; p++ {
			kept[p] = true
		}
	}
	name := inf.BestName()
	shared := e.sharesPath(en.ih, en.rec.SaveDir, name) || en.rec.Mode != ModeDownload || mayEmpty == nil
	var forget []int
	for i := range drop {
		own := true
		for p := spans[i].first; p < spans[i].end; p++ {
			if kept[p] {
				own = false
			}
		}
		if !own || shared {
			continue
		}
		p := filepath.Join(en.rec.SaveDir, name)
		if inf.IsDir() {
			p = filepath.Join(append([]string{p}, all[i].BestPath()...)...)
		}
		if !soleLink(p) || !mayEmpty(p) {
			continue // in the library (a hard link) or kept by the caller: data stays
		}
		// truncated, not unlinked: anacrolix's file storage may hold the
		// file open, and writes of a later selection would go to an
		// unlinked inode
		if err := os.Truncate(p, 0); err != nil {
			if !os.IsNotExist(err) {
				e.log.Warn("delete a file left out of the selection", "infohash", en.ih, "path", p, "err", err)
			}
			continue
		}
		for p := spans[i].first; p < spans[i].end; p++ {
			forget = append(forget, p)
		}
	}
	for _, p := range forget {
		if err := e.pc.forgetPieces(ctx, en.ih, p, p+1); err != nil {
			e.log.Error("forget piece state", "infohash", en.ih, "err", err)
		}
	}
	if len(forget) > 0 {
		if err := e.pc.markAllIncomplete(ctx, en.ih, inf.NumPieces()); err != nil {
			e.log.Error("store piece state", "infohash", en.ih, "err", err)
		}
		if t != nil && t.Info() != nil {
			for _, p := range forget {
				t.Piece(p).UpdateCompletion()
			}
		}
	}
	return nil
}

// soleLink reports whether p is a regular file with no other hard link
// (false when that cannot be told).
func soleLink(p string) bool {
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && sys.Nlink == 1
}

// watchSelection reports the completion of a selected torrent, waiting
// again when the selection grew meanwhile.
func (e *Engine) watchSelection(en *entry, t *atorrent.Torrent) {
	for e.waitSelection(en, t) {
		if e.completed(en, t) {
			return
		}
	}
}

// isSelected reports whether file i is selected (nil = every file).
func isSelected(files []int, i int) bool {
	if files == nil {
		return true
	}
	_, ok := slices.BinarySearch(files, i)
	return ok
}

// applySelection starts the download of the selection: nil downloads the
// whole torrent; otherwise selected files get PiecePriorityNormal and the
// rest PiecePriorityNone. DownloadAll is never called for a selection: it
// raises piece-level priorities that file priorities cannot lower again.
// The info must be there.
func applySelection(t *atorrent.Torrent, files []int) {
	if files == nil {
		t.DownloadAll()
		return
	}
	for i, f := range t.Files() {
		if isSelected(files, i) {
			f.SetPriority(atorrent.PiecePriorityNormal)
		} else {
			f.SetPriority(atorrent.PiecePriorityNone)
		}
	}
}

// replan makes anacrolix re-plan the requests of the wanted pieces: they
// leave the pending set and enter it again, which re-inserts them into the
// request order and wakes the peers that have them. Used by checkIdle for
// a torrent that stopped requesting (never a held one). The info must be
// there.
func replan(t *atorrent.Torrent, files []int) {
	clearWanted(t)
	applySelection(t, files)
}

// selectionComplete reports whether every piece of every selected non-empty
// file is hash-verified. The info must be there.
func selectionComplete(t *atorrent.Torrent, files []int) bool {
	for i, f := range t.Files() {
		if !isSelected(files, i) || f.Length() <= 0 {
			continue
		}
		for p := f.BeginPieceIndex(); p < f.EndPieceIndex(); p++ {
			if !t.PieceState(p).Complete {
				return false
			}
		}
	}
	return true
}

// selectionProgress sums the selected files (display: BytesCompleted also
// counts dirty chunks). The info must be there.
func selectionProgress(t *atorrent.Torrent, files []int) (size, done int64, selected int) {
	for i, f := range t.Files() {
		if !isSelected(files, i) {
			continue
		}
		size += f.Length()
		done += f.BytesCompleted()
		selected++
	}
	return
}

// waitSelection polls the entry's selection (as it is at each tick: it
// may grow, AddFiles) until it is complete (true) or the torrent or the
// engine closes (false). No engine lock is held while checking.
func (e *Engine) waitSelection(en *entry, t *atorrent.Torrent) bool {
	tk := time.NewTicker(e.cfg.tick)
	defer tk.Stop()
	for {
		e.mu.Lock()
		files := en.rec.Files
		e.mu.Unlock()
		if selectionComplete(t, files) {
			return true
		}
		select {
		case <-tk.C:
		case <-t.Closed():
			return false
		case <-e.closing:
			return false
		}
	}
}
