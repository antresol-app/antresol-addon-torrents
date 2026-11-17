package torrent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// persister writes the torrents' rows off the engine lock. The engine
// changes a torrent under e.mu and used to write its row there too; a write
// waits for SQLite's one write lock, which a commit stalled on the busy disk
// holds for seconds (busy_timeout 5 s, retried for 5 s more). Meanwhile
// every List, Get and Status waited for e.mu: «Загрузки», the downloads
// badge of every page, the library's pending cards (polled every 2 s) and
// the disk gauge froze for seconds. Now the row goes to a queue under e.mu
// (put) and one writer stores it after: the latest row of a torrent always
// wins (an older one queued before is never written after it), and a
// caller that reports the write's error waits for it with e.mu released
// (wait).
type persister struct {
	st  *store
	log *slog.Logger

	mu      sync.Mutex
	cond    *sync.Cond
	pending map[string]queued // by infohash: the latest row not written yet
	order   []string          // infohashes in the order they were queued
	ver     uint64            // of the last put
	written map[string]result // by infohash: the latest write done
	closed  bool

	// inflight: the infohash of the row being written ("" = none);
	// settle waits it out before a new torrent of it is inserted
	inflight string
	kick     chan struct{}
	done     chan struct{}
}

// errRowDropped: the row a waiter waits for was dropped before it was
// written (the torrent was deleted meanwhile).
var errRowDropped = errors.New("torrent: row dropped: the torrent was deleted")

type queued struct {
	r   record
	ver uint64
}

type result struct {
	ver uint64
	err error
}

func newPersister(st *store, log *slog.Logger) *persister {
	p := &persister{st: st, log: log, pending: map[string]queued{}, written: map[string]result{},
		kick: make(chan struct{}, 1), done: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	go p.run()
	return p
}

// put queues r (the latest row of its torrent) and returns its version
// for wait. Never blocks on the database.
func (p *persister) put(r record) uint64 {
	p.mu.Lock()
	p.ver++
	if _, ok := p.pending[r.InfoHash]; !ok {
		p.order = append(p.order, r.InfoHash)
	}
	p.pending[r.InfoHash] = queued{r, p.ver}
	v := p.ver
	p.mu.Unlock()
	select {
	case p.kick <- struct{}{}:
	default:
	}
	return v
}

// wait waits until the row of ih at version v (or a later one) is
// written, and returns that write's error; ctx ending stops the wait, not
// the write.
func (p *persister) wait(ctx context.Context, ih string, v uint64) error {
	stop := context.AfterFunc(ctx, func() {
		p.mu.Lock()
		p.cond.Broadcast()
		p.mu.Unlock()
	})
	defer stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if w, ok := p.written[ih]; ok && w.ver >= v {
			return w.err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.cond.Wait()
	}
}

// lastWrite is the version of the latest row of ih written or dropped (0:
// none known). The monitor saves a torrent's counters again when it moved:
// a full row written late carries the counters it was folded with.
func (p *persister) lastWrite(ih string) uint64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written[ih].ver
}

// drop forgets the queued row of ih: the row is about to be deleted. Its
// waiters return errRowDropped. It never waits for a write in flight (it
// runs under the engine lock): that write, an UPDATE, changes nothing once
// the row is gone; a new torrent of ih settles first.
func (p *persister) drop(ih string) {
	p.mu.Lock()
	delete(p.pending, ih)
	p.written[ih] = result{ver: p.ver, err: errRowDropped} // every version so far
	p.cond.Broadcast()
	p.mu.Unlock()
}

// settle waits until no row of ih is being written (a new torrent of ih
// is about to be inserted: the old one's write must not land on it).
func (p *persister) settle(ih string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.inflight == ih {
		p.cond.Wait()
	}
}

// run writes the queued rows, oldest first, until close.
func (p *persister) run() {
	defer close(p.done)
	for {
		if !p.writeOne() {
			p.mu.Lock()
			closed := p.closed
			p.mu.Unlock()
			if closed {
				return
			}
			<-p.kick
		}
	}
}

// writeOne writes the oldest queued row; false when none is queued.
func (p *persister) writeOne() bool {
	p.mu.Lock()
	var q queued
	found := false
	for len(p.order) > 0 && !found {
		ih := p.order[0]
		p.order = p.order[1:]
		q, found = p.pending[ih]
		delete(p.pending, ih)
	}
	if found {
		p.inflight = q.r.InfoHash
	}
	p.mu.Unlock()
	if !found {
		return false
	}
	err := p.st.update(context.Background(), q.r)
	if err != nil {
		p.log.Error("persist torrent", "infohash", q.r.InfoHash, "err", err)
	}
	p.mu.Lock()
	p.inflight = ""
	if w := p.written[q.r.InfoHash]; q.ver > w.ver {
		p.written[q.r.InfoHash] = result{q.ver, err}
	}
	if len(p.written) > 4096 { // only waiters read it: forget old results
		for k, w := range p.written {
			if w.ver < q.ver-1024 {
				delete(p.written, k)
			}
		}
	}
	p.cond.Broadcast()
	p.mu.Unlock()
	return true
}

// flush waits until what is queued now is written, for at most timeout
// (Close); false: rows are still queued.
func (p *persister) flush(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		idle := len(p.pending) == 0 && p.inflight == ""
		p.mu.Unlock()
		if idle {
			return true
		}
		select {
		case p.kick <- struct{}{}:
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// close ends the writer once the queue is written, waiting at most
// timeout (the writer goes on alone after that, until the process ends).
func (p *persister) close(timeout time.Duration) {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	select {
	case p.kick <- struct{}{}:
	default:
	}
	select {
	case <-p.done:
	case <-time.After(timeout):
	}
}
