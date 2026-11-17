package torrent

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// sameData builds a metafile of fx's very files under another infohash
// (another tracker's copy: only the "source" field differs).
func sameData(t *testing.T, fx fixture) fixture { return sameDataFrom(t, fx, "another tracker") }

// sameDataFrom is sameData with the source field (one infohash per source).
func sameDataFrom(t *testing.T, fx fixture, source string) fixture {
	t.Helper()
	mi, err := metainfo.Load(bytes.NewReader(fx.metafile))
	if err != nil {
		t.Fatal(err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	info.Source = source
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	out := metainfo.MetaInfo{InfoBytes: ib}
	var buf bytes.Buffer
	if err := out.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return fixture{data: fx.data, metafile: buf.Bytes(), ih: out.HashInfoBytes().HexString(), name: fx.name}
}

// namesakeOf builds a torrent named as newFixture's with other files (a
// file of another size).
func namesakeOf(t *testing.T) fixture {
	t.Helper()
	return newFixtureSize(t, t.TempDir(), testSize/2)
}

// rehashed builds a metafile of fx's files with another piece size (the
// same release uploaded again elsewhere).
func rehashed(t *testing.T, fx fixture) fixture {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, fx.name)
	if err := os.WriteFile(p, fx.data, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 512 << 10}
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
	return fixture{data: fx.data, metafile: buf.Bytes(), ih: mi.HashInfoBytes().HexString(), name: fx.name}
}

// Live (Suits S6 / S8): two different releases whose top folder has the
// same name each get a path of their own in the same save dir; neither is
// blocked. The same files under another infohash are still refused.
func TestNamesakeGetsOwnPath(t *testing.T) {
	ctx := context.Background()
	e := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	a := newFixture(t, t.TempDir())
	b := namesakeOf(t) // same name, other files
	if _, err := e.AddTorrentFile(ctx, a.metafile, AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddTorrentFile(ctx, b.metafile, AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatalf("a namesake was refused: %v", err)
	}
	sa, _ := status(e, a.ih)
	sb, _ := status(e, b.ih)
	if sa.SaveDir != save || sb.SaveDir != filepath.Join(save, b.ih) {
		t.Fatalf("save dirs %q and %q; want %q and %q", sa.SaveDir, sb.SaveDir, save, filepath.Join(save, b.ih))
	}
	if sb.Error != "" || sb.Status == StatusError {
		t.Fatalf("namesake blocked: %+v", sb)
	}
	dup := sameData(t, a)
	_, err := e.AddTorrentFile(ctx, dup.metafile, AddOptions{Mode: ModeDownload, SaveDir: save})
	if !errors.Is(err, ErrPathInUse) {
		t.Fatalf("the same files again: err %v, want ErrPathInUse", err)
	}
	var pe *PathInUseError
	if !errors.As(err, &pe) || pe.Owner != a.ih {
		t.Fatalf("the error does not name the owner %s: %#v", a.ih, err)
	}
	// the same files re-hashed (another piece size) are the same files too
	if _, err := e.AddTorrentFile(ctx, rehashed(t, a).metafile, AddOptions{Mode: ModeDownload, SaveDir: save}); !errors.Is(err, ErrPathInUse) {
		t.Fatalf("the same files re-hashed: err %v, want ErrPathInUse", err)
	}
	// removing the namesake with its data leaves no empty folder behind
	if err := e.Remove(ctx, b.ih, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(save, b.ih)); !os.IsNotExist(err) {
		t.Fatalf("the namesake's folder is left: %v", err)
	}
}

// A magnet that resolves to the name of a running torrent with other files
// moves to a path of its own and downloads there; the other is untouched.
func TestNamesakeMagnetMovesAside(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	e := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	b := namesakeOf(t) // same file name, other files
	if _, err := e.AddTorrentFile(context.Background(), b.metafile, AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddMagnet(context.Background(), "magnet:?xt=urn:btih:"+fx.ih+"&dn="+url.QueryEscape(fx.name), AddOptions{Mode: ModeDownload, SaveDir: save}); err != nil {
		t.Fatal(err)
	}
	connect(t, e, seed, fx.ih)
	s := waitFor(t, e, fx.ih, 15*time.Second, func(s Status) bool { return s.SaveDir == filepath.Join(save, fx.ih) && s.Status != StatusQueued })
	if s.Error != "" {
		t.Fatalf("namesake magnet: %+v", s)
	}
	// the re-attached torrent: the re-attach runs in the background
	for deadline := time.Now().Add(5 * time.Second); ; {
		e.mu.Lock()
		en := e.entries[fx.ih]
		at, relocating := en.t, en.relocating
		e.mu.Unlock()
		if at != nil && !relocating && at.AddClientPeer(seed.client) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the namesake was never re-attached")
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, e, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	got, _ := os.ReadFile(filepath.Join(save, fx.ih, fx.name))
	if !bytes.Equal(got, fx.data) {
		t.Fatal("the namesake's data differs")
	}
	if sb, _ := status(e, b.ih); sb.Error != "" || sb.SaveDir != save {
		t.Fatalf("the other torrent changed: %+v", sb)
	}
}

// Live (Suits S8): a namesake blocked by an earlier version (nothing of it
// downloaded) gets a path of its own on restart and runs; a genuine
// duplicate (the same files) stays blocked.
func TestBlockedNamesakeRecoversOnRestart(t *testing.T) {
	src := t.TempDir()
	a := newFixture(t, src)
	b := namesakeOf(t) // same name, other files
	c := sameData(t, a)
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
	for _, fx := range []fixture{b, c} {
		if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // distinct added_at: restore order
	}
	e.Close()
	// as an earlier version left them: both at a's path, blocked
	if _, err := d.Exec(`UPDATE torrents SET save_dir=?, error='файлы этого торрента совпадают с торрентом `+a.ih+`' WHERE infohash IN (?,?)`, src, b.ih, c.ih); err != nil {
		t.Fatal(err)
	}
	e2, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { e2.Close(); d.Close() }()
	sb := waitFor(t, e2, b.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusDownloading })
	if sb.Error != "" || sb.SaveDir != filepath.Join(src, b.ih) {
		t.Fatalf("namesake after restart: %+v", sb)
	}
	sc := waitFor(t, e2, c.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusError })
	if !strings.Contains(sc.Error, a.ih) || sc.SaveDir != src {
		t.Fatalf("duplicate after restart: %+v", sc)
	}
	if sa, _ := status(e2, a.ih); sa.Status != StatusSeeding || sa.Error != "" {
		t.Fatalf("owner: %+v", sa)
	}
	got, err := os.ReadFile(filepath.Join(src, a.name))
	if err != nil || !bytes.Equal(got, a.data) {
		t.Fatalf("the owner's file changed: %v", err)
	}
}
