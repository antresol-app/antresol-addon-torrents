package torrent

import (
	"context"
	"time"

	atorrent "github.com/anacrolix/torrent"
)

// verifyPoll is how often WaitVerified looks at the piece states.
const verifyPoll = 500 * time.Millisecond

// WaitVerified waits until the state of every piece of the torrent is
// known — checked against the data on disk, not hashing, not queued for a
// hash and not being stored — and then returns how much of it is complete.
// It never starts a new hash pass of its own (that would re-read the whole
// torrent): it waits for the one the engine runs for pieces without a
// recorded state, e.g. right after a seed-mode add. At least one look at
// the pieces happens after the metadata is there. Returns ErrNotFound for
// an unknown torrent and ctx's error when ctx ends first.
func (e *Engine) WaitVerified(ctx context.Context, ih string) (done, size int64, err error) {
	h, ok := e.Get(ih)
	if !ok {
		return 0, 0, ErrNotFound
	}
	en := h.(*entry)
	if err := en.WaitInfo(ctx); err != nil {
		return 0, 0, err
	}
	tk := time.NewTicker(verifyPoll)
	defer tk.Stop()
	for {
		if st := en.Status(); st.Parked {
			return st.Done, st.Size, nil
		}
		t, err := en.current()
		if err != nil {
			return 0, 0, err
		}
		if t != nil && t.Info() != nil && allPiecesKnown(t) {
			st := en.Status()
			return st.Done, st.Size, nil
		}
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-e.closing:
			return 0, 0, ErrClosed
		case <-tk.C:
		}
	}
}

func allPiecesKnown(t *atorrent.Torrent) bool {
	for i := range t.NumPieces() {
		ps := t.PieceState(i)
		if !ps.Ok || ps.Hashing || ps.QueuedForHash || ps.Marking {
			return false
		}
	}
	return true
}
