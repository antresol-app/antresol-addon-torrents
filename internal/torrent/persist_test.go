package torrent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// A pause waiting for the database's write lock (a commit stalled on the
// busy disk) never holds the engine: the list, a torrent's status and the
// disk check answer meanwhile, and the pause lands once the lock is free.
func TestRowWritesOffTheEngineLock(t *testing.T) {
	e, fx := newSeeder(t, engOpts{})
	tx, err := e.db.Begin() // BEGIN IMMEDIATE: the write lock
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE settings SET value=value`); err != nil {
		t.Fatal(err)
	}
	paused := make(chan error, 1)
	go func() { paused <- e.Pause(context.Background(), fx.ih) }()
	time.Sleep(200 * time.Millisecond) // the pause is waiting for the lock by now
	t0 := time.Now()
	s, ok := status(e, fx.ih)
	_ = e.Offline()
	if el := time.Since(t0); el > 500*time.Millisecond {
		t.Fatalf("List waited %v behind the pause's write", el)
	}
	if !ok || s.Status != StatusPaused {
		t.Fatalf("status during the write: %+v", s)
	}
	select {
	case err := <-paused:
		t.Fatalf("the pause returned before its row was written: %v", err)
	default:
	}
	tx.Commit()
	select {
	case err := <-paused:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the pause never returned")
	}
	var st string
	if err := e.db.QueryRow(`SELECT status FROM torrents WHERE infohash=?`, fx.ih).Scan(&st); err != nil || st != StatusPaused {
		t.Fatalf("row status %q %v", st, err)
	}
}

// holdDBWriteLock holds the database's write lock (a commit stalled on the
// disk) until the returned func is called.
func holdDBWriteLock(t *testing.T, e *Engine) func() {
	t.Helper()
	tx, err := e.db.Begin() // BEGIN IMMEDIATE
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE settings SET value=value`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { tx.Commit() }) }
	t.Cleanup(release)
	return release
}

func returnsWithin(t *testing.T, what string, d time.Duration, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(d):
		t.Fatalf("%s did not return within %v", what, d)
		return nil
	}
}

// A torrent deleted while its row waits in the queue behind another
// torrent's stalled write: the pause waiting for that row returns (the
// torrent is gone), and the engine still closes.
func TestDeleteReleasesRowWaiters(t *testing.T) {
	dataDir := t.TempDir()
	e := openEngine(t, dataDir, engOpts{})
	var ihs []string
	for i := 0; i < 2; i++ {
		src := t.TempDir()
		fx := newFixtureSize(t, src, 1<<20)
		if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
		ihs = append(ihs, fx.ih)
	}
	release := holdDBWriteLock(t, e)
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- e.Pause(context.Background(), ihs[0]) }() // the writer is stuck on this row
	time.Sleep(200 * time.Millisecond)
	go func() { second <- e.Pause(context.Background(), ihs[1]) }() // queued behind it
	time.Sleep(200 * time.Millisecond)
	removed := make(chan error, 1)
	go func() { removed <- e.Remove(context.Background(), ihs[1], false) }()
	time.Sleep(200 * time.Millisecond)
	release()
	returnsWithin(t, "the first pause", 20*time.Second, first)
	if err := returnsWithin(t, "the pause of the deleted torrent", 20*time.Second, second); err != nil && !errors.Is(err, ErrNotFound) {
		t.Fatalf("pause of the deleted torrent: %v", err)
	}
	if err := returnsWithin(t, "the delete", 20*time.Second, removed); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	returnsWithin(t, "Close", 20*time.Second, closed)
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM torrents WHERE infohash=?`, ihs[1]).Scan(&n)
	if n != 0 {
		t.Fatal("the deleted torrent's row came back")
	}
}

// A delete that fails while a completion waits for its row: the torrent
// stays, so the completion still runs the import hooks (once).
func TestFailedDeleteKeepsCompletionHooks(t *testing.T) {
	e, fx := newSeeder(t, engOpts{})
	var hooks hookCount
	e.OnComplete(hooks.add)
	if _, err := e.db.Exec(`CREATE TRIGGER no_delete BEFORE DELETE ON torrents BEGIN SELECT RAISE(ABORT, 'нельзя'); END`); err != nil {
		t.Fatal(err)
	}
	release := holdDBWriteLock(t, e)
	e.ps.put(record{InfoHash: "0000000000000000000000000000000000000000"}) // the writer is stuck on this
	time.Sleep(100 * time.Millisecond)
	e.mu.Lock()
	en := e.entries[fx.ih]
	en.rec.CompletedAt = nil // as if it completed only now
	tt := en.t
	e.mu.Unlock()
	completed := make(chan error, 1)
	go func() { e.completed(en, tt); completed <- nil }()
	time.Sleep(200 * time.Millisecond) // its row is queued, it waits
	removed := make(chan error, 1)
	go func() { removed <- e.Remove(context.Background(), fx.ih, false) }()
	time.Sleep(200 * time.Millisecond)
	release()
	if err := returnsWithin(t, "the delete", 20*time.Second, removed); err == nil {
		t.Fatal("the delete went through")
	}
	returnsWithin(t, "the completion", 20*time.Second, completed)
	if _, ok := e.Get(fx.ih); !ok {
		t.Fatal("the torrent is gone")
	}
	if n := hooks.get(); n != 1 {
		t.Fatalf("OnComplete ran %d times", n)
	}
}

// The persister alone (no writer running): a waiter of a row dropped
// before it was written returns at once, with errRowDropped.
func TestDropWakesWaiter(t *testing.T) {
	p := &persister{pending: map[string]queued{}, written: map[string]result{}, kick: make(chan struct{}, 1)}
	p.cond = sync.NewCond(&p.mu)
	v := p.put(record{InfoHash: "aa", Name: "y"})
	waited := make(chan error, 1)
	go func() { waited <- p.wait(context.Background(), "aa", v) }()
	time.Sleep(50 * time.Millisecond)
	p.drop("aa")
	if err := returnsWithin(t, "wait of a dropped row", 2*time.Second, waited); !errors.Is(err, errRowDropped) {
		t.Fatalf("wait: %v", err)
	}
	// a later row of the infohash is waited for as usual
	v = p.put(record{InfoHash: "aa", Name: "z"})
	if w := p.written["aa"]; w.ver >= v {
		t.Fatalf("the new row counts as written: %+v (v %d)", w, v)
	}
}

// A delete never waits under the engine lock for a write of the same
// torrent stalled on the disk; a new torrent of that infohash does (its
// row must not be overwritten by the old one's write in flight).
func TestDropDoesNotWaitForWriteInFlight(t *testing.T) {
	e, fx := newSeeder(t, engOpts{})
	release := holdDBWriteLock(t, e)
	e.mu.Lock()
	r := e.entries[fx.ih].rec
	e.mu.Unlock()
	e.ps.put(r) // in flight, stalled
	time.Sleep(200 * time.Millisecond)
	t0 := time.Now()
	e.ps.drop(fx.ih)
	if el := time.Since(t0); el > 500*time.Millisecond {
		t.Fatalf("drop waited %v for the write in flight", el)
	}
	settled := make(chan error, 1)
	go func() { e.ps.settle(fx.ih); settled <- nil }()
	select {
	case <-settled:
		t.Fatal("settle returned with the write still in flight")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	returnsWithin(t, "settle", 15*time.Second, settled)
}

// A row put for a torrent replaces its queued one (what Close relies on:
// its final rows are put over whatever is still queued).
func TestPutReplacesQueuedRow(t *testing.T) {
	p := &persister{pending: map[string]queued{}, written: map[string]result{}, kick: make(chan struct{}, 1)}
	p.cond = sync.NewCond(&p.mu)
	p.put(record{InfoHash: "aa", Name: "старая"})
	p.put(record{InfoHash: "bb", Name: "другая"})
	v := p.put(record{InfoHash: "aa", Name: "последняя"})
	if q := p.pending["aa"]; q.r.Name != "последняя" || q.ver != v || len(p.order) != 2 {
		t.Fatalf("queued %+v, order %v", q, p.order)
	}
}

// Close with an older row still queued and the database stalled past the
// flush: the rows Close stores last are the ones the next start restores.
func TestCloseFinalRowsWin(t *testing.T) {
	e, fx := newSeeder(t, engOpts{})
	release := holdDBWriteLock(t, e)
	e.mu.Lock()
	old := e.entries[fx.ih].rec
	e.mu.Unlock()
	old.Name, old.Status = "устаревшая строка", StatusPaused
	for i := 0; i < 500; i++ { // the writer is stuck on these (other torrents' rows)
		e.ps.put(record{InfoHash: fmt.Sprintf("%040d", i)})
	}
	time.Sleep(100 * time.Millisecond)
	e.ps.put(old) // queued behind them, older than Close's
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	time.Sleep(6 * time.Second) // past the flush's 5 s
	release()
	returnsWithin(t, "Close", 30*time.Second, closed)
	var name, st string
	if err := e.db.QueryRow(`SELECT name, status FROM torrents WHERE infohash=?`, fx.ih).Scan(&name, &st); err != nil {
		t.Fatal(err)
	}
	if name != fx.name || st != StatusSeeding {
		t.Fatalf("row after Close: %q %q, want %q %q", name, st, fx.name, StatusSeeding)
	}
}

// The latest row of a torrent wins: rows queued in a burst end as the last.
func TestPersisterLatestWins(t *testing.T) {
	e, fx := newSeeder(t, engOpts{})
	for i := 0; i < 20; i++ {
		if err := e.Pause(context.Background(), fx.ih); err != nil {
			t.Fatal(err)
		}
		if err := e.Resume(context.Background(), fx.ih); err != nil {
			t.Fatal(err)
		}
	}
	e.mu.Lock()
	en := e.entries[fx.ih]
	for i := 0; i < 50; i++ {
		en.rec.Error = ""
		e.save(en.rec)
	}
	en.rec.Name = "последнее имя"
	e.save(en.rec)
	e.mu.Unlock()
	e.ps.flush(5 * time.Second)
	var name, st string
	if err := e.db.QueryRow(`SELECT name, status FROM torrents WHERE infohash=?`, fx.ih).Scan(&name, &st); err != nil {
		t.Fatal(err)
	}
	if name != "последнее имя" || st != StatusSeeding {
		t.Fatalf("row %q %q", name, st)
	}
}
