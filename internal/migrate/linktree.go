package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// linkFile is one file of a torrent: Rel is its path inside the torrent
// (with the top-level name, "/"-separated), Target the real file on disk.
type linkFile struct {
	Rel, Target string
}

// errCrossDevice: a renamed file is on another file system than LinkDir,
// so it cannot be hard-linked.
var errCrossDevice = errors.New("migrate: renamed files are on another disk")

// buildLinkTree makes root/<Rel> a hard link of Target for every file, so
// the torrent's own layout exists under root while the data stays where
// qBittorrent renamed it. Links already in place are kept; anything else
// at a link path (it is inside Vault's own folder) is replaced. A missing
// Target is skipped: the hash check then reports the data as incomplete.
func buildLinkTree(root string, files []linkFile) error {
	for _, f := range files {
		if !filepath.IsLocal(filepath.FromSlash(f.Rel)) {
			return fmt.Errorf("недопустимый путь в торренте: %q", f.Rel)
		}
		dst := filepath.Join(root, filepath.FromSlash(f.Rel))
		tst, err := os.Stat(f.Target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if dst == f.Target {
			continue
		}
		if lst, err := os.Lstat(dst); err == nil {
			if os.SameFile(lst, tst) {
				continue
			}
			if err := os.RemoveAll(dst); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.Link(f.Target, dst); err != nil {
			if errors.Is(err, syscall.EXDEV) {
				return errCrossDevice
			}
			return err
		}
	}
	return nil
}

// canLink reports whether every existing Target is on the same file system
// as root (or its nearest existing parent): the dry run's check for
// buildLinkTree, without creating anything.
func canLink(root string, files []linkFile) (bool, error) {
	dir := root
	var rootDev uint64
	for {
		st, err := os.Stat(dir)
		if err == nil {
			rootDev = devOf(st)
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, err
		}
		dir = parent
	}
	for _, f := range files {
		st, err := os.Stat(f.Target)
		if err != nil {
			continue
		}
		if devOf(st) != rootDev {
			return false, nil
		}
	}
	return true, nil
}

// fileKey identifies a file across hard links.
type fileKey struct{ dev, ino uint64 }

func keyOf(st os.FileInfo) (fileKey, bool) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fileKey{}, false
	}
	return fileKey{uint64(s.Dev), uint64(s.Ino)}, true
}

func devOf(st os.FileInfo) uint64 {
	k, _ := keyOf(st)
	return k.dev
}
