package libraryhooks

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/antresol-app/antresol/addonkit"
)

// disk is disk.usage {}: the downloads folder and the watch cache as disk
// segments; the core walks them after the library (a download hardlinked
// into it counts once). A folder not on disk is left out.
func (d Deps) disk(context.Context, *addonkit.User, json.RawMessage) (any, error) {
	out := addonkit.DiskUsage{Segments: []addonkit.DiskSegment{}}
	for _, s := range []addonkit.DiskSegment{
		{ID: "downloads", Label: addonkit.Text{RU: "Загрузки", EN: "Downloads"}, Dir: d.DownloadDir},
		{ID: "stream-cache", Label: addonkit.Text{RU: "Кеш просмотра", EN: "Watch cache"}, Dir: d.StreamDir},
	} {
		if s.Dir == "" {
			continue
		}
		if fi, err := os.Stat(s.Dir); err != nil || !fi.IsDir() {
			continue
		}
		out.Segments = append(out.Segments, s)
	}
	return out, nil
}

// CheckSegmentDirs logs each folder of dirs the core will not walk as a
// disk segment, as files.Usage drops them: one not at or inside the
// media root or a file-browser root, the media root itself, one at,
// inside or around a library folder, and one at, inside or around an
// earlier folder of dirs (symlinks resolved as the core resolves them; a
// folder not there yet through its deepest existing parent). Such a
// segment is dropped silently; this says why at the start. It returns
// the folders logged.
func CheckSegmentDirs(log *slog.Logger, info addonkit.Info, dirs ...string) []string {
	var roots []string
	for _, r := range append([]string{info.MediaRoot}, info.FileRoots...) {
		if r != "" {
			roots = append(roots, resolve(r))
		}
	}
	lr := info.LibraryRoots
	var taken []string
	for _, l := range []string{lr.Movies, lr.Series, lr.Music, lr.Ebooks, lr.Audiobooks, lr.Comics} {
		if l != "" {
			taken = append(taken, resolve(l))
		}
	}
	media := ""
	if info.MediaRoot != "" {
		media = resolve(info.MediaRoot)
	}
	var bad []string
	for _, d := range dirs {
		if d == "" {
			continue
		}
		rd := resolve(d)
		why := ""
		switch {
		case !slices.ContainsFunc(roots, func(r string) bool { return atOrIn(rd, r) }):
			why = "outside the media root and the file roots"
		case rd == media:
			why = "the media root itself"
		case slices.ContainsFunc(taken, func(t string) bool { return atOrIn(rd, t) || atOrIn(t, rd) }):
			why = "at, inside or around a library folder or another segment"
		}
		if why != "" {
			log.Warn("torrents: the disk meter will not show this folder: "+why, "dir", d)
			bad = append(bad, d)
			continue
		}
		taken = append(taken, rd)
	}
	return bad
}

// atOrIn: p is dir or lies inside it (both resolved).
func atOrIn(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// resolve is p with its symlinks resolved; a path not there yet through
// its deepest existing parent (the rest as written).
func resolve(p string) string {
	p = filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}
