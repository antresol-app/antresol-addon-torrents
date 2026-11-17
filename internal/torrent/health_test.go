package torrent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// stallHint on a fixed clock: no peers for the window (since start or the
// last peer) is no_peers, peers without metadata is no_metadata, anything
// sooner or with peers and metadata is nothing.
func TestStallHint(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	after := 2 * time.Minute
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	cases := []struct {
		name             string
		now, run, peerAt time.Time
		peers            int
		hasInfo          bool
		want             string
	}{
		{"not running", at(time.Hour), time.Time{}, time.Time{}, 0, false, ""},
		{"no peers, too soon", at(119 * time.Second), t0, time.Time{}, 0, true, ""},
		{"no peers for the window", at(after), t0, time.Time{}, 0, true, StalledNoPeers},
		{"magnet, no peers", at(5 * time.Minute), t0, time.Time{}, 0, false, StalledNoPeers},
		{"peer left a minute ago", at(10 * time.Minute), t0, at(9 * time.Minute), 0, true, ""},
		{"peer left long ago", at(10 * time.Minute), t0, at(7 * time.Minute), 0, true, StalledNoPeers},
		{"peer seen before a resume", at(3 * time.Minute), t0, t0.Add(-time.Hour), 0, true, StalledNoPeers},
		{"peers, metadata", at(time.Hour), t0, at(time.Hour), 3, true, ""},
		{"peers, no metadata, too soon", at(time.Minute), t0, at(time.Minute), 2, false, ""},
		{"peers, no metadata", at(after), t0, at(after), 2, false, StalledNoMetadata},
	}
	for _, c := range cases {
		if got := stallHint(c.now, c.run, c.peerAt, after, c.peers, c.hasInfo); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIncomingBlockedRule(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	after := 15 * time.Minute
	switch {
	case incomingBlocked(t0.Add(time.Hour), time.Time{}, after, 0):
		t.Error("blocked with nothing running")
	case incomingBlocked(t0.Add(after-time.Second), t0, after, 0):
		t.Error("blocked before the window")
	case !incomingBlocked(t0.Add(after), t0, after, 0):
		t.Error("not blocked after the window without incoming")
	case incomingBlocked(t0.Add(time.Hour), t0, after, 1):
		t.Error("blocked although a peer connected in")
	}
}

// A download nobody seeds says no_peers once StalledAfter is over (not
// before); a seeder connecting clears it, and the finished torrent has no
// hint. Pausing clears it at once.
func TestStalledNoPeersThenRecovers(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{})
	leech := openEngine(t, t.TempDir(), engOpts{stalledAfter: 400 * time.Millisecond})
	from := time.Now()
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if s, _ := status(leech, fx.ih); s.Stalled != "" {
		t.Fatalf("stalled at once: %q", s.Stalled)
	}
	s := waitFor(t, leech, fx.ih, 5*time.Second, func(s Status) bool { return s.Stalled != "" })
	if s.Stalled != StalledNoPeers || s.Status != StatusDownloading {
		t.Fatalf("status %s, stalled %q", s.Status, s.Stalled)
	}
	if el := time.Since(from); el < 400*time.Millisecond {
		t.Fatalf("stalled after %v, before StalledAfter", el)
	}
	b, _ := json.Marshal(s)
	if !strings.Contains(string(b), `"stalled":"no_peers"`) {
		t.Fatalf("json %s", b)
	}

	ctx := context.Background()
	if err := leech.Pause(ctx, fx.ih); err != nil {
		t.Fatal(err)
	}
	if s, _ := status(leech, fx.ih); s.Stalled != "" {
		t.Fatalf("paused, still stalled %q", s.Stalled)
	}
	if err := leech.Resume(ctx, fx.ih); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	s = waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if s.Stalled != "" {
		t.Fatalf("seeding, stalled %q", s.Stalled)
	}
	b, _ = json.Marshal(s)
	if strings.Contains(string(b), "stalled") {
		t.Fatalf("json of a healthy torrent names stalled: %s", b)
	}
}

// A magnet with no peer at all: no_peers (its metadata cannot come either).
func TestStalledMagnetWithoutPeers(t *testing.T) {
	leech := openEngine(t, t.TempDir(), engOpts{stalledAfter: 300 * time.Millisecond})
	ih := randomIH()
	if _, err := leech.AddMagnet(context.Background(), magnetOf(ih, "x"), AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, leech, ih, 5*time.Second, func(s Status) bool { return s.Stalled != "" })
	if s.Status != StatusQueued || s.Stalled != StalledNoPeers {
		t.Fatalf("status %s, stalled %q", s.Status, s.Stalled)
	}
}

// IncomingBlocked: a leecher that only dials out is blocked once torrents
// ran IncomingCheckAfter; the seeder it dialled got an incoming handshake
// and is not.
func TestConnectivityIncoming(t *testing.T) {
	seed, fx := newSeeder(t, engOpts{incomingAfter: 300 * time.Millisecond})
	leech := openEngine(t, t.TempDir(), engOpts{incomingAfter: 300 * time.Millisecond})
	if c := leech.Connectivity(); c.IncomingBlocked || c.ListenPort == 0 || c.PortForwarding {
		t.Fatalf("idle engine: %+v", c)
	}
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, seed, fx.ih)
	waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	deadline := time.Now().Add(5 * time.Second)
	for !leech.Connectivity().IncomingBlocked {
		if time.Now().After(deadline) {
			t.Fatalf("leech never blocked: %+v", leech.Connectivity())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if c := leech.Connectivity(); c.IncomingConns != 0 {
		t.Fatalf("leech: %+v", c)
	}
	time.Sleep(400 * time.Millisecond)
	if c := seed.Connectivity(); c.IncomingBlocked || c.IncomingConns == 0 {
		t.Fatalf("seed: %+v", c)
	}
}
