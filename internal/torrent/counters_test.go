package torrent

import (
	"context"
	"testing"
	"time"
)

// The monitor's periodic save of the transfer counters writes only the
// torrents whose counters moved, all in one transaction: a hundred idle
// seeds cost no write at all (each was a commit of its own, every 30 s,
// each taking the database's one write lock).
func TestCountersSavedOnlyWhenChanged(t *testing.T) {
	dataDir := t.TempDir()
	e := openEngine(t, dataDir, engOpts{persist: 200 * time.Millisecond})
	var ihs []string
	for i := 0; i < 3; i++ {
		src := t.TempDir()
		fx := newFixtureSize(t, src, 1<<20)
		if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
			t.Fatal(err)
		}
		waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
		ihs = append(ihs, fx.ih)
	}
	for _, q := range []string{
		`CREATE TABLE counter_writes (infohash TEXT)`,
		`CREATE TRIGGER counter_writes AFTER UPDATE OF uploaded, downloaded ON torrents BEGIN INSERT INTO counter_writes VALUES (new.infohash); END`,
	} {
		if _, err := e.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	writes := func() map[string]int {
		t.Helper()
		rows, err := e.db.Query(`SELECT infohash, COUNT(*) FROM counter_writes GROUP BY infohash`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var ih string
			var n int
			rows.Scan(&ih, &n)
			out[ih] = n
		}
		return out
	}
	time.Sleep(time.Second) // the first saves after start, whatever they write
	e.db.Exec(`DELETE FROM counter_writes`)
	time.Sleep(time.Second) // several saves of idle seeds
	if w := writes(); len(w) != 0 {
		t.Fatalf("idle seeds written: %v", w)
	}
	// one torrent's counters move: only it is written, with its totals
	e.mu.Lock()
	e.entries[ihs[1]].baseUp += 12345
	e.mu.Unlock()
	time.Sleep(time.Second)
	w := writes()
	if len(w) != 1 || w[ihs[1]] != 1 {
		t.Fatalf("writes after one torrent's upload: %v", w)
	}
	var up int64
	if err := e.db.QueryRow(`SELECT uploaded FROM torrents WHERE infohash=?`, ihs[1]).Scan(&up); err != nil || up < 12345 {
		t.Fatalf("uploaded %d %v", up, err)
	}
}

// A full row queued with older counters that lands after the monitor's
// counter save (two writers, either order) is healed by the next save,
// though the counters did not move since.
func TestCountersHealAfterOlderFullRow(t *testing.T) {
	e, fx := newSeeder(t, engOpts{persist: 200 * time.Millisecond})
	e.mu.Lock()
	e.entries[fx.ih].baseUp += 777
	stale := e.entries[fx.ih].rec
	e.mu.Unlock()
	uploaded := func() int64 {
		var up int64
		if err := e.db.QueryRow(`SELECT uploaded FROM torrents WHERE infohash=?`, fx.ih).Scan(&up); err != nil {
			t.Fatal(err)
		}
		return up
	}
	deadline := time.Now().Add(5 * time.Second)
	for uploaded() < 777 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if uploaded() < 777 {
		t.Fatal("counters not saved")
	}
	stale.Uploaded = 0 // a row folded before the upload, written late
	e.ps.put(stale)
	e.ps.flush(5 * time.Second)
	deadline = time.Now().Add(3 * time.Second)
	for uploaded() < 777 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if up := uploaded(); up < 777 {
		t.Fatalf("uploaded %d after a late full row: never healed", up)
	}
}
