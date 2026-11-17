// Package coretest is an in-memory core for the addon's unit tests: the
// same method set as coreadapt.Library (coreadapt.Core) over plain slices,
// so a test sets the library up as data and reads back what was imported.
package coretest

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol/addonkit"
)

var _ coreadapt.Core = (*Library)(nil)

// Library is the fake core library. Items and Files are the library (an
// episode's ParentID is its season, a season's its series; a track's its
// album, an album's its artist). Import records each call's paths in
// Imports and binds the imported ones to the ref in Refs; the paths in
// Missing come back missing, those in Taken taken. Owners records the
// claimed paths per profile. Audio answers SeriesAudio by TMDB id. Use a
// *Library; the zero value is ready.
type Library struct {
	Items   []library.Item
	Files   []library.File
	Imports [][]string
	Refs    map[string][]string
	Missing map[string]bool // paths Import reports missing
	Taken   map[string]bool // paths Import reports bound to another addon
	Owners  map[int64][]string
	Audio   map[int64]addonkit.SeriesAudio
	// ItemAliases answers Aliases by item id (the API's Item.aliases; a
	// field cannot share the method's name).
	ItemAliases map[int64][]string

	mu sync.Mutex
}

func (l *Library) item(id int64) (library.Item, bool) {
	for _, it := range l.Items {
		if it.ID == id {
			return it, true
		}
	}
	return library.Item{}, false
}

// top is the top-level item of kind with the TMDB id.
func (l *Library) top(kind string, tmdbID int64) (library.Item, bool) {
	for _, it := range l.Items {
		if it.Kind == kind && it.TmdbID == tmdbID && it.ParentID == 0 {
			return it, true
		}
	}
	return library.Item{}, false
}

// under: item id is ancestor or is it.
func (l *Library) under(id, ancestor int64) bool {
	for range 4 {
		if id == ancestor {
			return true
		}
		it, ok := l.item(id)
		if !ok || it.ParentID == 0 {
			return false
		}
		id = it.ParentID
	}
	return false
}

func (l *Library) ItemByTMDB(_ context.Context, kind string, tmdbID int64) (library.Item, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if it, ok := l.top(kind, tmdbID); ok {
		return it, nil
	}
	return library.Item{}, library.ErrNotFound
}

func (l *Library) FindAlbum(_ context.Context, artist, album string) (library.Item, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.findAlbum(artist, album)
}

func (l *Library) findAlbum(artist, album string) (library.Item, bool, error) {
	a, t := release.Normalize(artist), release.Normalize(album)
	if a == "" || t == "" {
		return library.Item{}, false, nil
	}
	for _, it := range l.Items {
		if it.Kind != library.KindAlbum || release.Normalize(it.Title) != t {
			continue
		}
		if ar, ok := l.item(it.ParentID); ok && release.Normalize(ar.Title) == a {
			return it, true, nil
		}
	}
	return library.Item{}, false, nil
}

func (l *Library) FindBook(_ context.Context, kind, author, title string) (library.Item, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.findBook(kind, author, title)
}

func (l *Library) findBook(kind, author, title string) (library.Item, bool, error) {
	t := release.Normalize(title)
	if t == "" || (kind != library.KindBook && kind != library.KindAudiobook) {
		return library.Item{}, false, nil
	}
	for _, it := range l.Items {
		if it.Kind != kind || release.Normalize(it.Title) != t {
			continue
		}
		if author == "" || slices.ContainsFunc(authors(it), func(a string) bool { return release.Normalize(a) == release.Normalize(author) }) {
			return it, true, nil
		}
	}
	return library.Item{}, false, nil
}

// authors is the item's meta "authors".
func authors(it library.Item) []string {
	var m struct {
		Authors []string `json:"authors"`
	}
	json.Unmarshal([]byte(it.MetaJSON), &m)
	return m.Authors
}

func (l *Library) FindTitle(_ context.Context, kind string, tmdbID int64, title, creator string) (library.Item, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch kind {
	case library.KindMovie, library.KindSeries:
		it, ok := l.top(kind, tmdbID)
		return it, ok, nil
	case library.KindMusic:
		return l.findAlbum(creator, title)
	case library.KindBook, library.KindAudiobook:
		return l.findBook(kind, creator, title)
	}
	return library.Item{}, false, nil
}

// SeriesFolder is the name of the folder under root that holds a file of
// the series; library.ErrNotFound when none does.
func (l *Library) SeriesFolder(_ context.Context, root string, tmdbID int64) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.top(library.KindSeries, tmdbID)
	if !ok {
		return "", library.ErrNotFound
	}
	for _, f := range l.Files {
		if !l.under(f.ItemID, s.ID) {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(root), f.Path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		if i := strings.IndexRune(rel, filepath.Separator); i > 0 {
			return rel[:i], nil
		}
	}
	return "", library.ErrNotFound
}

// HaveEpisodes is the [season, episode] of the series' episodes with a
// file, multi-episode files by range.
func (l *Library) HaveEpisodes(_ context.Context, tmdbID int64) (map[[2]int]bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[[2]int]bool{}
	s, ok := l.top(library.KindSeries, tmdbID)
	if !ok {
		return out, nil
	}
	for _, f := range l.Files {
		ep, ok := l.item(f.ItemID)
		if !ok || ep.Kind != library.KindEpisode {
			continue
		}
		se, ok := l.item(ep.ParentID)
		if ok && se.ParentID == s.ID {
			out[[2]int{se.SeasonNo, ep.EpisodeNo}] = true
			// a file "S01E01-E02" holds the episodes after its own (as the core)
			info := release.Parse(filepath.Base(f.Path))
			if info.Episode == ep.EpisodeNo && (info.Season == 0 || info.Season == se.SeasonNo) {
				for e := ep.EpisodeNo + 1; e <= info.EpisodeEnd && e < ep.EpisodeNo+100; e++ {
					out[[2]int{se.SeasonNo, e}] = true
				}
			}
		}
	}
	return out, nil
}

func (l *Library) ItemIDsByTMDB(_ context.Context, kind string, ids []int64) (map[int64]int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[int64]int64{}
	for _, id := range ids {
		if it, ok := l.top(kind, id); ok {
			out[id] = it.ID
		}
	}
	return out, nil
}

// AlbumDir is the folder of the album's first file (its own or a
// track's); library.ErrNotFound for no such album.
func (l *Library) AlbumDir(_ context.Context, id int64) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if it, ok := l.item(id); !ok || it.Kind != library.KindAlbum {
		return "", library.ErrNotFound
	}
	for _, f := range l.Files {
		if l.under(f.ItemID, id) {
			return filepath.Dir(f.Path), nil
		}
	}
	return "", nil
}

func (l *Library) LibraryPaths(context.Context) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.Files))
	for _, f := range l.Files {
		out = append(out, f.Path)
	}
	sort.Strings(out)
	return out, nil
}

// Import records paths and binds the imported ones to ref.
func (l *Library) Import(_ context.Context, paths []string, ref string, owner int64) (imported, missing, taken []string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Imports = append(l.Imports, slices.Clone(paths))
	for _, p := range paths {
		switch {
		case l.Missing[p]:
			missing = append(missing, p)
		case l.Taken[p]:
			taken = append(taken, p)
		default:
			imported = append(imported, p)
		}
	}
	if ref != "" && len(imported) > 0 {
		if l.Refs == nil {
			l.Refs = map[string][]string{}
		}
		l.Refs[ref] = append(l.Refs[ref], imported...)
	}
	if owner != 0 {
		l.claim(owner, imported)
	}
	return imported, missing, taken, nil
}

func (l *Library) claim(userID int64, paths []string) {
	if l.Owners == nil {
		l.Owners = map[int64][]string{}
	}
	l.Owners[userID] = append(l.Owners[userID], paths...)
}

// ClaimRef claims the paths bound to ref.
func (l *Library) ClaimRef(_ context.Context, ref string, userID int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ps := l.Refs[ref]; len(ps) > 0 {
		l.claim(userID, ps)
	}
	return nil
}

func (l *Library) ClaimPaths(_ context.Context, paths []string, userID int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(paths) > 0 {
		l.claim(userID, paths)
	}
	return nil
}

func (l *Library) FileExists(_ context.Context, path string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.Files, func(f library.File) bool { return f.Path == path }), nil
}

func (l *Library) ItemOfFile(_ context.Context, fileID int64) (library.Item, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.Files {
		if f.ID == fileID {
			it, ok := l.item(f.ItemID)
			return it, ok, nil
		}
	}
	return library.Item{}, false, nil
}

func (l *Library) SeriesAudio(_ context.Context, tmdbID int64) (addonkit.SeriesAudio, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Audio[tmdbID], nil
}

// SearchTitles is the items of kind whose title or original title
// normalises to title's, at most limit (0: 20).
func (l *Library) SearchTitles(_ context.Context, kind, title string, limit int) ([]library.Item, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 {
		limit = 20
	}
	t := release.Normalize(title)
	out := []library.Item{}
	for _, it := range l.Items {
		if len(out) == limit {
			break
		}
		if it.Kind == kind && t != "" && (release.Normalize(it.Title) == t || release.Normalize(it.OriginalTitle) == t) {
			out = append(out, it)
		}
	}
	return out, nil
}

// ItemFiles is the item's own files, then its descendants' (a series' or
// season's episodes); library.ErrNotFound for no such item.
func (l *Library) ItemFiles(_ context.Context, id int64) ([]library.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.item(id); !ok {
		return nil, library.ErrNotFound
	}
	var own, below []library.File
	for _, f := range l.Files {
		switch {
		case f.ItemID == id:
			own = append(own, f)
		case l.under(f.ItemID, id):
			below = append(below, f)
		}
	}
	return append(append([]library.File{}, own...), below...), nil
}

func (l *Library) Item(_ context.Context, id int64) (library.Item, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if it, ok := l.item(id); ok {
		return it, nil
	}
	return library.Item{}, library.ErrNotFound
}

// Aliases is ItemAliases[id] ([] when none); library.ErrNotFound for no
// such item.
func (l *Library) Aliases(_ context.Context, id int64) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.item(id); !ok {
		return nil, library.ErrNotFound
	}
	return append([]string{}, l.ItemAliases[id]...), nil
}

// Bound is the ref's paths, sorted (a test's view of Refs).
func (l *Library) Bound(ref string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := slices.Clone(l.Refs[ref])
	sort.Strings(out)
	return out
}

// FileByPath is the file at path among Files.
func (l *Library) FileByPath(_ context.Context, path string) (library.File, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.Files {
		if f.Path == path {
			return f, true, nil
		}
	}
	return library.File{}, false, nil
}

// RefFiles is the Files at the ref's paths (Refs), by path.
func (l *Library) RefFiles(_ context.Context, ref string) ([]addonkit.RefFile, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []addonkit.RefFile{}
	paths := slices.Clone(l.Refs[ref])
	sort.Strings(paths)
	for _, p := range paths {
		for _, f := range l.Files {
			if f.Path == p {
				out = append(out, addonkit.RefFile{FileID: f.ID, ItemID: f.ItemID, Path: p})
			}
		}
	}
	return out, nil
}

// FilesUnder is the Files below the folder dir, by path.
func (l *Library) FilesUnder(_ context.Context, dir string) ([]library.File, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	prefix := strings.TrimSuffix(filepath.Clean(dir), "/") + "/"
	var out []library.File
	for _, f := range l.Files {
		if strings.HasPrefix(f.Path, prefix) {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// BindRef makes Refs[ref] exactly the paths among Files that Taken does
// not hold; the rest come back missing or taken.
func (l *Library) BindRef(_ context.Context, ref string, paths []string) (missing, taken []string, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var bound []string
	for _, p := range paths {
		switch {
		case l.Taken[p]:
			taken = append(taken, p)
		case !slices.ContainsFunc(l.Files, func(f library.File) bool { return f.Path == p }):
			missing = append(missing, p)
		default:
			bound = append(bound, p)
		}
	}
	if l.Refs == nil {
		l.Refs = map[string][]string{}
	}
	if len(bound) == 0 {
		delete(l.Refs, ref)
	} else {
		l.Refs[ref] = bound
	}
	return missing, taken, nil
}
