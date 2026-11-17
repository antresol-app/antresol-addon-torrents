package torrent

import (
	"context"
	"errors"
	"strings"
	"time"

	atorrent "github.com/anacrolix/torrent"
)

// Holds without DisallowDataDownload.
//
// anacrolix v1.61 keeps two views of "pieces to request": the pending set
// (Torrent._pendingPieces, recomputed per piece from its effective priority
// in updatePendingPieces, torrent.go:1568) and the client's request order
// (updatePieceRequestOrderPiece, torrent-piece-request-order.go:14). Both
// treat a piece as unwanted while downloads are disallowed
// (Piece.ignoreForRequests, piece.go:295 → effectivePriority None), but
// DisallowDataDownload (torrent.go:3070) only sets the flag and recomputes
// neither. needData (torrent.go:1886), called e.g. by newConnsAllowed for
// every incoming or outgoing connection attempt and by the tracker
// announcer, runs checkPendingPiecesMatchesRequestOrder
// (torrent-piece-request-order.go:95) on a global, exponentially rarer
// schedule (amortize.Try: the 1st, 2nd, 4th, 8th … call process-wide). It
// skips request-order entries of ignored pieces, so with the flag set and
// pieces still pending it panics ("piece request order has {} and pending
// pieces has {…}") while holding the client lock (the read lock on the
// Seeding path), so even a recovered panic leaves the client deadlocked:
// the whole process dies.
//
// So the engine never disallows downloads of a torrent that has its info.
// A held torrent — paused, waiting for space, blocked after a write error,
// or a blocked duplicate (its files belong to a senior torrent) — instead
// has every priority we set dropped (clearWanted): nothing is pending,
// nothing requested, and anacrolix stays consistent. Releasing the hold
// re-applies the selection (applyWanted), the single source of truth for
// what is wanted. Readers would raise priorities of their own, so they are
// refused while a torrent is held (NewReader and each Read), and a Read
// already waiting for data is woken by the hold (holdWaker) and returns
// ErrHeld; an open reader's readahead window (≤ 16 MiB) may still complete
// (anacrolix keeps a reader's priorities until it reads again or closes).
//
// The metadata-only fetch torrent (fetchinfo.go) sets
// TorrentSpec.DisallowDataDownload, a field anacrolix v1.61 never reads (it
// is not used in client.go AddTorrentSpec/MergeSpec); that torrent never
// gets a priority or a reader and is dropped once the info is there, so its
// pending set stays empty. Nothing in the engine calls
// Torrent.DisallowDataDownload.

const writeErrorPrefix = "ошибка записи"

// ErrHeld: the torrent is paused, waiting for disk space, blocked after a
// write error or a duplicate of another torrent's files; it serves no
// stream reads until that is resolved.
var ErrHeld = errors.New("torrent: paused or blocked")

// blocked reports whether nothing may be downloaded for en. e.mu held.
func (en *entry) blocked() bool {
	return en.hold != "" || en.dupOf != "" || en.relocating || strings.HasPrefix(en.rec.Error, writeErrorPrefix)
}

// applyWanted sets the priorities of t (whose info is there) from the
// entry: none while blocked or in stream mode (readers drive streams),
// else the selection. e.mu held.
func (e *Engine) applyWanted(en *entry, t *atorrent.Torrent) {
	en.syncHeld()
	if en.blocked() || en.rec.Mode == ModeStream {
		clearWanted(t)
		return
	}
	applySelection(t, en.rec.Files)
}

// clearWanted drops every priority applySelection sets: the piece-level
// priority of DownloadAll and the file priorities of a selection. Reader
// priorities are untouched.
func clearWanted(t *atorrent.Torrent) {
	t.CancelPieces(0, t.NumPieces())
	for _, f := range t.Files() {
		f.SetPriority(atorrent.PiecePriorityNone)
	}
}

// syncHeld publishes blocked() for readers (e.mu held). Every change of
// the hold, the duplicate state or a write error that matters to a reader
// goes through applyWanted or setHold, which call it; readers exist only
// after gotInfo applied the torrent.
// A hold beginning also closes the current holdWake channel.
func (en *entry) syncHeld() {
	b := en.blocked()
	if b == en.held.Load() {
		return
	}
	en.held.Store(b)
	if b {
		// after held is set: a Read that takes a channel after this Swap
		// sees held in its readBlocked check
		if ch := en.holdWake.Swap(nil); ch != nil {
			close(*ch)
		}
	}
}

// holdWaker is a channel closed when the next hold begins. A Read takes it
// before its readBlocked check, so a hold in between either fails the check
// or closes the channel.
func (en *entry) holdWaker() <-chan struct{} {
	for {
		if ch := en.holdWake.Load(); ch != nil {
			return *ch
		}
		ch := make(chan struct{})
		if en.holdWake.CompareAndSwap(nil, &ch) {
			return ch
		}
	}
}

// readBlocked is ErrHeld while en is held. Lock-free: it runs on every
// stream Read, and e.mu can be held across database writes.
func (en *entry) readBlocked() error {
	if en.held.Load() {
		return ErrHeld
	}
	return nil
}

// waitApplied waits until gotInfo has handled t (a duplicate is only known
// then), bounded by ctx.
func (en *entry) waitApplied(ctx context.Context, t *atorrent.Torrent) error {
	for {
		en.e.mu.Lock()
		done := en.applied == t || en.t != t
		en.e.mu.Unlock()
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-en.e.closing:
			return ErrClosed
		case <-t.Closed():
			return nil
		case <-time.After(10 * time.Millisecond):
		}
	}
}
