package torrent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/storage"
)

var (
	// ErrKnown: the engine already has this torrent; nothing was fetched.
	ErrKnown = errors.New("torrent: already added")
	// ErrNoInfo: the metadata did not arrive before the caller gave up.
	ErrNoInfo = errors.New("torrent: metadata not received")
	// ErrInfoClosed: the torrent closed before its metadata arrived.
	ErrInfoClosed = errors.New("torrent: closed before the metadata")
)

// fetch is one metadata fetch shared by every concurrent caller of
// FetchInfo for an infohash. Fields other than done/meta/err are guarded
// by e.mu; meta/err are set before done is closed.
type fetch struct {
	done    chan struct{}
	meta    []byte
	err     error
	waiters int
	stopped bool // every caller left; the fetch is being torn down
	cancel  context.CancelFunc
}

// FetchInfo fetches a magnet's metafile for a file-list preview. It never
// touches the engine's torrents, piece store, StreamDir or SaveDir, and
// leaves nothing behind: the temporary anacrolix torrent (no data
// download or upload, its own temp storage) is dropped when done.
// Concurrent callers for one infohash share a single fetch. A torrent the
// engine already has → ErrKnown; ctx done before the metadata → ErrNoInfo.
func (e *Engine) FetchInfo(ctx context.Context, magnet string) ([]byte, error) {
	if e.cfg.Off {
		return nil, ErrOff
	}
	spec, err := atorrent.TorrentSpecFromMagnetUri(magnet)
	if err != nil {
		return nil, fmt.Errorf("magnet: %w", err)
	}
	if spec.InfoHash.IsZero() {
		return nil, errors.New("torrent: v2-only magnets are not supported")
	}
	ih := spec.InfoHash.HexString()
	for {
		e.mu.Lock()
		select {
		case <-e.closing:
			e.mu.Unlock()
			return nil, ErrClosed
		default:
		}
		if _, ok := e.entries[ih]; ok {
			e.mu.Unlock()
			return nil, ErrKnown
		}
		f := e.fetching[ih]
		if f != nil && f.stopped {
			// Being torn down: wait for it to leave the client, then retry.
			e.mu.Unlock()
			select {
			case <-f.done:
				continue
			case <-ctx.Done():
				return nil, ErrNoInfo
			}
		}
		if f == nil {
			if t, ok := e.client.Torrent(spec.InfoHash); ok {
				// Not ours to drop.
				e.mu.Unlock()
				return e.waitForeign(ctx, t)
			}
			fctx, cancel := context.WithCancel(context.Background())
			f = &fetch{done: make(chan struct{}), cancel: cancel}
			e.fetching[ih] = f
			e.wg.Add(1)
			go e.runFetch(fctx, f, ih, spec)
		}
		f.waiters++
		e.mu.Unlock()
		return e.awaitFetch(ctx, f)
	}
}

// awaitFetch waits for f or ctx. The last caller to give up stops the fetch
// and waits until it has left the client.
func (e *Engine) awaitFetch(ctx context.Context, f *fetch) ([]byte, error) {
	select {
	case <-f.done:
		if f.err != nil {
			return nil, f.err
		}
		return bytes.Clone(f.meta), nil
	case <-ctx.Done():
	}
	e.mu.Lock()
	f.waiters--
	last := f.waiters == 0
	if last {
		f.stopped = true
	}
	e.mu.Unlock()
	if last {
		f.cancel()
		<-f.done
	}
	return nil, ErrNoInfo
}

// waitForeign waits for the metadata of a torrent the client holds outside
// the engine's entries. It is never dropped here.
func (e *Engine) waitForeign(ctx context.Context, t *atorrent.Torrent) ([]byte, error) {
	select {
	case <-t.GotInfo():
		return metafileOf(t)
	case <-ctx.Done():
		return nil, ErrNoInfo
	case <-t.Closed():
		return nil, ErrInfoClosed
	case <-e.closing:
		return nil, ErrClosed
	}
}

func metafileOf(t *atorrent.Torrent) ([]byte, error) {
	mi := t.Metainfo()
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// runFetch adds the temporary torrent, waits for its metadata (or ctx /
// engine close), drops it, removes its storage and publishes the result.
func (e *Engine) runFetch(ctx context.Context, f *fetch, ih string, spec *atorrent.TorrentSpec) {
	defer e.wg.Done()
	meta, err := e.fetchOnce(ctx, spec)
	e.mu.Lock()
	delete(e.fetching, ih)
	f.meta, f.err = meta, err
	close(f.done)
	e.mu.Unlock()
	f.cancel()
}

func (e *Engine) fetchOnce(ctx context.Context, spec *atorrent.TorrentSpec) ([]byte, error) {
	dir, err := os.MkdirTemp(e.cfg.tempDir, "vault-fetch-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	st := storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   dir,
		PieceCompletion: storage.NewMapPieceCompletion(),
	})
	defer st.Close()
	spec.Storage = st
	spec.DisallowDataDownload = true
	spec.DisallowDataUpload = true
	if e.cfg.LocalOnly {
		spec.Trackers = nil
	}
	t, isNew, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		if isNew {
			t.Drop()
		}
		return nil, err
	}
	if isNew {
		// Drop only the torrent this fetch created.
		defer t.Drop()
	}
	select {
	case <-t.GotInfo():
		return metafileOf(t)
	case <-ctx.Done():
		return nil, ErrNoInfo
	case <-t.Closed():
		return nil, ErrInfoClosed
	case <-e.closing:
		return nil, ErrClosed
	}
}
