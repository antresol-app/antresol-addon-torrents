package torrent

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	g "github.com/anacrolix/generics"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

// safeStorage wraps a storage client so that a piece's completion is never
// an error to anacrolix. On one (Torrent.setCachedPieceCompletionFromStorage,
// anacrolix torrent.go:1780-1783) anacrolix disallows downloads by itself,
// which can trip its consistency panic (see holds.go). The file storage
// reports one when a piece is recorded complete and stat-ing its file fails
// with anything but ENOENT (storage/file-piece.go:119-131: EIO or EACCES on
// a flaky media disk). Such a piece is reported as unknown instead.
// anacrolix hashes an unknown piece only in a torrent's initial check (its
// first verification of the session); after that an unknown piece is
// neither requested nor seeded until its completion reads successfully
// again (a later read of it) or the server restarts. Safe, and the rest of
// the torrent keeps going; only that piece waits.
type safeStorage struct {
	storage.ClientImpl
	warn *logLimiter
}

func newSafeStorage(inner storage.ClientImpl) safeStorage {
	return safeStorage{ClientImpl: inner, warn: &logLimiter{}}
}

func (s safeStorage) OpenTorrent(ctx context.Context, info *metainfo.Info, ih metainfo.Hash) (storage.TorrentImpl, error) {
	ti, err := s.ClientImpl.OpenTorrent(ctx, info, ih)
	if err != nil {
		return ti, err
	}
	if piece := ti.Piece; piece != nil {
		ti.Piece = func(p metainfo.Piece) storage.PieceImpl { return s.wrap(piece(p)) }
	}
	if piece := ti.PieceWithHash; piece != nil {
		ti.PieceWithHash = func(p metainfo.Piece, h g.Option[[]byte]) storage.PieceImpl { return s.wrap(piece(p, h)) }
	}
	return ti, nil
}

// Close closes the wrapped client when it can be closed.
func (s safeStorage) Close() error {
	if c, ok := s.ClientImpl.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

func (s safeStorage) wrap(p storage.PieceImpl) storage.PieceImpl {
	if wt, ok := p.(writerToPiece); ok {
		return safeWriterToPiece{safePiece{p, s.warn}, wt}
	}
	return safePiece{p, s.warn}
}

type writerToPiece interface {
	storage.PieceImpl
	io.WriterTo
}

type safePiece struct {
	storage.PieceImpl
	warn *logLimiter
}

func (p safePiece) Completion() storage.Completion {
	c := p.PieceImpl.Completion()
	if c.Err != nil {
		p.warn.warn("piece completion: treated as unknown (not requested or seeded until it reads again or the server restarts)", "err", c.Err)
		return storage.Completion{}
	}
	return c
}

// safeWriterToPiece keeps the file storage's io.WriterTo (anacrolix uses it
// to hash pieces) visible through the wrapper.
type safeWriterToPiece struct {
	safePiece
	wt io.WriterTo
}

func (p safeWriterToPiece) WriteTo(w io.Writer) (int64, error) { return p.wt.WriteTo(w) }

// logLimiter logs at most one warning per minute (a failing disk would
// otherwise log for every piece).
type logLimiter struct {
	mu   sync.Mutex
	last time.Time
}

func (l *logLimiter) warn(msg string, args ...any) {
	l.mu.Lock()
	now := time.Now()
	if now.Sub(l.last) < time.Minute {
		l.mu.Unlock()
		return
	}
	l.last = now
	l.mu.Unlock()
	slog.Default().With("component", "torrent").Warn(msg, args...)
}
