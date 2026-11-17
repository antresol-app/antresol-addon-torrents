package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// replaceMu: one Replace at a time (each reads a whole release from disk).
var replaceMu sync.Mutex

// replaceFault, when set (tests), is asked at each step of a Replace after
// the old torrent is removed ("removed", "added", "verified", "unlinked");
// an error fails the Replace there, as a real failure would.
var replaceFault func(step string) error

func replaceStep(step string) error {
	if replaceFault == nil {
		return nil
	}
	return replaceFault(step)
}

// Replace swaps torrent oldIH for the release in metafile (the same release
// re-uploaded: a new infohash, e.g. with new episodes) keeping the data on
// disk. The new torrent gets the old one's mode, SaveDir, item, adder,
// source URL, release, hold and AddedAt (and its completion when it was
// complete and nothing new is selected); its selection is the files the old
// one selected, by path, plus those keepSelected (nil = none) reports
// (paths as FileInfo.Path); none at all (every file renamed) is the whole
// torrent. A stream torrent's StreamDir/<old> becomes StreamDir/<new>.
//
// Every piece of the new torrent is hash-checked against the files there
// before Replace returns (the add's own check: no piece state is known for
// the new infohash), so whatever is already on disk counts — also when a new
// file shifts the offsets — and only pieces touching new or changed files
// are fetched. The new torrent is paused until then (its old hold is
// restored after); before it fetches anything, every file with pieces to fetch that has other hard
// links (the library's copy, organize links when it can) is given an inode
// of its own, so a changed file re-downloaded under the same path never
// writes into the library's copy. Then the files the new release no
// longer lists are deleted (stream) or left where they are (download:
// imported already). The old torrent's piece rows and metafile are gone.
//
// On any failure after the old torrent was removed, the new one is removed
// (its data kept), the stream dir renamed back and the old torrent re-added
// as it was (completion included, so its OnComplete hooks do not run
// again); the error is returned. The same infohash is a no-op returning the
// existing handle.
//
// ctx must be long-lived (a background one, not a request's): the check
// reads the whole release and can take minutes; a ctx ending first fails
// the Replace and rolls it back. Selection caveats: a new path is selected
// only if keepSelected says so, also for a whole old torrent; a release with
// every file renamed is downloaded whole and fresh, and its old files are
// left for the caller to clean. The carried completion means no OnComplete
// for files organized already; a changed old file re-downloaded under it
// gets none either. A stream torrent's AddedAt is the time of the Replace
// (the stream cache's last-use record stays under the old infohash).
func (e *Engine) Replace(ctx context.Context, oldIH string, metafile []byte, keepSelected func(path string) bool) (Handle, error) {
	oldIH = strings.ToLower(oldIH)
	newIH, newName, newFiles, err := ParseFiles(metafile)
	if err != nil {
		return nil, err
	}
	replaceMu.Lock()
	defer replaceMu.Unlock()

	e.mu.Lock()
	en, err := e.entry(oldIH)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	if newIH == oldIH {
		e.mu.Unlock()
		return en, nil
	}
	if err := e.replaceable(en, newIH); err != nil {
		e.mu.Unlock()
		return nil, err
	}
	was, hold := en.rec, en.hold
	e.mu.Unlock()

	oldMeta, err := os.ReadFile(e.metafilePath(oldIH))
	if err != nil {
		return nil, fmt.Errorf("torrent: no metafile of %s to fall back on: %w", oldIH, err)
	}
	_, oldName, oldFiles, err := ParseFiles(oldMeta)
	if err != nil {
		return nil, err
	}
	sel, grows := carrySelection(was.Files, oldFiles, newFiles, keepSelected)
	if was.Mode == ModeStream {
		// a stale cache of the new infohash (not in the engine: checked)
		if err := os.RemoveAll(e.streamDir(newIH)); err != nil {
			return nil, err
		}
	}

	// 1. the old torrent out, its files kept (the mode re-checked under e.mu)
	if err := e.removeIf(ctx, oldIH, was.Mode, false, func(en *entry) error {
		return e.replaceable(en, newIH)
	}, nil); err != nil {
		return nil, err
	}
	// from here on every failure puts the old torrent back
	renamed, added := false, false
	fail := func(cause error) (Handle, error) {
		return nil, e.rollback(ctx, cause, oldIH, newIH, oldMeta, was, hold, renamed, added)
	}
	if err := replaceStep("removed"); err != nil {
		return fail(err)
	}
	// 2. stream data follows the infohash; download data stays at SaveDir/<name>
	if was.Mode == ModeStream && exists(e.streamDir(oldIH)) {
		if err := os.Rename(e.streamDir(oldIH), e.streamDir(newIH)); err != nil {
			return fail(err)
		}
		renamed = true
	}
	// 3. the new torrent on that directory, every piece checked. A stream
	// torrent counts as just added: the stream cache prune spares it by
	// AddedAt or by its last use, and that is kept under the old infohash.
	carried := &was
	if was.Mode == ModeStream {
		fresh := was
		fresh.AddedAt = time.Now().UTC().Truncate(time.Second)
		carried = &fresh
	}
	h, err := e.AddTorrentFile(ctx, metafile, AddOptions{
		Mode: was.Mode, SaveDir: was.SaveDir, ItemID: was.ItemID, AddedBy: deref(was.AddedBy), AddedByUID: was.AddedByUID,
		Release: was.Release, SourceURL: was.SourceURL, Files: sel,
		// paused until its hard links are broken (step 3b), whatever the
		// old hold: nothing is fetched, so nothing is written meanwhile.
		// Not waiting_space: the disk guard resumes those when space frees.
		Paused: true,
		// complete with nothing new selected: its OnComplete (organize) ran
		// for these files already; AddFiles of new ones resets it
		history: carried, keepCompleted: was.CompletedAt != nil && !grows,
	})
	if err != nil {
		return fail(err)
	}
	added = true
	if err := replaceStep("added"); err != nil {
		return fail(err)
	}
	want := was.SaveDir
	if was.Mode == ModeStream {
		want = e.streamDir(newIH)
	}
	if got := h.Status().SaveDir; got != want {
		return fail(fmt.Errorf("torrent: the new release went to %s, not %s", got, want))
	}
	if _, _, err := e.WaitVerified(ctx, newIH); err != nil {
		return fail(err)
	}
	if err := replaceStep("verified"); err != nil {
		return fail(err)
	}
	// 3b. a file with pieces to fetch that is a hard link (organize links
	// into the library) gets an inode of its own first: the download must
	// never write into the library's copy
	for _, f := range h.Files() {
		if f.Progress >= 1 || f.FullPath == "" {
			continue
		}
		if err := breakLink(f.FullPath); err != nil {
			return fail(fmt.Errorf("torrent: separate %s from its hard links: %w", f.FullPath, err))
		}
	}
	if err := replaceStep("unlinked"); err != nil {
		return fail(err)
	}
	// the old hold back
	switch hold {
	case "":
		err = e.Resume(context.WithoutCancel(ctx), newIH)
	case StatusWaitingSpace:
		err = e.HoldForSpace(context.WithoutCancel(ctx), newIH)
	}
	if err != nil {
		return fail(err)
	}
	// 4. files the new release no longer lists: a stream cache drops them
	if was.Mode == ModeStream {
		root := e.streamDir(newIH)
		if err := removeOwnFiles(root, droppedPaths(root, oldFiles, newFiles), func(string) bool { return false }); err != nil {
			e.log.Warn("replace: delete files the new release dropped", "infohash", newIH, "err", err)
		}
	}
	// 5. the old piece rows went with its row (store.delete); a write queued
	// meanwhile must not bring them back
	if err := e.pc.forget(context.WithoutCancel(ctx), oldIH); err != nil {
		e.log.Warn("replace: forget old piece state", "infohash", oldIH, "err", err)
	}
	e.log.Info("torrent replaced, data kept", "old", oldIH, "new", newIH, "oldName", oldName, "name", newName)
	return h, nil
}

// breakLink gives p an inode of its own when it is a regular file with
// other hard links: a copy in the same directory, synced, renamed over p.
// The other links keep the old inode, untouched. No file, a sole link or
// not a regular file: nothing to do.
func breakLink(p string) error {
	st, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !st.Mode().IsRegular() {
		return nil
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || sys.Nlink <= 1 {
		return nil
	}
	src, err := os.Open(p)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(p), ".replace-*")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, src); err != nil {
		return err
	}
	if err := tmp.Chmod(st.Mode().Perm()); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return err
	}
	done = true
	if d, err := os.Open(filepath.Dir(p)); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}

// replaceable refuses a Replace of en that would lose track of its data:
// one being moved or relocated, with a pending move, without metadata, whose
// files another torrent shares, or when the new release is in the engine
// already. e.mu must be held.
func (e *Engine) replaceable(en *entry, newIH string) error {
	select {
	case <-e.closing:
		return ErrClosed
	default:
	}
	if e.offline != "" {
		return fmt.Errorf("%w: %s", ErrStorageOffline, e.offline)
	}
	if en.moving || en.relocating || en.rec.TargetDir != "" {
		return ErrBusy
	}
	if !en.nameKnown {
		return errors.New("torrent: no metadata yet, nothing to replace")
	}
	if _, ok := e.entries[newIH]; ok {
		return fmt.Errorf("torrent: %s is in the engine already", newIH)
	}
	if e.sharesPath(en.ih, en.rec.SaveDir, en.rec.Name) {
		return pathInUse(filepath.Join(en.rec.SaveDir, en.rec.Name), en.ih)
	}
	return nil
}

// rollback undoes a failed Replace and returns cause (joined with whatever
// failed while undoing). ctx may be done already: undoing runs on regardless.
func (e *Engine) rollback(ctx context.Context, cause error, oldIH, newIH string, oldMeta []byte, was record, hold string, renamed, added bool) error {
	ctx = context.WithoutCancel(ctx)
	errs := []error{cause}
	if added {
		if err := e.Remove(ctx, newIH, false); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, fmt.Errorf("remove the new torrent: %w", err))
		}
		e.pc.forget(ctx, newIH)
	}
	if renamed {
		if err := os.Rename(e.streamDir(newIH), e.streamDir(oldIH)); err != nil {
			errs = append(errs, fmt.Errorf("rename the stream data back: %w", err))
		}
	}
	if _, err := e.AddTorrentFile(ctx, oldMeta, AddOptions{
		Mode: was.Mode, SaveDir: was.SaveDir, ItemID: was.ItemID, AddedBy: deref(was.AddedBy), AddedByUID: was.AddedByUID,
		Release: was.Release, SourceURL: was.SourceURL, Files: was.Files,
		Paused: hold == StatusPaused, WaitingSpace: hold == StatusWaitingSpace,
		history: &was, keepCompleted: true,
	}); err != nil {
		e.log.Error("replace: the old torrent could not be re-added; its data is on disk", "infohash", oldIH, "dir", was.SaveDir, "err", err)
		errs = append(errs, fmt.Errorf("re-add the old torrent: %w", err))
	}
	return errors.Join(errs...)
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// carrySelection is the new torrent's selection: the new files whose path
// the old one selected (old nil = every file), plus those keep reports;
// nil when that is every file, or no file at all (a release with every
// file renamed downloads anew). grows: it selects a path the old one did
// not (or is such a fresh download).
func carrySelection(old []int, oldFiles, newFiles []FileEntry, keep func(string) bool) (sel []int, grows bool) {
	selected := map[string]bool{}
	for _, f := range oldFiles {
		if !f.Padding && (old == nil || isSelected(old, f.Index)) {
			selected[f.Path] = true
		}
	}
	all := true
	for _, f := range newFiles {
		if f.Padding {
			continue
		}
		switch {
		case selected[f.Path]:
			sel = append(sel, f.Index)
		case keep != nil && keep(f.Path):
			sel = append(sel, f.Index)
			grows = true
		default:
			all = false
		}
	}
	if len(sel) == 0 {
		return nil, true
	}
	if all {
		return nil, grows
	}
	return sel, grows
}

// droppedPaths are the files (on disk, under dir) of the old release that
// the new one no longer lists.
func droppedPaths(dir string, oldFiles, newFiles []FileEntry) []string {
	listed := map[string]bool{}
	for _, f := range newFiles {
		listed[f.Path] = true
	}
	var out []string
	for _, f := range oldFiles {
		if f.Padding || listed[f.Path] {
			continue
		}
		ok := true
		for _, c := range strings.Split(f.Path, "/") {
			if c == "" || c == "." || c == ".." || strings.Contains(c, `\`) {
				ok = false
			}
		}
		if ok {
			out = append(out, filepath.Join(dir, filepath.FromSlash(f.Path)))
		}
	}
	return out
}
