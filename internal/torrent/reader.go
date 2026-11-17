package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	atorrent "github.com/anacrolix/torrent"
)

const readahead = 16 << 20

// ErrReadTimeout is returned by a stream reader when the requested data did
// not arrive within EngineConfig.ReadTimeout. It is never io.EOF, so HTTP
// handlers can tell "not downloaded yet" from "end of file".
var ErrReadTimeout = errors.New("torrent: timed out waiting for data")

// fileReader wraps an anacrolix file reader. Reads block until the pieces at
// the current position are downloaded (the reader's position and 16 MiB
// readahead get top piece priority, and a Seek moves that window), bounded by
// a per-Read timeout and the reader's context.
type fileReader struct {
	r       atorrent.Reader
	ctx     context.Context
	timeout time.Duration
	pos     int64
	// onRead learns the position each Read waits at (buffering progress).
	onRead func(off int64)
	// blocked refuses reads while the torrent is held (nil = never).
	blocked func() error
	// wake is closed when a hold begins: a waiting Read returns ErrHeld
	// (nil = never).
	wake func() <-chan struct{}
	// onClose runs once, at the first Close (nil = nothing).
	onClose func()
}

var _ io.ReadSeekCloser = (*fileReader)(nil)

func newFileReader(ctx context.Context, f *atorrent.File, timeout time.Duration) *fileReader {
	r := f.NewReader()
	r.SetReadahead(readahead)
	// Return bytes as soon as their chunks arrive instead of waiting for the
	// whole piece hash (pieces can be 16 MiB). Trade-off: a malicious peer's
	// chunk may be served once before the piece fails verification.
	r.SetResponsive()
	return &fileReader{r: r, ctx: ctx, timeout: timeout}
}

func (fr *fileReader) Read(p []byte) (int, error) {
	if err := fr.ctx.Err(); err != nil {
		return 0, err
	}
	var wake <-chan struct{}
	if fr.wake != nil {
		wake = fr.wake() // before the check (see holdWaker)
	}
	if fr.blocked != nil {
		if err := fr.blocked(); err != nil {
			return 0, err
		}
	}
	if fr.onRead != nil {
		fr.onRead(fr.pos)
	}
	ctx, cancel := context.WithTimeout(fr.ctx, fr.timeout)
	defer cancel()
	woken := make(chan struct{})
	if wake != nil {
		go func() {
			select {
			case <-wake:
				close(woken)
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	fr.r.SetContext(ctx)
	n, err := fr.r.Read(p)
	fr.pos += int64(n)
	if n == 0 && err != nil {
		select {
		case <-woken:
			return 0, ErrHeld
		default:
		}
	}
	if n == 0 && err != nil && errors.Is(err, context.DeadlineExceeded) && fr.ctx.Err() == nil {
		return 0, fmt.Errorf("%w (%s)", ErrReadTimeout, fr.timeout)
	}
	return n, err
}

func (fr *fileReader) Seek(off int64, whence int) (int64, error) {
	pos, err := fr.r.Seek(off, whence)
	if err == nil {
		fr.pos = pos
	}
	return pos, err
}

func (fr *fileReader) Close() error {
	if f := fr.onClose; f != nil {
		fr.onClose = nil
		f()
	}
	return fr.r.Close()
}
