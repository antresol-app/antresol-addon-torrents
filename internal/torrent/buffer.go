package torrent

import (
	"sync"
)

// readPositions remembers, per file, where the latest stream read waited:
// that is where the viewer needs data next.
type readPositions struct {
	mu  sync.Mutex
	pos map[int]int64
}

func (r *readPositions) set(file int, off int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pos == nil {
		r.pos = map[int]int64{}
	}
	r.pos[file] = off
}

func (r *readPositions) get(file int) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pos[file]
}

// Buffer reports how much of the readahead window of a file is downloaded:
// the window starts where the latest stream read of the file waited (the
// start of the file before any read) and is as long as the reader's
// readahead, or the rest of the file. It is the «Загружается… N%» a viewer
// sees while a torrent stream buffers. want = 0: nothing to tell (no
// metadata yet, a bad index).
func (en *entry) Buffer(fileIndex int) (have, want int64) {
	t, err := en.current()
	if err != nil || t == nil || t.Info() == nil {
		return 0, 0
	}
	fs := t.Files()
	if fileIndex < 0 || fileIndex >= len(fs) {
		return 0, 0
	}
	f := fs[fileIndex]
	off := min(max(en.reads.get(fileIndex), 0), f.Length())
	want = min(int64(readahead), f.Length()-off)
	if want <= 0 {
		return 0, 0
	}
	pl := t.Info().PieceLength
	begin, end := f.Offset()+off, f.Offset()+off+want
	for i := int(begin / pl); int64(i)*pl < end && i < t.NumPieces(); i++ {
		ps, pe := int64(i)*pl, int64(i)*pl+t.Piece(i).Info().Length()
		ov := min(pe, end) - max(ps, begin)
		if ov <= 0 {
			continue
		}
		plen := pe - ps
		missing := min(max(t.PieceBytesMissing(i), 0), plen)
		have += ov * (plen - missing) / plen
	}
	return have, want
}
