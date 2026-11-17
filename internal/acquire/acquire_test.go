package acquire

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// --- fakes ---

type fakeHandle struct {
	e     *fakeEngine
	ih    string
	files []torrent.FileInfo
	data  map[int][]byte
	// selection: the selected file indexes (AddOptions.Files); nil = every
	// file is selected, as the engine reports a whole torrent.
	selection []int
}

func (h *fakeHandle) InfoHash() string                   { return h.ih }
func (h *fakeHandle) WaitInfo(ctx context.Context) error { return nil }
func (h *fakeHandle) Files() []torrent.FileInfo {
	out := make([]torrent.FileInfo, len(h.files))
	for i, f := range h.files {
		f.Selected = h.selection == nil || slices.Contains(h.selection, f.Index)
		out[i] = f
	}
	return out
}
func (h *fakeHandle) Status() torrent.Status {
	h.e.mu.Lock()
	defer h.e.mu.Unlock()
	return h.e.st[h.ih]
}
func (h *fakeHandle) NewReader(ctx context.Context, i int) (io.ReadSeekCloser, error) {
	return nopCloser{bytes.NewReader(h.data[i])}, nil
}

type nopCloser struct{ io.ReadSeeker }

func (nopCloser) Close() error { return nil }

type fakeEngine struct {
	mu          sync.Mutex
	st          map[string]torrent.Status
	handles     map[string]*fakeHandle
	added       []torrent.AddOptions
	addedData   [][]byte
	modes       []string // "ih mode dir"
	removed     map[string]bool
	removedData map[string]bool
	hooks       []func(torrent.Status)
	nextFiles   []torrent.FileInfo // files of the next added torrent
	nextIH      string
	held        []string                       // HoldForSpace calls
	resumed     []string                       // Resume calls
	beforeRm    func(e *fakeEngine, ih string) // runs inside RemoveIfMode before the check
	offline     string                         // Offline(): why the media disk is held
	fetchInfo   func(ctx context.Context, magnet string) ([]byte, error)
	fetches     int             // FetchInfo calls
	grown       [][]int         // AddFiles calls
	addErr      error           // what adding a torrent the engine lacks fails with
	shrunk      [][]int         // RemoveFiles calls
	mayEmpty    map[string]bool // RemoveFiles: what the caller let empty, by path
}

func (e *fakeEngine) FetchInfo(ctx context.Context, magnet string) ([]byte, error) {
	e.mu.Lock()
	e.fetches++
	f := e.fetchInfo
	e.mu.Unlock()
	if f == nil {
		return nil, torrent.ErrNoInfo
	}
	return f(ctx, magnet)
}

func (e *fakeEngine) Offline() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.offline
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{st: map[string]torrent.Status{}, handles: map[string]*fakeHandle{}, removed: map[string]bool{}, removedData: map[string]bool{}}
}

func (e *fakeEngine) put(s torrent.Status) *fakeHandle {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st[s.InfoHash] = s
	h := e.handles[s.InfoHash]
	if h == nil {
		h = &fakeHandle{e: e, ih: s.InfoHash, data: map[int][]byte{}}
		e.handles[s.InfoHash] = h
	}
	return h
}

func (e *fakeEngine) add(data []byte, opt torrent.AddOptions) (torrent.Handle, error) {
	e.mu.Lock()
	ih := e.nextIH
	if ih == "" {
		ih = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	}
	if h, ok := e.handles[ih]; ok {
		e.mu.Unlock()
		return h, nil
	}
	if e.addErr != nil {
		e.mu.Unlock()
		return nil, e.addErr
	}
	e.added = append(e.added, opt)
	e.addedData = append(e.addedData, data)
	status := torrent.StatusDownloading
	if opt.WaitingSpace {
		status = torrent.StatusWaitingSpace
	}
	files := e.nextFiles
	e.mu.Unlock()
	st := torrent.Status{InfoHash: ih, Mode: opt.Mode, Status: status, SaveDir: opt.SaveDir}
	if opt.AddedBy != 0 && opt.AddedByUID != "" {
		by := opt.AddedBy
		st.AddedBy, st.AddedByUID = &by, opt.AddedByUID
	}
	h := e.put(st)
	h.files, h.selection = files, opt.Files
	return h, nil
}

func (e *fakeEngine) AddTorrentFile(_ context.Context, data []byte, opt torrent.AddOptions) (torrent.Handle, error) {
	return e.add(data, opt)
}
func (e *fakeEngine) AddMagnet(_ context.Context, uri string, opt torrent.AddOptions) (torrent.Handle, error) {
	return e.add([]byte(uri), opt)
}
func (e *fakeEngine) Get(ih string) (torrent.Handle, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, ok := e.handles[ih]
	if !ok {
		return nil, false
	}
	return h, true
}
func (e *fakeEngine) List() []torrent.Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []torrent.Status
	for _, s := range e.st {
		out = append(out, s)
	}
	return out
}
func (e *fakeEngine) Remove(_ context.Context, ih string, deleteData bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.st[ih]; !ok {
		return torrent.ErrNotFound
	}
	delete(e.st, ih)
	delete(e.handles, ih)
	e.removed[ih], e.removedData[ih] = true, deleteData
	return nil
}
func (e *fakeEngine) RemoveIfMode(ctx context.Context, ih, mode string, deleteData bool) error {
	if e.beforeRm != nil {
		e.beforeRm(e, ih)
	}
	e.mu.Lock()
	s, ok := e.st[ih]
	e.mu.Unlock()
	if ok && s.Mode != mode {
		return torrent.ErrModeChanged
	}
	return e.Remove(ctx, ih, deleteData)
}
func (e *fakeEngine) HoldForSpace(_ context.Context, ih string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.st[ih]
	if !ok {
		return torrent.ErrNotFound
	}
	s.Status = torrent.StatusWaitingSpace
	e.st[ih] = s
	e.held = append(e.held, ih)
	return nil
}

// AddFiles grows a selection as the engine does (the union, sorted; a
// whole torrent is left alone).
func (e *fakeEngine) AddFiles(_ context.Context, ih string, files []int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, ok := e.handles[ih]
	if !ok {
		return torrent.ErrNotFound
	}
	e.grown = append(e.grown, slices.Clone(files))
	if h.selection == nil {
		return nil
	}
	sel := append(slices.Clone(h.selection), files...)
	slices.Sort(sel)
	h.selection = slices.Compact(sel)
	// a complete selection that grew downloads again
	if s := e.st[ih]; s.Status == torrent.StatusSeeding {
		s.Status, s.CompletedAt = torrent.StatusDownloading, nil
		e.st[ih] = s
	}
	return nil
}

// RemoveFiles shrinks a selection as the engine does (a whole torrent
// becomes a selection of the others).
func (e *fakeEngine) RemoveFiles(_ context.Context, ih string, files []int, mayEmpty func(string) bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	h, ok := e.handles[ih]
	if !ok {
		return torrent.ErrNotFound
	}
	e.shrunk = append(e.shrunk, slices.Clone(files))
	if e.mayEmpty == nil {
		e.mayEmpty = map[string]bool{}
	}
	for _, f := range h.files {
		if slices.Contains(files, f.Index) {
			e.mayEmpty[f.FullPath] = mayEmpty != nil && mayEmpty(f.FullPath)
		}
	}
	var keep []int
	for _, f := range h.files {
		if (h.selection == nil || slices.Contains(h.selection, f.Index)) && !slices.Contains(files, f.Index) {
			keep = append(keep, f.Index)
		}
	}
	if len(keep) == 0 {
		return errors.New("fake: nothing left selected")
	}
	h.selection = keep
	return nil
}

func (e *fakeEngine) setMode(ih, mode string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.st[ih]
	s.Mode = mode
	e.st[ih] = s
}
func (e *fakeEngine) SetModeTo(_ context.Context, ih, mode, dir string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.st[ih]
	if !ok {
		return torrent.ErrNotFound
	}
	s.Mode = mode
	e.st[ih] = s
	e.modes = append(e.modes, ih+" "+mode+" "+dir)
	return nil
}
func (e *fakeEngine) Pause(context.Context, string) error { return nil }
func (e *fakeEngine) Resume(_ context.Context, ih string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.st[ih]
	if !ok {
		return torrent.ErrNotFound
	}
	if s.Status == torrent.StatusWaitingSpace || s.Status == torrent.StatusPaused {
		s.Status = torrent.StatusDownloading
	}
	e.st[ih] = s
	e.resumed = append(e.resumed, ih)
	return nil
}
func (e *fakeEngine) OnComplete(f func(torrent.Status)) {
	e.mu.Lock()
	e.hooks = append(e.hooks, f)
	e.mu.Unlock()
}
func (e *fakeEngine) complete(ih string) {
	e.mu.Lock()
	s := e.st[ih]
	s.Status = torrent.StatusSeeding
	if s.CompletedAt == nil {
		now := time.Now()
		s.CompletedAt = &now
	}
	e.st[ih] = s
	hooks := append([]func(torrent.Status){}, e.hooks...)
	e.mu.Unlock()
	for _, h := range hooks {
		h(s)
	}
}

type fakeIndexer struct {
	mu        sync.Mutex
	resp      indexer.SearchResponse
	requests  []indexer.SearchRequest
	downloads []string
	dl        *cardigann.DownloadResult           // what Download returns (nil: a stub metafile)
	byLink    map[string]cardigann.DownloadResult // what Download returns for a link (before dl)
	err       error                               // what Search fails with
}

func (f *fakeIndexer) Search(_ context.Context, q indexer.SearchRequest) (indexer.SearchResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, q)
	return f.resp, f.err
}
func (f *fakeIndexer) CheckReleaseLink(context.Context, int64, string) error { return nil }
func (f *fakeIndexer) Download(_ context.Context, src int64, link string) (cardigann.DownloadResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads = append(f.downloads, itoa(src)+" "+link)
	if r, ok := f.byLink[link]; ok {
		return r, nil
	}
	if f.dl != nil {
		return *f.dl, nil
	}
	return cardigann.DownloadResult{Torrent: []byte("d8:announce0:e")}, nil
}

type fakeMeta struct{}

func (fakeMeta) SearchMovie(context.Context, string, int) ([]tmdb.Result, error) {
	return []tmdb.Result{{ID: 693134, Title: "Дюна: Часть вторая", Year: 2024, Poster: "/p.jpg"}}, nil
}
func (fakeMeta) SearchTV(context.Context, string, int) ([]tmdb.Result, error) {
	return []tmdb.Result{{ID: 126308, Title: "Сёгун", Year: 2024}}, nil
}
func (fakeMeta) Movie(_ context.Context, id int) (tmdb.Details, error) {
	return tmdb.Details{ID: id, Title: "Дюна", OriginalTitle: "Dune", Year: 2024, Poster: "/dune.jpg", AltTitles: []string{"Dune: Part Two"}}, nil
}
func (fakeMeta) TV(_ context.Context, id int) (tmdb.Details, error) {
	return tmdb.Details{ID: id, Title: "Сёгун", OriginalTitle: "Shōgun", Year: 2024, Poster: "/dune.jpg", IMDbID: "tt2788316", OriginalLanguage: "en",
		Seasons: []tmdb.SeasonCount{{Number: 0, Episodes: 3}, {Number: 1, Episodes: 10}, {Number: 2, Episodes: 8}}}, nil
}
func (fakeMeta) ImageURL(size, p string) string { return "https://img/" + size + p }

type fakeLib struct {
	*coretest.Library // the rest of the core's library (its Items, Files, Audio)
	mu                sync.Mutex
	items             map[int64]library.Item // by tmdb id
	bound             map[string]string      // path -> infohash
	bindErr           error
	// albums: "artist|album" -> item, books: "kind|author|title" -> item
	albums map[string]library.Item
	books  map[string]library.Item
	// only: when set, SetFileTorrent knows only these paths (a scan that
	// indexed them); others are library.ErrNotFound
	only map[string]bool
	// claims: «Моя медиатека» owners recorded (infohash or path -> profile)
	claims map[string]int64
	// scanned (optional) hears of each Import (the core scans)
	scanned func()
}

func (l *fakeLib) ClaimRef(_ context.Context, ih string, userID int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claims == nil {
		l.claims = map[string]int64{}
	}
	l.claims[ih] = userID
	return nil
}
func (l *fakeLib) ClaimPaths(_ context.Context, paths []string, userID int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.claims == nil {
		l.claims = map[string]int64{}
	}
	for _, p := range paths {
		l.claims[p] = userID
	}
	return nil
}

func (l *fakeLib) ItemByTMDB(_ context.Context, kind string, id int64) (library.Item, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if it, ok := l.items[id]; ok && it.Kind == kind {
		return it, nil
	}
	return library.Item{}, library.ErrNotFound
}

// Import scans (scanned) and binds paths to ih: bindErr library.ErrNotFound
// (or a path outside only) is a path the scan did not pick up, another
// bindErr fails the call. An owner claims what was imported.
func (l *fakeLib) Import(_ context.Context, paths []string, ih string, owner int64) (imported, missing, taken []string, err error) {
	if l.scanned != nil {
		l.scanned() // unlocked: a test may hold the import here
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Library.Imports = append(l.Library.Imports, slices.Clone(paths))
	if l.bindErr != nil && !errors.Is(l.bindErr, library.ErrNotFound) {
		return nil, nil, nil, l.bindErr
	}
	for _, path := range paths {
		if l.bindErr != nil || l.only != nil && !l.only[path] {
			missing = append(missing, path)
			continue
		}
		if ih != "" {
			l.bound[path] = ih
		}
		if owner != 0 {
			if l.claims == nil {
				l.claims = map[string]int64{}
			}
			l.claims[path] = owner
		}
		imported = append(imported, path)
	}
	return imported, missing, nil, nil
}

type env struct {
	t     *testing.T
	root  string
	eng   *fakeEngine
	idx   *fakeIndexer
	lib   *fakeLib
	disk  *Disk
	svc   *Service
	scans int
	mb    *fakeMB
	ol    *fakeOL
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	root := t.TempDir()
	e := &env{t: t, root: root, eng: newFakeEngine(), idx: &fakeIndexer{},
		lib: &fakeLib{Library: &coretest.Library{}, items: map[int64]library.Item{}, bound: map[string]string{}}, mb: &fakeMB{}, ol: &fakeOL{}}
	e.lib.scanned = func() { e.scans++ }
	e.disk = diskAt(50)
	e.disk.Engine = e.eng
	e.svc = New(Options{
		DB: d, Engine: e.eng, Indexer: e.idx, Metadata: fakeMeta{}, Library: e.lib, Disk: e.disk,
		TorrentsDir: filepath.Join(root, "torrents"), MoviesDir: filepath.Join(root, "media/movies"),
		SeriesDir: filepath.Join(root, "media/series"), StreamCacheBytes: 1 << 30,
		MusicDir: filepath.Join(root, "media/music"), EbooksDir: filepath.Join(root, "media/books/ebooks"),
		AudiobooksDir: filepath.Join(root, "media/books/audiobooks"), ComicsDir: filepath.Join(root, "media/books/manga"),
		MusicBrainz: e.mb, OpenLibrary: e.ol, Users: testUsers,
	})
	return e
}

func relx(title, link string, src int64, score int) indexer.ScoredRelease {
	return indexer.ScoredRelease{Result: cardigann.Result{Title: title, Link: link, Seeders: 10}, SourceID: src, SourceName: "src", Score: score}
}

// --- tests ---

func TestAcquirePicksBestRelease(t *testing.T) {
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{
		relx("Дюна 2024 1080p", "https://t/best", 2, 50),
		relx("Дюна 2024 720p", "https://t/worse", 1, 10),
	}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload, UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.idx.downloads) != 1 || e.idx.downloads[0] != "2 https://t/best" {
		t.Fatalf("downloads %v", e.idx.downloads)
	}
	q := e.idx.requests[0]
	if q.Text != "Дюна" || q.Year != 2024 || q.Kind != "movie" || !contains(q.AltTitles, "Dune") || !contains(q.AltTitles, "Dune: Part Two") {
		t.Fatalf("search request %+v", q)
	}
	opt := e.eng.added[0]
	if opt.Mode != torrent.ModeDownload || opt.SaveDir != filepath.Join(e.root, "torrents", "movies") || opt.AddedBy != 1 || opt.WaitingSpace {
		t.Fatalf("add options %+v", opt)
	}
	if res.InfoHash == "" || res.Status != torrent.StatusDownloading || res.StreamURL != "" {
		t.Fatalf("result %+v", res)
	}
}

func TestAcquireNoReleases(t *testing.T) {
	e := newEnv(t)
	_, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 1, Kind: KindMovie, Mode: ModeDownload})
	if !errors.Is(err, ErrNoRelease) {
		t.Fatalf("err = %v", err)
	}
}

func TestAcquireWaitsWhenFull(t *testing.T) {
	e := newEnv(t)
	*e.disk = *diskAt(96)
	e.disk.Engine = e.eng
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024", "https://t/1", 1, 5)}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != torrent.StatusWaitingSpace || !e.eng.added[0].WaitingSpace {
		t.Fatalf("result %+v options %+v", res, e.eng.added[0])
	}

	// Force (admin) starts it anyway.
	e.eng.nextIH = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	res, err = e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == torrent.StatusWaitingSpace || e.eng.added[1].WaitingSpace {
		t.Fatalf("forced: %+v", res)
	}
}

func TestAcquireStreamPicksEpisode(t *testing.T) {
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Сёгун S01 1080p", "magnet:?xt=urn:btih:cccccccccccccccccccccccccccccccccccccccc", 1, 5)}}
	e.eng.nextIH = "cccccccccccccccccccccccccccccccccccccccc"
	e.eng.nextFiles = []torrent.FileInfo{
		{Index: 0, Path: "Shogun.S01/Shogun.S01E01.mkv", Size: 900},
		{Index: 1, Path: "Shogun.S01/Shogun.S01E03.mkv", Size: 800},
		{Index: 2, Path: "Shogun.S01/Shogun.S01E03.rus.srt", Size: 10},
	}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Episode: 3, Mode: ModeStream})
	if err != nil {
		t.Fatal(err)
	}
	if res.FileIndex != 1 || res.StreamURL != "/api/torrent-stream/cccccccccccccccccccccccccccccccccccccccc/1" {
		t.Fatalf("result %+v", res)
	}
	if len(e.idx.downloads) != 0 {
		t.Fatal("a magnet link must not be downloaded from the source")
	}
	if e.eng.added[0].Mode != torrent.ModeStream || string(e.eng.addedData[0]) != "magnet:?xt=urn:btih:cccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("added %+v", e.eng.added[0])
	}
}

func TestPromoteMovesToTorrentsDir(t *testing.T) {
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Сёгун S01E03", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "Shogun.S01E03.mkv", Size: 800}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Episode: 3, Mode: ModeStream})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Promote(context.Background(), res.InfoHash, false); err != nil {
		t.Fatal(err)
	}
	want := res.InfoHash + " download " + filepath.Join(e.root, "torrents", "series")
	if len(e.eng.modes) != 1 || e.eng.modes[0] != want {
		t.Fatalf("modes %v, want %s", e.eng.modes, want)
	}
	if _, err := e.svc.Promote(context.Background(), "ffffffffffffffffffffffffffffffffffffffff", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown torrent: %v", err)
	}
}

func TestOrganizeOnCompleteOnlyDownloadAndOnce(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.root, "torrents", "movies")
	src := writeFile(t, dir, "Dune.2024/Dune.2024.mkv", "movie")
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "Dune.2024/Dune.2024.mkv", FullPath: src, Size: 5, Progress: 1}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeStream})
	if err != nil {
		t.Fatal(err)
	}
	ih := res.InfoHash
	want := filepath.Join(e.root, "media/movies", "Дюна (2024)", "Дюна (2024).mkv")

	e.eng.complete(ih) // stream mode: nothing is organized
	e.svc.wait()
	if _, err := os.Stat(want); err == nil || e.scans != 0 {
		t.Fatal("stream torrent was organized")
	}

	if _, err := e.svc.Promote(context.Background(), ih, false); err != nil {
		t.Fatal(err)
	}
	e.eng.complete(ih)
	e.svc.wait()
	if _, err := os.Stat(want); err != nil {
		t.Fatal(err)
	}
	if e.scans != 1 || e.lib.bound[want] != ih {
		t.Fatalf("scans %d bound %v", e.scans, e.lib.bound)
	}

	e.eng.complete(ih) // e.g. a restart: no second organize
	e.svc.wait()
	if e.scans != 1 {
		t.Fatalf("organized twice: %d scans", e.scans)
	}
}

// The profile that asked for a download owns its titles once organized
// («Моя медиатека»); a download nobody asked for (the admin's torrent
// screen) is nobody's.
func TestOrganizeClaimsTheAsker(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.root, "torrents", "movies")
	src := writeFile(t, dir, "Dune.2024/Dune.2024.mkv", "movie")
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "Dune.2024/Dune.2024.mkv", FullPath: src, Size: 5, Progress: 1}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload, UserID: 7})
	if err != nil {
		t.Fatal(err)
	}
	if e.lib.claims[res.InfoHash] != 0 {
		t.Fatal("claimed before organize")
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); !a.OrganizedAt.Valid || a.AddedBy != 7 {
		t.Fatalf("not organized: %+v", a)
	}
	if got := e.lib.claims[res.InfoHash]; got != 7 {
		t.Fatalf("claims %v", e.lib.claims)
	}
}

// TestOrganizeClaims_ReusedID: a download is keyed by its asker's
// uid. Profile (3, A) asks; the core deletes it and creates (3, B) before
// the download is organized: B gets no claim on A's title («Моя
// медиатека»), is not its «Добавил», and its downloads list it not.
func TestOrganizeClaims_ReusedID(t *testing.T) {
	e := newEnv(t)
	users := &coretest.Users{Names: map[int64]string{3: "Мама"}, UIDs: map[int64]string{3: "A"}}
	e.svc.o.Users = users.Adapter()
	dir := filepath.Join(e.root, "torrents", "movies")
	src := writeFile(t, dir, "Dune.2024/Dune.2024.mkv", "movie")
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "Dune.2024/Dune.2024.mkv", FullPath: src, Size: 5, Progress: 1}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload, UserID: 3, UserUID: "A"})
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); a.AddedBy != 3 || a.AddedByUID != "A" {
		t.Fatalf("asker %+v", a)
	}
	if opt := e.eng.added[0]; opt.AddedBy != 3 || opt.AddedByUID != "A" {
		t.Fatalf("engine options %+v", opt)
	}
	named := func() string {
		for _, v := range e.svc.Torrents() {
			if v.InfoHash == res.InfoHash {
				return v.AddedByName
			}
		}
		return "?"
	}
	if n := named(); n != "Мама" {
		t.Fatalf("«Добавил» %q", n)
	}
	dls := e.svc.LibraryDownloads(context.Background())
	if len(dls) != 1 || !slices.Equal(dls[0].By, []string{"A"}) {
		t.Fatalf("downloads %+v", dls)
	}
	users.Names[3], users.UIDs[3] = "Новый", "B" // deleted, and id 3 given to a new profile
	if n := named(); n != "" {
		t.Fatalf("B named as the asker: %q", n)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); !a.OrganizedAt.Valid {
		t.Fatalf("not organized: %+v", a)
	}
	if got, ok := e.lib.claims[res.InfoHash]; ok {
		t.Fatalf("B (id %d) got A's title: %v", got, e.lib.claims)
	}
}

func TestOrganizeFailureShownAndNotLooping(t *testing.T) {
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "readme.txt", FullPath: filepath.Join(e.root, "nope.txt"), Size: 5, Progress: 1}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	for range maxOrganizeAttempts + 2 {
		e.eng.complete(res.InfoHash)
		e.svc.wait()
		e.svc.reconcile(context.Background())
		e.svc.wait()
	}
	if got := e.svc.attempts(res.InfoHash); got != maxOrganizeAttempts {
		t.Fatalf("attempts %d", got)
	}
	var found bool
	for _, tv := range e.svc.Torrents() {
		if tv.InfoHash == res.InfoHash {
			found = true
			if !strings.Contains(tv.OrganizeError, "видео") || tv.Error == "" {
				t.Fatalf("view %+v", tv)
			}
		}
	}
	if !found {
		t.Fatal("torrent lost")
	}
	if e.eng.removed[res.InfoHash] {
		t.Fatal("torrent removed after a failed organize")
	}
}

func TestDiscoverMarksLibrary(t *testing.T) {
	e := newEnv(t)
	e.lib.items[693134] = library.Item{ID: 42, Kind: library.KindMovie, TmdbID: 693134}
	cs, err := e.svc.Discover(context.Background(), "дюна")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("%+v", cs)
	}
	var movie, tv Candidate
	for _, c := range cs {
		if c.Kind == KindMovie {
			movie = c
		} else {
			tv = c
		}
	}
	if !movie.InLibrary || movie.LibraryItemID == nil || *movie.LibraryItemID != 42 || movie.Poster != "https://img/w342/p.jpg" {
		t.Fatalf("movie %+v", movie)
	}
	if tv.InLibrary || tv.LibraryItemID != nil || tv.Kind != KindSeries {
		t.Fatalf("tv %+v", tv)
	}
}

func (s *Service) wait() { s.wg.Wait() }

func (s *Service) attempts(ih string) int {
	a, _ := s.acquisition(context.Background(), ih)
	return a.Attempts
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func TestPromoteOnFullDiskWaits(t *testing.T) {
	e := newEnv(t)
	*e.disk = *diskAt(96)
	e.disk.Engine = e.eng
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "Dune.mkv", Size: 5}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeStream})
	if err != nil {
		t.Fatal(err)
	}
	st, err := e.svc.Promote(context.Background(), res.InfoHash, false)
	if err != nil {
		t.Fatal(err)
	}
	if st != torrent.StatusWaitingSpace || len(e.eng.held) != 1 || len(e.eng.modes) != 1 {
		t.Fatalf("status %s held %v modes %v", st, e.eng.held, e.eng.modes)
	}

	// «На сервер» for a torrent that is streaming goes the same way.
	e.eng.nextIH = "dddddddddddddddddddddddddddddddddddddddd"
	if _, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeStream}); err != nil {
		t.Fatal(err)
	}
	out, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != torrent.StatusWaitingSpace || len(e.eng.held) != 2 {
		t.Fatalf("download of a streaming torrent: %+v held %v", out, e.eng.held)
	}

	// Force (admin): no hold.
	e.eng.nextIH = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	r3, _ := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeStream})
	if _, err := e.svc.Promote(context.Background(), r3.InfoHash, true); err != nil || len(e.eng.held) != 2 {
		t.Fatalf("forced promote held: %v %v", e.eng.held, err)
	}
}

func TestReconcileOrganizesPausedCompleteDownload(t *testing.T) {
	e := newEnv(t)
	dir := filepath.Join(e.root, "torrents", "movies")
	src := writeFile(t, dir, "Dune.2024/Dune.2024.mkv", "movie")
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "Dune.2024/Dune.2024.mkv", FullPath: src, Size: 5, Progress: 1}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	// Finished before a restart (no hook now), and the admin paused it.
	now := time.Now()
	e.eng.mu.Lock()
	st := e.eng.st[res.InfoHash]
	st.Status, st.CompletedAt = torrent.StatusPaused, &now
	e.eng.st[res.InfoHash] = st
	e.eng.mu.Unlock()
	e.svc.reconcile(context.Background())
	e.svc.wait()
	if _, err := os.Stat(filepath.Join(e.root, "media/movies", "Дюна (2024)", "Дюна (2024).mkv")); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.RetryOrganize(context.Background(), res.InfoHash); err != nil {
		t.Fatalf("retry of a paused complete torrent: %v", err)
	}
	e.svc.wait()
}

func TestOrganizeBindFailureIsAFailure(t *testing.T) {
	e := newEnv(t)
	e.lib.bindErr = library.ErrNotFound // the scanner did not pick the file up
	dir := filepath.Join(e.root, "torrents", "movies")
	src := writeFile(t, dir, "Dune.2024.mkv", "movie")
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "Dune.2024.mkv", FullPath: src, Size: 5, Progress: 1}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	a, _ := e.svc.acquisition(context.Background(), res.InfoHash)
	if a.OrganizedAt.Valid || a.Attempts != 1 || !strings.Contains(a.Error, "не появился в медиатеке") {
		t.Fatalf("%+v", a)
	}
	// Fixed (next scan finds it): the retry binds without duplicating the file.
	e.lib.mu.Lock()
	e.lib.bindErr = nil
	e.lib.mu.Unlock()
	e.svc.reconcile(context.Background())
	e.svc.wait()
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); !a.OrganizedAt.Valid {
		t.Fatalf("not organized on retry: %+v", a)
	}
	es, _ := os.ReadDir(filepath.Join(e.root, "media/movies", "Дюна (2024)"))
	if len(es) != 2 { // video + nfo
		t.Fatalf("%d entries", len(es))
	}
}

func TestPickFileNoWrongEpisode(t *testing.T) {
	files := []torrent.FileInfo{
		{Index: 0, Path: "S01/Show.S01E01.mkv", Size: 900},
		{Index: 1, Path: "S01/Show.S01E02.mkv", Size: 800},
	}
	if i := PickFile(files, 1, 5); i != -1 {
		t.Fatalf("picked %d for a missing episode", i)
	}
	if i := PickFile(files, 1, 2); i != 1 {
		t.Fatalf("picked %d", i)
	}
	if i := PickFile(files[:1], 1, 5); i != 0 {
		t.Fatalf("single-file torrent: %d", i)
	}
}

func TestRemoveRefusedWhileOrganizing(t *testing.T) {
	e := newEnv(t)
	e.eng.put(torrent.Status{InfoHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Mode: torrent.ModeDownload})
	e.svc.mu.Lock()
	e.svc.organizing["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"] = true
	e.svc.mu.Unlock()
	if err := e.svc.Remove(context.Background(), "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}

// «Сезон N на сервер»: a series is downloaded one season at a time; the
// search asks for that season (no episode) and the acquisition remembers it.
func TestAcquireSeriesDownloadIsPerSeason(t *testing.T) {
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Сёгун S02 1080p", "https://t/s2", 1, 50)}}
	_, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 126308, Kind: KindSeries, Mode: ModeDownload})
	if !errors.Is(err, ErrInvalid) || len(e.idx.requests) != 0 {
		t.Fatalf("whole series without a season: err=%v requests=%v", err, e.idx.requests)
	}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 2, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	q := e.idx.requests[0]
	if q.Kind != KindSeries || q.Season != 2 || q.Episode != 0 {
		t.Fatalf("search request %+v", q)
	}
	a, err := e.svc.acquisition(context.Background(), res.InfoHash)
	if err != nil || a.Season != 2 || a.Episode != 0 {
		t.Fatalf("acquisition %+v %v", a, err)
	}
	if e.eng.added[0].SaveDir != filepath.Join(e.root, "torrents", "series") {
		t.Fatalf("save dir %s", e.eng.added[0].SaveDir)
	}
	// a pack chosen by hand (admin release table) needs no season
	e.eng.nextIH = "dddddddddddddddddddddddddddddddddddddddd"
	if _, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 126308, Kind: KindSeries, Mode: ModeDownload,
		ReleaseLink: "https://t/all", SourceID: 1}); err != nil {
		t.Fatalf("hand-picked pack: %v", err)
	}
}

func TestTitleListsSeasons(t *testing.T) {
	e := newEnv(t)
	ti, err := e.svc.Title(context.Background(), KindSeries, 126308)
	if err != nil {
		t.Fatal(err)
	}
	if len(ti.Seasons) != 2 || ti.Seasons[0] != (SeasonInfo{1, 10}) || ti.Seasons[1] != (SeasonInfo{2, 8}) {
		t.Fatalf("seasons %+v", ti.Seasons)
	}
	if ti.Cast == nil { // «cast»: [] for the page, never null
		t.Fatal("cast is nil")
	}
	if ti.ImdbID != "tt2788316" { // the page asks /api/ratings by it
		t.Fatalf("imdbId %q", ti.ImdbID)
	}
}

// --- album track selection (AcquireRequest.Files) ---

// albumPick is the confirm of «Выбрать релиз…» for groupaKrovi's release.
func albumPick(files ...int) AcquireRequest {
	return AcquireRequest{Kind: KindMusic, Title: "Группа крови", Artist: "Кино", Year: 1988, Mode: ModeDownload,
		ReleaseLink: groupaKrovi.ReleaseLink, SourceID: groupaKrovi.SourceID, UserID: 1, Files: files}
}

// selectionTorrent makes the source return a metafile named name with
// paths, and the fake engine add it with those files on disk.
func selectionTorrent(t *testing.T, e *env, name string, paths ...string) []byte {
	t.Helper()
	mf := testMetafile(t, name, paths...)
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	e.eng.nextIH = metafileIH(t, mf)
	rels := make([]string, len(paths))
	for i, p := range paths {
		rels[i] = name + "/" + p
	}
	e.eng.nextFiles = torrentFiles(t, filepath.Join(e.root, "torrents", "music"), rels...)
	return mf
}

func TestAcquireWithFilesPassesSelection(t *testing.T) {
	e := newEnv(t)
	const name = "Кино - Группа крови (1988)"
	mf := selectionTorrent(t, e, name, "01.flac", "02.flac", "03.flac", "04.flac", "cover.jpg")
	previewFiles(t, e, groupaKrovi)
	res, err := e.svc.Acquire(context.Background(), albumPick(0, 2, 3))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.eng.added) != 1 || !slices.Equal(e.eng.added[0].Files, []int{0, 2, 3}) || !bytes.Equal(e.eng.addedData[0], mf) {
		t.Fatalf("added %+v", e.eng.added)
	}
	if opt := e.eng.added[0]; opt.Mode != ModeDownload || opt.SaveDir != filepath.Join(e.root, "torrents", "music") {
		t.Fatalf("options %+v", opt)
	}
	if len(e.idx.downloads) != 1 || e.eng.fetches != 0 {
		t.Fatalf("downloads %v, fetches %d", e.idx.downloads, e.eng.fetches)
	}
	a, err := e.svc.acquisition(context.Background(), res.InfoHash)
	if err != nil || !a.Meta.Picked || a.Meta.RequestedRoot != name || a.Meta.Artist != "Кино" {
		t.Fatalf("meta %+v %v", a.Meta, err)
	}
}

func TestAcquireFilesPickedOnlyWhenChanged(t *testing.T) {
	cases := []struct {
		name, torrent string
		paths         []string
		files         []int
		picked        bool
		root          string
	}{
		// the junk ._01.flac is not in the preview, so it is not sent
		{"untouched single album", "Кино - Группа крови (1988)", []string{"01.flac", "02.flac", "cover.jpg", "._01.flac"}, []int{0, 1, 2}, false, "Кино - Группа крови (1988)"},
		{"single album, a track unchecked", "Кино - Группа крови (1988)", []string{"01.flac", "02.flac", "cover.jpg"}, []int{0, 2}, true, "Кино - Группа крови (1988)"},
		{"single album, the cover unchecked", "Кино - Группа крови (1988)", []string{"01.flac", "02.flac", "cover.jpg"}, []int{0, 1}, true, "Кино - Группа крови (1988)"},
		// the only album is the requested one, whatever its folder is called
		{"untouched, folder not matching", "Kino - Gruppa krovi [FLAC]", []string{"01.flac", "02.flac"}, []int{0, 1}, false, "Kino - Gruppa krovi [FLAC]"},
		{"whole discography", "Кино - Дискография", []string{"1988 - Группа крови/01.flac", "1989 - Звезда по имени Солнце/01.flac"}, []int{0, 1}, true, "Кино - Дискография/1988 - Группа крови"},
		{"one album of a discography", "Кино - Дискография", []string{"1988 - Группа крови/01.flac", "1989 - Звезда по имени Солнце/01.flac"}, []int{0}, true, "Кино - Дискография/1988 - Группа крови"},
		{"discography without the requested album", "Кино - Дискография", []string{"1986 - Ночь/01.flac", "1989 - Звезда по имени Солнце/01.flac"}, []int{0, 1}, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			selectionTorrent(t, e, c.torrent, c.paths...)
			previewFiles(t, e, groupaKrovi)
			res, err := e.svc.Acquire(context.Background(), albumPick(c.files...))
			if err != nil {
				t.Fatal(err)
			}
			a, err := e.svc.acquisition(context.Background(), res.InfoHash)
			if err != nil || a.Meta.Picked != c.picked || a.Meta.RequestedRoot != c.root {
				t.Fatalf("meta %+v %v", a.Meta, err)
			}
		})
	}
}

func TestAcquireFilesCacheMiss(t *testing.T) {
	e := newEnv(t)
	mf := testMetafile(t, "Кино - Группа крови (1988)", "01.flac", "02.flac")
	ih := metafileIH(t, mf)
	e.idx.dl = &cardigann.DownloadResult{Magnet: "magnet:?xt=urn:btih:" + ih}
	_, err := e.svc.Acquire(context.Background(), albumPick(0))
	if msg, _ := UserMessage(err); !errors.Is(err, ErrNoFiles) || msg != msgFileListTimeout {
		t.Fatalf("err %v", err)
	}
	if len(e.idx.downloads) != 1 || e.eng.fetches != 1 || len(e.eng.added) != 0 {
		t.Fatalf("downloads %v, fetches %d, added %d", e.idx.downloads, e.eng.fetches, len(e.eng.added))
	}
	var deadline time.Time
	e.eng.fetchInfo = func(ctx context.Context, _ string) ([]byte, error) {
		deadline, _ = ctx.Deadline()
		return mf, nil
	}
	e.eng.nextIH = ih
	if _, err := e.svc.Acquire(context.Background(), albumPick(0)); err != nil {
		t.Fatal(err)
	}
	if len(e.idx.downloads) != 2 || e.eng.fetches != 2 || len(e.eng.added) != 1 || !bytes.Equal(e.eng.addedData[0], mf) || !slices.Equal(e.eng.added[0].Files, []int{0}) {
		t.Fatalf("downloads %v, fetches %d, added %+v", e.idx.downloads, e.eng.fetches, e.eng.added)
	}
	if left := time.Until(deadline); deadline.IsZero() || left > 40*time.Second {
		t.Fatalf("fetch deadline in %v", left)
	}
}

func TestAcquireFilesValidation(t *testing.T) {
	e := newEnv(t)
	// a BEP-47 padding file at index 2
	info := metainfo.Info{Name: "Кино - Группа крови (1988)", PieceLength: 16384, Pieces: make([]byte, 20), Files: []metainfo.FileInfo{
		{Length: 1000, Path: []string{"01.flac"}}, {Length: 1000, Path: []string{"02.flac"}},
		{Length: 100, Path: []string{".pad", "100"}, Attr: "p"}, {Length: 1000, Path: []string{"cover.jpg"}},
	}}
	ib, _ := bencode.Marshal(info)
	mf, _ := bencode.Marshal(metainfo.MetaInfo{InfoBytes: ib})
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	e.eng.nextIH = metafileIH(t, mf)
	previewFiles(t, e, groupaKrovi)
	for _, c := range []struct {
		name string
		req  AcquireRequest
		msg  string
	}{
		{"empty", func() AcquireRequest { r := albumPick(); r.Files = []int{}; return r }(), "Выберите хотя бы одну песню"},
		{"only the cover", albumPick(3), "Выберите хотя бы одну песню"},
		{"out of range", albumPick(99), "Неверный список файлов"},
		{"negative", albumPick(0, -1), "Неверный список файлов"},
		{"padding", albumPick(0, 2), "Неверный список файлов"},
		{"movie", AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload, ReleaseLink: "https://t/1", SourceID: 3, Files: []int{0}}, "Выбор песен доступен только для альбома"},
		{"book", AcquireRequest{Kind: KindBook, Title: "Солярис", Artist: "Лем", Mode: ModeDownload, ReleaseLink: "https://t/1", SourceID: 3, Files: []int{0}}, "Выбор песен доступен только для альбома"},
		{"no release link", func() AcquireRequest { r := albumPick(0); r.ReleaseLink = ""; return r }(), "Выбор песен доступен только для альбома"},
		{"direct", func() AcquireRequest { r := albumPick(0); r.Direct = true; return r }(), "Выбор песен доступен только для альбома"},
		{"stream", func() AcquireRequest { r := albumPick(0); r.Mode = ModeStream; return r }(), "Выбор песен доступен только для альбома"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := e.svc.Acquire(context.Background(), c.req)
			if msg, _ := UserMessage(err); !errors.Is(err, ErrInvalid) || msg != c.msg {
				t.Fatalf("err %v", err)
			}
			if len(e.eng.added) != 0 {
				t.Fatalf("added %+v", e.eng.added)
			}
		})
	}
	// no files: today's add (the whole torrent)
	if _, err := e.svc.Acquire(context.Background(), albumPick(nil...)); err != nil {
		t.Fatal(err)
	}
	if len(e.eng.added) != 1 || e.eng.added[0].Files != nil {
		t.Fatalf("added %+v", e.eng.added)
	}
	a, _ := e.svc.acquisition(context.Background(), e.eng.nextIH)
	if a.Meta.Picked || a.Meta.RequestedRoot != "" || !a.Meta.HandPicked {
		t.Fatalf("meta %+v", a.Meta)
	}
}

func TestAcquireFilesKnownTorrent409(t *testing.T) {
	e := newEnv(t)
	mf := testMetafile(t, "Кино - Группа крови (1988)", "01.flac", "02.flac")
	ih := metafileIH(t, mf)
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	running := torrent.Status{InfoHash: ih, Mode: ModeDownload, Status: torrent.StatusDownloading, Name: "Кино"}
	e.eng.put(running)
	const want = "Эта раздача уже добавлена. Чтобы выбрать другие песни, удалите её в «Загрузках» и добавьте снова."
	check := func(req AcquireRequest) {
		t.Helper()
		_, err := e.svc.Acquire(context.Background(), req)
		if msg, _ := UserMessage(err); !errors.Is(err, ErrConflict) || msg != want {
			t.Fatalf("err %v", err)
		}
		if e.eng.fetches != 0 || len(e.eng.added) != 0 {
			t.Fatalf("fetches %d, added %+v", e.eng.fetches, e.eng.added)
		}
		if st := e.eng.st[ih]; st != running {
			t.Fatalf("running torrent changed: %+v", st)
		}
		if _, err := e.svc.acquisition(context.Background(), ih); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("acquisition saved: %v", err)
		}
	}
	check(albumPick(0)) // the source's .torrent
	for _, link := range []string{"magnet:?xt=urn:btih:" + ih, "magnet:?xt=urn:btih:" + strings.ToUpper(ih)} {
		req := albumPick(0)
		req.ReleaseLink = link
		n := len(e.idx.downloads)
		check(req)
		if len(e.idx.downloads) != n {
			t.Fatalf("magnet downloaded from the source: %v", e.idx.downloads)
		}
	}
}

func TestAcquireFilesStaleRowReplaced(t *testing.T) {
	e := newEnv(t)
	selectionTorrent(t, e, "Кино - Группа крови (1988)", "01.flac", "02.flac", "03.flac")
	ih := e.eng.nextIH
	// a row left by an earlier add of the whole torrent, already organized
	stale := acquisition{InfoHash: ih, Kind: KindMusic, Title: "Звезда по имени Солнце", Year: 1989, ExtID: "old", Meta: acquisitionMeta{Artist: "Кино"}}
	if err := e.svc.saveAcquisition(context.Background(), stale, 1, "uid-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.o.DB.Exec(`UPDATE acquisitions SET organized_at='2026-01-01T00:00:00Z', organize_error='x', organize_attempts=3 WHERE infohash=?`, ih); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Acquire(context.Background(), albumPick(0, 2)); err != nil {
		t.Fatal(err)
	}
	a, err := e.svc.acquisition(context.Background(), ih)
	if err != nil || !a.Meta.Picked || a.Meta.RequestedRoot != "Кино - Группа крови (1988)" || a.OrganizedAt.Valid || a.Error != "" || a.Attempts != 0 {
		t.Fatalf("row %+v %v", a, err)
	}
	// the row names the new request, not the stale one
	if a.Title != "Группа крови" || a.Year != 1988 || a.ExtID == "old" {
		t.Fatalf("stale title kept: %q %d %q", a.Title, a.Year, a.ExtID)
	}
}

func TestOrganizeSelectedOnly(t *testing.T) {
	e := newEnv(t)
	selectionTorrent(t, e, "Кино - Группа крови (1988)", "01.flac", "02.flac", "03.flac", "04.flac", "05.flac", "cover.jpg")
	// file 1 is a sparse partial file holding a boundary piece
	if err := os.Truncate(e.eng.nextFiles[1].FullPath, 1<<20); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Acquire(context.Background(), albumPick(0, 2, 5))
	if err != nil {
		t.Fatal(err)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); !a.OrganizedAt.Valid || a.Error != "" {
		t.Fatalf("not organized: %+v", a)
	}
	var placed []string
	base := filepath.Join(e.root, "media", "music")
	filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			r, _ := filepath.Rel(base, p)
			placed = append(placed, filepath.ToSlash(r))
		}
		return nil
	})
	slices.Sort(placed)
	want := []string{"Кино/Группа крови (1988)/01.flac", "Кино/Группа крови (1988)/03.flac", "Кино/Группа крови (1988)/cover.jpg"}
	if !slices.Equal(placed, want) {
		t.Fatalf("placed %q, want %q", placed, want)
	}
}

// A file left out of a partial download is not streamable: a reader would
// download and seed it.
func TestOpenStreamRefusesUnselectedFile(t *testing.T) {
	e := newEnv(t)
	const ih = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	h := e.eng.put(torrent.Status{InfoHash: ih, Mode: ModeDownload, Status: torrent.StatusDownloading})
	h.files = []torrent.FileInfo{{Index: 0, Path: "a/01.flac", Size: 4}, {Index: 1, Path: "a/02.flac", Size: 4}}
	h.selection = []int{0}
	h.data[0], h.data[1] = []byte("one!"), []byte("two!")
	if _, _, _, err := e.svc.OpenStream(context.Background(), ih, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unselected: %v", err)
	}
	fi, open, release, err := e.svc.OpenStream(context.Background(), ih, 0)
	if err != nil || fi.Index != 0 {
		t.Fatalf("selected: %+v %v", fi, err)
	}
	defer release()
	r, err := open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}

// TitleProgress sums the download progress of one title's torrents for the
// download button's ring.
func TestTitleProgress(t *testing.T) {
	type tor struct {
		ih, kind, mode, status string
		tmdb, season           int
		done, size             int64
		organized              bool
		age                    int // minutes ago it was added
	}
	cases := []struct {
		name         string
		tors         []tor
		kind         string
		tmdb, season int
		want         TitleProgress
		ok           bool
	}{
		{"downloading movie", []tor{{"a", "movie", torrent.ModeDownload, torrent.StatusDownloading, 7, 0, 40, 100, false, 1}},
			"movie", 7, 0, TitleProgress{Status: "active", Done: 40, Size: 100}, true},
		{"stream mode only", []tor{{"a", "movie", torrent.ModeStream, torrent.StatusDownloading, 7, 0, 40, 100, false, 1}},
			"movie", 7, 0, TitleProgress{}, false},
		{"another title", []tor{{"a", "movie", torrent.ModeDownload, torrent.StatusDownloading, 8, 0, 40, 100, false, 1}},
			"movie", 7, 0, TitleProgress{}, false},
		{"another kind", []tor{{"a", "series", torrent.ModeDownload, torrent.StatusDownloading, 7, 1, 40, 100, false, 1}},
			"movie", 7, 0, TitleProgress{}, false},
		{"season 1", []tor{
			{"a", "series", torrent.ModeDownload, torrent.StatusDownloading, 5, 1, 10, 50, false, 2},
			{"b", "series", torrent.ModeDownload, torrent.StatusQueued, 5, 2, 20, 50, false, 1}},
			"series", 5, 1, TitleProgress{Status: "active", Done: 10, Size: 50}, true},
		{"whole series sums seasons", []tor{
			{"a", "series", torrent.ModeDownload, torrent.StatusDownloading, 5, 1, 10, 50, false, 2},
			{"b", "series", torrent.ModeDownload, torrent.StatusWaitingSpace, 5, 2, 20, 50, false, 1}},
			"series", 5, 0, TitleProgress{Status: "active", Done: 30, Size: 100}, true},
		{"season without a torrent", []tor{
			{"a", "series", torrent.ModeDownload, torrent.StatusDownloading, 5, 1, 10, 50, false, 2},
			{"b", "series", torrent.ModeDownload, torrent.StatusDownloading, 5, 2, 20, 50, false, 1}},
			"series", 5, 3, TitleProgress{}, false},
		{"a pack is not a season's (its cancel would not stop it)", []tor{
			{"a", "series", torrent.ModeDownload, torrent.StatusDownloading, 5, 0, 15, 60, false, 1}},
			"series", 5, 2, TitleProgress{}, false},
		{"a pack counts for the whole series", []tor{
			{"a", "series", torrent.ModeDownload, torrent.StatusDownloading, 5, 0, 15, 60, false, 1}},
			"series", 5, 0, TitleProgress{Status: "active", Done: 15, Size: 60}, true},
		{"paused only", []tor{{"a", "movie", torrent.ModeDownload, torrent.StatusPaused, 7, 0, 30, 100, false, 1}},
			"movie", 7, 0, TitleProgress{Status: "paused", Done: 30, Size: 100}, true},
		{"active wins over paused", []tor{
			{"a", "series", torrent.ModeDownload, torrent.StatusPaused, 5, 1, 30, 100, false, 2},
			{"b", "series", torrent.ModeDownload, torrent.StatusDownloading, 5, 2, 5, 100, false, 1}},
			"series", 5, 0, TitleProgress{Status: "active", Done: 5, Size: 100}, true},
		{"seeding only", []tor{{"a", "movie", torrent.ModeDownload, torrent.StatusSeeding, 7, 0, 100, 100, false, 1}},
			"movie", 7, 0, TitleProgress{Status: "done", Done: 100, Size: 100}, true},
		{"organized", []tor{{"a", "movie", torrent.ModeDownload, torrent.StatusError, 7, 0, 100, 100, true, 1}},
			"movie", 7, 0, TitleProgress{Status: "done", Done: 100, Size: 100}, true},
		{"error only", []tor{{"a", "movie", torrent.ModeDownload, torrent.StatusError, 7, 0, 40, 100, false, 1}},
			"movie", 7, 0, TitleProgress{}, false},
		{"a newer failed retry hides an older done one", []tor{
			{"a", "movie", torrent.ModeDownload, torrent.StatusSeeding, 7, 0, 100, 100, false, 5},
			{"b", "movie", torrent.ModeDownload, torrent.StatusError, 7, 0, 0, 100, false, 1}},
			"movie", 7, 0, TitleProgress{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			for _, x := range c.tors {
				e.eng.put(torrent.Status{InfoHash: x.ih, Mode: x.mode, Status: x.status, Done: x.done, Size: x.size,
					AddedAt: time.Now().Add(-time.Duration(x.age) * time.Minute)})
				org := any(nil)
				if x.organized {
					org = "2024-01-01T00:00:00Z"
				}
				if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, organized_at) VALUES (?,?,?,?,?,?)`,
					x.ih, x.kind, x.tmdb, "T", x.season, org); err != nil {
					t.Fatal(err)
				}
			}
			got, ok := e.svc.TitleProgress(context.Background(), c.kind, c.tmdb, c.season)
			if ok != c.ok || got != c.want {
				t.Fatalf("got %+v %v, want %+v %v", got, ok, c.want, c.ok)
			}
		})
	}
}

// CancelTitle removes a title's unfinished downloads with their data and
// leaves finished, organized, stream, other-title, other-season and kept
// ones; a whole-series pack goes only with the whole title (season 0).
func TestCancelTitle(t *testing.T) {
	e := newEnv(t)
	tors := []struct {
		ih, kind, mode, status string
		tmdb, season           int
		organized              bool
	}{
		{"a", "series", torrent.ModeDownload, torrent.StatusDownloading, 5, 1, false},
		{"b", "series", torrent.ModeDownload, torrent.StatusPaused, 5, 0, false},       // a pack: not for season 1 alone
		{"c", "series", torrent.ModeDownload, torrent.StatusDownloading, 5, 2, false},  // another season
		{"d", "series", torrent.ModeDownload, torrent.StatusSeeding, 5, 1, false},      // finished
		{"e", "series", torrent.ModeDownload, torrent.StatusPaused, 5, 1, true},        // organized
		{"f", "series", torrent.ModeStream, torrent.StatusDownloading, 5, 1, false},    // «Смотреть сейчас»
		{"g", "series", torrent.ModeDownload, torrent.StatusQueued, 6, 1, false},       // another title
		{"h", "series", torrent.ModeDownload, torrent.StatusWaitingSpace, 5, 1, false}, // kept
	}
	for _, x := range tors {
		e.eng.put(torrent.Status{InfoHash: x.ih, Mode: x.mode, Status: x.status, Size: 100})
		org := any(nil)
		if x.organized {
			org = "2024-01-01T00:00:00Z"
		}
		if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season, organized_at) VALUES (?,?,?,?,?,?)`,
			x.ih, x.kind, x.tmdb, "T", x.season, org); err != nil {
			t.Fatal(err)
		}
	}
	n, err := e.svc.CancelTitle(context.Background(), "series", 5, 1, "H")
	if err != nil || n != 1 {
		t.Fatalf("cancelled %d, %v; want 1", n, err)
	}
	if !e.eng.removed["a"] || !e.eng.removedData["a"] {
		t.Errorf("a: not removed with its data")
	}
	for _, ih := range []string{"b", "c", "d", "e", "f", "g", "h"} {
		if e.eng.removed[ih] {
			t.Errorf("%s: removed", ih)
		}
	}
	// the whole title: the pack and the other season too
	n, err = e.svc.CancelTitle(context.Background(), "series", 5, 0, "H")
	if err != nil || n != 2 {
		t.Fatalf("cancelled %d, %v; want 2", n, err)
	}
	for _, ih := range []string{"b", "c"} {
		if !e.eng.removed[ih] || !e.eng.removedData[ih] {
			t.Errorf("%s: not removed with its data", ih)
		}
	}
	for _, ih := range []string{"d", "e", "f", "g", "h"} {
		if e.eng.removed[ih] {
			t.Errorf("%s: removed", ih)
		}
	}
}
