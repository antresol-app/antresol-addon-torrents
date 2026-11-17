package importer

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// fileDirs are the old build's data dirs the addon takes over:
// metafiles, tracker definitions, streaming-service tokens, qBittorrent's
// saved state.
var fileDirs = []string{"torrents", "definitions", "import-tokens", filepath.Join("migrate", "qbt-state")}

// reportsDir is where the addon keeps its «Переезд» reports; the old
// build's are the *.json files directly under from/migrate.
var reportsDir = filepath.Join("migrate", "reports")

// copyFiles copies every file of fileDirs under from, and the old
// build's reports (from/migrate/*.json → reportsDir), into staging, then
// renames each into its place under dataDir (overwriting: a rerun after a
// failure redoes it), and syncs the directories it renamed into.
// from is only read. A dir that does not exist is skipped; a dir that is a
// symlink is resolved first; inside, a symlink to a regular file is copied
// by its content. Anything else — a dangling or looping link, a link to a
// dir, a device, a socket, a pipe, an unreadable dir — fails the import:
// never a silent skip.
func copyFiles(from, staging, dataDir string) (int, error) {
	var copied []string
	for _, rel := range fileDirs {
		root := filepath.Join(from, rel)
		if _, err := os.Lstat(root); os.IsNotExist(err) {
			continue
		}
		real, err := filepath.EvalSymlinks(root)
		if err != nil {
			return 0, fmt.Errorf("importer: resolve %s: %w", root, err)
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() {
			return 0, fmt.Errorf("importer: %s is not a directory", root)
		}
		err = filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			switch t := d.Type(); {
			case t.IsDir():
				return nil
			case t.IsRegular():
			case t&fs.ModeSymlink != 0:
				fi, err := os.Stat(p) // follows the link
				if err != nil {
					return fmt.Errorf("link %s: %w", p, err)
				}
				if !fi.Mode().IsRegular() {
					return fmt.Errorf("link %s does not point to a regular file", p)
				}
			default:
				return fmt.Errorf("%s is not a regular file (%v)", p, t)
			}
			r, err := filepath.Rel(real, p)
			if err != nil {
				return err
			}
			r = filepath.Join(rel, r)
			if err := copyFile(p, filepath.Join(staging, r)); err != nil {
				return err
			}
			copied = append(copied, r)
			return nil
		})
		if err != nil {
			return 0, fmt.Errorf("importer: copy %s: %w", root, err)
		}
	}
	reports, err := copyReports(from, staging)
	if err != nil {
		return 0, err
	}
	copied = append(copied, reports...)
	dirs := map[string]bool{}
	for _, r := range copied {
		dst := filepath.Join(dataDir, r)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		if err := os.Rename(filepath.Join(staging, r), dst); err != nil {
			return 0, err
		}
		dirs[filepath.Dir(dst)] = true
	}
	for d := range dirs {
		if err := syncDir(d); err != nil {
			return 0, err
		}
	}
	return len(copied), nil
}

// copyReports copies the *.json entries directly under from/migrate into
// staging/reportsDir and returns their staging paths. A link to a regular
// file is copied by its content; any other entry so named (a dir, a
// device, a dangling link) fails the import.
func copyReports(from, staging string) ([]string, error) {
	dir := filepath.Join(from, "migrate")
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("importer: read %s: %w", dir, err)
	}
	var out []string
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		p := filepath.Join(dir, e.Name())
		fi, err := os.Stat(p) // follows a link
		if err != nil {
			return nil, fmt.Errorf("importer: report %s: %w", p, err)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("importer: report %s is not a regular file (%v)", p, fi.Mode().Type())
		}
		r := filepath.Join(reportsDir, e.Name())
		if err := copyFile(p, filepath.Join(staging, r)); err != nil {
			return nil, fmt.Errorf("importer: copy %s: %w", p, err)
		}
		out = append(out, r)
	}
	return out, nil
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
