package torrent

import (
	"context"
	"io"
	"testing"
	"time"
)

func TestBufferFollowsTheWaitingReader(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	if h, ok := seed.Get(fx.ih); !ok {
		t.Fatal("no seed")
	} else if have, want := h.(*entry).Buffer(0); want == 0 || have != want {
		t.Fatalf("complete torrent: have %d of %d", have, want)
	}

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
	en := h.(*entry)
	if have, want := en.Buffer(0); want != min(int64(readahead), int64(testSize)) || have != 0 {
		t.Fatalf("before any read: have %d of %d, want 0 of the readahead from the start", have, want)
	}
	r, err := h.NewReader(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	mid := int64(testSize - 1<<20)
	r.Seek(mid, io.SeekStart)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(r, make([]byte, 4096)); done <- err }()
	time.Sleep(300 * time.Millisecond)
	// The reader waits at mid: the window is the 1 MiB left from there.
	if have, want := en.Buffer(0); want != 1<<20 || have != 0 {
		t.Fatalf("waiting at the end: have %d of %d", have, want)
	}
	connect(t, leech, seed, fx.ih)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		have, want := en.Buffer(0)
		if have == want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("buffer never filled: %d of %d", have, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if have, want := en.Buffer(7); want != 0 || have != 0 {
		t.Fatalf("bad index: %d %d", have, want)
	}
}
