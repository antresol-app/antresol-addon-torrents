// Package coreadapt is the core as acquire and organize know it, reached
// only through the public SDK (addonkit.Client): the library, the
// profiles' names, the image cache, the metadata pass-through and the
// app's events. Answers come back in the copied library model
// (corecopy/library), so the moved code keeps its types.
package coreadapt

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol/addonkit"
)

// Core is the library acquire and organize use: Library over the core
// API, coretest.Library in unit tests.
type Core interface {
	ItemByTMDB(ctx context.Context, kind string, tmdbID int64) (library.Item, error)
	FindAlbum(ctx context.Context, artist, album string) (library.Item, bool, error)
	FindBook(ctx context.Context, kind, author, title string) (library.Item, bool, error)
	FindTitle(ctx context.Context, kind string, tmdbID int64, title, creator string) (library.Item, bool, error)
	SeriesFolder(ctx context.Context, root string, tmdbID int64) (string, error)
	HaveEpisodes(ctx context.Context, tmdbID int64) (map[[2]int]bool, error)
	ItemIDsByTMDB(ctx context.Context, kind string, ids []int64) (map[int64]int64, error)
	AlbumDir(ctx context.Context, id int64) (string, error)
	LibraryPaths(ctx context.Context) ([]string, error)
	Import(ctx context.Context, paths []string, ref string, owner int64) (imported, missing, taken []string, err error)
	ClaimRef(ctx context.Context, ref string, userID int64) error
	ClaimPaths(ctx context.Context, paths []string, userID int64) error
	FileExists(ctx context.Context, path string) (bool, error)
	ItemOfFile(ctx context.Context, fileID int64) (library.Item, bool, error)
	SeriesAudio(ctx context.Context, tmdbID int64) (addonkit.SeriesAudio, error)
	SearchTitles(ctx context.Context, kind, title string, limit int) ([]library.Item, error)
	ItemFiles(ctx context.Context, id int64) ([]library.File, error)
	Item(ctx context.Context, id int64) (library.Item, error)
	// Aliases is the item's other titles in the library (its item titles
	// but the title and original title); library.ErrNotFound when
	// there is no such item.
	Aliases(ctx context.Context, id int64) ([]string, error)
	// FileByPath is the library's file at path (false: none);
	// RefFiles the library files carrying ref (a download's infohash).
	FileByPath(ctx context.Context, path string) (library.File, bool, error)
	RefFiles(ctx context.Context, ref string) ([]addonkit.RefFile, error)
	// FilesUnder is the library's files under the folder dir; BindRef puts
	// ref on exactly the library files at paths (this addon's other files
	// with ref lose it, none clears it) and answers the paths the library
	// has not and those another addon holds.
	FilesUnder(ctx context.Context, dir string) ([]library.File, error)
	BindRef(ctx context.Context, ref string, paths []string) (missing, taken []string, err error)
}

var _ Core = Library{}

// Library is the core's library through addonkit.
type Library struct{ C *addonkit.Client }

// ToItem is the copied model of an SDK item (no sort title, overview,
// backdrop or update time: the core API does not carry them).
func ToItem(it addonkit.Item) library.Item {
	return library.Item{
		ID: it.ID, Kind: it.Kind, ParentID: it.ParentID, Title: it.Title, OriginalTitle: it.OriginalTitle,
		Year: it.Year, Rating: it.Rating, TmdbID: it.TMDBID, SeasonNo: it.SeasonNo, EpisodeNo: it.EpisodeNo,
		Poster: it.Poster, AddedAt: it.AddedAt, MetaJSON: rawString(it.Meta),
	}
}

// ToFile is the copied model of an SDK file (no season or episode: an
// episode's numbers are on its item).
func ToFile(f addonkit.File) library.File {
	return library.File{
		ID: f.ID, ItemID: f.ItemID, Path: f.Path, Size: f.Size, Mtime: f.Mtime,
		DurationMs: f.DurationMs, ProbeJSON: rawString(f.Probe),
	}
}

// rawString is a JSON value as stored text, "" for none.
func rawString(b []byte) string {
	if s := strings.TrimSpace(string(b)); s != "null" {
		return s
	}
	return ""
}

func items(in []addonkit.Item) []library.Item {
	out := make([]library.Item, len(in))
	for i, it := range in {
		out[i] = ToItem(it)
	}
	return out
}

// notFound turns the core's 404 into library.ErrNotFound.
func notFound(err error) error {
	if addonkit.IsNotFound(err) {
		return library.ErrNotFound
	}
	return err
}

func found(it addonkit.Item, ok bool, err error) (library.Item, bool, error) {
	if err != nil || !ok {
		return library.Item{}, false, err
	}
	return ToItem(it), true, nil
}

// ItemByTMDB is the library's movie or series with this TMDB id;
// library.ErrNotFound when it has none.
func (l Library) ItemByTMDB(ctx context.Context, kind string, tmdbID int64) (library.Item, error) {
	it, ok, err := l.C.ItemByTMDB(ctx, kind, tmdbID)
	if err != nil {
		return library.Item{}, err
	}
	if !ok {
		return library.Item{}, library.ErrNotFound
	}
	return ToItem(it), nil
}

func (l Library) FindAlbum(ctx context.Context, artist, album string) (library.Item, bool, error) {
	return found(l.C.FindAlbum(ctx, artist, album))
}

func (l Library) FindBook(ctx context.Context, kind, author, title string) (library.Item, bool, error) {
	return found(l.C.FindBook(ctx, kind, author, title))
}

func (l Library) FindTitle(ctx context.Context, kind string, tmdbID int64, title, creator string) (library.Item, bool, error) {
	return found(l.C.FindTitle(ctx, kind, tmdbID, title, creator))
}

// SeriesFolder is the name of the folder directly under root that holds
// the series (as *library.Repo gave it: organize joins it to SeriesDir);
// library.ErrNotFound when the library has none there.
func (l Library) SeriesFolder(ctx context.Context, root string, tmdbID int64) (string, error) {
	p, err := l.C.SeriesFolder(ctx, root, tmdbID)
	if err != nil {
		return "", notFound(err)
	}
	return FolderUnder(root, p)
}

// FolderUnder is the first path element of p below root;
// library.ErrNotFound when p is not below it.
func FolderUnder(root, p string) (string, error) {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(p))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", library.ErrNotFound
	}
	if i := strings.IndexRune(rel, filepath.Separator); i > 0 {
		rel = rel[:i]
	}
	return rel, nil
}

func (l Library) HaveEpisodes(ctx context.Context, tmdbID int64) (map[[2]int]bool, error) {
	return l.C.HaveEpisodes(ctx, tmdbID)
}

func (l Library) ItemIDsByTMDB(ctx context.Context, kind string, ids []int64) (map[int64]int64, error) {
	if len(ids) == 0 {
		return map[int64]int64{}, nil
	}
	return l.C.ItemIDsByTMDB(ctx, kind, ids)
}

// AlbumDir is the album's folder; library.ErrNotFound for no such album.
func (l Library) AlbumDir(ctx context.Context, id int64) (string, error) {
	p, err := l.C.AlbumDir(ctx, id)
	return p, notFound(err)
}

// LibraryPaths is the path of every library file, missing ones too.
func (l Library) LibraryPaths(ctx context.Context) ([]string, error) {
	return l.C.FilePaths(ctx)
}

// Import adds paths to the library with ref and owner (one scan): the
// paths of the files it now has, those it has none at, and those another
// addon's ref holds.
func (l Library) Import(ctx context.Context, paths []string, ref string, owner int64) (imported, missing, taken []string, err error) {
	res, err := l.C.Import(ctx, paths, ref, owner)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, f := range res.Files {
		imported = append(imported, f.Path)
	}
	return imported, res.Missing, res.Taken, nil
}

// ClaimRef makes userID an owner of the titles of the files with ref.
func (l Library) ClaimRef(ctx context.Context, ref string, userID int64) error {
	return l.C.ClaimOwner(ctx, ref, nil, userID)
}

// ClaimPaths makes userID an owner of the titles of the files at paths.
func (l Library) ClaimPaths(ctx context.Context, paths []string, userID int64) error {
	if len(paths) == 0 {
		return nil
	}
	return l.C.ClaimOwner(ctx, "", paths, userID)
}

// FileExists: the library has a file at path.
func (l Library) FileExists(ctx context.Context, path string) (bool, error) {
	_, ok, err := l.C.FileByPath(ctx, path)
	return ok, err
}

// ItemOfFile is the item the file belongs to (an episode's file: the
// episode); ok is false when the library has no such file or item.
func (l Library) ItemOfFile(ctx context.Context, fileID int64) (library.Item, bool, error) {
	f, ok, err := l.C.FileByID(ctx, fileID)
	if err != nil || !ok {
		return library.Item{}, false, err
	}
	it, err := l.C.Item(ctx, f.ItemID)
	if addonkit.IsNotFound(err) {
		return library.Item{}, false, nil
	}
	if err != nil {
		return library.Item{}, false, err
	}
	return ToItem(it), true, nil
}

// SeriesAudio is the audio titles of the series' episodes and the voices
// profiles chose; empty for a series the library does not have.
func (l Library) SeriesAudio(ctx context.Context, tmdbID int64) (addonkit.SeriesAudio, error) {
	sa, err := l.C.SeriesAudio(ctx, tmdbID)
	if addonkit.IsNotFound(err) {
		return addonkit.SeriesAudio{}, nil
	}
	return sa, err
}

func (l Library) SearchTitles(ctx context.Context, kind, title string, limit int) ([]library.Item, error) {
	its, err := l.C.SearchTitles(ctx, kind, title, limit)
	if err != nil {
		return nil, err
	}
	return items(its), nil
}

// ItemFiles is the item's own files, then its episodes'; library.ErrNotFound
// for no such item.
func (l Library) ItemFiles(ctx context.Context, id int64) ([]library.File, error) {
	fs, err := l.C.ItemFiles(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	out := make([]library.File, len(fs))
	for i, f := range fs {
		out[i] = ToFile(f)
	}
	return out, nil
}

// Item is the library item id; library.ErrNotFound when there is none.
func (l Library) Item(ctx context.Context, id int64) (library.Item, error) {
	it, err := l.C.Item(ctx, id)
	if err != nil {
		return library.Item{}, notFound(err)
	}
	return ToItem(it), nil
}

// Aliases is the item's alias titles (the API's Item.aliases: the copied
// library.Item has no field for them); library.ErrNotFound when there is
// no such item.
func (l Library) Aliases(ctx context.Context, id int64) ([]string, error) {
	it, err := l.C.Item(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return it.Aliases, nil
}

func (l Library) FileByPath(ctx context.Context, path string) (library.File, bool, error) {
	f, ok, err := l.C.FileByPath(ctx, path)
	if err != nil || !ok {
		return library.File{}, false, err
	}
	return ToFile(f), true, nil
}

func (l Library) RefFiles(ctx context.Context, ref string) ([]addonkit.RefFile, error) {
	return l.C.RefFiles(ctx, ref)
}

func (l Library) FilesUnder(ctx context.Context, dir string) ([]library.File, error) {
	fs, err := l.C.FilesUnder(ctx, dir)
	if err != nil {
		return nil, err
	}
	out := make([]library.File, len(fs))
	for i, f := range fs {
		out[i] = ToFile(f)
	}
	return out, nil
}

func (l Library) BindRef(ctx context.Context, ref string, paths []string) (missing, taken []string, err error) {
	b, err := l.C.BindRef(ctx, ref, paths)
	return b.Missing, b.Taken, err
}
