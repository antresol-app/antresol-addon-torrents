package acquire

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// dataKeeper is implemented by *torrent.Engine.
type dataKeeper interface {
	DataRoot(infohash string) (string, bool)
	RemoveKeeping(ctx context.Context, infohash, root string, keep func(path string) bool) error
}

// libraryKeep makes the keep of RemoveKeeping for torrent ih: true for a
// file of its data that is a library file — by its path (cleaned, in NFC,
// through symlinked folders), or because the library has a file of that
// name in that same folder (os.SameFile of the folders: the same entry
// under another spelling, a missing row too), or because it is the same
// inode as a library file inside the torrent's folder. When in doubt (a
// library file or folder that cannot be checked) the file is kept. A hard
// link the library made elsewhere is another name of its own: the
// torrent's name goes, the library's stays. ok false: the engine or the
// library cannot tell, or the torrent has no data root (stream, moving):
// Remove deletes as it always did.
func (s *Service) libraryKeep(ctx context.Context, ih string) (dataKeeper, string, func(string) bool, bool, error) {
	dk, ok := s.o.Engine.(dataKeeper)
	if !ok {
		return nil, "", nil, false, nil
	}
	lp := s.o.Library // LibraryPaths: every file the library has, those it marks missing too
	if lp == nil {
		return nil, "", nil, false, nil
	}
	root, ok := dk.DataRoot(ih)
	if !ok {
		return nil, "", nil, false, nil
	}
	paths, err := lp.LibraryPaths(ctx)
	if err != nil {
		return nil, "", nil, false, err
	}
	c := newKeepCheck(root)
	// the library's files of the names in the torrent's data, and those
	// inside its folder by any spelling
	names := map[string]bool{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names[nfc(filepath.Base(p))] = true
		}
		return nil
	})
	for _, l := range paths {
		if names[nfc(filepath.Base(l))] || c.under(l) {
			c.add(l)
		}
	}
	return dk, root, c.keep, true, nil
}

func nfc(s string) string { return norm.NFC.String(s) }

// keepCheck answers keep for the library files it was given (add); stats
// and resolved folders are cached.
type keepCheck struct {
	roots  []string            // the data root's spellings (normalised)
	byName map[string][]string // NFC base name → library paths
	inside []string            // library paths inside the data root
	norms  map[string]bool     // every spelling of every library path
	stats  map[string]statResult
	dirs   map[string]string // folder → resolved, normalised
}

type statResult struct {
	fi  os.FileInfo
	err error
}

func newKeepCheck(root string) *keepCheck {
	c := &keepCheck{byName: map[string][]string{}, norms: map[string]bool{}, stats: map[string]statResult{}, dirs: map[string]string{}}
	c.roots = c.spellings(root)
	return c
}

// spellings: p cleaned in NFC, and with its folder's symlinks resolved.
func (c *keepCheck) spellings(p string) []string {
	clean := nfc(filepath.Clean(p))
	out := []string{clean}
	if d := c.resolved(filepath.Dir(p)); d != "" {
		if r := filepath.Join(d, nfc(filepath.Base(p))); r != clean {
			out = append(out, r)
		}
	}
	return out
}

func (c *keepCheck) resolved(dir string) string {
	if r, ok := c.dirs[dir]; ok {
		return r
	}
	r := ""
	if e, err := filepath.EvalSymlinks(dir); err == nil {
		r = nfc(filepath.Clean(e))
	}
	c.dirs[dir] = r
	return r
}

func (c *keepCheck) stat(p string) statResult {
	if s, ok := c.stats[p]; ok {
		return s
	}
	fi, err := os.Stat(p)
	s := statResult{fi, err}
	c.stats[p] = s
	return s
}

// under reports whether library path l lies inside the data root.
func (c *keepCheck) under(l string) bool {
	for _, s := range c.spellings(l) {
		for _, r := range c.roots {
			if s == r || strings.HasPrefix(s, r+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func (c *keepCheck) add(l string) {
	name := nfc(filepath.Base(l))
	c.byName[name] = append(c.byName[name], l)
	if c.under(l) {
		c.inside = append(c.inside, l)
	}
	for _, s := range c.spellings(l) {
		c.norms[s] = true
	}
}

// keep reports whether the torrent's file p is (or may be) a library file.
func (c *keepCheck) keep(p string) bool {
	for _, s := range c.spellings(p) {
		if c.norms[s] {
			return true
		}
	}
	ps := c.stat(p)
	if ps.err != nil {
		return !errors.Is(ps.err, fs.ErrNotExist) // gone already: nothing to keep
	}
	dir := c.stat(filepath.Dir(p))
	for _, l := range c.byName[nfc(filepath.Base(p))] {
		ld := c.stat(filepath.Dir(l))
		if ld.err != nil && !errors.Is(ld.err, fs.ErrNotExist) {
			return true // cannot tell
		}
		if ld.err == nil && dir.err == nil && os.SameFile(ld.fi, dir.fi) {
			return true // that name in this very folder is the library's
		}
	}
	for _, l := range c.inside {
		ls := c.stat(l)
		if ls.err == nil && os.SameFile(ls.fi, ps.fi) {
			return true
		}
		if ls.err != nil && !errors.Is(ls.err, fs.ErrNotExist) {
			return true // cannot tell
		}
	}
	return false
}
