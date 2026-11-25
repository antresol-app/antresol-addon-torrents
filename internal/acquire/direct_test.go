package acquire

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/events"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// directEnv is a Service over a real indexer.Manager with one OPDS source
// (a fake catalog) and the core's library (in memory).
type directEnv struct {
	root, ebooks, comics string
	svc                  *Service
	src                  indexer.Source
	srv                  *httptest.Server
	repo                 *coretest.Library
	eng                  *fakeEngine
	bus                  *events.Bus
}

func cbzBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, err := z.Create("01.jpg")
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("\xff\xd8\xff\xe0 not really a picture"))
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// fakeCatalog answers book searches with an epub (+fb2) entry and comic
// searches with a cbz entry; /c/1.cbz is also reachable by hand.
func fakeCatalog(t *testing.T) *httptest.Server {
	cbz := cbzBytes(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/opds", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>К</title>
<link rel="search" type="application/atom+xml" href="/opds/search?q={searchTerms}"/></feed>`)
	})
	mux.HandleFunc("/opds/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if strings.Contains(q, "Берсерк") {
			fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>П</title>
<entry><title>Берсерк</title><id>c1</id>
<link rel="http://opds-spec.org/acquisition" type="application/vnd.comicbook+zip" href="/c/1.cbz"/></entry></feed>`)
			return
		}
		fmt.Fprint(w, `<feed xmlns="http://www.w3.org/2005/Atom"><title>П</title>
<entry><title>Мастер и Маргарита</title><id>b1</id><author><name>Михаил Булгаков</name></author>
<link rel="http://opds-spec.org/acquisition" type="application/fb2+zip" href="/b/1/fb2"/>
<link rel="http://opds-spec.org/acquisition" type="application/epub+zip" href="/b/1/epub"/></entry></feed>`)
	})
	mux.HandleFunc("/b/1/epub", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/epub+zip")
		w.Header().Set("Content-Disposition", `attachment; filename="Bulgakov_Master.epub"`)
		fmt.Fprint(w, newEPUB)
	})
	mux.HandleFunc("/b/expired.epub", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/epub+zip")
		fmt.Fprint(w, "link expired")
	})
	mux.HandleFunc("/b/slow.epub", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/epub+zip")
		w.WriteHeader(200)
		fmt.Fprint(w, "first part")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // the rest never comes
	})
	mux.HandleFunc("/c/1.cbz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.comicbook+zip")
		w.Write(cbz)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newEPUB is the catalog's book: a ZIP signature, as an EPUB starts.
const newEPUB = "PK\x03\x04BOOK"

func newDirectEnv(t *testing.T, diskPercent int64) *directEnv {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	root := t.TempDir()
	e := &directEnv{root: root, ebooks: filepath.Join(root, "media/books/ebooks"), comics: filepath.Join(root, "media/books/manga"),
		srv: fakeCatalog(t), repo: &coretest.Library{}, eng: newFakeEngine(), bus: &events.Bus{}}
	m := indexer.NewManager(d, indexer.NewCatalog(""), indexer.Options{})
	e.src, err = m.Add(context.Background(), indexer.OPDSDefinitionID, map[string]string{"url": e.srv.URL + "/opds", "allow_local": "true"})
	if err != nil || e.src.Status != indexer.StatusOK {
		t.Fatalf("source %+v %v", e.src, err)
	}
	movies, series := filepath.Join(root, "media/movies"), filepath.Join(root, "media/series")
	for _, dir := range []string{movies, series, e.ebooks, e.comics} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	disk := diskAt(diskPercent)
	disk.Engine = e.eng
	e.svc = New(Options{
		DB: d, Engine: e.eng, Indexer: m, Library: e.repo, Disk: disk, Bus: e.bus, Users: testUsers,
		TorrentsDir: filepath.Join(root, "torrents"), MoviesDir: movies, SeriesDir: series,
		EbooksDir: e.ebooks, ComicsDir: e.comics,
		MusicDir: filepath.Join(root, "media/music"), AudiobooksDir: filepath.Join(root, "media/books/audiobooks"),
	})
	t.Cleanup(e.svc.Close) // before the database closes
	return e
}

func (e *directEnv) noTorrentsNoRows(t *testing.T, d *Service) {
	t.Helper()
	if len(e.eng.added) != 0 {
		t.Fatalf("a torrent was added: %+v", e.eng.added)
	}
	var n int
	if err := d.o.DB.QueryRow(`SELECT COUNT(*) FROM acquisitions`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("acquisitions rows %d %v", n, err)
	}
	left, _ := os.ReadDir(filepath.Join(e.root, "torrents", ".direct"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// waitDirect waits for the direct download id to finish and returns its
// listing.
func waitDirect(t *testing.T, s *Service, id string) TorrentView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range s.Torrents() {
			if v.InfoHash == id && v.Status.Status != torrent.StatusDownloading && v.Status.Status != torrent.StatusQueued {
				return v
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("direct download %s did not finish: %+v", id, s.Torrents())
	return TorrentView{}
}

func TestAcquireDirectBook(t *testing.T) {
	e := newDirectEnv(t, 50)
	ch, cancel := e.bus.Subscribe()
	defer cancel()
	// the request goes away right after the answer (the tab is closed):
	// the download and the scan go on
	ctx, stop := context.WithCancel(context.Background())
	res, err := e.svc.Acquire(ctx, AcquireRequest{Kind: KindBook, Title: "Мастер и Маргарита", Artist: "Михаил Булгаков", Mode: ModeDownload, UserID: 1})
	stop()
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != torrent.StatusDownloading || !IsDirect(res.InfoHash) {
		t.Fatalf("result %+v", res)
	}
	v := waitDirect(t, e.svc, res.InfoHash)
	if v.Status.Status != DirectDone || !v.Direct || !v.Organized || v.Kind != KindBook || v.Title != "Мастер и Маргарита" || v.Done != 8 || v.Error != "" {
		t.Fatalf("listing %+v", v)
	}
	path := filepath.Join(e.ebooks, "Михаил Булгаков", "Мастер и Маргарита.epub")
	if b, err := os.ReadFile(path); err != nil || string(b) != newEPUB {
		t.Fatalf("file %q %v", b, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}
	if !library.IsBookFile(path) || !slices.EqualFunc(e.repo.Imports, [][]string{{path}}, slices.Equal) || !slices.Equal(e.repo.Owners[1], []string{path}) {
		t.Fatalf("after scan: imports %q owners %v", e.repo.Imports, e.repo.Owners)
	}
	e.noTorrentsNoRows(t, e.svc)
	got := false
	for len(ch) > 0 {
		got = got || (<-ch).Type == events.LibraryUpdated
	}
	if !got {
		t.Fatal("no library.updated event")
	}
}

func TestAcquireDirectNoOverwrite(t *testing.T) {
	e := newDirectEnv(t, 50)
	dir := filepath.Join(e.ebooks, "Михаил Булгаков")
	os.MkdirAll(dir, 0o755)
	old := filepath.Join(dir, "Мастер и Маргарита.epub")
	if err := os.WriteFile(old, []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Мастер и Маргарита", Artist: "Михаил Булгаков", Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	waitDirect(t, e.svc, res.InfoHash)
	if b, _ := os.ReadFile(old); string(b) != "OLD" {
		t.Fatalf("overwritten: %q", b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "Мастер и Маргарита (2).epub")); err != nil || string(b) != newEPUB {
		t.Fatalf("second copy %q %v", b, err)
	}
}

// TestAcquireDirectAcrossFileSystems: the downloads volume is not the media
// one (rename fails with EXDEV): the book is copied into place.
func TestAcquireDirectAcrossFileSystems(t *testing.T) {
	e := newDirectEnv(t, 50)
	renameFile = func(src, dst string) error {
		if strings.HasPrefix(dst, e.ebooks) && !strings.HasPrefix(src, e.ebooks) {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EXDEV}
		}
		return os.Rename(src, dst)
	}
	t.Cleanup(func() { renameFile = os.Rename })
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Мастер и Маргарита", Artist: "Михаил Булгаков", Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	if v := waitDirect(t, e.svc, res.InfoHash); v.Status.Status != DirectDone || v.Error != "" {
		t.Fatalf("listing %+v", v)
	}
	path := filepath.Join(e.ebooks, "Михаил Булгаков", "Мастер и Маргарита.epub")
	if b, err := os.ReadFile(path); err != nil || string(b) != newEPUB {
		t.Fatalf("file %q %v", b, err)
	}
	left, _ := filepath.Glob(filepath.Join(e.root, "torrents", ".direct", "*"))
	staged, _ := filepath.Glob(filepath.Join(e.ebooks, "Михаил Булгаков", ".*"))
	if len(left)+len(staged) != 0 {
		t.Fatalf("temporary files left: %v %v", left, staged)
	}
}

// A file whose first bytes are not of its format (a file host's error
// message saved as .epub) does not enter the library.
func TestAcquireDirectRefusesWrongContent(t *testing.T) {
	e := newDirectEnv(t, 50)
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Мастер и Маргарита", Artist: "Михаил Булгаков", Mode: ModeDownload,
		ReleaseLink: e.srv.URL + "/b/expired.epub", SourceID: e.src.ID, Direct: true})
	if err != nil {
		t.Fatal(err)
	}
	v := waitDirect(t, e.svc, res.InfoHash)
	if v.Status.Status == DirectDone || !strings.Contains(v.Error, "не EPUB") {
		t.Fatalf("listing %+v", v)
	}
	if got, _ := filepath.Glob(filepath.Join(e.ebooks, "*", "*")); len(got) != 0 {
		t.Fatalf("placed: %v", got)
	}
}

// New sweeps copies a crash cut off ("*.vault-partial") from the library.
func TestNewRemovesStalePartials(t *testing.T) {
	e := newDirectEnv(t, 50)
	dir := filepath.Join(e.ebooks, "Автор")
	os.MkdirAll(dir, 0o755)
	stale := filepath.Join(dir, ".direct-123.vault-partial")
	book := filepath.Join(dir, "Книга.epub")
	os.WriteFile(stale, []byte("half"), 0o644)
	os.WriteFile(book, []byte(newEPUB), 0o644)
	cleanPartials(e.svc.log, e.ebooks)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale partial kept: %v", err)
	}
	if _, err := os.Stat(book); err != nil {
		t.Fatalf("book removed: %v", err)
	}
}

func TestFileLooksLike(t *testing.T) {
	dir := t.TempDir()
	mobi := make([]byte, 70)
	copy(mobi[60:], "BOOKMOBI")
	for _, c := range []struct {
		name, body string
		ok         bool
	}{
		{"a.epub", newEPUB, true}, {"a.epub", "<html>expired</html>", false},
		{"a.pdf", "%PDF-1.7\n", true}, {"a.pdf", "{\"error\":1}", false},
		{"a.fb2", "\xef\xbb\xbf<?xml version=\"1.0\"?><FictionBook>", true}, {"a.fb2", "<FictionBook>", true}, {"a.fb2", "not found", false},
		{"a.fb2.zip", newEPUB, true}, {"a.mobi", string(mobi), true}, {"a.azw3", "x", false},
		{"a.cbr", "Rar!\x1a\x07\x00", true}, {"a.cbz", "Rar!", false},
		{"a.cb7", "7z\xbc\xaf\x27\x1c", true},
		{"a.mp3", "ID3\x04", true}, {"a.mp3", "\xff\xfb\x90", true}, {"a.m4b", "\x00\x00\x00\x20ftypM4B ", true}, {"a.m4b", "oops", false},
		{"a.djvu", "anything", true},
	} {
		p := filepath.Join(dir, "f")
		os.WriteFile(p, []byte(c.body), 0o644)
		if got := fileLooksLike(p, c.name); got != c.ok {
			t.Errorf("%s %q: %v", c.name, c.body, got)
		}
	}
}

func TestAcquireDirectComicGoesToComicsDir(t *testing.T) {
	e := newDirectEnv(t, 50)
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindComic, Title: "Берсерк", Mode: ModeDownload})
	if err != nil {
		t.Fatalf("%+v %v", res, err)
	}
	if v := waitDirect(t, e.svc, res.InfoHash); v.Status.Status != DirectDone {
		t.Fatalf("listing %+v", v)
	}
	path := filepath.Join(e.comics, "Берсерк", "1.cbz")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if !library.IsComicVolume(path) || !slices.EqualFunc(e.repo.Imports, [][]string{{path}}, slices.Equal) {
		t.Fatalf("after scan: imports %q", e.repo.Imports)
	}
	e.noTorrentsNoRows(t, e.svc)
}

func TestAcquireDirectRefusals(t *testing.T) {
	e := newDirectEnv(t, 50)
	// a comic archive is not a book: the book scanner would not open it
	_, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Берсерк", Mode: ModeDownload,
		ReleaseLink: e.srv.URL + "/c/1.cbz", SourceID: e.src.ID, Direct: true})
	if msg, _ := UserMessage(err); !errors.Is(err, ErrInvalid) || msg != "Формат не открывается в Antresol" {
		t.Fatalf("cbz as a book: %v", err)
	}
	// an epub is not a comic
	_, err = e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindComic, Title: "Мастер", Mode: ModeDownload,
		ReleaseLink: e.srv.URL + "/b/1/epub", SourceID: e.src.ID, Direct: true})
	if msg, _ := UserMessage(err); !errors.Is(err, ErrInvalid) || msg != "Формат не открывается в Antresol" {
		t.Fatalf("epub as a comic: %v", err)
	}
	// another site
	_, err = e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Мастер", Mode: ModeDownload,
		ReleaseLink: "https://evil.example.org/b/1/epub", SourceID: e.src.ID, Direct: true})
	if msg, _ := UserMessage(err); !errors.Is(err, ErrSource) || !strings.Contains(msg, "другой сайт") {
		t.Fatalf("foreign link: %v", err)
	}
	e.noTorrentsNoRows(t, e.svc)
	entries, _ := os.ReadDir(e.ebooks)
	if len(entries) != 0 {
		t.Fatalf("files placed: %v", entries)
	}

	full := newDirectEnv(t, 96)
	_, err = full.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Мастер и Маргарита", Artist: "Михаил Булгаков", Mode: ModeDownload})
	if msg, _ := UserMessage(err); !errors.Is(err, ErrInvalid) || msg != "Диск заполнен" {
		t.Fatalf("full disk: %v", err)
	}
	res, err := full.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Мастер и Маргарита", Artist: "Михаил Булгаков", Mode: ModeDownload, Force: true})
	if err != nil {
		t.Fatalf("forced on a full disk: %v", err)
	}
	waitDirect(t, full.svc, res.InfoHash)
}

// with the media disk unplugged, MediaRoot is an empty mount point on
// the root disk. A direct download must not write there (the next scan
// would then find one book and prune the rest of the library).
func TestAcquireDirectRefusedWhenMediaOffline(t *testing.T) {
	e := newDirectEnv(t, 50)
	e.eng.mu.Lock()
	e.eng.offline = "Медиадиск недоступен"
	e.eng.mu.Unlock()
	_, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Мастер и Маргарита", Artist: "Михаил Булгаков", Mode: ModeDownload, Force: true})
	if msg, _ := UserMessage(err); !errors.Is(err, ErrUnavailable) || !strings.Contains(msg, "Медиадиск недоступен") {
		t.Fatalf("offline disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "torrents", ".direct")); !os.IsNotExist(err) {
		t.Fatalf("temp dir made on the root disk: %v", err)
	}
	if entries, _ := os.ReadDir(e.ebooks); len(entries) != 0 {
		t.Fatalf("files placed: %v", entries)
	}
}

// The disk goes away while the file downloads: nothing is placed.
func TestRunDirectRefusesWhenMediaGoesOffline(t *testing.T) {
	e := newDirectEnv(t, 50)
	job, _, err := e.svc.newDirectJob(AcquireRequest{Kind: KindBook, Title: "Мастер"}, "Мастер")
	if err != nil {
		t.Fatal(err)
	}
	e.eng.mu.Lock()
	e.eng.offline = "Медиадиск недоступен"
	e.eng.mu.Unlock()
	dst := filepath.Join(e.ebooks, "Автор", "Мастер.epub")
	func() {
		defer e.svc.wg.Done()
		defer job.cancel()
		e.svc.runDirect(context.Background(), job, strings.NewReader("EPUB"), dst)
	}()
	var v TorrentView
	for _, t := range e.svc.Torrents() {
		if t.InfoHash == job.id {
			v = t
		}
	}
	if !strings.Contains(v.Error, "Медиадиск недоступен") {
		t.Fatalf("job %+v", v)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("placed on an offline disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, "torrents", ".direct")); !os.IsNotExist(err) {
		t.Fatalf("temp dir made on the root disk: %v", err)
	}
}

// TestDirectCloseStopsCleanly: Close cancels a running direct download,
// waits for it and leaves no temporary file; Remove forgets a finished one
// and refuses pause.
func TestDirectCloseStopsCleanly(t *testing.T) {
	e := newDirectEnv(t, 50)
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Медленная", Mode: ModeDownload,
		ReleaseLink: e.srv.URL + "/b/slow.epub", SourceID: e.src.ID, Direct: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Pause(context.Background(), res.InfoHash); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pause: %v", err)
	}
	done := make(chan struct{})
	go func() { e.svc.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the direct download")
	}
	v := waitDirect(t, e.svc, res.InfoHash)
	if v.Status.Status != torrent.StatusError || !strings.Contains(v.Error, "останавливается") {
		t.Fatalf("listing %+v", v)
	}
	left, _ := os.ReadDir(filepath.Join(e.root, "torrents", ".direct"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	if err := e.svc.Remove(context.Background(), res.InfoHash, true); err != nil {
		t.Fatal(err)
	}
	for _, v := range e.svc.Torrents() {
		if v.InfoHash == res.InfoHash {
			t.Fatal("removed download still listed")
		}
	}
}

// TestDirectQueueAndStaleTemp: at most 3 direct downloads run, the 4th
// waits queued (Close ends it too); temp files of an earlier run are
// removed when the service starts.
func TestDirectQueueAndStaleTemp(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "torrents", ".direct", "dl-123")
	os.MkdirAll(filepath.Dir(stale), 0o755)
	os.WriteFile(stale, []byte("half a book"), 0o644)
	New(Options{Engine: newFakeEngine(), TorrentsDir: filepath.Join(root, "torrents")})
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale temp file kept: %v", err)
	}

	e := newDirectEnv(t, 50)
	var ids []string
	for i := 0; i < 4; i++ {
		res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: fmt.Sprintf("Медленная %d", i), Mode: ModeDownload,
			ReleaseLink: e.srv.URL + "/b/slow.epub", SourceID: e.src.ID, Direct: true})
		if err != nil {
			t.Fatal(err)
		}
		want := torrent.StatusDownloading
		if i == 3 {
			want = torrent.StatusQueued
		}
		if res.Status != want {
			t.Fatalf("download %d: %+v", i, res)
		}
		ids = append(ids, res.InfoHash)
	}
	e.svc.Close()
	for _, id := range ids {
		if v := waitDirect(t, e.svc, id); v.Status.Status != torrent.StatusError {
			t.Fatalf("%s after Close: %+v", id, v)
		}
	}
}

// Past maxDirectAtOnce running and maxDirectQueued waiting a direct
// download is refused (no job, no goroutine left behind); a place frees up
// when a waiting one is removed.
func TestDirectQueueCapped(t *testing.T) {
	e := newDirectEnv(t, 50)
	start := func(i int) (AcquireResult, error) {
		return e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: fmt.Sprintf("Медленная %d", i), Mode: ModeDownload,
			ReleaseLink: e.srv.URL + "/b/slow.epub", SourceID: e.src.ID, Direct: true})
	}
	var last AcquireResult
	for i := 0; i < maxDirectAtOnce+maxDirectQueued; i++ {
		res, err := start(i)
		if err != nil {
			t.Fatalf("download %d: %v", i, err)
		}
		last = res
	}
	_, err := start(99)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("past the queue: %v", err)
	}
	if n := len(e.svc.directViews(func(string) string { return "" })); n != maxDirectAtOnce+maxDirectQueued {
		t.Fatalf("%d jobs listed, want %d", n, maxDirectAtOnce+maxDirectQueued)
	}
	if err := e.svc.removeDirect(last.InfoHash); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err = start(100); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no place freed by removing a waiting download: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDirectPlacement(t *testing.T) {
	for _, c := range []struct {
		kind, title, artist, relTitle, name, ctype string
		want                                       string
	}{
		{KindBook, "Мастер и Маргарита", "", "Михаил Булгаков — Мастер и Маргарита", "x.fb2.zip", "", "ebooks/Михаил Булгаков/Мастер и Маргарита.fb2.zip"},
		{KindBook, "Мастер и Маргарита", "", "Мастер и Маргарита", "1", "application/epub+zip", "ebooks/Мастер и Маргарита.epub"},
		{KindBook, "Мастер и Маргарита", "Булгаков", "", "b.MOBI", "", "ebooks/Булгаков/Мастер и Маргарита.mobi"},
		{KindComic, "Берсерк", "", "", "Berserk v01.cbz", "", "manga/Берсерк/Berserk v01.cbz"},
		{KindComic, "Берсерк", "", "", "5", "application/vnd.comicbook-rar", "manga/Берсерк/5.cbr"},
	} {
		got, err := directPath(c.kind, "ebooks", "manga", c.title, c.artist, c.relTitle, c.name, c.ctype)
		if err != nil || filepath.ToSlash(got) != c.want {
			t.Errorf("%+v: %q %v", c, got, err)
		}
	}
	for _, c := range [][2]string{{KindBook, "a.cbz"}, {KindBook, "a.zip"}, {KindBook, "a.djvu"}, {KindComic, "a.epub"}, {KindComic, "a.pdf"}} {
		if _, err := directPath(c[0], "ebooks", "manga", "T", "", "", c[1], ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v accepted", c)
		}
	}
}
