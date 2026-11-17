package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	atorrent "github.com/anacrolix/torrent"

	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

const holdChildEnv = "VAULT_TORRENT_HOLD_CHILD"

// anacrolix v1.61 panics in checkPendingPiecesMatchesRequestOrder when a
// torrent with pending pieces has downloads disallowed (see holds.go). The
// check runs on the 1st, 2nd, 4th, 8th … needData call of the process, so
// each scenario runs in a fresh child process (a small global counter) and
// drives 2^20 needData calls through Torrent.Seeding (with
// DisableAggressiveUpload): every power of two in that range runs the
// check on the held torrent. The old way (DisallowDataDownload) must crash
// the child; every hold the engine has must not.
func TestHoldsNeverTripAnacrolixCheck(t *testing.T) {
	if mode := os.Getenv(holdChildEnv); mode != "" {
		holdChild(t, mode)
		return
	}
	if testing.Short() {
		t.Skip("spawns child processes")
	}
	// "dup" is a smoke test: a duplicate is blocked before any piece is
	// wanted, so even the old way never tripped there. "completion-db" and
	// "completion-stat" make reading a piece's completion fail (SQLite
	// error; a non-ENOENT stat error on the media disk): anacrolix then
	// disallows downloads on its own (torrent.go:1780-1783).
	for _, mode := range []string{"disallow", "pause", "space", "writeerr", "dup", "stream", "resume", "completion-db", "completion-stat"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "completion-stat" && os.Geteuid() == 0 {
				t.Skip("root ignores the chmod 0 that makes the stat fail")
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestHoldsNeverTripAnacrolixCheck$", "-test.v")
			cmd.Env = append(os.Environ(), holdChildEnv+"="+mode)
			out, err := cmd.CombinedOutput()
			crashed := strings.Contains(string(out), "piece request order has")
			if mode == "disallow" {
				if err == nil || !crashed {
					t.Fatalf("the old way did not crash (err %v):\n%s", err, tail(out))
				}
				return
			}
			if err != nil || crashed || !strings.Contains(string(out), "HOLD CHILD OK") {
				t.Fatalf("err %v:\n%s", err, tail(out))
			}
		})
	}
}

func tail(b []byte) string {
	if len(b) > 3000 {
		b = b[len(b)-3000:]
	}
	return string(b)
}

func holdChild(t *testing.T, mode string) {
	ctx := context.Background()
	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true,
		StallTimeout: time.Hour, tick: 100 * time.Millisecond, noAggressiveUpload: true}
	e, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	// No deferred Close: in the "disallow" child the panic leaves the
	// client lock held, and Close would hang instead of letting it crash.
	fx := newMultiFixture(t, 16<<10, 64<<10, 64<<10, 64<<10)
	ih := fx.ih
	addMode := ModeDownload
	if mode == "stream" {
		addMode = ModeStream
	}
	save := t.TempDir()
	h, err := e.AddTorrentFile(ctx, fx.metafile, AddOptions{Mode: addMode, SaveDir: save, Files: []int{0, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatal(err)
	}
	var idle io.ReadSeekCloser
	if mode == "stream" {
		// A blocked read raises priorities of its own (no peers: they stay
		// pending through the hold).
		rctx, cancel := context.WithCancel(ctx)
		defer cancel()
		r, err := h.NewReader(rctx, 2)
		if err != nil {
			t.Fatal(err)
		}
		go r.Read(make([]byte, 1))
		if idle, err = h.NewReader(ctx, 0); err != nil {
			t.Fatal(err)
		}
	}
	at := liveTorrent(e, ih)
	pendingWait(t, at)

	switch mode {
	case "disallow":
		at.DisallowDataDownload() // what the engine used to do
	case "pause", "stream", "resume":
		if err := e.Pause(ctx, ih); err != nil {
			t.Fatal(err)
		}
		if mode == "resume" {
			if err := e.Resume(ctx, ih); err != nil {
				t.Fatal(err)
			}
		}
	case "space":
		if err := e.HoldForSpace(ctx, ih); err != nil {
			t.Fatal(err)
		}
	case "writeerr":
		e.mu.Lock()
		en := e.entries[ih]
		e.mu.Unlock()
		e.writeFailed(en, at, errors.New("no space left on device"))
		if s, _ := status(e, ih); !strings.HasPrefix(s.Error, writeErrorPrefix) {
			t.Fatalf("status %+v", s)
		}
	case "completion-db":
		if _, err := d.Exec(`DROP TABLE torrent_pieces`); err != nil {
			t.Fatal(err)
		}
		at.Piece(0).UpdateCompletion() // one piece read fails; the rest stay pending
	case "completion-stat":
		// Piece 0 recorded complete, its file unreadable (EACCES, not
		// ENOENT): the file storage's size check fails.
		if _, err := d.Exec(`INSERT OR REPLACE INTO torrent_pieces(infohash, piece, complete) VALUES (?,0,1)`, ih); err != nil {
			t.Fatal(err)
		}
		fx.writeFiles(t, save, []int{0})
		dir := filepath.Join(save, albumName)
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		// the stat really fails with EACCES (else the mode proves nothing)
		if _, err := os.Stat(filepath.Join(dir, "any")); !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("stat under the unreadable dir: %v, want a permission error", err)
		}
		at.Piece(0).UpdateCompletion()
	case "dup":
		e.Close()
		e = nil
		at = dupChild(t)
	}
	if mode == "stream" {
		if _, err := h.NewReader(ctx, 0); !errors.Is(err, ErrHeld) {
			t.Fatalf("new reader on a paused torrent: %v", err)
		}
		if _, err := idle.Read(make([]byte, 1)); !errors.Is(err, ErrHeld) {
			t.Fatalf("read on a paused torrent: %v", err)
		}
	}
	if mode != "disallow" && mode != "resume" && !strings.HasPrefix(mode, "completion") {
		if n := pendingCount(at); n != 0 && mode != "stream" {
			t.Fatalf("%d pieces still wanted while held", n)
		}
	}
	// Seeding() reaches needData only while uploads are allowed; a pause
	// disallows them. The upload flag plays no part in the pending/request
	// order bookkeeping, so lift it to drive the check.
	at.AllowDataUpload()
	for i := 0; i < 1<<20; i++ {
		at.Seeding()
	}
	if e != nil {
		e.Close()
	}
	fmt.Println("HOLD CHILD OK", mode)
}

// pendingWait waits until some piece of t is wanted.
func pendingWait(t *testing.T, at *atorrent.Torrent) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); pendingCount(at) == 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("nothing pending")
		}
	}
}

func pendingCount(at *atorrent.Torrent) int {
	n := 0
	for i := range at.NumPieces() {
		if at.PieceState(i).Priority != atorrent.PiecePriorityNone {
			n++
		}
	}
	return n
}

// dupChild builds a blocked duplicate (as TestDupPathAfterRestart) on a
// second engine and returns its anacrolix torrent.
func dupChild(t *testing.T) *atorrent.Torrent {
	src := t.TempDir()
	a := newFixture(t, src)
	b := sameData(t, a) // the same files under another infohash
	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true,
		StallTimeout: time.Hour, tick: 100 * time.Millisecond, noAggressiveUpload: true}
	e, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddTorrentFile(context.Background(), a.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, e, a.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if _, err := e.AddTorrentFile(context.Background(), b.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	e.Close()
	if _, err := d.Exec(`UPDATE torrents SET save_dir=? WHERE infohash=?`, src, b.ih); err != nil {
		t.Fatal(err)
	}
	e2, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e2.Close(); d.Close() })
	waitFor(t, e2, b.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusError })
	return liveTorrent(e2, b.ih)
}

func filePriorities(at *atorrent.Torrent) []atorrent.PiecePriority {
	var out []atorrent.PiecePriority
	for _, f := range at.Files() {
		out = append(out, f.Priority())
	}
	return out
}

// Every hold stops the download (nothing wanted) and releasing it restores
// exactly the selection: whole torrent and selection, pause / space /
// write error, and a paused torrent restored after a restart (the
// TorrentSpec hold flags are never read by anacrolix v1.61).
func TestHoldsStopAndRestoreSelection(t *testing.T) {
	ctx := context.Background()
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	seed := multiSeeder(t, fx, nil)
	for _, tc := range []struct {
		name  string
		files []int
		hold  func(t *testing.T, e *Engine)
	}{
		{"pause-selection", []int{0, 2}, func(t *testing.T, e *Engine) {
			if err := e.Pause(ctx, fx.ih); err != nil {
				t.Fatal(err)
			}
		}},
		{"space-whole", nil, func(t *testing.T, e *Engine) {
			if err := e.HoldForSpace(ctx, fx.ih); err != nil {
				t.Fatal(err)
			}
		}},
		{"writeerr-selection", []int{0, 2}, func(_ *testing.T, e *Engine) {
			e.mu.Lock()
			en := e.entries[fx.ih]
			e.mu.Unlock()
			e.writeFailed(en, liveTorrent(e, fx.ih), errors.New("no space left on device"))
		}},
		{"paused-restart", []int{0, 2}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir, save := t.TempDir(), t.TempDir()
			d, err := db.Open(filepath.Join(dataDir, "vault.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true, StallTimeout: 2 * time.Second, tick: 100 * time.Millisecond}
			e, err := New(cfg, d)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { e.Close() }()
			h, err := e.AddTorrentFile(ctx, fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: tc.files})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.WaitInfo(ctx); err != nil {
				t.Fatal(err)
			}
			pendingWait(t, liveTorrent(e, fx.ih))
			if tc.hold != nil {
				tc.hold(t, e)
			} else {
				if err := e.Pause(ctx, fx.ih); err != nil {
					t.Fatal(err)
				}
				e.Close()
				if e, err = New(cfg, d); err != nil {
					t.Fatal(err)
				}
				h, _ = e.Get(fx.ih)
				if err := h.WaitInfo(ctx); err != nil {
					t.Fatal(err)
				}
				waitFor(t, e, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusPaused })
			}
			at := liveTorrent(e, fx.ih)
			e.mu.Lock()
			applied := e.entries[fx.ih].applied == at
			e.mu.Unlock()
			for deadline := time.Now().Add(5 * time.Second); !applied; time.Sleep(10 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("info never applied")
				}
				e.mu.Lock()
				applied = e.entries[fx.ih].applied == at
				e.mu.Unlock()
			}
			if n := pendingCount(at); n != 0 {
				t.Fatalf("%d pieces wanted while held", n)
			}
			connect(t, e, seed, fx.ih)
			time.Sleep(time.Second)
			if s, _ := status(e, fx.ih); s.Done != 0 || s.Downloaded != 0 {
				t.Fatalf("downloaded while held: %+v", s)
			}
			if err := e.Resume(ctx, fx.ih); err != nil {
				t.Fatal(err)
			}
			if tc.files != nil {
				want := []atorrent.PiecePriority{atorrent.PiecePriorityNormal, atorrent.PiecePriorityNone, atorrent.PiecePriorityNormal}
				if got := filePriorities(at); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("file priorities after resume %v, want %v", got, want)
				}
			}
			connect(t, e, seed, fx.ih)
			s := waitFor(t, e, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
			if s.Error != "" || s.Done != s.Size {
				t.Fatalf("status %+v", s)
			}
			if tc.files != nil {
				if n := fileBytesCompleted(e, fx.ih, 1); n != 0 {
					t.Fatalf("unselected file got %d bytes", n)
				}
			}
		})
	}
}

// A stream Read blocked waiting for data (no peers) returns ErrHeld as soon
// as the torrent is held, instead of keeping its reader priorities until
// the read times out; after Resume the reader reads again.
func TestHoldWakesBlockedRead(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true,
		StallTimeout: time.Hour, ReadTimeout: time.Minute, tick: 100 * time.Millisecond}
	e, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	fx := newMultiFixture(t, 16<<10, 64<<10, 64<<10, 64<<10)
	h, err := e.AddTorrentFile(ctx, fx.metafile, AddOptions{Mode: ModeStream, SaveDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := h.NewReader(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	read := func() <-chan error {
		ch := make(chan error, 1)
		go func() { _, err := r.Read(make([]byte, 1)); ch <- err }()
		return ch
	}
	got := read()
	pendingWait(t, liveTorrent(e, fx.ih)) // the read is waiting
	select {
	case err := <-got:
		t.Fatalf("read returned before the hold: %v", err)
	default:
	}
	if err := e.Pause(ctx, fx.ih); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if !errors.Is(err, ErrHeld) {
			t.Fatalf("blocked read after the hold: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked read was not woken by the hold")
	}
	if err := e.Resume(ctx, fx.ih); err != nil {
		t.Fatal(err)
	}
	got = read()
	pendingWait(t, liveTorrent(e, fx.ih))
	select {
	case err := <-got:
		t.Fatalf("read after resume returned at once: %v", err)
	case <-time.After(300 * time.Millisecond): // waiting for data again
	}
}
