package torrent

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// A paused seed is hash-checked without starting: WaitVerified reports all
// of it, the torrent stays paused and nothing is uploaded.
func TestWaitVerifiedPaused(t *testing.T) {
	src := t.TempDir()
	fx := newFixture(t, src)
	e := openEngine(t, t.TempDir(), engOpts{})
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src, Paused: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	done, size, err := e.WaitVerified(ctx, fx.ih)
	if err != nil {
		t.Fatal(err)
	}
	if size != testSize || done != size {
		t.Fatalf("done %d of %d", done, size)
	}
	st, _ := status(e, fx.ih)
	if st.Status != StatusPaused || st.Uploaded != 0 {
		t.Fatalf("status %+v", st)
	}
	if _, _, err := e.WaitVerified(ctx, "0000000000000000000000000000000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown torrent: %v", err)
	}
}

// Called right after the add, WaitVerified waits for the hash pass instead
// of reporting the not-yet-checked pieces as missing.
func TestWaitVerifiedNotEarly(t *testing.T) {
	src := t.TempDir()
	data := make([]byte, 8<<20)
	rand.Read(data)
	name := "Тест 64 куска.mkv"
	p := filepath.Join(src, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 128 << 10}
	if err := info.BuildFromFilePath(p); err != nil {
		t.Fatal(err)
	}
	if info.NumPieces() != 64 {
		t.Fatalf("%d pieces", info.NumPieces())
	}
	ib, _ := bencode.Marshal(info)
	mi := metainfo.MetaInfo{InfoBytes: ib}
	var buf bytes.Buffer
	mi.Write(&buf)
	e := openEngine(t, t.TempDir(), engOpts{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := e.AddTorrentFile(ctx, buf.Bytes(), AddOptions{Mode: ModeSeed, SaveDir: src, Paused: true}); err != nil {
		t.Fatal(err)
	}
	done, size, err := e.WaitVerified(ctx, mi.HashInfoBytes().HexString())
	if err != nil {
		t.Fatal(err)
	}
	if done == 0 || done != size {
		t.Fatalf("done %d of %d", done, size)
	}
}
