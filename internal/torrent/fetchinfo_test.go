package torrent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

func magnetOf(ih, name string) string {
	return "magnet:?xt=urn:btih:" + ih + "&dn=" + name
}

// fetchEngine is a leecher whose fetch temp dirs go to a dir the test owns.
func fetchEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	e := openEngine(t, t.TempDir(), engOpts{})
	tmp := t.TempDir()
	e.cfg.tempDir = tmp
	return e, tmp
}

type fetchResult struct {
	meta []byte
	err  error
}

func startFetch(e *Engine, ctx context.Context, magnet string) chan fetchResult {
	ch := make(chan fetchResult, 1)
	go func() {
		b, err := e.FetchInfo(ctx, magnet)
		ch <- fetchResult{b, err}
	}()
	return ch
}

func hashOf(t *testing.T, ih string) metainfo.Hash {
	t.Helper()
	var h metainfo.Hash
	if err := h.FromHexString(ih); err != nil {
		t.Fatal(err)
	}
	return h
}

// waitFetchWaiters waits until a fetch of ih has n callers.
func waitFetchWaiters(t *testing.T, e *Engine, ih string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		f := e.fetching[ih]
		got := 0
		if f != nil {
			got = f.waiters
		}
		e.mu.Unlock()
		if got == n {
			if _, ok := e.client.Torrent(hashOf(t, ih)); ok {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fetch of %s has %d callers, want %d", ih, got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitResult(t *testing.T, ch chan fetchResult, d time.Duration) fetchResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(d):
		t.Fatal("FetchInfo did not return")
	}
	return fetchResult{}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, d := range des {
		out = append(out, d.Name())
	}
	return out
}

func rowCount(t *testing.T, e *Engine, ih string) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM torrents WHERE infohash=?`, ih).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// assertNothingLeft checks that a fetch left no trace: no client torrent, no
// rows, no data in the engine's dirs, no temp dir.
func assertNothingLeft(t *testing.T, e *Engine, tmp, ih, name string) {
	t.Helper()
	if n := len(e.client.Torrents()); n != 0 {
		t.Fatalf("%d torrents left in the client", n)
	}
	if _, ok := e.Get(ih); ok {
		t.Fatal("fetch created an engine torrent")
	}
	if n := rowCount(t, e, ih); n != 0 {
		t.Fatalf("%d torrents rows", n)
	}
	if all, _ := pieceRows(t, e.db, ih); all != 0 {
		t.Fatalf("%d torrent_pieces rows", all)
	}
	if left := dirEntries(t, tmp); len(left) != 0 {
		t.Fatalf("temp dirs left: %v", left)
	}
	if _, err := os.Stat(e.metafilePath(ih)); err == nil {
		t.Fatal("metafile written")
	}
	for _, root := range []string{e.cfg.StreamDir, e.cfg.DataDir} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && (d.Name() == name || d.Name() == ih) {
				t.Errorf("fetch left %s", p)
			}
			return nil
		})
	}
	e.mu.Lock()
	n := len(e.fetching)
	e.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d fetches still registered", n)
	}
}

func TestFetchInfoMagnet(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 30000, 40000, 50000)
	seed := multiSeeder(t, fx, nil)
	leech, tmp := fetchEngine(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ch := startFetch(leech, ctx, magnetOf(fx.ih, "x"))
	waitFetchWaiters(t, leech, fx.ih, 1)
	connect(t, seed, leech, fx.ih) // the seeder dials the leecher
	r := waitResult(t, ch, 20*time.Second)
	if r.err != nil {
		t.Fatal(r.err)
	}
	ih, name, files, err := ParseFiles(r.meta)
	if err != nil {
		t.Fatal(err)
	}
	if ih != fx.ih || name != albumName || len(files) != 3 {
		t.Fatalf("ih %s name %q files %+v", ih, name, files)
	}
	for i, f := range files {
		if f.Size != int64(len(fx.files[i])) || f.Path != albumName+"/"+fx.names[i] {
			t.Fatalf("file %d %+v", i, f)
		}
	}
	assertNothingLeft(t, leech, tmp, fx.ih, albumName)
	// The seeder's own torrent is untouched.
	if s, _ := status(seed, fx.ih); s.Status != StatusSeeding {
		t.Fatalf("seeder %+v", s)
	}
}

func TestFetchInfoKnownTorrentUntouched(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 30000, 40000, 50000)
	seed := multiSeeder(t, fx, nil)
	leech, tmp := fetchEngine(t)
	save := t.TempDir()
	if _, err := leech.AddMagnet(context.Background(), magnetOf(fx.ih, "x"), AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatal(err)
	}
	leech.mu.Lock()
	before := leech.entries[fx.ih].t
	leech.mu.Unlock()

	h := hashOf(t, fx.ih)
	b32 := "magnet:?xt=urn:btih:" + base32.StdEncoding.EncodeToString(h[:])
	for _, m := range []string{magnetOf(fx.ih, "x"), magnetOf(strings.ToUpper(fx.ih), "x"), b32} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		start := time.Now()
		_, err := leech.FetchInfo(ctx, m)
		cancel()
		if !errors.Is(err, ErrKnown) {
			t.Fatalf("%s: err %v, want ErrKnown", m, err)
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Fatalf("%s: ErrKnown took %s (a fetch ran)", m, time.Since(start))
		}
	}
	leech.mu.Lock()
	after := leech.entries[fx.ih].t
	leech.mu.Unlock()
	if after != before || closed(after) {
		t.Fatal("the live torrent was replaced or closed")
	}
	if left := dirEntries(t, tmp); len(left) != 0 {
		t.Fatalf("temp dirs: %v", left)
	}
	connect(t, leech, seed, fx.ih)
	waitFor(t, leech, fx.ih, 20*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	for i := range fx.files {
		got, err := os.ReadFile(fx.path(save, i))
		if err != nil || !bytes.Equal(got, fx.files[i]) {
			t.Fatalf("file %d not downloaded: %v", i, err)
		}
	}
}

// A torrent anacrolix holds outside the engine's entries is waited on, never
// dropped.
func TestFetchInfoForeignTorrentNotDropped(t *testing.T) {
	leech, tmp := fetchEngine(t)
	var h metainfo.Hash
	rand.Read(h[:])
	ft, _, err := leech.client.AddTorrentSpec(&atorrent.TorrentSpec{AddTorrentOpts: atorrent.AddTorrentOpts{InfoHash: h}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := leech.FetchInfo(ctx, magnetOf(h.HexString(), "x")); !errors.Is(err, ErrNoInfo) {
		t.Fatalf("err %v", err)
	}
	if closed(ft) {
		t.Fatal("foreign torrent dropped")
	}
	if left := dirEntries(t, tmp); len(left) != 0 {
		t.Fatalf("temp dirs: %v", left)
	}
}

func TestAddDuringFetchInfo(t *testing.T) {
	t.Run("magnet, fetch succeeds", func(t *testing.T) {
		fx := newMultiFixture(t, 16<<10, 30000, 40000, 50000)
		seed := multiSeeder(t, fx, nil)
		leech, tmp := fetchEngine(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		fch := startFetch(leech, ctx, magnetOf(fx.ih, "x"))
		waitFetchWaiters(t, leech, fx.ih, 1)

		save := t.TempDir()
		ach := make(chan error, 1)
		go func() {
			_, err := leech.AddMagnet(context.Background(), magnetOf(fx.ih, "x"), AddOptions{Mode: ModeDownload, SaveDir: save})
			ach <- err
		}()
		select {
		case err := <-ach:
			t.Fatalf("add returned during the fetch: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		connect(t, seed, leech, fx.ih)
		if r := waitResult(t, fch, 20*time.Second); r.err != nil {
			t.Fatal(r.err)
		}
		if err := <-ach; err != nil {
			t.Fatal(err)
		}
		assertAddedAndDownloads(t, leech, seed, fx, save, tmp)
	})

	t.Run("torrent file, fetch times out", func(t *testing.T) {
		fx := newMultiFixture(t, 16<<10, 30000, 40000, 50000)
		seed := multiSeeder(t, fx, nil)
		leech, tmp := fetchEngine(t)
		ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
		defer cancel()
		fch := startFetch(leech, ctx, magnetOf(fx.ih, "x"))
		waitFetchWaiters(t, leech, fx.ih, 1)

		save := t.TempDir()
		ach := make(chan error, 1)
		go func() {
			_, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: save})
			ach <- err
		}()
		if r := waitResult(t, fch, 5*time.Second); !errors.Is(r.err, ErrNoInfo) {
			t.Fatalf("fetch err %v", r.err)
		}
		if err := <-ach; err != nil {
			t.Fatal(err)
		}
		assertAddedAndDownloads(t, leech, seed, fx, save, tmp)
	})
}

func assertAddedAndDownloads(t *testing.T, leech, seed *Engine, fx multiFixture, save, tmp string) {
	t.Helper()
	if _, ok := leech.Get(fx.ih); !ok {
		t.Fatal("add did not register the torrent")
	}
	leech.mu.Lock()
	lt := leech.entries[fx.ih].t
	leech.mu.Unlock()
	if lt == nil || closed(lt) {
		t.Fatal("engine torrent is closed (attached to the fetch torrent)")
	}
	if left := dirEntries(t, tmp); len(left) != 0 {
		t.Fatalf("temp dirs: %v", left)
	}
	connect(t, leech, seed, fx.ih)
	waitFor(t, leech, fx.ih, 20*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	for i := range fx.files {
		got, err := os.ReadFile(fx.path(save, i))
		if err != nil || !bytes.Equal(got, fx.files[i]) {
			t.Fatalf("file %d not in the engine's save dir: %v", i, err)
		}
	}
	if all, complete := pieceRows(t, leech.db, fx.ih); all == 0 || all != complete {
		t.Fatalf("piece rows %d/%d", complete, all)
	}
}

func TestFetchInfoTwiceConcurrently(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 30000, 40000, 50000)
	seed := multiSeeder(t, fx, nil)
	leech, tmp := fetchEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	a := startFetch(leech, ctx, magnetOf(fx.ih, "a"))
	waitFetchWaiters(t, leech, fx.ih, 1)
	b := startFetch(leech, ctx, magnetOf(fx.ih, "b"))
	waitFetchWaiters(t, leech, fx.ih, 2)
	if n := len(leech.client.Torrents()); n != 1 {
		t.Fatalf("%d client torrents, want one shared fetch", n)
	}
	if left := dirEntries(t, tmp); len(left) != 1 {
		t.Fatalf("temp dirs %v, want one", left)
	}
	connect(t, seed, leech, fx.ih)
	ra, rb := waitResult(t, a, 20*time.Second), waitResult(t, b, 20*time.Second)
	if ra.err != nil || rb.err != nil {
		t.Fatal(ra.err, rb.err)
	}
	if !bytes.Equal(ra.meta, rb.meta) {
		t.Fatal("callers got different metafiles")
	}
	assertNothingLeft(t, leech, tmp, fx.ih, albumName)
}

// One caller giving up does not end the fetch for the other.
func TestFetchInfoOneCallerLeaves(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 30000, 40000, 50000)
	seed := multiSeeder(t, fx, nil)
	leech, tmp := fetchEngine(t)
	short, cancelShort := context.WithCancel(context.Background())
	long, cancelLong := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelLong()
	a := startFetch(leech, short, magnetOf(fx.ih, "a"))
	waitFetchWaiters(t, leech, fx.ih, 1)
	b := startFetch(leech, long, magnetOf(fx.ih, "b"))
	waitFetchWaiters(t, leech, fx.ih, 2)
	cancelShort()
	if r := waitResult(t, a, 5*time.Second); !errors.Is(r.err, ErrNoInfo) {
		t.Fatalf("cancelled caller err %v", r.err)
	}
	connect(t, seed, leech, fx.ih)
	if r := waitResult(t, b, 20*time.Second); r.err != nil {
		t.Fatal(r.err)
	}
	assertNothingLeft(t, leech, tmp, fx.ih, albumName)
}

func randomIH() string {
	b := make([]byte, 20)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func TestFetchInfoTimeoutLeavesNothing(t *testing.T) {
	leech, tmp := fetchEngine(t)
	ih, name := randomIH(), "Призрак"
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := leech.FetchInfo(ctx, magnetOf(ih, name))
	if !errors.Is(err, ErrNoInfo) {
		t.Fatalf("err %v, want ErrNoInfo", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("returned after %s", d)
	}
	assertNothingLeft(t, leech, tmp, ih, name)
}

func TestFetchInfoCancelledLeavesNothing(t *testing.T) {
	leech, tmp := fetchEngine(t)
	ih, name := randomIH(), "Призрак"
	ctx, cancel := context.WithCancel(context.Background())
	ch := startFetch(leech, ctx, magnetOf(ih, name))
	waitFetchWaiters(t, leech, ih, 1)
	cancel()
	if r := waitResult(t, ch, 5*time.Second); !errors.Is(r.err, ErrNoInfo) {
		t.Fatalf("err %v, want ErrNoInfo", r.err)
	}
	assertNothingLeft(t, leech, tmp, ih, name)
}

func TestFetchInfoBadMagnet(t *testing.T) {
	leech, tmp := fetchEngine(t)
	for _, m := range []string{"", "http://example.com", "magnet:?dn=x"} {
		if _, err := leech.FetchInfo(context.Background(), m); err == nil || errors.Is(err, ErrNoInfo) || errors.Is(err, ErrKnown) {
			t.Fatalf("%q: err %v", m, err)
		}
	}
	if left := dirEntries(t, tmp); len(left) != 0 {
		t.Fatalf("temp dirs: %v", left)
	}
}

func closed(t *atorrent.Torrent) bool {
	select {
	case <-t.Closed():
		return true
	default:
		return false
	}
}
