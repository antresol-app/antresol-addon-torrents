package acquire

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"sync"

	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// «Выбрать треки…»: some songs of an album downloaded by hand. A second
// pick of the same release adds its songs to that download (one download,
// a larger selection, organized again when it completes); the track step
// marks the songs it has and those the library's album has. «Загрузки»
// says «Группа крови — 2 трека из 12» (TorrentView.SelectedTracks).

const (
	msgReleaseWhole  = "Эта раздача уже скачивается целиком — все её песни будут в медиатеке"
	msgNoAlbumFolder = "«%s» уже есть в медиатеке, но в раздаче не нашлась папка этого альбома: песни лягут отдельным альбомом"
	msgGrowFailed    = "Не удалось добавить песни к загрузке"
)

// selRevIs matches an acquisition whose selection revision is still the
// one an organize started with (the query's last argument).
const selRevIs = `COALESCE(json_extract(meta_json, '$.selRev'), 0) = ?`

// selectionGrower is implemented by *torrent.Engine: AddFiles grows a
// torrent's file selection.
type selectionGrower interface {
	AddFiles(ctx context.Context, infohash string, files []int) error
}

var _ selectionGrower = (*torrent.Engine)(nil)

// entriesOf is a torrent's file list as a metafile's.
func entriesOf(files []torrent.FileInfo) []torrent.FileEntry {
	out := make([]torrent.FileEntry, len(files))
	for i, f := range files {
		out[i] = torrent.FileEntry{Index: f.Index, Path: f.Path, Size: f.Size}
	}
	return out
}

// partial: some files of the torrent are not selected.
func partial(files []torrent.FileInfo) bool {
	for _, f := range files {
		if !f.Selected {
			return true
		}
	}
	return false
}

// downloadPreview is the track step of a release «Загрузки» has: with some
// of its songs, its file list from the download itself, those songs
// Downloading (InDownload); downloaded whole (or no metadata yet), only a
// message.
func (s *Service) downloadPreview(ctx context.Context, ih, name string, req ReleaseFilesRequest) ReleaseFiles {
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return alreadyAdded(ih, name)
	}
	files := h.Files()
	if len(files) == 0 {
		return alreadyAdded(ih, name)
	}
	if st := h.Status(); st.Name != "" {
		name = st.Name
	}
	if !partial(files) {
		return ReleaseFiles{InfoHash: ih, Name: name, Added: true, Message: msgReleaseWhole, Files: []ReleaseFile{}}
	}
	entries := entriesOf(files)
	list := releaseFileList(entries, req)
	markSong(list, req.Song, req.Artist)
	for i := range list {
		if j := list[i].Index; j >= 0 && j < len(files) && files[j].Selected {
			list[i].Downloading = true
		}
	}
	s.markInLibrary(ctx, list, entries, req)
	return ReleaseFiles{InfoHash: ih, Name: name, InDownload: true, Files: list}
}

// markInLibrary sets InLibrary on the tracks whose song the library's
// album of their album folder has (organize.AlbumSongs.has).
func (s *Service) markInLibrary(ctx context.Context, list []ReleaseFile, entries []torrent.FileEntry, req ReleaseFilesRequest) {
	roots := albumRoots(entries)
	if len(roots) == 0 || s.o.Library == nil {
		return
	}
	requested := requestedRoot(entries, req.Title, req.Aliases)
	if requested == "" && len(roots) == 1 {
		requested = roots[0]
	}
	dirs := s.libraryAlbumDirs(ctx, roots, requested, req.Title, req.Artist)
	if len(dirs) == 0 {
		return
	}
	songs := map[string]organize.AlbumSongs{}
	for i, f := range list {
		if f.Kind != fileKindTrack {
			continue
		}
		tf, ok := tfileOf(torrent.FileEntry{Index: f.Index, Path: f.Path, Size: f.Size})
		if !ok {
			continue
		}
		root, disc := organize.AlbumRootOf(tf)
		dir := dirs[root]
		if dir == "" {
			continue
		}
		have, ok := songs[dir]
		if !ok {
			have = organize.AlbumSongsIn(dir)
			songs[dir] = have
		}
		rel := tf.Base()
		if disc != "" {
			rel = path.Join(disc, tf.Base())
		}
		list[i].InLibrary = have.Has(rel)
	}
}

// growSelection adds the files of a pick (req.Files) to the music download
// ih that has some songs of the release already: the union downloads,
// what is done stays, and the acquisition is organized again when the
// larger selection completes (into the same album folders; the songs
// placed before are kept). ok false: not such a download (whole, unknown,
// another kind, an engine that cannot grow a selection) — the caller
// answers as before.
func (s *Service) growSelection(ctx context.Context, ih string, req AcquireRequest) (AcquireResult, bool, error) {
	g, ok := s.o.Engine.(selectionGrower)
	if !ok {
		return AcquireResult{}, false, nil
	}
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return AcquireResult{}, false, nil
	}
	files := h.Files()
	if len(files) == 0 || !partial(files) {
		return AcquireResult{}, false, nil
	}
	a, err := s.acquisition(ctx, ih)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AcquireResult{}, false, nil
	case err != nil:
		return AcquireResult{}, false, err
	case a.Kind != KindMusic:
		return AcquireResult{}, false, nil
	}
	entries := entriesOf(files)
	if _, err := selectionOf(entries, req.Files, a.Title, a.Meta.Aliases); err != nil {
		return AcquireResult{}, false, err
	}
	var add, union []int
	for _, f := range files {
		if f.Selected {
			union = append(union, f.Index)
		}
	}
	for _, i := range req.Files {
		if !files[i].Selected {
			add = append(add, i)
			union = append(union, i)
		}
	}
	if len(add) == 0 {
		return s.existingResult(ih), true, nil // it has them all
	}
	// No organize starts while the selection grows and the revision moves
	// (startOrganizeJob takes s.mu): one that runs already finishes with the
	// old revision and records nothing; one after sees the larger selection
	// still downloading, never its half-downloaded files.
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, err = s.acquisition(ctx, ih); err != nil {
		return AcquireResult{}, false, err
	}
	// the larger selection's albums the library has (a first pick's album
	// may be in it by now)
	if sel, err := selectionOf(entries, union, a.Title, a.Meta.Aliases); err == nil && a.Meta.AlbumDir == "" {
		a.Meta.Picked = true
		if dirs := s.libraryAlbumDirs(ctx, sel.roots, sel.requestedRoot, a.Title, a.Meta.Artist); len(dirs) > 0 {
			a.Meta.AlbumDirs = dirs
		}
	}
	a.Meta.SelRev++
	meta, err := json.Marshal(a.Meta)
	if err != nil {
		return AcquireResult{}, false, err
	}
	// the selection grows first: until the revision moves, an organize
	// that runs keeps its result; after it, none counts but the larger
	// selection's (a completion hook in between finds the old
	// organized_at and leaves it to reconcile)
	if err := g.AddFiles(ctx, ih, add); err != nil {
		return AcquireResult{}, false, growErr(err)
	}
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET organized_at=NULL, organize_error='', organize_attempts=0, meta_json=?
		WHERE infohash=?`, string(meta), ih); err != nil {
		return AcquireResult{}, false, err
	}
	s.log.Info("picked songs added to a download", "infohash", ih, "files", len(add))
	return s.existingResult(ih), true, nil
}

// trackCountsMax bounds the cache (dropped whole when full).
const trackCountsMax = 256

// trackCounts caches the song counts of partial music torrents per
// selection (infohash and how many files are selected: a selection only
// grows): Torrents runs every 2 s for torrent.progress.
type trackCounts struct {
	mu sync.Mutex
	m  map[string][2]int
}

// pickedTracks counts the selected tracks of a torrent with selected files
// selected, and all its tracks (as the preview lists them:
// releaseFileKind); 0, 0 before its metadata.
func (s *Service) pickedTracks(ih string, selectedFiles int) (selected, total int) {
	key := ih + "|" + strconv.Itoa(selectedFiles)
	s.tracks.mu.Lock()
	c, ok := s.tracks.m[key]
	s.tracks.mu.Unlock()
	if ok {
		return c[0], c[1]
	}
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return 0, 0
	}
	files := h.Files()
	if len(files) == 0 {
		return 0, 0 // no metadata yet: not cached
	}
	for _, f := range files {
		if releaseFileKind(torrent.FileEntry{Index: f.Index, Path: f.Path, Size: f.Size}) != fileKindTrack {
			continue
		}
		total++
		if f.Selected {
			selected++
		}
	}
	s.tracks.mu.Lock()
	if s.tracks.m == nil || len(s.tracks.m) >= trackCountsMax {
		s.tracks.m = map[string][2]int{}
	}
	s.tracks.m[key] = [2]int{selected, total}
	s.tracks.mu.Unlock()
	return selected, total
}

// forgetTracks drops the cached counts of a removed torrent.
func (s *Service) forgetTracks(ih string) {
	s.tracks.mu.Lock()
	defer s.tracks.mu.Unlock()
	for k := range s.tracks.m {
		if len(k) > len(ih) && k[:len(ih)] == ih && k[len(ih)] == '|' {
			delete(s.tracks.m, k)
		}
	}
}
