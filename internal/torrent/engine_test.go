package torrent

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

const testSize = 8 << 20

type fixture struct {
	data     []byte
	metafile []byte
	ih       string
	name     string
}

// newFixture writes an 8 MiB random file into dir and builds its metainfo.
func newFixture(t *testing.T, dir string) fixture {
	t.Helper()
	return newFixtureSize(t, dir, testSize)
}

// newFixtureSize is newFixture with a file of size bytes.
func newFixtureSize(t *testing.T, dir string, size int) fixture {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	name := "Брат 2 (2000) BDRip.mkv"
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 256 << 10}
	if err := info.BuildFromFilePath(p); err != nil {
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
	return fixture{data: data, metafile: buf.Bytes(), ih: mi.HashInfoBytes().HexString(), name: name}
}

type engOpts struct {
	upLimit     int64
	readTimeout time.Duration
	stall       time.Duration
	// stalledAfter, incomingAfter: EngineConfig.StalledAfter and
	// IncomingCheckAfter (0 = the defaults).
	stalledAfter, incomingAfter time.Duration
	persist                     time.Duration // EngineConfig.persistEvery (0 = the default)
	noSeeding                   bool          // EngineConfig.NoSeeding
}

func openEngine(t *testing.T, dataDir string, o engOpts) *Engine {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := New(EngineConfig{
		DataDir:            dataDir,
		StreamDir:          filepath.Join(dataDir, "stream"),
		ListenPort:         0,
		Seed:               true,
		UploadLimit:        o.upLimit,
		LocalOnly:          true,
		ReadTimeout:        o.readTimeout,
		StallTimeout:       stallOr(o.stall),
		StalledAfter:       o.stalledAfter,
		IncomingCheckAfter: o.incomingAfter,
		tick:               100 * time.Millisecond,
		persistEvery:       o.persist,
		NoSeeding:          o.noSeeding,
	}, d)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close(); d.Close() })
	return e
}

func stallOr(d time.Duration) time.Duration {
	if d == 0 {
		return 2 * time.Second
	}
	return d
}

// newSeeder imports existing data (mode seed) and waits until it is verified.
func newSeeder(t *testing.T, o engOpts) (*Engine, fixture) {
	t.Helper()
	src := t.TempDir()
	fx := newFixture(t, src)
	e := openEngine(t, t.TempDir(), o)
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	return e, fx
}

func status(e *Engine, ih string) (Status, bool) {
	for _, s := range e.List() {
		if s.InfoHash == ih {
			return s, true
		}
	}
	return Status{}, false
}

func waitFor(t *testing.T, e *Engine, ih string, d time.Duration, cond func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(d)
	var last Status
	for time.Now().Before(deadline) {
		if s, ok := status(e, ih); ok {
			last = s
			if cond(s) {
				return s
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s; last status %+v", ih, last)
	return last
}

// connect lets `from` dial `to` directly (no trackers/DHT in tests).
func connect(t *testing.T, from, to *Engine, ih string) {
	t.Helper()
	from.mu.Lock()
	en := from.entries[ih]
	from.mu.Unlock()
	if en == nil || en.t == nil {
		t.Fatalf("no torrent %s", ih)
	}
	if n := en.t.AddClientPeer(to.client); n == 0 {
		t.Fatal("no peer addresses added")
	}
}

func TestSeedImportExistingData(t *testing.T) {
	e, fx := newSeeder(t, engOpts{})
	s, _ := status(e, fx.ih)
	if s.Done != testSize || s.Size != testSize {
		t.Fatalf("done %d size %d", s.Done, s.Size)
	}
	if s.Downloaded != 0 {
		t.Fatalf("import downloaded %d bytes, want 0", s.Downloaded)
	}
	if s.Mode != ModeSeed || s.Name != fx.name {
		t.Fatalf("status %+v", s)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, "torrents", fx.ih+".torrent")); err != nil {
		t.Fatalf("metafile not stored: %v", err)
	}
}

func TestDownloadComplete(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	leech := openEngine(t, t.TempDir(), engOpts{})
	var mu sync.Mutex
	var completed []Status
	leech.OnComplete(func(s Status) { mu.Lock(); completed = append(completed, s); mu.Unlock() })

	save := t.TempDir()
	h, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save})
	if err != nil {
		t.Fatal(err)
	}
	if h.InfoHash() != fx.ih {
		t.Fatalf("infohash %s want %s", h.InfoHash(), fx.ih)
	}
	connect(t, leech, seed, fx.ih)
	s := waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	got, err := os.ReadFile(filepath.Join(save, fx.name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fx.data) {
		t.Fatal("downloaded file differs")
	}
	if s.Done != testSize || s.Downloaded < testSize {
		t.Fatalf("status %+v", s)
	}
	files := h.Files()
	if len(files) != 1 || files[0].Size != testSize || files[0].Progress != 1 || files[0].Path != fx.name {
		t.Fatalf("files %+v", files)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(completed)
		mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			if n != 1 {
				t.Fatalf("OnComplete called %d times", n)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStreamReaderSeekBlocksUntilData(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	leech := openEngine(t, t.TempDir(), engOpts{readTimeout: 20 * time.Second})
	h, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeStream})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := h.NewReader(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	mid := int64(testSize/2 + 12345)
	if pos, err := r.Seek(mid, io.SeekStart); err != nil || pos != mid {
		t.Fatalf("seek %d %v", pos, err)
	}
	type res struct {
		buf []byte
		err error
	}
	done := make(chan res, 1)
	go func() {
		buf := make([]byte, 64<<10)
		_, err := io.ReadFull(r, buf)
		done <- res{buf, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("read returned without peers: err=%v", got.err)
	case <-time.After(400 * time.Millisecond):
	}
	connect(t, leech, seed, fx.ih)
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatal(got.err)
		}
		if !bytes.Equal(got.buf, fx.data[mid:mid+64<<10]) {
			t.Fatal("bytes after seek differ")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("read did not complete after peer connected")
	}
	// Stream data lives in StreamDir/<infohash>.
	if _, err := os.Stat(filepath.Join(leech.cfg.StreamDir, fx.ih, fx.name)); err != nil {
		t.Fatalf("stream data location: %v", err)
	}
	// Stream mode only fetches what readers ask for.
	if s, _ := status(leech, fx.ih); s.Done >= testSize {
		t.Fatalf("stream mode downloaded everything: %+v", s)
	}
}

func TestStreamReaderTimeout(t *testing.T) {
	_, fx := newSeeder(t, engOpts{})
	leech := openEngine(t, t.TempDir(), engOpts{readTimeout: 300 * time.Millisecond})
	h, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeStream})
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.NewReader(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.Seek(testSize/2, io.SeekStart)
	start := time.Now()
	n, err := r.Read(make([]byte, 4096))
	if n != 0 || !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("n=%d err=%v, want ErrReadTimeout", n, err)
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("timeout must not look like EOF")
	}
	if el := time.Since(start); el < 250*time.Millisecond {
		t.Fatalf("returned after %v, did not block", el)
	}
}

func TestRestartResumes(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{upLimit: 2 << 20})
	dataDir := t.TempDir()
	save := t.TempDir()

	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), Seed: true, LocalOnly: true, StallTimeout: 2 * time.Second, tick: 100 * time.Millisecond}
	leech, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	mid := waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Done >= testSize/4 })
	if mid.Status == StatusSeeding {
		t.Fatal("finished before restart; slow the seeder down")
	}
	leech.mu.Lock()
	piecesBefore := leech.entries[fx.ih].t.Stats().PiecesComplete
	leech.mu.Unlock()
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
	h, ok := leech2.Get(fx.ih)
	if !ok {
		t.Fatal("torrent not restored")
	}
	if err := h.WaitInfo(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, _ := status(leech2, fx.ih)
	leech2.mu.Lock()
	piecesAfter := leech2.entries[fx.ih].t.Stats().PiecesComplete
	leech2.mu.Unlock()
	if piecesBefore == 0 || piecesAfter < piecesBefore || s.Status == StatusSeeding {
		t.Fatalf("after restart pieces=%d status=%s, before close pieces=%d", piecesAfter, s.Status, piecesBefore)
	}
	if s.Mode != ModeDownload {
		t.Fatalf("mode %s", s.Mode)
	}
	connect(t, leech2, seed, fx.ih)
	waitFor(t, leech2, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	got, _ := os.ReadFile(filepath.Join(save, fx.name))
	if !bytes.Equal(got, fx.data) {
		t.Fatal("resumed file differs")
	}
}

func TestSeedAfterComplete(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	leech := openEngine(t, t.TempDir(), engOpts{})
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	seed.Close() // only the second engine can serve now

	third := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	if _, err := third.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatal(err)
	}
	connect(t, third, leech, fx.ih)
	waitFor(t, third, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	got, _ := os.ReadFile(filepath.Join(save, fx.name))
	if !bytes.Equal(got, fx.data) {
		t.Fatal("third client file differs")
	}
	s := waitFor(t, leech, fx.ih, 3*time.Second, func(s Status) bool { return s.Uploaded >= testSize })
	if s.Status != StatusSeeding || s.Ratio < 1 {
		t.Fatalf("leech status %+v", s)
	}
}

func TestSetModeStreamToDownloadMoves(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	leech := openEngine(t, t.TempDir(), engOpts{})
	var mu sync.Mutex
	var completed []Status
	leech.OnComplete(func(s Status) { mu.Lock(); completed = append(completed, s); mu.Unlock() })
	h, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeStream})
	if err != nil {
		t.Fatal(err)
	}
	h.WaitInfo(context.Background())
	target := t.TempDir()
	if err := leech.SetModeTo(context.Background(), fx.ih, ModeDownload, target); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	deadline := time.Now().Add(15 * time.Second)
	for {
		mu.Lock()
		n := len(completed)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no completion")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	c := completed[0]
	mu.Unlock()
	if c.Mode != ModeDownload {
		t.Fatalf("completion %+v", c)
	}
	got, err := os.ReadFile(filepath.Join(target, fx.name))
	if err != nil || !bytes.Equal(got, fx.data) {
		t.Fatalf("moved file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(leech.cfg.StreamDir, fx.ih)); !os.IsNotExist(err) {
		t.Fatalf("stream dir left behind: %v", err)
	}
	s := waitFor(t, leech, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if s.Mode != ModeDownload {
		t.Fatalf("status %+v", s)
	}
	// After the move the torrent still serves data from the new place.
	r, err := h.NewReader(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	buf := make([]byte, 1000)
	if _, err := io.ReadFull(r, buf); err != nil || !bytes.Equal(buf, fx.data[:1000]) {
		t.Fatalf("read after move: %v", err)
	}
}

func TestRemoveDeleteData(t *testing.T) {
	e, fx := newSeeder(t, engOpts{})
	e.mu.Lock()
	en := e.entries[fx.ih]
	e.mu.Unlock()
	dataPath := filepath.Join(en.rec.SaveDir, fx.name)
	if err := e.Remove(context.Background(), fx.ih, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Get(fx.ih); ok {
		t.Fatal("still present")
	}
	if _, err := os.Stat(dataPath); !os.IsNotExist(err) {
		t.Fatalf("data not deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, "torrents", fx.ih+".torrent")); !os.IsNotExist(err) {
		t.Fatal("metafile not deleted")
	}
	var n int
	e.db.QueryRow(`SELECT COUNT(*) FROM torrent_pieces WHERE infohash=?`, fx.ih).Scan(&n)
	if n != 0 {
		t.Fatalf("%d piece rows left", n)
	}
}

func TestPauseResumePersists(t *testing.T) {
	_, fx := newSeeder(t, engOpts{})
	dataDir := t.TempDir()
	e := openEngine(t, dataDir, engOpts{})
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(context.Background(), fx.ih); err != nil {
		t.Fatal(err)
	}
	if s, _ := status(e, fx.ih); s.Status != StatusPaused {
		t.Fatalf("status %s", s.Status)
	}
	var st string
	e.db.QueryRow(`SELECT status FROM torrents WHERE infohash=?`, fx.ih).Scan(&st)
	if st != StatusPaused {
		t.Fatalf("db status %s", st)
	}
	if err := e.Resume(context.Background(), fx.ih); err != nil {
		t.Fatal(err)
	}
	if s, _ := status(e, fx.ih); s.Status == StatusPaused {
		t.Fatal("still paused")
	}
}

func TestMagnetFetchesMetadata(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	leech := openEngine(t, t.TempDir(), engOpts{})
	h, err := leech.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+fx.ih+"&dn=x", AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := status(leech, fx.ih); s.Status != StatusQueued {
		t.Fatalf("status before metadata %s", s.Status)
	}
	connect(t, leech, seed, fx.ih)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.WaitInfo(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding && s.Name == fx.name })
	if _, err := os.Stat(leech.metafilePath(fx.ih)); err != nil {
		t.Fatalf("metafile not saved after metadata: %v", err)
	}
}

func TestPromoteCompletedStream(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	leech := openEngine(t, t.TempDir(), engOpts{})
	var mu sync.Mutex
	var modes []string
	leech.OnComplete(func(s Status) { mu.Lock(); modes = append(modes, s.Mode); mu.Unlock() })
	h, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeStream})
	if err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	r, err := h.NewReader(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r) // watching the whole film completes the torrent
	r.Close()
	if err != nil || !bytes.Equal(got, fx.data) {
		t.Fatalf("read all: %v", err)
	}
	waitFor(t, leech, fx.ih, 10*time.Second, func(s Status) bool { return s.CompletedAt != nil })
	target := t.TempDir()
	if err := leech.SetModeTo(context.Background(), fx.ih, ModeDownload, target); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(modes)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(modes) != 2 || modes[0] != ModeStream || modes[1] != ModeDownload {
		t.Fatalf("completion modes %v", modes)
	}
	if b, err := os.ReadFile(filepath.Join(target, fx.name)); err != nil || !bytes.Equal(b, fx.data) {
		t.Fatalf("promoted file: %v", err)
	}
	if err := leech.SetMode(context.Background(), fx.ih, ModeStream); err == nil {
		t.Fatal("download -> stream must be refused")
	}
}

// Review fix 1: a move whose destination already exists must not loop
// (drop/re-attach), must keep the torrent seeding in place, and must not be
// retried after a restart.
func TestMoveFailureKeepsTorrentInPlace(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true, StallTimeout: 2 * time.Second, tick: 100 * time.Millisecond}
	leech, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeStream})
	connect(t, leech, seed, fx.ih)
	r, _ := h.NewReader(context.Background(), 0)
	io.ReadAll(r)
	r.Close()
	waitFor(t, leech, fx.ih, 10*time.Second, func(s Status) bool { return s.CompletedAt != nil })
	target := t.TempDir()
	os.WriteFile(filepath.Join(target, fx.name), []byte("other"), 0o644)

	countTorrents := func(e *Engine, d time.Duration) int {
		seen := map[*atorrent.Torrent]bool{}
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
			e.mu.Lock()
			if p := e.entries[fx.ih].t; p != nil {
				seen[p] = true
			}
			e.mu.Unlock()
		}
		return len(seen)
	}
	if err := leech.SetModeTo(context.Background(), fx.ih, ModeDownload, target); err != nil {
		t.Fatal(err)
	}
	if n := countTorrents(leech, time.Second); n != 1 {
		t.Fatalf("torrent re-attached %d times", n)
	}
	s, _ := status(leech, fx.ih)
	if s.Status != StatusError || s.Error == "" || s.SaveDir != leech.streamDir(fx.ih) {
		t.Fatalf("status %+v", s)
	}
	if b, _ := os.ReadFile(filepath.Join(target, fx.name)); string(b) != "other" {
		t.Fatal("existing destination file was touched")
	}
	var target2 sql.NullString
	d.QueryRow(`SELECT target_dir FROM torrents WHERE infohash=?`, fx.ih).Scan(&target2)
	if target2.Valid {
		t.Fatalf("pending move kept in DB: %q", target2.String)
	}
	leech.Close()
	leech2, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { leech2.Close(); d.Close() }()
	if n := countTorrents(leech2, time.Second); n != 1 {
		t.Fatalf("after restart torrent re-attached %d times", n)
	}
	s = waitFor(t, leech2, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if s.Mode != ModeDownload || s.SaveDir != leech2.streamDir(fx.ih) {
		t.Fatalf("after restart %+v", s)
	}
}

// Review fix 2: a peer that simply lacks the pieces we want is not a stall.
func TestWatchdogIgnoresPeerWithoutWantedPieces(t *testing.T) {
	src := t.TempDir()
	fx := newFixture(t, src)
	half := append(append([]byte{}, fx.data[:testSize/2]...), make([]byte, testSize/2)...)
	if err := os.WriteFile(filepath.Join(src, fx.name), half, 0o644); err != nil {
		t.Fatal(err)
	}
	seed := openEngine(t, t.TempDir(), engOpts{stall: time.Second})
	seed.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src})
	waitFor(t, seed, fx.ih, 10*time.Second, func(s Status) bool { return s.Done == testSize/2 })
	leech := openEngine(t, t.TempDir(), engOpts{stall: time.Second})
	leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()})
	connect(t, leech, seed, fx.ih)
	waitFor(t, leech, fx.ih, 10*time.Second, func(s Status) bool { return s.Done == testSize/2 })
	time.Sleep(3500 * time.Millisecond) // 3+ stall timeouts with nothing more to get
	s, _ := status(leech, fx.ih)
	if n := leech.stallKicks.Load() + seed.stallKicks.Load(); n != 0 {
		t.Fatalf("watchdog fired %d times", n)
	}
	if s.Peers != 1 {
		t.Fatalf("peer dropped: %+v", s)
	}
}

// Review fix 2: a connection that takes requests and never answers is
// closed by the watchdog.
func TestWatchdogClosesStuckConnection(t *testing.T) {
	_, fx := newSeeder(t, engOpts{})
	leech := openEngine(t, t.TempDir(), engOpts{stall: time.Second})
	leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()})

	// A fake peer: plaintext handshake, claims every piece, unchokes, then
	// swallows requests forever.
	ih, _ := hex.DecodeString(fx.ih)
	hs := append([]byte("\x13BitTorrent protocol"), make([]byte, 8)...)
	hs = append(hs, ih...)
	hs = append(hs, []byte("-XX0001-stuckpeer000")...)
	// The client may refuse conns for a moment right after the add; retry.
	var conn net.Conn
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		c, err := net.Dial("tcp", leech.client.ListenAddrs()[0].String())
		if err != nil {
			t.Fatal(err)
		}
		c.Write(hs)
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(c, make([]byte, 68)); err == nil {
			c.SetReadDeadline(time.Time{})
			conn = c
			break
		}
		c.Close()
		if time.Now().After(deadline) {
			t.Fatal("engine never accepted the fake peer")
		}
	}
	defer conn.Close()
	conn.Write([]byte{0, 0, 0, 5, 5, 0xff, 0xff, 0xff, 0xff}) // bitfield: 32 pieces
	conn.Write([]byte{0, 0, 0, 1, 1})                         // unchoke
	closed := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn)
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(8 * time.Second):
		t.Fatalf("stuck connection not closed; kicks=%d", leech.stallKicks.Load())
	}
	if leech.stallKicks.Load() == 0 {
		t.Fatal("connection closed, but not by the watchdog")
	}
}

func TestAddRefusesSharedPath(t *testing.T) {
	e := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	a := newFixture(t, t.TempDir())
	b := sameData(t, a) // the same files under another infohash (a namesake: TestNamesakeGetsOwnPath)
	if _, err := e.AddTorrentFile(context.Background(), a.metafile, AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddTorrentFile(context.Background(), b.metafile, AddOptions{Mode: ModeDownload, SaveDir: save}); !errors.Is(err, ErrPathInUse) {
		t.Fatalf("err %v, want ErrPathInUse", err)
	}
	if _, err := e.AddTorrentFile(context.Background(), b.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

// Review round 2: after a restart only the newer torrent of a pair that
// shares files is blocked, and removing it with deleteData keeps the files.
func TestDupPathAfterRestart(t *testing.T) {
	src := t.TempDir()
	a := newFixture(t, src)
	b := sameData(t, a) // the same files under another infohash
	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true, StallTimeout: 2 * time.Second, tick: 100 * time.Millisecond}
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
	// State as if b were a magnet that resolved to a's path.
	if _, err := d.Exec(`UPDATE torrents SET save_dir=? WHERE infohash=?`, src, b.ih); err != nil {
		t.Fatal(err)
	}
	e2, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { e2.Close(); d.Close() }()
	waitFor(t, e2, b.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusError })
	sa := waitFor(t, e2, a.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if sa.Error != "" {
		t.Fatalf("senior torrent blocked: %+v", sa)
	}
	if err := e2.Remove(context.Background(), b.ih, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(src, a.name))
	if err != nil || !bytes.Equal(got, a.data) {
		t.Fatalf("removing the duplicate deleted/damaged the owner's file: %v", err)
	}
}

// Review round 2: a move refused up front still completes the torrent in
// place (CompletedAt + one OnComplete).
func TestRefusedMoveStillCompletes(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	leech := openEngine(t, t.TempDir(), engOpts{})
	var mu sync.Mutex
	var done []Status
	leech.OnComplete(func(s Status) { mu.Lock(); done = append(done, s); mu.Unlock() })
	h, _ := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeStream})
	h.WaitInfo(context.Background())
	target := t.TempDir()
	os.WriteFile(filepath.Join(target, fx.name), []byte("other"), 0o644)
	if err := leech.SetModeTo(context.Background(), fx.ih, ModeDownload, target); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	s := waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.CompletedAt != nil })
	if s.SaveDir != leech.streamDir(fx.ih) || s.Error == "" {
		t.Fatalf("status %+v", s)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(done) != 1 || done[0].SaveDir != leech.streamDir(fx.ih) {
		t.Fatalf("OnComplete %+v", done)
	}
}

// Review round 2: path ownership counts pending move targets, and an
// unresolved magnet's display name blocks nothing.
func TestPathOwnershipRules(t *testing.T) {
	e := openEngine(t, t.TempDir(), engOpts{})
	a := newFixture(t, t.TempDir())
	b := sameData(t, a)
	c := newFixture(t, t.TempDir())
	if _, err := e.AddTorrentFile(context.Background(), a.metafile, AddOptions{Mode: ModeStream}); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := e.SetModeTo(context.Background(), a.ih, ModeDownload, target); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddTorrentFile(context.Background(), b.metafile, AddOptions{Mode: ModeDownload, SaveDir: target}); !errors.Is(err, ErrPathInUse) {
		t.Fatalf("pending move target not protected: %v", err)
	}
	other := t.TempDir()
	if _, err := e.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+strings.Repeat("ab", 20)+"&dn="+url.QueryEscape(c.name), AddOptions{Mode: ModeDownload, SaveDir: other}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddTorrentFile(context.Background(), c.metafile, AddOptions{Mode: ModeDownload, SaveDir: other}); err != nil {
		t.Fatalf("unresolved magnet blocked a .torrent: %v", err)
	}
}

// Review round 3: an older magnet that resolves after a newer .torrent of
// the same files took the same path is the one blocked; removing the owner
// unblocks it. (Other files: TestNamesakeMagnetMovesAside.)
func TestMagnetResolvingLaterIsJunior(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	e := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	if _, err := e.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+fx.ih, AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatal(err)
	}
	b := sameData(t, fx) // the same files under another infohash
	if _, err := e.AddTorrentFile(context.Background(), b.metafile, AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatal(err)
	}
	connect(t, e, seed, fx.ih) // A resolves now
	sa := waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusError })
	if !strings.Contains(sa.Error, b.ih) {
		t.Fatalf("A error %q", sa.Error)
	}
	time.Sleep(300 * time.Millisecond)
	if s, _ := status(e, fx.ih); s.Done != 0 {
		t.Fatalf("blocked magnet wrote data: %+v", s)
	}
	if sb, _ := status(e, b.ih); sb.Error != "" || sb.Status == StatusError {
		t.Fatalf("B affected: %+v", sb)
	}
	// Owner removed (data kept: A shares the path) → A is unblocked and downloads.
	if err := e.Remove(context.Background(), b.ih, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, e, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding && s.Error == "" })
	got, _ := os.ReadFile(filepath.Join(save, fx.name))
	if !bytes.Equal(got, fx.data) {
		t.Fatal("unblocked torrent data differs")
	}
}

func TestRemoveIfModeKeepsOtherMode(t *testing.T) {
	_, fx := newSeeder(t, engOpts{})
	e := openEngine(t, t.TempDir(), engOpts{})
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeStream}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetModeTo(context.Background(), fx.ih, ModeDownload, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveIfMode(context.Background(), fx.ih, ModeStream, true); !errors.Is(err, ErrModeChanged) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := e.Get(fx.ih); !ok {
		t.Fatal("promoted torrent removed")
	}
	if err := e.HoldForSpace(context.Background(), fx.ih); err != nil {
		t.Fatal(err)
	}
	if s, _ := status(e, fx.ih); s.Status != StatusWaitingSpace {
		t.Fatalf("status %s", s.Status)
	}
	if err := e.RemoveIfMode(context.Background(), fx.ih, ModeDownload, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Get(fx.ih); ok {
		t.Fatal("still present")
	}
}
