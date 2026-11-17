package torrent

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

const albumName = "Кино — Группа крови (1988)"

type multiFixture struct {
	files    [][]byte // contents, in UpvertedFiles order
	names    []string
	metafile []byte
	ih       string
	pieceLen int64
}

// newMultiFixture builds an album with one random file per size and its
// metainfo; writeFiles puts (some of) the files on disk.
func newMultiFixture(t *testing.T, pieceLen int64, sizes ...int) multiFixture {
	t.Helper()
	full := t.TempDir()
	root := filepath.Join(full, albumName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := multiFixture{pieceLen: pieceLen}
	for i, n := range sizes {
		b := make([]byte, n)
		if _, err := rand.Read(b); err != nil {
			t.Fatal(err)
		}
		name := fmt.Sprintf("%02d трек.flac", i+1)
		if err := os.WriteFile(filepath.Join(root, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
		fx.files = append(fx.files, b)
		fx.names = append(fx.names, name)
	}
	info := metainfo.Info{PieceLength: pieceLen}
	if err := info.BuildFromFilePath(root); err != nil {
		t.Fatal(err)
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: ib}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	fx.metafile = buf.Bytes()
	fx.ih = mi.HashInfoBytes().HexString()
	return fx
}

// writeFiles puts the listed files (nil = all) into dir/albumName.
func (fx multiFixture) writeFiles(t *testing.T, dir string, idx []int) {
	t.Helper()
	if idx == nil {
		for i := range fx.files {
			idx = append(idx, i)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, albumName), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, i := range idx {
		if err := os.WriteFile(fx.path(dir, i), fx.files[i], 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (fx multiFixture) path(dir string, i int) string {
	return filepath.Join(dir, albumName, fx.names[i])
}

// multiSeeder imports the files listed in have (nil = all) in seed mode and
// waits until every piece state is known.
func multiSeeder(t *testing.T, fx multiFixture, have []int) *Engine {
	t.Helper()
	src := t.TempDir()
	fx.writeFiles(t, src, have)
	e := openEngine(t, t.TempDir(), engOpts{})
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := e.WaitVerified(ctx, fx.ih); err != nil {
		t.Fatal(err)
	}
	return e
}

type hookCount struct {
	mu sync.Mutex
	n  int
}

func (h *hookCount) add(Status) { h.mu.Lock(); h.n++; h.mu.Unlock() }
func (h *hookCount) get() int   { h.mu.Lock(); defer h.mu.Unlock(); return h.n }

func filesJSON(t *testing.T, d *sql.DB, ih string) sql.NullString {
	t.Helper()
	var s sql.NullString
	if err := d.QueryRow(`SELECT files_json FROM torrents WHERE infohash=?`, ih).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func fileByIndex(t *testing.T, e *Engine, ih string, i int) FileInfo {
	t.Helper()
	h, ok := e.Get(ih)
	if !ok {
		t.Fatal("no torrent")
	}
	fs := h.Files()
	if i >= len(fs) {
		t.Fatalf("files %+v", fs)
	}
	return fs[i]
}

func fileBytesCompleted(e *Engine, ih string, i int) int64 {
	e.mu.Lock()
	t := e.entries[ih].t
	e.mu.Unlock()
	return t.Files()[i].BytesCompleted()
}

func waitHooks(t *testing.T, h *hookCount, n int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for h.get() < n {
		if time.Now().After(deadline) {
			t.Fatalf("OnComplete fired %d times, want %d", h.get(), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSelectionDownloadsOnlySelected(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	seed := multiSeeder(t, fx, nil)
	leech := openEngine(t, t.TempDir(), engOpts{})
	var hooks hookCount
	leech.OnComplete(hooks.add)
	save := t.TempDir()
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: []int{0, 2}}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	s := waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	waitHooks(t, &hooks, 1, 2*time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := hooks.get(); n != 1 {
		t.Fatalf("OnComplete fired %d times", n)
	}
	if s.Size != 65536 || s.Done != 65536 || s.SelectedFiles != 2 || s.TotalFiles != 3 {
		t.Fatalf("status %+v", s)
	}
	for _, i := range []int{0, 2} {
		got, err := os.ReadFile(fx.path(save, i))
		if err != nil || !bytes.Equal(got, fx.files[i]) {
			t.Fatalf("file %d differs: %v", i, err)
		}
		if f := fileByIndex(t, leech, fx.ih, i); !f.Selected || f.Progress != 1 {
			t.Fatalf("file %d %+v", i, f)
		}
	}
	if f := fileByIndex(t, leech, fx.ih, 1); f.Selected {
		t.Fatalf("file 1 selected: %+v", f)
	}
	if n := fileBytesCompleted(leech, fx.ih, 1); n != 0 {
		t.Fatalf("file 1 BytesCompleted %d", n)
	}
	if _, err := os.Stat(fx.path(save, 1)); !os.IsNotExist(err) {
		t.Fatalf("file 1 on disk: %v", err)
	}
	if got := filesJSON(t, leech.db, fx.ih); !got.Valid || got.String != "[0,2]" {
		t.Fatalf("files_json %+v", got)
	}
}

func TestSelectionNilIsWholeTorrent(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	seed := multiSeeder(t, fx, nil)
	for _, files := range [][]int{nil, {0, 1, 2}, {2, 1, 0}} {
		leech := openEngine(t, t.TempDir(), engOpts{})
		save := t.TempDir()
		if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: files}); err != nil {
			t.Fatal(err)
		}
		if got := filesJSON(t, leech.db, fx.ih); got.Valid {
			t.Fatalf("files %v: files_json %q, want NULL", files, got.String)
		}
		connect(t, leech, seed, fx.ih)
		s := waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
		if s.Size != 3*32<<10 || s.Done != s.Size || s.SelectedFiles != 3 || s.TotalFiles != 3 {
			t.Fatalf("files %v: status %+v", files, s)
		}
		leech.mu.Lock()
		whole := leech.entries[fx.ih].t.Complete().Bool()
		leech.mu.Unlock()
		if !whole {
			t.Fatal("whole torrent not complete")
		}
		for i := range fx.files {
			if f := fileByIndex(t, leech, fx.ih, i); !f.Selected || f.Progress != 1 {
				t.Fatalf("file %d %+v", i, f)
			}
		}
	}
}

func TestSelectionRejectsBad(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	e := openEngine(t, t.TempDir(), engOpts{})
	for _, files := range [][]int{{}, {5}, {-1}, {0, 3}} {
		if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir(), Files: files}); err == nil {
			t.Fatalf("files %v accepted", files)
		}
	}
	if _, err := e.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+fx.ih, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir(), Files: []int{0}}); err == nil {
		t.Fatal("magnet with a selection accepted")
	}
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM torrents`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows %d %v", n, err)
	}
	if _, ok := e.Get(fx.ih); ok {
		t.Fatal("torrent in the engine")
	}
	if _, err := os.Stat(e.metafilePath(fx.ih)); !os.IsNotExist(err) {
		t.Fatalf("metafile left: %v", err)
	}
}

func TestSelectionSurvivesRestart(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	partial := multiSeeder(t, fx, []int{0}) // can only serve file 0
	dataDir, save := t.TempDir(), t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true, StallTimeout: 2 * time.Second, tick: 100 * time.Millisecond}
	leech, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	var hooks hookCount
	leech.OnComplete(hooks.add)
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: []int{0, 2}}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, partial, fx.ih)
	mid := waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Done == 32<<10 })
	if mid.Status == StatusSeeding || hooks.get() != 0 {
		t.Fatalf("complete with file 2 missing: %+v", mid)
	}
	leech.Close()
	d.Close()

	d2, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	if got := filesJSON(t, d2, fx.ih); !got.Valid || got.String != "[0,2]" {
		t.Fatalf("files_json %+v", got)
	}
	leech2, err := New(cfg, d2)
	if err != nil {
		t.Fatal(err)
	}
	defer leech2.Close()
	var hooks2 hookCount
	leech2.OnComplete(hooks2.add)
	h, _ := leech2.Get(fx.ih)
	if err := h.WaitInfo(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, leech2, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusDownloading })
	if s.SelectedFiles != 2 || s.TotalFiles != 3 || s.Size != 65536 || s.Done != 32<<10 {
		t.Fatalf("after restart %+v", s)
	}
	full := multiSeeder(t, fx, nil)
	connect(t, leech2, full, fx.ih)
	waitFor(t, leech2, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	time.Sleep(3 * time.Second)
	if n := fileBytesCompleted(leech2, fx.ih, 1); n != 0 {
		t.Fatalf("unselected file 1 got %d bytes", n)
	}
	if _, err := os.Stat(fx.path(save, 1)); !os.IsNotExist(err) {
		t.Fatalf("file 1 on disk: %v", err)
	}
	if n := hooks2.get(); n != 1 {
		t.Fatalf("OnComplete fired %d times", n)
	}
}

func TestSelectionCompleteOnlyAfterVerify(t *testing.T) {
	// File 2 is a single piece (piece 4), and file 0 comes first from a
	// clean partial seeder: once the corrupt piece finally verifies, the
	// selection is complete. anacrolix then smart-bans the IP that sent
	// the bad blocks (all test peers are 127.0.0.1), so nothing may be
	// left to fetch from it at that point.
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 16<<10)
	partial := multiSeeder(t, fx, []int{0})
	src := t.TempDir()
	fx.writeFiles(t, src, nil)
	seed := openEngine(t, t.TempDir(), engOpts{})
	if _, err := seed.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, seed, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	// The seeder trusts its recorded piece state, so it now serves a
	// corrupt chunk of file 2 (its only piece) until the file is put back.
	// The data is patched in place: the seeder reads its files through
	// mmap, and truncating a mapped file (os.WriteFile) makes its reads
	// fault or return stale bytes.
	p := fx.path(src, 2)
	const badOff, badLen = 4 << 10, 1 << 10
	patch := func(b []byte) {
		t.Helper()
		f, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteAt(b, badOff); err != nil {
			t.Fatal(err)
		}
	}
	bad := append([]byte(nil), fx.files[2][badOff:badOff+badLen]...)
	for i := range bad {
		bad[i] ^= 0xff
	}
	patch(bad)

	dataDir, save := t.TempDir(), t.TempDir()
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true, StallTimeout: 2 * time.Second, tick: 100 * time.Millisecond}
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	leech, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { leech.Close() })
	var hooks hookCount
	leech.OnComplete(hooks.add)
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: []int{0, 2}}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, partial, fx.ih)
	waitFor(t, leech, fx.ih, 10*time.Second, func(s Status) bool { return s.Done == 32<<10 })
	connect(t, leech, seed, fx.ih)
	leech.mu.Lock()
	lt := leech.entries[fx.ih].t
	leech.mu.Unlock()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := lt.Stats()
		if st.PiecesDirtiedBad.Int64() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("corrupt piece never hashed")
		}
		time.Sleep(time.Millisecond)
	}
	// The corrupt piece failed its hash: nothing may report completion.
	time.Sleep(500 * time.Millisecond)
	if n := hooks.get(); n != 0 {
		t.Fatalf("OnComplete fired %d times with a corrupt piece", n)
	}
	if s, _ := status(leech, fx.ih); s.Status == StatusSeeding {
		t.Fatalf("seeding with a corrupt piece: %+v", s)
	}
	patch(fx.files[2][badOff : badOff+badLen])
	// After a quick run of hash failures from its only peer (trusted, so
	// never banned for them) anacrolix v1.61 sometimes stops requesting the
	// piece (about 1 run in 20 under load). The engine's idle check
	// (checkIdle) re-plans the requests after StallTimeout (2 s here) and,
	// when that does not help, reconnects the peer 2× StallTimeout later
	// (untrusted); the reconnect is what revives this case.
	//
	// A reconnected peer is untrusted, so when one of the engine's
	// reconnects (this one, or the in-flight watchdog's while the bad
	// chunk was being served) lands before the bad data, anacrolix bans
	// 127.0.0.1 for it, drops every local connection and refuses to re-add
	// it: the only peer is gone for this client. That is right for a real
	// swarm; here the test restarts the engine (a new client has no bans,
	// the selection and verified pieces persist) and adds its peer back.
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		s, _ := status(leech, fx.ih)
		if s.Status == StatusSeeding {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not complete: %+v", s)
		}
		if s.Peers == 0 && len(leech.client.BadPeerIPs()) > 0 {
			t.Logf("only peer banned (%v): restarting the engine", leech.client.BadPeerIPs())
			leech.Close()
			if leech, err = New(cfg, d); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { leech.Close() })
			leech.OnComplete(hooks.add)
			h, _ := leech.Get(fx.ih)
			if err := h.WaitInfo(context.Background()); err != nil {
				t.Fatal(err)
			}
			connect(t, leech, seed, fx.ih)
		}
	}
	waitHooks(t, &hooks, 1, 2*time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := hooks.get(); n != 1 {
		t.Fatalf("OnComplete fired %d times", n)
	}
	got, _ := os.ReadFile(fx.path(save, 2))
	if !bytes.Equal(got, fx.files[2]) {
		t.Fatal("file 2 differs")
	}
}

func TestSelectionCompletesOnceAndSeeds(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	seed := multiSeeder(t, fx, nil)
	leech := openEngine(t, t.TempDir(), engOpts{})
	// Count every full-row save (store.update writes status; the
	// monitor's counter save does not).
	if _, err := leech.db.Exec(`CREATE TABLE test_saves(n INTEGER);
		CREATE TRIGGER test_count_saves AFTER UPDATE OF status ON torrents BEGIN INSERT INTO test_saves VALUES (1); END;`); err != nil {
		t.Fatal(err)
	}
	saves := func() int {
		var n int
		if err := leech.db.QueryRow(`SELECT COUNT(*) FROM test_saves`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var hooks hookCount
	var atHook atomic.Int64
	leech.OnComplete(func(s Status) { atHook.Store(int64(saves())); hooks.add(s) })
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir(), Files: []int{0, 2}}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	waitHooks(t, &hooks, 1, 15*time.Second)
	time.Sleep(6 * leech.cfg.tick)
	if n := hooks.get(); n != 1 {
		t.Fatalf("OnComplete fired %d times", n)
	}
	if after := saves(); int64(after) != atHook.Load() {
		t.Fatalf("saves after completion: %d, at completion %d", after, atHook.Load())
	}
	s, _ := status(leech, fx.ih)
	if s.Status != StatusSeeding || s.CompletedAt == nil {
		t.Fatalf("status %+v", s)
	}
}

func TestSelectionBoundaryNeighbourStaysSparse(t *testing.T) {
	fx := newMultiFixture(t, 16384, 20000, 20000)
	seed := multiSeeder(t, fx, nil)
	dataDir, save := t.TempDir(), t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true, StallTimeout: 2 * time.Second, tick: 100 * time.Millisecond}
	leech, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save, Files: []int{0}}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	s := waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if s.Size != 20000 || s.Done != 20000 {
		t.Fatalf("status %+v", s)
	}
	got, _ := os.ReadFile(fx.path(save, 0))
	if !bytes.Equal(got, fx.files[0]) {
		t.Fatal("file 0 differs")
	}
	// The boundary piece [16384, 32768) holds 12768 bytes of file 1.
	st, err := os.Stat(fx.path(save, 1))
	if err != nil || st.Size() < 32768-20000 {
		t.Fatalf("neighbour on disk: %v %v", st, err)
	}
	// Only the boundary piece of the neighbour, never its own last piece.
	if n := fileBytesCompleted(leech, fx.ih, 1); n != 32768-20000 {
		t.Fatalf("neighbour BytesCompleted %d, want %d", n, 32768-20000)
	}
	time.Sleep(300 * time.Millisecond)
	before, _ := status(leech, fx.ih)
	leech.Close()
	d.Close()

	d2, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	leech2, err := New(cfg, d2)
	if err != nil {
		t.Fatal(err)
	}
	defer leech2.Close()
	s = waitFor(t, leech2, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	connect(t, leech2, seed, fx.ih)
	time.Sleep(1500 * time.Millisecond)
	after, _ := status(leech2, fx.ih)
	if after.Done != before.Done || after.Downloaded != before.Downloaded || after.Status != StatusSeeding {
		t.Fatalf("before %+v after %+v", before, after)
	}
	if _, err := os.Stat(fx.path(save, 1)); err != nil {
		t.Fatalf("neighbour removed: %v", err)
	}
}

func TestSelectionNeverAnnouncesCompleted(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	seed := multiSeeder(t, fx, nil)
	leech := openEngine(t, t.TempDir(), engOpts{})
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir(), Files: []int{0, 2}}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	s := waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if s.Done != s.Size {
		t.Fatalf("status %+v", s)
	}
	time.Sleep(500 * time.Millisecond)
	leech.mu.Lock()
	whole := leech.entries[fx.ih].t.Complete().Bool()
	leech.mu.Unlock()
	if whole {
		t.Fatal("anacrolix reports the whole torrent complete (tracker would get event=completed)")
	}
}
