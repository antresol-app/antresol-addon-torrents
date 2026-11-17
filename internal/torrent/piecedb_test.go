package torrent

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"

	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// openShortBusy opens the vault schema, then a second handle on it whose
// busy_timeout is short (the production 5 s would make these tests slow),
// and a locker that can hold the write lock.
func openShortBusy(t *testing.T) (pc *sql.DB, locker *sql.DB) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vault.db")
	d, err := db.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	open := func() *sql.DB {
		h, err := sql.Open("sqlite", "file:"+p+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(50)&_txlock=immediate")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { h.Close() })
		return h
	}
	return open(), open()
}

// holdWriteLock takes the database write lock for d and releases it after
// hold (in the background).
func holdWriteLock(t *testing.T, d *sql.DB, hold time.Duration) <-chan struct{} {
	t.Helper()
	tx, err := d.BeginTx(context.Background(), nil) // _txlock=immediate: the write lock now
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES('lock','x') ON CONFLICT(key) DO UPDATE SET value='y'`); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(hold)
		tx.Commit()
		close(done)
	}()
	return done
}

func pieceRow(t *testing.T, d *sql.DB, ih string, piece int) (complete, ok bool) {
	t.Helper()
	err := d.QueryRow(`SELECT complete FROM torrent_pieces WHERE infohash=? AND piece=?`, ih, piece).Scan(&complete)
	if err == sql.ErrNoRows {
		return false, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return complete, true
}

var testPK = metainfo.PieceKey{InfoHash: metainfo.NewHashFromHex("c08a9acd5f8f68fe578f843285c0be4547202c2f"), Index: 87}

func newTestPieceDB(t *testing.T, d *sql.DB) *pieceDB {
	t.Helper()
	p := newPieceDB(d)
	t.Cleanup(func() { p.stop(time.Second) })
	return p
}

// waitRow waits until the piece's row holds want.
func waitRow(t *testing.T, d *sql.DB, pk metainfo.PieceKey, want bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		c, ok := pieceRow(t, d, pk.InfoHash.HexString(), pk.Index)
		if ok && c == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("piece %d: row complete=%v ok=%v after %v, want complete=%v", pk.Index, c, ok, within, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Set never blocks anacrolix (it calls Set with its client lock held):
// with the database locked it returns at once, the value reads back at
// once, and it lands in the db after the unlock — not lost, as it was
// ("error marking piece not complete ... SQLITE_BUSY").
func TestPieceSetDoesNotBlockWhileLocked(t *testing.T) {
	d, locker := openShortBusy(t)
	p := newTestPieceDB(t, d)
	ih := testPK.InfoHash.HexString()
	p.Set(testPK, true)
	waitRow(t, d, testPK, true, 2*time.Second)

	done := holdWriteLock(t, locker, time.Second) // 20× the busy_timeout
	start := time.Now()
	if err := p.Set(testPK, false); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if el := time.Since(start); el > 40*time.Millisecond { // under one busy_timeout (50 ms)
		t.Fatalf("Set took %v with the db locked", el)
	}
	if c, err := p.Get(testPK); err != nil || !c.Ok || c.Complete {
		t.Fatalf("Get = %+v, %v; want known not complete", c, err)
	}
	for c := range p.GetRange(testPK.InfoHash, testPK.Index, testPK.Index+1) {
		if !c.Ok || c.Complete {
			t.Fatalf("GetRange = %+v; want known not complete", c)
		}
	}
	if c, _ := pieceRow(t, d, ih, testPK.Index); !c {
		t.Fatal("precondition: the row changed while the db was locked")
	}
	<-done
	waitRow(t, d, testPK, false, 3*time.Second)
}

// The flusher keeps retrying past its budget (the value stays pending and
// readable), then writes it; a newer Set wins over the value being written.
func TestPieceFlushRetriesPastBudget(t *testing.T) {
	d, locker := openShortBusy(t)
	p := newTestPieceDB(t, d)
	p.retry = 150 * time.Millisecond
	p.Set(testPK, true)
	waitRow(t, d, testPK, true, 2*time.Second)
	done := holdWriteLock(t, locker, 1500*time.Millisecond) // the flusher's budget runs out at least once
	p.Set(testPK, false)
	time.Sleep(400 * time.Millisecond)
	if c, err := p.Get(testPK); err != nil || !c.Ok || c.Complete {
		t.Fatalf("Get = %+v, %v; want known not complete", c, err)
	}
	other := metainfo.PieceKey{InfoHash: testPK.InfoHash, Index: 3}
	p.Set(other, true)
	<-done
	waitRow(t, d, testPK, false, 4*time.Second)
	waitRow(t, d, other, true, 4*time.Second)
	if n := p.pendingCount(); n != 0 {
		t.Fatalf("%d values still pending", n)
	}
}

// forgetPieces and forget retry a locked db, and drop pending values so the
// flusher never writes them back after the delete.
func TestPieceForgetRetriesAndDropsPending(t *testing.T) {
	d, locker := openShortBusy(t)
	p := newTestPieceDB(t, d)
	ih := testPK.InfoHash.HexString()
	p.Set(testPK, true)
	waitRow(t, d, testPK, true, 2*time.Second)
	done := holdWriteLock(t, locker, 400*time.Millisecond)
	if err := p.forgetPieces(context.Background(), ih, 0, 100); err != nil {
		t.Fatalf("forgetPieces while locked: %v", err)
	}
	<-done
	if _, ok := pieceRow(t, d, ih, testPK.Index); ok {
		t.Fatal("row still there after forgetPieces")
	}

	done = holdWriteLock(t, locker, 400*time.Millisecond)
	p.Set(testPK, true) // pending: the db is locked
	if err := p.forget(context.Background(), ih); err != nil {
		t.Fatal(err)
	}
	<-done
	if c, err := p.Get(testPK); err != nil || c.Ok {
		t.Fatalf("after forget: Get = %+v, %v; want unknown", c, err)
	}
	time.Sleep(300 * time.Millisecond) // give a stale flush the chance to show
	if _, ok := pieceRow(t, d, ih, testPK.Index); ok {
		t.Fatal("a pending value was written back after forget")
	}
}

// stop flushes what is still pending (Engine.Close calls it).
func TestPieceStopFlushes(t *testing.T) {
	d, locker := openShortBusy(t)
	p := newPieceDB(d)
	done := holdWriteLock(t, locker, 300*time.Millisecond)
	p.Set(testPK, false)
	p.stop(5 * time.Second)
	<-done
	if c, ok := pieceRow(t, d, testPK.InfoHash.HexString(), testPK.Index); !ok || c {
		t.Fatalf("after stop: complete=%v ok=%v, want stored 0", c, ok)
	}
}

// A forget whose delete fails keeps the pending values: they still override
// the rows it could not delete.
func TestPieceForgetFailureKeepsPending(t *testing.T) {
	d, locker := openShortBusy(t)
	p := newTestPieceDB(t, d)
	p.retry = 100 * time.Millisecond
	ih := testPK.InfoHash.HexString()
	p.Set(testPK, true)
	waitRow(t, d, testPK, true, 2*time.Second)
	done := holdWriteLock(t, locker, time.Second)
	p.Set(testPK, false)
	if err := p.forgetPieces(context.Background(), ih, 0, 100); err == nil {
		t.Fatal("forgetPieces succeeded under a lock longer than its retries")
	}
	if c, err := p.Get(testPK); err != nil || !c.Ok || c.Complete {
		t.Fatalf("Get = %+v, %v; want the pending not complete, not the stale row", c, err)
	}
	<-done
	waitRow(t, d, testPK, false, 4*time.Second)
}

// flushFor writes pending values without stopping the flusher (Engine.Close
// runs it before saving the counters).
func TestPieceFlushForWhileRunning(t *testing.T) {
	d, locker := openShortBusy(t)
	p := newTestPieceDB(t, d)
	done := holdWriteLock(t, locker, 300*time.Millisecond)
	p.Set(testPK, false)
	p.flushFor(5 * time.Second)
	<-done
	if c, ok := pieceRow(t, d, testPK.InfoHash.HexString(), testPK.Index); !ok || c {
		t.Fatalf("after flushFor: complete=%v ok=%v, want stored 0", c, ok)
	}
	p.Set(testPK, true) // the flusher still runs
	waitRow(t, d, testPK, true, 2*time.Second)
}
