package acquire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/events"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// DirectFetcher is implemented by *indexer.Manager: the file behind a
// direct link of an OPDS source.
type DirectFetcher interface {
	Fetch(ctx context.Context, sourceID int64, link string) (io.ReadCloser, indexer.FetchInfo, error)
}

// maxDirectBytes bounds a direct download (the indexer enforces it too).
const maxDirectBytes = 500 << 20

// directMu serialises picking a free name and renaming into it.
var directMu sync.Mutex

// directRelease tells whether the chosen release is a direct download and
// returns its title ("<Автор> — <Название>"): the request says so for a
// release picked by hand, the scored release (rel) for the best one.
func directRelease(req AcquireRequest, rel json.RawMessage) (title string, ok bool) {
	if strings.TrimSpace(req.ReleaseLink) != "" {
		return "", req.Direct
	}
	var r struct {
		Title  string `json:"title"`
		Direct bool   `json:"direct"`
	}
	if len(rel) == 0 || json.Unmarshal(rel, &r) != nil {
		return "", false
	}
	return r.Title, r.Direct
}

// contentExt maps acquisition types to extensions for names without one.
var contentExt = map[string]string{
	"application/epub+zip": ".epub", "application/x-fictionbook+xml": ".fb2", "application/fb2": ".fb2", "text/fb2+xml": ".fb2",
	"application/fb2+zip": ".fb2.zip", "application/x-zip-compressed-fb2": ".fb2.zip",
	"application/x-mobipocket-ebook": ".mobi", "application/x-mobi8-ebook": ".azw3", "application/vnd.amazon.ebook": ".azw3",
	"application/pdf":               ".pdf",
	"application/vnd.comicbook+zip": ".cbz", "application/x-cbz": ".cbz",
	"application/vnd.comicbook-rar": ".cbr", "application/x-cbr": ".cbr", "application/x-cb7": ".cb7",
}

// directPath is where a direct download goes: a book to
// <ebooks>/<Автор>/<Название>.<ext> (the author from the request, else the
// part of the release title before « — »; no author: right in <ebooks>),
// a comic to <comics>/<Название>/<file name>. A type the scanner of that
// root does not open is refused.
func directPath(kind, ebooks, comicsDir, title, artist, relTitle, name, ctype string) (string, error) {
	ext := library.BookExt(name)
	if !library.IsBookFile(name) && !library.IsComicVolume(name) {
		if e, ok := contentExt[strings.ToLower(ctype)]; ok {
			ext = e
			name += e
		}
	}
	bad := uerr(ErrInvalid, "Формат не открывается в Antresol")
	switch kind {
	case KindBook:
		if !library.IsBookFile(name) {
			return "", bad
		}
		author := strings.TrimSpace(artist)
		if author == "" {
			if head, _, ok := strings.Cut(relTitle, " — "); ok {
				author = strings.TrimSpace(head)
			}
		}
		if strings.TrimSpace(title) == "" {
			if _, tail, ok := strings.Cut(relTitle, " — "); ok {
				title = tail
			} else {
				title = relTitle
			}
		}
		return filepath.Join(organize.FolderOf(ebooks, author), organize.SafeName(title)+ext), nil
	case KindComic:
		if ext == ".fb2.zip" || !library.IsComicVolume(name) {
			return "", bad
		}
		base := organize.SafeName(strings.TrimSuffix(name, filepath.Ext(name))) + ext
		return filepath.Join(organize.FolderOf(comicsDir, title), base), nil
	}
	return "", uerr(ErrInvalid, "Прямая загрузка — только для книг и комиксов")
}

// fetchDirect starts a direct download. What can be checked at once is
// checked on the request (kind, disk, the link and the file type, from the
// response headers); the transfer, the placing and the scan then run as a
// background job of the service (directJob), not on the request: closing
// the tab does not lose the book, Close stops it cleanly. The job shows up
// in Torrents() under its id until it is removed. No acquisitions row:
// there is no torrent.
func (s *Service) fetchDirect(ctx context.Context, req AcquireRequest, title, artist string, sourceID int64, link, relTitle string) (AcquireResult, error) {
	if req.Kind != KindBook && req.Kind != KindComic {
		return AcquireResult{}, uerr(ErrInvalid, "Прямая загрузка — только для книг и комиксов")
	}
	if msg := s.mediaOffline(); msg != "" {
		return AcquireResult{}, uerr(ErrUnavailable, msg)
	}
	if s.o.Disk != nil && s.o.Disk.Level() == LevelFull && !req.Force {
		return AcquireResult{}, uerr(ErrInvalid, "Диск заполнен")
	}
	f, ok := s.o.Indexer.(DirectFetcher)
	if !ok || s.o.Indexer == nil {
		return AcquireResult{}, uerr(ErrUnavailable, "Источники раздач не настроены")
	}
	job, jctx, err := s.newDirectJob(req, title)
	if err != nil {
		return AcquireResult{}, err
	}
	slots := s.direct.slotsChan()
	select {
	case slots <- struct{}{}:
	default:
		// maxDirectAtOnce run already: queued; checks and errors then
		// show on the job
		if !s.direct.enqueue() {
			s.dropDirectJob(job)
			return AcquireResult{}, uerr(ErrUnavailable, "Слишком много загрузок в очереди — подождите")
		}
		job.setStatus(torrent.StatusQueued)
		go func() {
			defer s.wg.Done()
			defer job.cancel()
			select {
			case slots <- struct{}{}:
				s.direct.dequeue()
			case <-jctx.Done():
				s.direct.dequeue()
				job.finish("", s.directCancelMsg())
				return
			}
			defer func() { <-slots }()
			job.setStatus(torrent.StatusDownloading)
			body, dst, err := s.openDirect(jctx, f, req, title, artist, sourceID, link, relTitle, job)
			if err != nil {
				msg, _ := UserMessage(err)
				if msg == "" || jctx.Err() != nil {
					msg = s.directCancelMsg()
				}
				job.finish("", msg)
				return
			}
			defer body.Close()
			s.runDirect(jctx, job, body, dst)
		}()
		return AcquireResult{InfoHash: job.id, FileIndex: -1, Status: torrent.StatusQueued}, nil
	}
	started := false
	defer func() {
		if !started {
			<-slots
			s.dropDirectJob(job)
		}
	}()
	body, dst, err := s.openDirect(jctx, f, req, title, artist, sourceID, link, relTitle, job)
	if err != nil {
		return AcquireResult{}, err
	}
	started = true
	go func() {
		defer s.wg.Done()
		defer job.cancel()
		defer func() { <-slots }()
		defer body.Close()
		s.runDirect(jctx, job, body, dst)
	}()
	return AcquireResult{InfoHash: job.id, FileIndex: -1, Status: torrent.StatusDownloading}, nil
}

// openDirect fetches the headers of a direct download (on the job's
// context: the download lives as long as the service, not the request) and
// picks its place; the caller closes body.
func (s *Service) openDirect(jctx context.Context, f DirectFetcher, req AcquireRequest, title, artist string, sourceID int64, link, relTitle string, job *directJob) (io.ReadCloser, string, error) {
	body, info, err := f.Fetch(jctx, sourceID, link)
	if err != nil {
		if jctx.Err() != nil {
			return nil, "", uerr(ErrUnavailable, "Сервер останавливается")
		}
		return nil, "", wrapErr(ErrSource, err.Error(), err)
	}
	dst, err := directPath(req.Kind, s.o.EbooksDir, s.o.ComicsDir, title, artist, relTitle, info.Name, info.ContentType)
	if err != nil {
		body.Close()
		return nil, "", err
	}
	job.setSize(info.Size, info.Name)
	return body, dst, nil
}

// mediaOffline says why direct downloads must not write now ("" = go on):
// the engine holds torrents because the media disk is missing. MediaRoot is
// then an empty mount point on the root disk; a book written there would
// also make the next scan prune every other book.
func (s *Service) mediaOffline() string {
	if o, ok := s.o.Engine.(offliner); ok && o.Offline() != "" {
		return "Медиадиск недоступен: загрузки остановлены, пока его не подключат"
	}
	return ""
}

func (s *Service) directCancelMsg() string {
	if s.root.Err() != nil {
		return "Загрузка прервана: сервер останавливается"
	}
	return "Загрузка отменена"
}

// runDirect streams body into TorrentsDir/.direct, checks that its first
// bytes are of the format its name says, moves it to its place under a
// free name (an existing file gets « (2)»; another file system: copied,
// see placeFree), scans as organize does after a torrent and announces
// library.updated.
func (s *Service) runDirect(ctx context.Context, job *directJob, body io.Reader, dst string) {
	fail := func(msg string, err error) {
		cancelled := ctx.Err() != nil
		if cancelled {
			msg = s.directCancelMsg()
		}
		s.log.Warn("direct download failed", "id", job.id, "msg", msg, "err", err)
		job.finish("", msg)
		if !cancelled { // a cancel is no news to whoever cancelled it
			s.finished(Finished{Key: job.id, Kind: job.kind, Title: job.title, Failed: true, Error: msg})
		}
	}
	// the disk may have gone away since the request (or while queued)
	if msg := s.mediaOffline(); msg != "" {
		fail(msg, nil)
		return
	}
	tmpDir := filepath.Join(s.o.TorrentsDir, ".direct")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		fail(organize.HumanFSError(err, tmpDir).Error(), err)
		return
	}
	tmp, err := os.CreateTemp(tmpDir, "dl-*")
	if err != nil {
		fail(organize.HumanFSError(err, tmpDir).Error(), err)
		return
	}
	keep := false
	defer func() {
		if !keep {
			os.Remove(tmp.Name())
		}
	}()
	n, err := io.Copy(tmp, io.LimitReader(organize.NewCtxReader(ctx, &countReader{r: body, n: &job.done}), maxDirectBytes+1))
	if err == nil {
		// readable by the other users of the media share, as torrent files are
		err = tmp.Chmod(0o644)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	switch {
	case err != nil:
		fail(fmt.Sprintf("Не удалось скачать файл: %s", err.Error()), err)
		return
	case n > maxDirectBytes:
		fail("Файл больше 500 МБ", nil)
		return
	case n == 0:
		fail("Каталог прислал пустой файл", nil)
		return
	}
	if msg := s.mediaOffline(); msg != "" { // unplugged during the transfer
		fail(msg, nil)
		return
	}
	if !fileLooksLike(tmp.Name(), dst) {
		fail(fmt.Sprintf("Сайт прислал не %s, а что-то другое (возможно, ссылка устарела)", strings.ToUpper(strings.TrimPrefix(library.BookExt(dst), "."))), nil)
		return
	}
	placed, copied, err := placeFree(ctx, tmp.Name(), dst)
	if err != nil {
		fail(organize.HumanFSError(err, filepath.Dir(dst)).Error(), err)
		return
	}
	keep = true
	if copied {
		if err := os.Remove(tmp.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.log.Warn("remove a direct download's temporary file", "path", tmp.Name(), "err", err)
		}
	}
	s.log.Info("direct download placed", "kind", job.kind, "path", placed, "bytes", n)
	if s.o.Library != nil {
		// the library scans and records the book's owner
		// by uid: a profile given the asker's id since owns nothing
		owner := s.profileID(ctx, job.addedByUID)
		if _, missing, _, err := s.o.Library.Import(ctx, []string{placed}, "", owner); err != nil || len(missing) > 0 {
			s.log.Warn("import a direct download", "path", placed, "missing", missing, "err", err) // the file is in place; the next scan finds it
		}
	}
	if s.o.Changed != nil {
		s.o.Changed()
	}
	job.finish(placed, "")
	s.publish(events.LibraryUpdated, map[string]any{"kind": job.kind, "title": job.title, "path": placed, "infoHash": job.id})
	s.finished(Finished{Key: job.id, Kind: job.kind, Title: job.title})
	s.checkDisk()
}

// placeFree moves src to dst or, when dst exists, to "<name> (2)<ext>",
// "(3)"… (never overwriting). Across file systems (EXDEV: the downloads
// volume is not the media one) src is first copied next to its place,
// without holding the lock, and the copy is renamed; copied then says src
// is still there for the caller to remove.
func placeFree(ctx context.Context, src, dst string) (placed string, copied bool, err error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}
	placed, err = placeUnder(src, dst)
	if !errors.Is(err, syscall.EXDEV) {
		return placed, false, err
	}
	staged, err := stageCopy(ctx, src, dir)
	if err != nil {
		return "", false, err
	}
	if placed, err = placeUnder(staged, dst); err != nil {
		os.Remove(staged)
		return "", false, err
	}
	syncDir(dir)
	return placed, true, nil
}

// placeUnder renames src to the first free name of dst, under directMu.
func placeUnder(src, dst string) (string, error) {
	ext := library.BookExt(dst)
	stem := dst[:len(dst)-len(ext)]
	directMu.Lock()
	defer directMu.Unlock()
	for i := 1; i <= 100; i++ {
		cand := dst
		if i > 1 {
			cand = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		if _, err := os.Lstat(cand); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if err := renameFile(src, cand); err != nil {
			return "", err
		}
		return cand, nil
	}
	return "", i18n.Errorf("в папке %s уже 100 копий этого файла", filepath.Dir(dst))
}

// renameFile is os.Rename (a variable for tests).
var renameFile = os.Rename

// partialSuffix marks a copy in progress; New sweeps leftovers of a crash.
const partialSuffix = organize.PartialSuffix

// stageCopy copies src into dir as a hidden "*.vault-partial" file, synced
// to disk, readable by the media share (0644). A failed copy is removed.
func stageCopy(ctx context.Context, src, dir string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(dir, ".direct-*"+partialSuffix)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, organize.NewCtxReader(ctx, in))
	if err == nil {
		err = out.Sync()
	}
	if err == nil {
		err = out.Chmod(0o644)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// syncDir flushes a directory entry (a rename into it) to disk; best effort.
func syncDir(dir string) { organize.SyncDir(dir) }

// cleanPartials removes copies a crash left half-done ("*.vault-partial")
// under the library roots; called by New before any job runs.
func cleanPartials(log *slog.Logger, roots ...string) {
	for _, root := range roots {
		if root == "" {
			continue
		}
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), partialSuffix) {
				return nil
			}
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				log.Warn("remove a stale partial copy", "path", p, "err", err)
			} else {
				log.Info("removed a stale partial copy", "path", p)
			}
			return nil
		})
	}
}

// fileLooksLike checks the first bytes of path against the format the
// extension of name stands for: a file host's error message saved as
// .epub would show up as a broken book and take the good name. Unknown
// extensions pass.
func fileLooksLike(path, name string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 1024)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	at := func(off int, magic string) bool {
		return len(head) >= off+len(magic) && string(head[off:off+len(magic)]) == magic
	}
	switch strings.ToLower(library.BookExt(name)) {
	case ".epub", ".cbz", ".fb2.zip", ".zip":
		return at(0, "PK\x03\x04")
	case ".pdf":
		return bytes.Contains(head, []byte("%PDF-"))
	case ".fb2":
		t := bytes.TrimLeft(bytes.TrimPrefix(head, []byte("\xef\xbb\xbf")), " \t\r\n")
		return bytes.HasPrefix(t, []byte("<?xml")) || bytes.HasPrefix(t, []byte("<FictionBook"))
	case ".mobi", ".azw", ".azw3":
		return at(60, "BOOKMOBI")
	case ".cbr", ".rar":
		return at(0, "Rar!\x1a\x07")
	case ".cb7", ".7z":
		return at(0, "7z\xbc\xaf\x27\x1c")
	case ".mp3":
		return at(0, "ID3") || len(head) > 1 && head[0] == 0xff && head[1]&0xe0 == 0xe0
	case ".m4b", ".m4a":
		return at(4, "ftyp")
	}
	return true
}
