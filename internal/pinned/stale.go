package pinned

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// Old files of a renamed re-upload (the owner's decision). A download whose
// pinned release is replaced (Engine.Replace) by a re-upload that no longer
// lists some of its files — e.g. every file renamed — keeps those files on
// disk (Replace leaves a download's old files where they are). They are
// deleted once the new torrent is complete and imported: pinned_releases.
// stale_json records them durably (a restart keeps them), SweepStale
// deletes them when the time has come. Only regular files inside the old
// torrent's own data root (SaveDir/<its name>) are ever deleted, never a
// file the library names; when the new download is removed (or the pin
// is), nothing is.

// stale is pinned_releases.stale_json.
type stale struct {
	// Root: the old torrent's own data root (SaveDir/<name>).
	Root string `json:"root"`
	// Paths: the old files the new release does not list, on disk.
	Paths []string `json:"paths"`
	// Since: when the release was replaced; only an import after it
	// counts (RFC3339).
	Since string `json:"since"`
}

// staleOf is what the engine's download h (oldIH, before the Replace)
// leaves behind when g's release takes its place: its files that g does
// not list by path, inside its own data root. Nil for a stream (Replace
// deletes those itself), a single-file torrent (its root is the file) or
// nothing left behind.
func staleOf(h torrent.Handle, g got) *stale {
	if h == nil {
		return nil
	}
	st := h.Status()
	if st.Mode != torrent.ModeDownload || st.SaveDir == "" {
		return nil
	}
	saveDir := filepath.Clean(st.SaveDir)
	listed := map[string]bool{}
	for _, f := range g.files {
		listed[filepath.Join(saveDir, filepath.FromSlash(f.Path))] = true
	}
	var s stale
	for _, f := range h.Files() {
		name, rest, ok := strings.Cut(f.Path, "/")
		if !ok || name == "" || rest == "" || f.FullPath == "" {
			continue // a single-file torrent: no root folder of its own
		}
		root := filepath.Join(saveDir, name)
		if s.Root == "" {
			s.Root = root
		}
		p := filepath.Clean(f.FullPath)
		if root != s.Root || listed[p] || !inside(s.Root, p) {
			continue
		}
		s.Paths = append(s.Paths, p)
	}
	if len(s.Paths) == 0 {
		return nil
	}
	slices.Sort(s.Paths)
	return &s
}

// inside: p is strictly inside root (root a folder under its parent).
func inside(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// recordStale adds st (nil: none) to the pin's stale files, merged with
// any recorded: a file the latest release lists again is dropped (also
// when this replace left nothing behind).
func (s *Service) recordStale(ctx context.Context, pinID int64, st *stale, latest got) error {
	old, err := s.staleOfPin(ctx, pinID)
	if err != nil {
		return err
	}
	if st == nil && len(old) == 0 {
		return nil
	}
	all := old
	if st != nil {
		all = append(all, *st)
	}
	merged := []stale{}
	for _, x := range all {
		x.Paths = slices.DeleteFunc(x.Paths, func(p string) bool { return listedBy(latest, x.Root, p) })
		if len(x.Paths) > 0 {
			merged = append(merged, x)
		}
	}
	return s.setStale(ctx, pinID, merged)
}

// listedBy: g lists p (a file of a torrent in root's parent folder).
func listedBy(g got, root, p string) bool {
	dir := filepath.Dir(root)
	for _, f := range g.files {
		if filepath.Join(dir, filepath.FromSlash(f.Path)) == p {
			return true
		}
	}
	return false
}

func (s *Service) staleOfPin(ctx context.Context, pinID int64) ([]stale, error) {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT stale_json FROM pinned_releases WHERE id=?`, pinID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || raw == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []stale
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		s.log().Warn("pinned: bad stale_json: nothing is deleted", "pin", pinID, "err", err)
		return nil, nil
	}
	return out, nil
}

func (s *Service) setStale(ctx context.Context, pinID int64, list []stale) error {
	raw := ""
	if len(list) > 0 {
		b, err := json.Marshal(list)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE pinned_releases SET stale_json=? WHERE id=?`, raw, pinID)
	return err
}

// SweepStale deletes the stale files of the pins (only those whose
// release is ih, when ih is not "") whose new download is complete in the
// engine and imported since (Imported). A pin whose torrent the engine no
// longer has forgets its stale files (nothing deleted); one still
// downloading, failed, or not imported yet waits. A file the library names
// waits too (it is deleted once the library has let go of it); a file gone
// already, a folder or a link is forgotten. Each deleted path is logged;
// emptied folders inside the root go too.
func (s *Service) SweepStale(ctx context.Context, ih string) error {
	q := `SELECT id, infohash FROM pinned_releases WHERE stale_json != ''`
	var args []any
	if ih != "" {
		q += ` AND infohash=?`
		args = append(args, strings.ToLower(ih))
	}
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	type pinIH struct {
		id int64
		ih string
	}
	var pins []pinIH
	for rows.Next() {
		var p pinIH
		if err := rows.Scan(&p.id, &p.ih); err != nil {
			rows.Close()
			return err
		}
		pins = append(pins, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var errs []error
	for _, p := range pins {
		if err := s.sweepPin(ctx, p.id, p.ih); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) sweepPin(ctx context.Context, pinID int64, ih string) error {
	list, err := s.staleOfPin(ctx, pinID)
	if err != nil || len(list) == 0 {
		return err
	}
	if s.Engine == nil {
		return nil
	}
	h, ok := s.Engine.Get(ih)
	if !ok || h == nil {
		if s.torrentStored(ctx, ih) {
			return nil // stored, not loaded (yet): wait
		}
		s.log().Info("pinned: the new download is gone: its old files are kept", "pin", pinID, "infohash", ih)
		return s.setStale(ctx, pinID, nil)
	}
	st := h.Status()
	if st.Mode != torrent.ModeDownload || st.CompletedAt == nil || st.Status == torrent.StatusError || s.Imported == nil {
		return nil
	}
	at, imported, err := s.Imported(ctx, ih)
	if err != nil {
		return err
	}
	// files the current torrent lists are its own, whatever was recorded
	live := map[string]bool{}
	for _, f := range h.Files() {
		if f.FullPath != "" {
			live[filepath.Clean(f.FullPath)] = true
		}
	}
	var keep []stale
	for _, x := range list {
		since, _ := time.Parse(time.RFC3339, x.Since)
		if !imported || at.Before(since) {
			keep = append(keep, x)
			continue
		}
		var left []string
		for _, p := range x.Paths {
			if live[p] {
				continue // listed again: a file of the current release
			}
			fi, err := os.Lstat(p)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if !inside(x.Root, p) || !insideResolved(x.Root, p) {
				s.log().Warn("pinned: a stale file outside the release's folder is kept", "path", p, "root", x.Root)
				continue
			}
			if err != nil {
				left = append(left, p)
				continue
			}
			if !fi.Mode().IsRegular() {
				continue // a folder or a link: never followed nor deleted
			}
			if s.libraryNames(ctx, p) {
				left = append(left, p) // the library still plays it
				continue
			}
			if err := os.Remove(p); err != nil {
				s.log().Warn("pinned: delete an old file of a replaced release", "path", p, "err", err)
				left = append(left, p)
				continue
			}
			s.log().Info("pinned: deleted an old file of a replaced release", "path", p, "pin", pinID, "infohash", ih)
			removeEmptyDirs(x.Root, filepath.Dir(p))
		}
		if len(left) > 0 {
			x.Paths = left
			keep = append(keep, x)
		}
	}
	return s.setStale(ctx, pinID, keep)
}

// insideResolved: p's folder, links resolved, is still inside root (links
// resolved): no folder link leads the delete elsewhere.
func insideResolved(root, p string) bool {
	r, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	d, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		return false
	}
	return inside(r, filepath.Join(d, filepath.Base(p)))
}

// torrentStored: the engine's store has the torrent ih (true when that
// cannot be told: keep the record).
func (s *Service) torrentStored(ctx context.Context, ih string) bool {
	var one int
	err := s.DB.QueryRowContext(ctx, `SELECT 1 FROM torrents WHERE lower(infohash)=lower(?) LIMIT 1`, ih).Scan(&one)
	return !errors.Is(err, sql.ErrNoRows)
}

// libraryNames: a library file names p (true when that cannot be told,
// or without Library: keep the file).
func (s *Service) libraryNames(ctx context.Context, p string) bool {
	if s.Library == nil {
		return true
	}
	ok, err := s.Library.FileExists(ctx, p)
	return err != nil || ok
}

// removeEmptyDirs removes dir and its parents while empty, inside root
// (root itself stays).
func removeEmptyDirs(root, dir string) {
	for inside(root, dir) {
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
