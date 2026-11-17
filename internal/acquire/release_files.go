package acquire

import (
	"bytes"
	"context"
	"errors"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// The file-list preview of «Выбрать релиз…» (POST /api/acquire/files).
const (
	// releaseFilesTimeout bounds the metadata wait of one preview request
	// (the reverse proxy cuts a request at 60 s).
	releaseFilesTimeout = 40 * time.Second
	// releaseFilesTTL and releaseFilesCacheMax bound the metafile cache.
	releaseFilesTTL      = 10 * time.Minute
	releaseFilesCacheMax = 32

	msgNoFileList      = "У этой раздачи нет списка файлов"
	msgFileListFailed  = "Не удалось получить список файлов"
	msgFileListTimeout = "Раздающие не прислали список файлов вовремя. Попробуйте ещё раз или выберите другую раздачу"
	msgFileListClosed  = "Раздача закрылась, пока приходил список файлов. Попробуйте ещё раз"
	msgReleaseAdded    = "Эта раздача уже добавлена. Чтобы выбрать другие песни, удалите её в «Загрузках» и добавьте снова."
	fileKindTrack      = "track"
	fileKindExtra      = "extra"
	msgNoReleaseSource = "Не указан источник раздачи"
	msgForeignRelease  = "Эта раздача не из выбранного источника"
	msgFilesAlbumOnly  = "Выбор песен доступен только для альбома"
	msgPickSong        = "Выберите хотя бы одну песню"
	msgBadFileList     = "Неверный список файлов"
)

// ReleaseFilesRequest asks for the files of a release the admin chose in
// the release table, for the album Title by Artist.
type ReleaseFilesRequest struct {
	SourceID    int64    `json:"sourceId"`
	ReleaseLink string   `json:"releaseLink"`
	Title       string   `json:"title"`
	Artist      string   `json:"artist"`
	Year        int      `json:"year"`
	Aliases     []string `json:"aliases"`
	// Direct: a file of an OPDS source, no torrent (no file list).
	Direct bool `json:"direct"`
	// Song: the song the admin searched for ("" = none): the track file
	// of that title is Matched («Выбрать треки…» preselects it).
	Song string `json:"song,omitempty"`
}

// ReleaseFile is one file of a release preview. Index is the engine's file
// index (AddOptions.Files); Album names the library album folder the file
// goes to; Checked is the default selection.
type ReleaseFile struct {
	Index   int    `json:"index"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	Kind    string `json:"kind"` // track | extra
	Album   string `json:"album"`
	Checked bool   `json:"checked"`
	// Disc and No: a track's disc and number from its file name (0 =
	// unknown; never for an extra).
	Disc int `json:"disc,omitempty"`
	No   int `json:"no,omitempty"`
	// Matched: the track is the requested Song.
	Matched bool `json:"matched,omitempty"`
	// Downloading: the file is in the download «Загрузки» has of this
	// release (ReleaseFiles.InDownload).
	Downloading bool `json:"downloading,omitempty"`
	// InLibrary: the library's album has this song already (another
	// release's copy, an earlier pick).
	InLibrary bool `json:"inLibrary,omitempty"`
}

// ReleaseFiles is a release preview. Added: the engine has this torrent
// already (Message says what to do, Files is empty).
type ReleaseFiles struct {
	InfoHash string `json:"infoHash"`
	Name     string `json:"name"`
	Added    bool   `json:"added"`
	// InDownload: «Загрузки» has this release with some of its songs
	// (Files.Downloading): a pick adds songs to that download.
	InDownload bool          `json:"inDownload,omitempty"`
	Message    string        `json:"message,omitempty"`
	Files      []ReleaseFile `json:"files"`
}

// cachedMetafile is a release's metafile, kept so that a second preview
// (and the confirm) neither downloads it from the source again nor waits
// for the swarm.
type cachedMetafile struct {
	metafile []byte
	infoHash string
	at       time.Time
}

// metafileCache maps "sourceId|releaseLink" to the release's metafile.
type metafileCache struct {
	mu sync.Mutex
	m  map[string]cachedMetafile
}

func cacheKey(sourceID int64, link string) string {
	return strconv.FormatInt(sourceID, 10) + "|" + link
}

func (c *metafileCache) get(k string, now time.Time) (cachedMetafile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.m[k]
	if !ok || now.Sub(v.at) > releaseFilesTTL {
		return cachedMetafile{}, false
	}
	return v, true
}

func (c *metafileCache) put(k string, v cachedMetafile, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]cachedMetafile{}
	}
	for key, e := range c.m {
		if now.Sub(e.at) > releaseFilesTTL {
			delete(c.m, key)
		}
	}
	for len(c.m) >= releaseFilesCacheMax {
		oldest := ""
		for key, e := range c.m {
			if oldest == "" || e.at.Before(c.m[oldest].at) {
				oldest = key
			}
		}
		delete(c.m, oldest)
	}
	v.at = now
	c.m[k] = v
}

// ReleaseFiles lists the files of a chosen release with their kind, album
// and default selection. A .torrent from the source is read at once; a
// magnet's metafile is fetched from the swarm (at most
// releaseFilesTimeout) without adding the torrent. A torrent the engine
// already has is answered Added, without any fetch.
func (s *Service) ReleaseFiles(ctx context.Context, req ReleaseFilesRequest) (ReleaseFiles, error) {
	if req.Direct {
		return ReleaseFiles{}, uerr(ErrInvalid, msgNoFileList)
	}
	deadline := time.Now().Add(releaseFilesTimeout)
	link := strings.TrimSpace(req.ReleaseLink)
	if link == "" {
		return ReleaseFiles{}, uerr(ErrInvalid, msgNoReleaseSource)
	}
	key := cacheKey(req.SourceID, link)
	c, ok := s.files.get(key, time.Now())
	if !ok {
		var added *ReleaseFiles
		var err error
		c, added, err = s.releaseMetafile(ctx, req.SourceID, link, deadline)
		if err != nil {
			return ReleaseFiles{}, err
		}
		if added != nil {
			return s.downloadPreview(ctx, added.InfoHash, added.Name, req), nil
		}
	}
	if h, ok := s.o.Engine.Get(c.infoHash); ok {
		return s.downloadPreview(ctx, c.infoHash, h.Status().Name, req), nil
	}
	_, name, entries, err := torrent.ParseFiles(c.metafile)
	if err != nil {
		return ReleaseFiles{}, wrapErr(ErrInvalid, msgNoFileList, err)
	}
	s.files.put(key, c, time.Now())
	files := releaseFileList(entries, req)
	markSong(files, req.Song, req.Artist)
	s.markInLibrary(ctx, files, entries, req)
	return ReleaseFiles{InfoHash: c.infoHash, Name: name, Files: files}, nil
}

// markSong sets Matched on the track file that holds song (as the music
// import matches a wanted song: matchTrackFiles), trying the song also
// without a leading artist («Кино Война» → «Война»), and the tracks' Disc
// and No.
func markSong(files []ReleaseFile, song, artist string) {
	for i, f := range files {
		if f.Kind == fileKindTrack {
			tf := organize.ParseTrackFile(f.Path)
			files[i].Disc, files[i].No = tf.Disc, tf.No
		}
	}
	song = strings.TrimSpace(song)
	if song == "" {
		return
	}
	tries := []string{song}
	if a := strings.TrimSpace(artist); a != "" {
		low, la := strings.ToLower(song), strings.ToLower(a)
		if strings.HasPrefix(low, la) && len(song) > len(a) {
			rest := strings.TrimLeft(song[len(a):], " -–—:")
			if rest != "" {
				tries = append(tries, rest)
			}
		}
	}
	for _, s := range tries {
		_, matched, _ := matchTrackFiles(files, []WantedTrack{{Key: "song", Title: s}}, true)
		if idx, ok := matched["song"]; ok {
			for i := range files {
				if files[i].Index == idx {
					files[i].Matched = true
				}
			}
			return
		}
	}
}

// releaseMetafile gets the metafile of a release: from the source, or
// from the swarm for a magnet. A torrent the engine has is answered
// (added) before any fetch.
func (s *Service) releaseMetafile(ctx context.Context, sourceID int64, link string, deadline time.Time) (cachedMetafile, *ReleaseFiles, error) {
	magnet := ""
	if strings.HasPrefix(strings.ToLower(link), "magnet:") {
		magnet = link
	} else {
		if sourceID <= 0 {
			return cachedMetafile{}, nil, uerr(ErrInvalid, msgNoReleaseSource)
		}
		if s.o.Indexer == nil {
			return cachedMetafile{}, nil, uerr(ErrUnavailable, "Источники раздач не настроены")
		}
		res, err := s.o.Indexer.Download(ctx, sourceID, link)
		if err != nil {
			if ctx.Err() != nil {
				return cachedMetafile{}, nil, ctx.Err()
			}
			return cachedMetafile{}, nil, wrapErr(ErrSource, err.Error(), err)
		}
		switch {
		case res.Magnet != "":
			magnet = res.Magnet
		case len(res.Torrent) > 0:
			mi, err := metainfo.Load(bytes.NewReader(res.Torrent))
			if err != nil {
				return cachedMetafile{}, nil, wrapErr(ErrInvalid, msgNoFileList, err)
			}
			// the caller checks the engine before parsing the file list
			return cachedMetafile{metafile: res.Torrent, infoHash: mi.HashInfoBytes().HexString()}, nil, nil
		default:
			return cachedMetafile{}, nil, uerr(ErrInvalid, msgNoFileList)
		}
	}
	spec, err := atorrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil || spec.InfoHash.IsZero() {
		return cachedMetafile{}, nil, wrapErr(ErrInvalid, msgNoFileList, err)
	}
	ih := spec.InfoHash.HexString()
	if h, ok := s.o.Engine.Get(ih); ok {
		a := alreadyAdded(ih, h.Status().Name)
		return cachedMetafile{}, &a, nil
	}
	fctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	mf, err := s.o.Engine.FetchInfo(fctx, magnet)
	switch {
	case errors.Is(err, torrent.ErrKnown):
		name := spec.DisplayName
		if h, ok := s.o.Engine.Get(ih); ok {
			name = h.Status().Name
		}
		a := alreadyAdded(ih, name)
		return cachedMetafile{}, &a, nil
	case err != nil && ctx.Err() != nil:
		return cachedMetafile{}, nil, ctx.Err()
	case errors.Is(err, torrent.ErrInfoClosed), errors.Is(err, torrent.ErrClosed):
		return cachedMetafile{}, nil, wrapErr(ErrNoFiles, msgFileListClosed, err)
	case errors.Is(err, torrent.ErrNoInfo):
		return cachedMetafile{}, nil, wrapErr(ErrNoFiles, msgFileListTimeout, err)
	case err != nil:
		return cachedMetafile{}, nil, wrapErr(ErrNoFiles, msgFileListFailed, err)
	}
	return cachedMetafile{metafile: mf, infoHash: ih}, nil, nil
}

func alreadyAdded(ih, name string) ReleaseFiles {
	return ReleaseFiles{InfoHash: ih, Name: name, Added: true, Message: msgReleaseAdded, Files: []ReleaseFile{}}
}

// tfileOf checks a metafile entry the way the organizer checks a torrent
// file (torrentFilesOf): false for junk (macOS resource forks, NAS
// folders, unsafe paths).
func tfileOf(f torrent.FileEntry) (organize.SetFile, bool) {
	fs := organize.SetFiles([]organize.File{{Path: f.Path, FullPath: f.Path, Size: f.Size}})
	if len(fs) == 0 {
		return organize.SetFile{}, false
	}
	return fs[0], true
}

// releaseFileKind is the kind of a release file: "track" for what the
// organizer places as a track, "extra" for any other file, "" for junk and
// padding files (left out of the preview; the other indexes unchanged).
func releaseFileKind(f torrent.FileEntry) string {
	if f.Padding {
		return ""
	}
	tf, ok := tfileOf(f)
	switch {
	case !ok:
		return ""
	case organize.IsTrack(tf):
		return fileKindTrack
	}
	return fileKindExtra
}

// albumRoots returns the album folders of a release's tracks (disc
// folders folded into their album), sorted.
func albumRoots(files []torrent.FileEntry) []string {
	seen := map[string]bool{}
	var roots []string
	for _, f := range files {
		if f.Padding {
			continue
		}
		tf, ok := tfileOf(f)
		if !ok || !organize.IsTrack(tf) {
			continue
		}
		if root, _ := organize.AlbumRootOf(tf); !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	return roots
}

// requestedRoot is the album folder of the release that holds the
// requested album: its folder's title (year, artist, edition and disc
// suffixes dropped: «1988 - Группа крови (Remastered 2012)», «Группа крови
// [Deluxe Edition]») is the title or one of its aliases. "" when none is.
func requestedRoot(files []torrent.FileEntry, title string, aliases []string) string {
	want := map[string]bool{}
	for _, t := range append([]string{title}, release.UsefulAliases(aliases)...) {
		if n := release.Normalize(t); n != "" {
			want[n] = true
		}
	}
	if len(want) == 0 {
		return ""
	}
	for _, root := range albumRoots(files) {
		if root == "." {
			continue
		}
		folder := path.Base(root)
		_, parsed, _ := library.ParseAlbumFolder(folder)
		if strings.TrimSpace(parsed) == "" {
			parsed = folder
		}
		if want[release.Normalize(organize.AlbumTitle(parsed))] {
			return root
		}
	}
	return ""
}

// releaseFileList builds the preview list, sorted by path. Each file
// belongs to the deepest album folder holding it; files outside every
// album folder belong to the requested album. Defaults: a release with one
// album (or none matching the request) is checked whole; of several, the
// requested album's files are checked and the others' not.
func releaseFileList(entries []torrent.FileEntry, req ReleaseFilesRequest) []ReleaseFile {
	roots := albumRoots(entries)
	requested := requestedRoot(entries, req.Title, req.Aliases)
	j := OrganizeJob{Kind: KindMusic, Title: req.Title, Year: req.Year, Artist: req.Artist}
	label := func(root string) string {
		return organize.AlbumFolderName(j, root, len(roots) == 1 || root == requested)
	}
	owner := func(p string) string {
		best, depth := "", -1
		for _, r := range roots {
			d := 0
			if r != "." {
				d = strings.Count(r, "/") + 1
			}
			if organize.Under(r, p) && d > depth {
				best, depth = r, d
			}
		}
		return best
	}
	out := []ReleaseFile{}
	for _, f := range entries {
		kind := releaseFileKind(f)
		if kind == "" {
			continue
		}
		tf, _ := tfileOf(f)
		root := owner(tf.Path())
		var album string
		switch {
		case root != "":
			album = label(root)
		case requested != "":
			album = label(requested)
		case len(roots) == 1:
			album = label(roots[0])
		default:
			album = organize.AlbumFolderName(j, ".", true)
		}
		checked := len(roots) <= 1 || requested == "" || root == "" || root == requested
		out = append(out, ReleaseFile{Index: f.Index, Path: f.Path, Size: f.Size, Kind: kind, Album: album, Checked: checked})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out
}
