// Package holds tells the core which data the downloads hold (PUT
// /files/holds), so the file browser asks before moving or trashing it,
// and answers the core's files.* hooks for the moves it agreed to.
package holds

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// Info is one torrent of the engine: its status and its files.
type Info struct {
	Status torrent.Status
	Files  []torrent.FileInfo
}

// Source lists the engine's torrents (what the core's index of torrent
// data was rebuilt from).
type Source interface{ Torrents() []Info }

// EngineSource is the engine as a Source. A torrent's files are read again
// only when its shape changed (a selection, a move, a state change) or
// every filesRefresh: listing them costs a FileInfo per file.
type EngineSource struct {
	Engine *torrent.Engine

	mu    sync.Mutex
	cache map[string]cachedFiles
}

// filesRefresh bounds how long a torrent's files are served from the cache
// whatever its shape says.
const filesRefresh = time.Minute

type cachedFiles struct {
	shape shape
	at    time.Time
	files []torrent.FileInfo
}

// Torrents lists every torrent with its files, oldest first.
func (s *EngineSource) Torrents() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Info
	out, s.cache = withFiles(s.cache, s.Engine.List(), time.Now(), func(ih string) []torrent.FileInfo {
		if h, ok := s.Engine.Get(ih); ok {
			return h.Files()
		}
		return nil
	})
	return out
}

// Forget drops the cached files: the next Torrents lists every torrent's
// files afresh (a forced push builds from what is on the engine now).
func (s *EngineSource) Forget() {
	s.mu.Lock()
	s.cache = nil
	s.mu.Unlock()
}

// withFiles pairs each torrent of list with its files: those of cache when
// its shape is the same and they are younger than filesRefresh, else
// load(infohash). It returns the pairs and the cache for the next call; an
// empty answer is not kept (a torrent being moved lists no files: asked
// again at the next call).
func withFiles(cache map[string]cachedFiles, list []torrent.Status, now time.Time, load func(ih string) []torrent.FileInfo) ([]Info, map[string]cachedFiles) {
	out := make([]Info, 0, len(list))
	next := make(map[string]cachedFiles, len(list))
	for _, st := range list {
		c, ok := cache[st.InfoHash]
		if sh := shapeOf(st); !ok || c.shape != sh || now.Sub(c.at) >= filesRefresh {
			c = cachedFiles{shape: sh, at: now, files: load(st.InfoHash)}
		}
		if c.files != nil {
			next[st.InfoHash] = c
		}
		out = append(out, Info{Status: st, Files: c.files})
	}
	return out, next
}

// shape is what a torrent's holds are made of besides its files: when it
// stays the same, so do they (and a change of the files that leaves it the
// same is picked up at the next full cycle). Busy is set by the pusher.
type shape struct {
	Hash, Name, SaveDir, Mode, Status string
	Parked, Busy                      bool
	Item                              int64 // -1: none
	Selected, Total                   int
	Size                              int64
}

func shapeOf(st torrent.Status) shape {
	sh := shape{Hash: st.InfoHash, Name: st.Name, SaveDir: st.SaveDir, Mode: st.Mode, Status: st.Status,
		Parked: st.Parked, Item: -1, Selected: st.SelectedFiles, Total: st.TotalFiles, Size: st.Size}
	if st.ItemID != nil {
		sh.Item = *st.ItemID
	}
	return sh
}

// Build makes the holds of list: one per torrent with metadata, keyed by
// its lower-case infohash, rooted at its own top-level file or folder,
// listing every file (an unselected one of a partial download, a sparse
// file holding boundary pieces, as Selected false: still protected).
// Active is "running and not parked"; Busy is busy(ih) (being put into the
// library); ItemID is itemOf(ih) (the item the download was made for).
//
// The core refuses a whole set holding a root outside the file browser's
// roots (fileRoots, as /info reports them: symlinks resolved), so a hold
// whose root, its symlinks resolved, is not at or inside one of them is
// left out and counted in outside.
//
// Build is buildIn with no root missing on disk.
func Build(list []Info, busy func(ih string) bool, itemOf func(ih string) int64, fileRoots []string) (holds []addonkit.Hold, outside int) {
	return buildIn(list, busy, itemOf, fileRoots, nil)
}

// buildIn is Build where missing are the file browser's roots not on disk
// (/info's missingRoots, as configured: a mount not up yet): a hold whose
// root, as spelled, is at or inside one of them is inside, kept in the set
// (the core keeps it, and it protects the path once it is there), never
// counted outside.
func buildIn(list []Info, busy func(ih string) bool, itemOf func(ih string) int64, fileRoots, missing []string) (holds []addonkit.Hold, outside int) {
	roots := make([]string, 0, len(fileRoots))
	for _, r := range fileRoots {
		if r != "" {
			roots = append(roots, resolve(r))
		}
	}
	holds = []addonkit.Hold{}
	for _, in := range list {
		st := in.Status
		if st.Name == "" || st.SaveDir == "" {
			continue // no metadata yet: no data of its own
		}
		ih := lower(st.InfoHash)
		root := filepath.Join(st.SaveDir, st.Name)
		if !insideAny(missing, root) && !insideAny(roots, resolve(root)) {
			if st.Mode != torrent.ModeStream {
				outside++ // the stream cache's own data is not the library's
			}
			continue
		}
		h := addonkit.Hold{
			Key:         ih,
			Root:        root,
			Files:       []addonkit.HoldFile{},
			Active:      running(st),
			Label:       labelText(st.Name),
			MoveWarning: seedWarningText("{name}"),
			KeptWarning: keptWarningText("{name}"),
		}
		for _, f := range in.Files {
			if f.FullPath == "" {
				continue
			}
			p := filepath.Clean(f.FullPath)
			if !within(root, p) {
				continue
			}
			h.Files = append(h.Files, addonkit.HoldFile{Path: p, Size: f.Size, Selected: f.Selected})
		}
		if busy != nil {
			h.Busy = busy(ih)
		}
		if itemOf != nil && st.Mode != torrent.ModeStream {
			h.ItemID = itemOf(ih)
		}
		holds = append(holds, h)
	}
	return holds, outside
}

// running: the torrent uses its data now (downloading or seeding, not
// paused, failed or parked).
func running(st torrent.Status) bool {
	if st.Parked {
		return false
	}
	switch st.Status {
	case torrent.StatusPaused, torrent.StatusError:
		return false
	}
	return true
}

// resolve is p with its symlinks resolved; a path not on disk yet keeps
// its missing tail below the deepest folder that resolves, and one with no
// part that resolves stays as given.
func resolve(p string) string {
	p = filepath.Clean(p)
	var tail []string
	for cur := p; ; cur = filepath.Dir(cur) {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(append([]string{r}, tail...)...)
		}
		if !errors.Is(err, os.ErrNotExist) || cur == filepath.Dir(cur) {
			return p
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
	}
}

func insideAny(roots []string, p string) bool {
	for _, r := range roots {
		if within(r, p) {
			return true
		}
	}
	return false
}

// within reports whether p is dir or lies inside it.
func within(dir, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(p))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func lower(ih string) string { return strings.ToLower(ih) }

// text is key formatted with args in both interface languages.
func text(key string, args ...any) addonkit.Text {
	return addonkit.Text{RU: i18n.TL(i18n.RU, key, args...), EN: i18n.TL(i18n.EN, key, args...)}
}

// labelText is the badge of held files in «Файлы».
func labelText(name string) addonkit.Text { return text("Загрузка «%s»", name) }

// seedWarningText is the warning of a file operation on torrent data
// ({name}: the core puts the file's name in).
func seedWarningText(name string) addonkit.Text {
	return text("Раздача «%s» прекратится", name)
}
