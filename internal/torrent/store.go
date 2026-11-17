package torrent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/anacrolix/torrent/types/infohash"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const timeLayout = "2006-01-02T15:04:05Z"

// record is one row of the torrents table.
type record struct {
	InfoHash    string
	Name        string
	Mode        string
	SaveDir     string
	TargetDir   string // "" = no pending move
	ItemID      *int64
	AddedBy     *int64
	AddedByUID  string // AddedBy's uid ("" = unknown)
	AddedAt     time.Time
	CompletedAt *time.Time
	Uploaded    int64
	Downloaded  int64
	Status      string
	Error       string
	SourceURL   string
	Magnet      string
	Release     json.RawMessage
	// Files is the selection (AddOptions.Files, normalised); nil = whole
	// torrent. Written by insert only: it never changes.
	Files []int
}

type store struct{ db *sql.DB }

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(timeLayout)
}

func (s *store) insert(ctx context.Context, r record) error {
	var rel any
	if len(r.Release) > 0 {
		rel = string(r.Release)
	}
	var files any // NULL = whole torrent, never "[]"
	if len(r.Files) > 0 {
		b, err := json.Marshal(r.Files)
		if err != nil {
			return err
		}
		files = string(b)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO torrents
		(infohash, name, mode, save_dir, target_dir, item_id, added_by, added_by_uid, added_at, completed_at,
		 uploaded, downloaded, status, error, source_url, magnet, release_json, files_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.InfoHash, r.Name, r.Mode, r.SaveDir, nullStr(r.TargetDir), r.ItemID, r.AddedBy, nullStr(r.AddedByUID),
		r.AddedAt.UTC().Format(timeLayout), nullTime(r.CompletedAt), r.Uploaded, r.Downloaded,
		r.Status, nullStr(r.Error), nullStr(r.SourceURL), nullStr(r.Magnet), rel, files)
	return err
}

// updateFiles writes a grown selection (nil = the whole torrent).
func (s *store) updateFiles(ctx context.Context, ih string, files []int) error {
	var v any
	if len(files) > 0 {
		b, err := json.Marshal(files)
		if err != nil {
			return err
		}
		v = string(b)
	}
	return retryBusy(ctx, syncBusyRetry, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE torrents SET files_json=? WHERE infohash=?`, v, ih)
		return err
	})
}

// update writes back every mutable column.
func (s *store) update(ctx context.Context, r record) error {
	return retryBusy(ctx, syncBusyRetry, func() error { return s.updateOnce(ctx, r) })
}

func (s *store) updateOnce(ctx context.Context, r record) error {
	_, err := s.db.ExecContext(ctx, `UPDATE torrents SET name=?, mode=?, save_dir=?, target_dir=?,
		completed_at=?, uploaded=?, downloaded=?, status=?, error=?, magnet=? WHERE infohash=?`,
		r.Name, r.Mode, r.SaveDir, nullStr(r.TargetDir), nullTime(r.CompletedAt), r.Uploaded,
		r.Downloaded, r.Status, nullStr(r.Error), nullStr(r.Magnet), r.InfoHash)
	return err
}

// counters are a torrent's transfer totals for updateCounters (en: whose).
type counters struct {
	en       *entry
	ih       string
	up, down int64
	row      uint64 // persister.lastWrite of ih when they were taken
}

// updateCounters writes only the transfer counters (the monitor's periodic
// save, done without the engine lock), all of cs in one transaction: one
// commit, however many torrents.
func (s *store) updateCounters(ctx context.Context, cs []counters) error {
	return retryBusy(ctx, syncBusyRetry, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		st, err := tx.PrepareContext(ctx, `UPDATE torrents SET uploaded=?, downloaded=? WHERE infohash=?`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, c := range cs {
			if _, err := st.ExecContext(ctx, c.up, c.down, c.ih); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

func (s *store) delete(ctx context.Context, ih string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM torrents WHERE infohash=?`, ih); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM torrent_pieces WHERE infohash=?`, ih); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *store) all(ctx context.Context) ([]record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT infohash, name, mode, save_dir, target_dir, item_id,
		added_by, added_at, completed_at, uploaded, downloaded, status, error, source_url, magnet,
		release_json, files_json, COALESCE(added_by_uid, '') FROM torrents ORDER BY added_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []record
	for rows.Next() {
		var r record
		var target, completed, errStr, src, magnet, rel, files sql.NullString
		var item, by sql.NullInt64
		var added string
		if err := rows.Scan(&r.InfoHash, &r.Name, &r.Mode, &r.SaveDir, &target, &item, &by, &added,
			&completed, &r.Uploaded, &r.Downloaded, &r.Status, &errStr, &src, &magnet, &rel, &files, &r.AddedByUID); err != nil {
			return nil, err
		}
		r.TargetDir, r.Error, r.SourceURL, r.Magnet = target.String, errStr.String, src.String, magnet.String
		if item.Valid {
			v := item.Int64
			r.ItemID = &v
		}
		if by.Valid {
			v := by.Int64
			r.AddedBy = &v
		}
		r.AddedAt, _ = time.Parse(timeLayout, added)
		if completed.Valid {
			if t, err := time.Parse(timeLayout, completed.String); err == nil {
				r.CompletedAt = &t
			}
		}
		if rel.Valid {
			r.Release = json.RawMessage(rel.String)
		}
		if files.Valid {
			if err := json.Unmarshal([]byte(files.String), &r.Files); err != nil {
				return nil, fmt.Errorf("torrent %s: files_json: %w", r.InfoHash, err)
			}
			if len(r.Files) == 0 {
				r.Files = nil
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// pieceDB implements anacrolix storage.PieceCompletion on top of the Vault
// database (torrent_pieces). anacrolix's own sqlite completion needs cgo and
// bolt adds a second single-writer file; this keeps everything in vault.db
// and works with CGO_ENABLED=0. Pieces without a row report Ok=false, which
// makes anacrolix hash-check them on start (used for importing existing data).
//
// Fresh torrents (nothing on disk yet) report unknown pieces as known
// incomplete instead, so anacrolix skips the initial hash pass: that pass
// racing with the first downloads was observed to stall transfers in
// anacrolix v1.61. markAllIncomplete then persists explicit rows.
//
// Set never blocks anacrolix (it calls Set with its client lock held, e.g.
// Completion → markIncompletePieces): the value goes to pending, which reads
// prefer to the row, and one background flusher writes pending values in
// batches, retrying SQLITE_BUSY until they land. A lost "not complete" would
// otherwise leave a stale complete=1 row: anacrolix re-reads completion from
// storage, so the piece would be seeded with bad data, and after a restart
// trusted without a hash check.
type pieceDB struct {
	db       *sql.DB
	mu       sync.Mutex
	fresh    map[string]bool
	pending  map[metainfo.PieceKey]bool // set in memory, not yet in the db
	wmu      sync.Mutex                 // one write attempt at a time: a forget never races a batch
	warn     logLimiter                 // read failures
	warnW    logLimiter                 // write failures
	retry    time.Duration              // retry budget override (tests); 0 = the defaults
	kick     chan struct{}
	quit     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func newPieceDB(db *sql.DB) *pieceDB {
	p := &pieceDB{db: db, fresh: map[string]bool{}, pending: map[metainfo.PieceKey]bool{},
		kick: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
	go p.flusher()
	return p
}

// Retry budgets for SQLITE_BUSY (each attempt also waits busy_timeout, 5 s).
// With synchronous=NORMAL a commit holds the write lock for milliseconds;
// these cover a disk stalled for longer. Foreground writes (under the
// engine lock, which API requests wait for) give up sooner; the background
// flusher keeps its values pending and tries again.
const (
	flushBusyRetry = 30 * time.Second
	syncBusyRetry  = 5 * time.Second
	flushBatch     = 1000
)

func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// retryBusy runs fn until it returns anything but SQLITE_BUSY, ctx ends or
// budget has passed (then the last error).
func retryBusy(ctx context.Context, budget time.Duration, fn func() error) error {
	deadline := time.Now().Add(budget)
	wait := 50 * time.Millisecond
	for {
		err := fn()
		if !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(min(wait, time.Until(deadline)+time.Millisecond)):
		}
		wait = min(wait*2, time.Second)
	}
}

func (p *pieceDB) budget(def time.Duration) time.Duration {
	if p.retry > 0 {
		return p.retry
	}
	return def
}

func (p *pieceDB) pendingValue(pk metainfo.PieceKey) (complete, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	complete, ok = p.pending[pk]
	return
}

func (p *pieceDB) pendingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

// dropPending forgets pending values of ih's pieces in [begin, end) (end<0:
// all of them). p.wmu must be held, so no batch writes one back after.
func (p *pieceDB) dropPending(ih string, begin, end int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for pk := range p.pending {
		if pk.InfoHash.HexString() == ih && pk.Index >= begin && (end < 0 || pk.Index < end) {
			delete(p.pending, pk)
		}
	}
}

// flusher writes pending values until stop.
func (p *pieceDB) flusher() {
	defer close(p.done)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-p.quit:
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		select {
		case <-p.kick:
		case <-p.quit:
			return
		}
		for p.pendingCount() > 0 {
			err := retryBusy(ctx, p.budget(flushBusyRetry), func() error { return p.writeBatch(ctx) })
			if err == nil {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			p.warnW.warn("store piece completion: kept in memory, retrying", "pending", p.pendingCount(), "err", err)
			select {
			case <-time.After(time.Second):
			case <-p.quit:
				return
			}
		}
	}
}

// writeBatch writes up to flushBatch pending values in one transaction and
// drops those that still hold the value written (a newer Set stays).
func (p *pieceDB) writeBatch(ctx context.Context) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.mu.Lock()
	batch := make(map[metainfo.PieceKey]bool, min(len(p.pending), flushBatch))
	for pk, v := range p.pending {
		if len(batch) == flushBatch {
			break
		}
		batch[pk] = v
	}
	p.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `INSERT INTO torrent_pieces(infohash, piece, complete) VALUES (?,?,?)
		ON CONFLICT(infohash, piece) DO UPDATE SET complete=excluded.complete`)
	if err != nil {
		return err
	}
	defer st.Close()
	for pk, v := range batch {
		if _, err := st.ExecContext(ctx, pk.InfoHash.HexString(), pk.Index, v); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.mu.Lock()
	for pk, v := range batch {
		if cur, ok := p.pending[pk]; ok && cur == v {
			delete(p.pending, pk)
		}
	}
	p.mu.Unlock()
	return nil
}

// flushFor writes what is pending now, for at most timeout (beside the
// flusher; p.wmu serialises them).
func (p *pieceDB) flushFor(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for p.pendingCount() > 0 && ctx.Err() == nil {
		if err := retryBusy(ctx, timeout, func() error { return p.writeBatch(ctx) }); err != nil {
			return
		}
	}
}

// stop ends the flusher after a last flush of at most timeout; values still
// pending then are lost (logged). Engine.Close calls it.
func (p *pieceDB) stop(timeout time.Duration) {
	p.stopOnce.Do(func() {
		close(p.quit)
		<-p.done
		p.flushFor(timeout)
		if n := p.pendingCount(); n > 0 {
			slog.Default().With("component", "torrent").Error("piece completion not stored at shutdown", "pieces", n)
		}
	})
}

func (p *pieceDB) readFailed(err error) {
	p.warn.warn("read piece completion: treated as unknown (not requested or seeded until it reads again or the server restarts)", "err", err)
}

func (p *pieceDB) setFresh(ih string, fresh bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if fresh {
		p.fresh[ih] = true
	} else {
		delete(p.fresh, ih)
	}
}

func (p *pieceDB) isFresh(ih string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.fresh[ih]
}

// markAllIncomplete stores complete=0 for every piece without a row and
// clears the fresh flag.
func (p *pieceDB) markAllIncomplete(ctx context.Context, ih string, numPieces int) error {
	if err := retryBusy(ctx, p.budget(syncBusyRetry), func() error { return p.markAllIncompleteOnce(ctx, ih, numPieces) }); err != nil {
		return err
	}
	p.setFresh(ih, false)
	return nil
}

func (p *pieceDB) markAllIncompleteOnce(ctx context.Context, ih string, numPieces int) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO torrent_pieces(infohash, piece, complete) VALUES (?,?,0)`)
	if err != nil {
		return err
	}
	defer st.Close()
	for i := 0; i < numPieces; i++ {
		if _, err := st.ExecContext(ctx, ih, i); err != nil {
			return err
		}
	}
	return tx.Commit()
}

var (
	_ storage.PieceCompletion          = (*pieceDB)(nil)
	_ storage.PieceCompletionGetRanger = (*pieceDB)(nil)
)

func (p *pieceDB) Get(pk metainfo.PieceKey) (c storage.Completion, err error) {
	if v, ok := p.pendingValue(pk); ok {
		return storage.Completion{Ok: true, Complete: v}, nil
	}
	var complete bool
	err = p.db.QueryRow(`SELECT complete FROM torrent_pieces WHERE infohash=? AND piece=?`,
		pk.InfoHash.HexString(), pk.Index).Scan(&complete)
	if errors.Is(err, sql.ErrNoRows) {
		if p.isFresh(pk.InfoHash.HexString()) {
			c.Ok = true
		}
		return c, nil
	}
	if err != nil {
		// Never an error to anacrolix: on one it disallows downloads by
		// itself, which can trip its consistency panic (see holds.go).
		// Unknown instead: the piece is neither requested nor seeded until
		// its completion reads again or the server restarts (see safePiece).
		p.readFailed(err)
		return storage.Completion{}, nil
	}
	c.Ok, c.Complete = true, complete
	return c, nil
}

func (p *pieceDB) GetRange(ih infohash.T, begin, end int) iter.Seq[storage.Completion] {
	return func(yield func(storage.Completion) bool) {
		fresh := p.isFresh(ih.HexString())
		// pending first: a value the flusher stores and drops from pending
		// between the two reads is then still seen (as with Get)
		p.mu.Lock()
		var pend map[int]bool
		for pk, v := range p.pending {
			if pk.InfoHash == ih && pk.Index >= begin && pk.Index < end {
				if pend == nil {
					pend = map[int]bool{}
				}
				pend[pk.Index] = v
			}
		}
		p.mu.Unlock()
		known := make(map[int]bool, end-begin)
		rows, err := p.db.Query(`SELECT piece, complete FROM torrent_pieces
			WHERE infohash=? AND piece>=? AND piece<?`, ih.HexString(), begin, end)
		if err == nil {
			for rows.Next() {
				var i int
				var c bool
				if rows.Scan(&i, &c) == nil {
					known[i] = c
				}
			}
			err = rows.Err()
			rows.Close()
		}
		if err != nil {
			p.readFailed(err) // reported as unknown, never as an error (see Get)
		}
		for i := begin; i < end; i++ {
			var c storage.Completion
			if v, ok := pend[i]; ok {
				c.Ok, c.Complete = true, v
			} else if err != nil {
				// unknown
			} else if v, ok := known[i]; ok {
				c.Ok, c.Complete = true, v
			} else if fresh {
				c.Ok = true
			}
			if !yield(c) {
				return
			}
		}
	}
}

// Set records the value in memory (reads see it at once) and leaves the
// write to the flusher: it never blocks anacrolix, and never fails.
func (p *pieceDB) Set(pk metainfo.PieceKey, complete bool) error {
	p.mu.Lock()
	p.pending[pk] = complete
	p.mu.Unlock()
	select {
	case p.kick <- struct{}{}:
	default: // a flush is already due
	}
	return nil
}

func (p *pieceDB) forgetPieces(ctx context.Context, ih string, begin, end int) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	err := retryBusy(ctx, p.budget(syncBusyRetry), func() error {
		_, err := p.db.ExecContext(ctx, `DELETE FROM torrent_pieces WHERE infohash=? AND piece>=? AND piece<?`, ih, begin, end)
		return err
	})
	if err == nil { // after the delete (p.wmu held: no batch in between); on failure pending still overrides the rows
		p.dropPending(ih, begin, end)
	}
	return err
}

// anyComplete reports whether any piece of ih is recorded complete.
func (p *pieceDB) anyComplete(ih string) bool {
	p.mu.Lock()
	for pk, v := range p.pending {
		if v && pk.InfoHash.HexString() == ih {
			p.mu.Unlock()
			return true
		}
	}
	p.mu.Unlock()
	var one int
	err := p.db.QueryRow(`SELECT 1 FROM torrent_pieces WHERE infohash=? AND complete=1 LIMIT 1`, ih).Scan(&one)
	return err == nil
}

func (p *pieceDB) forget(ctx context.Context, ih string) error {
	p.setFresh(ih, false)
	p.wmu.Lock()
	defer p.wmu.Unlock()
	err := retryBusy(ctx, p.budget(syncBusyRetry), func() error {
		_, err := p.db.ExecContext(ctx, `DELETE FROM torrent_pieces WHERE infohash=?`, ih)
		return err
	})
	if err == nil { // see forgetPieces
		p.dropPending(ih, 0, -1)
	}
	return err
}

// Close is a no-op: the *sql.DB belongs to the caller of New, and the same
// completion instance is shared by every per-torrent storage.
func (p *pieceDB) Close() error { return nil }
