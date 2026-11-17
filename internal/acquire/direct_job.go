package acquire

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// Direct download states (a running one is torrent.StatusDownloading).
const (
	DirectDone  = "done"
	directIDPre = "direct-"
	// directKeep: how long a finished direct download stays in the list.
	directKeep = 24 * time.Hour
)

// directJob is one direct download in the background, listed by Torrents
// like a torrent (id "direct-<hex>").
type directJob struct {
	id, kind, title string
	addedBy         *int64
	addedByUID      string // addedBy's uid
	addedAt         time.Time
	done            atomic.Int64
	cancel          context.CancelFunc

	mu        sync.Mutex
	name      string
	size      int64
	status    string
	errMsg    string
	path      string
	completed *time.Time
}

// maxDirectAtOnce direct downloads run at once; more wait (queued).
const maxDirectAtOnce = 3

// directJobs are the service's direct downloads.
type directJobs struct {
	mu    sync.Mutex
	jobs  map[string]*directJob
	slots chan struct{}
}

func (d *directJobs) slotsChan() chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.slots == nil {
		d.slots = make(chan struct{}, maxDirectAtOnce)
	}
	return d.slots
}

func (j *directJob) setStatus(st string) {
	j.mu.Lock()
	j.status = st
	j.mu.Unlock()
}

// cleanDirectTemp removes what an earlier run left in TorrentsDir/.direct
// (a download cut by a crash); called before any job starts.
func cleanDirectTemp(torrentsDir string, log *slog.Logger) {
	if torrentsDir == "" {
		return
	}
	dir := filepath.Join(torrentsDir, ".direct")
	names, err := filepath.Glob(filepath.Join(dir, "dl-*"))
	if err != nil {
		return
	}
	for _, n := range names {
		if err := os.Remove(n); err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warn("remove a stale direct download", "path", n, "err", err)
		}
	}
}

// IsDirect reports whether id names a direct download, not a torrent.
func IsDirect(id string) bool { return strings.HasPrefix(id, directIDPre) }

// newDirectJob registers a job with a context of the service's lifetime
// (cancelled by Close) and holds a place in the service's wait group.
func (s *Service) newDirectJob(req AcquireRequest, title string) (*directJob, context.Context, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	select {
	case <-s.closing:
		s.mu.Unlock()
		return nil, nil, uerr(ErrUnavailable, "Сервер останавливается")
	default:
	}
	s.wg.Add(1)
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(s.root)
	j := &directJob{id: directIDPre + hex.EncodeToString(b[:]), kind: req.Kind, title: title, addedAt: time.Now().UTC(),
		cancel: cancel, status: torrent.StatusDownloading, size: -1}
	if req.UserID != 0 && req.UserUID != "" {
		id := req.UserID
		j.addedBy, j.addedByUID = &id, req.UserUID
	}
	s.direct.mu.Lock()
	if s.direct.jobs == nil {
		s.direct.jobs = map[string]*directJob{}
	}
	s.direct.jobs[j.id] = j
	s.direct.mu.Unlock()
	return j, ctx, nil
}

// dropDirectJob forgets a job that never started (and frees its place).
func (s *Service) dropDirectJob(j *directJob) {
	j.cancel()
	s.direct.mu.Lock()
	delete(s.direct.jobs, j.id)
	s.direct.mu.Unlock()
	s.wg.Done()
}

func (j *directJob) setSize(size int64, name string) {
	j.mu.Lock()
	j.size, j.name = size, name
	j.mu.Unlock()
}

// finish marks the job done (path) or failed (errMsg).
func (j *directJob) finish(path, errMsg string) {
	now := time.Now().UTC()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.path, j.errMsg, j.completed = path, errMsg, &now
	j.status = DirectDone
	if errMsg != "" {
		j.status = torrent.StatusError
	}
}

func (j *directJob) view(names map[string]string) TorrentView {
	j.mu.Lock()
	defer j.mu.Unlock()
	done := j.done.Load()
	size := j.size
	if size < 0 || (j.status == DirectDone && size < done) {
		size = done
	}
	v := TorrentView{Status: torrent.Status{
		InfoHash: j.id, Name: j.name, Mode: ModeDownload, Status: j.status, Size: size, Done: done, Downloaded: done,
		AddedBy: j.addedBy, AddedByUID: j.addedByUID, AddedAt: j.addedAt, CompletedAt: j.completed, Error: j.errMsg,
	}, Kind: j.kind, Title: j.title, Organized: j.status == DirectDone, Direct: true}
	if v.Name == "" {
		v.Name = j.title
	}
	if j.addedByUID != "" {
		v.AddedByName = names[j.addedByUID]
	}
	return v
}

// directViews lists the direct downloads, dropping finished ones older
// than directKeep.
func (s *Service) directViews(names map[string]string) []TorrentView {
	s.direct.mu.Lock()
	jobs := make([]*directJob, 0, len(s.direct.jobs))
	for id, j := range s.direct.jobs {
		j.mu.Lock()
		old := j.completed != nil && time.Since(*j.completed) > directKeep
		j.mu.Unlock()
		if old {
			delete(s.direct.jobs, id)
			continue
		}
		jobs = append(jobs, j)
	}
	s.direct.mu.Unlock()
	out := make([]TorrentView, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.view(names))
	}
	sort.SliceStable(out, func(i, k int) bool { return out[i].AddedAt.Before(out[k].AddedAt) })
	return out
}

// removeDirect cancels a running direct download or forgets a finished
// one. The placed file stays: it is part of the library now.
func (s *Service) removeDirect(id string) error {
	s.direct.mu.Lock()
	j, ok := s.direct.jobs[id]
	delete(s.direct.jobs, id)
	s.direct.mu.Unlock()
	if !ok {
		return uerr(ErrNotFound, "Загрузка не найдена")
	}
	j.cancel()
	return nil
}

// countReader counts the bytes read into n.
type countReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n.Add(int64(k))
	return k, err
}
