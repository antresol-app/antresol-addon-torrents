package watch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/httpx"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// contentTypes of the files the core's player reads.
var contentTypes = map[string]string{
	".mkv": "video/x-matroska", ".mka": "audio/x-matroska", ".mp4": "video/mp4", ".m4v": "video/mp4",
	".avi": "video/x-msvideo", ".mov": "video/quicktime", ".webm": "video/webm", ".ts": "video/mp2t",
	".m2ts": "video/mp2t", ".ac3": "audio/ac3", ".eac3": "audio/eac3", ".dts": "audio/vnd.dts", ".aac": "audio/aac",
	".flac": "audio/flac", ".m4a": "audio/mp4", ".mp3": "audio/mpeg", ".opus": "audio/ogg", ".wav": "audio/wav",
	".srt": "application/x-subrip", ".ass": "text/x-ssa", ".ssa": "text/x-ssa", ".vtt": "text/vtt",
}

// Media is the socket-only reader of the inputs (never under /api/, so the
// core's proxy cannot reach it): GET /media/{ih}/{idx} the video and GET
// /media/{ih}/{idx}/s/{n} a sidecar of it, with Range. A read waits for
// pieces; a torrent re-attached under it (a promote) is reopened at the
// same place.
func Media(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /media/{ih}/{idx}", func(w http.ResponseWriter, r *http.Request) {
		ih, idx, ok := params(w, r)
		if ok {
			serve(w, r, d.Acquire, ih, idx)
		}
	})
	mux.HandleFunc("GET /media/{ih}/{idx}/s/{n}", func(w http.ResponseWriter, r *http.Request) {
		ih, idx, ok := params(w, r)
		if !ok {
			return
		}
		n, err := strconv.Atoi(r.PathValue("n"))
		if err != nil || n < 0 {
			httpx.WriteErr(w, r, http.StatusNotFound, "Файл не найден")
			return
		}
		video, _, release, err := d.Acquire.OpenStream(r.Context(), ih, idx)
		if err != nil {
			acquireErr(w, r, "media", err)
			return
		}
		release()
		if !isSidecar(video, d.Acquire.StreamFiles(ih), n) {
			httpx.WriteErr(w, r, http.StatusNotFound, "Файл не найден")
			return
		}
		serve(w, r, d.Acquire, ih, n)
	})
	return mux
}

// serve answers file idx of torrent ih with Range support.
func serve(w http.ResponseWriter, r *http.Request, a Acquire, ih string, idx int) {
	fi, open, release, err := a.OpenStream(r.Context(), ih, idx)
	if err != nil {
		acquireErr(w, r, "media", err)
		return
	}
	defer release()
	rd := newResumingReader(r.Context(), open, fi.Size)
	defer rd.Close()
	name := path.Base(fi.Path)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if t, ok := contentTypes[strings.ToLower(path.Ext(name))]; ok {
		w.Header().Set("Content-Type", t)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeContent(w, r, name, time.Time{}, rd)
}

// resumingReader is a ReadSeeker over a torrent file of known size that
// reopens the underlying reader when it breaks (the torrent was dropped and
// re-attached, e.g. moved after a promote) and continues at the same
// offset. Seeks are lazy, so ServeContent's size probing costs nothing.
type resumingReader struct {
	ctx      context.Context
	open     func(context.Context) (io.ReadSeekCloser, error)
	size     int64
	pos      int64
	cur      io.ReadSeekCloser
	curPos   int64
	retryFor time.Duration
	backoff  time.Duration
}

func newResumingReader(ctx context.Context, open func(context.Context) (io.ReadSeekCloser, error), size int64) *resumingReader {
	return &resumingReader{ctx: ctx, open: open, size: size, retryFor: 2 * time.Minute, backoff: 250 * time.Millisecond}
}

func (rr *resumingReader) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = rr.pos + off
	case io.SeekEnd:
		abs = rr.size + off
	default:
		return 0, errors.New("resumingReader: bad whence")
	}
	if abs < 0 {
		return 0, errors.New("resumingReader: negative position")
	}
	rr.pos = abs
	return abs, nil
}

func (rr *resumingReader) drop() {
	if rr.cur != nil {
		rr.cur.Close()
		rr.cur = nil
	}
}

func (rr *resumingReader) Close() error {
	rr.drop()
	return nil
}

// reopen opens a fresh reader, retrying until retryFor elapses.
func (rr *resumingReader) reopen() error {
	deadline := time.Now().Add(rr.retryFor)
	for {
		c, err := rr.open(rr.ctx)
		if err == nil {
			rr.cur, rr.curPos = c, -1
			return nil
		}
		if rr.ctx.Err() != nil {
			return rr.ctx.Err()
		}
		if errors.Is(err, torrent.ErrNotFound) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-rr.ctx.Done():
			return rr.ctx.Err()
		case <-time.After(rr.backoff):
		}
	}
}

func (rr *resumingReader) Read(p []byte) (int, error) {
	if rr.pos >= rr.size {
		return 0, io.EOF
	}
	if int64(len(p)) > rr.size-rr.pos {
		p = p[:rr.size-rr.pos]
	}
	deadline := time.Now().Add(rr.retryFor)
	for {
		if rr.cur == nil {
			if err := rr.reopen(); err != nil {
				return 0, err
			}
		}
		if rr.curPos != rr.pos {
			if _, err := rr.cur.Seek(rr.pos, io.SeekStart); err != nil {
				rr.drop()
				if rr.ctx.Err() != nil || time.Now().After(deadline) {
					return 0, err
				}
				continue
			}
			rr.curPos = rr.pos
		}
		n, err := rr.cur.Read(p)
		rr.pos += int64(n)
		rr.curPos = rr.pos
		if n > 0 {
			return n, nil
		}
		if err == nil {
			// (0, nil): allowed by io.Reader but must not spin
			if time.Now().After(deadline) {
				return 0, io.ErrNoProgress
			}
			if !rr.wait() {
				return 0, rr.ctx.Err()
			}
			continue
		}
		if rr.ctx.Err() != nil {
			return 0, rr.ctx.Err()
		}
		if errors.Is(err, torrent.ErrReadTimeout) {
			// a swarm stall: keep the request open while the player
			// waits; its disconnect ends it
			deadline = time.Now().Add(rr.retryFor)
			if !rr.wait() {
				return 0, rr.ctx.Err()
			}
			continue
		}
		// EOF before the end, "torrent closed" and the like: the torrent
		// was re-attached under us. Reopen at the same position.
		rr.drop()
		if time.Now().After(deadline) {
			return 0, err
		}
		if !rr.wait() {
			return 0, rr.ctx.Err()
		}
	}
}

// wait sleeps one backoff; false: the request ended meanwhile.
func (rr *resumingReader) wait() bool {
	select {
	case <-rr.ctx.Done():
		return false
	case <-time.After(rr.backoff):
		return true
	}
}
