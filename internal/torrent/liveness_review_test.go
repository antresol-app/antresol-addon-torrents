package torrent

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/dht/v2/krpc"
	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// Any probe (a pick's, a switch candidate's, a re-probe) asks a tracker
// only once the interval of its last answer is over (min interval when
// larger; 30 minutes after a refusal), and a probe of an infohash within
// probeCacheFor asks nobody.
func TestProbeHonoursTrackerInterval(t *testing.T) {
	p := testProber()
	now := time.Now()
	p.now = func() time.Time { return now }
	srv, got := httpTracker(t, 200, map[string]any{"interval": 1800, "min interval": 3000, "complete": 0})
	refuses, rgot := httpTracker(t, 200, map[string]any{"failure reason": "Torrent not registered"})
	tg := ProbeTarget{InfoHash: testIH, Trackers: []string{srv.URL + "/a", refuses.URL + "/a"}}
	asked := func(want, rwant int, when string) {
		t.Helper()
		if len(*got) != want || len(*rgot) != rwant {
			t.Fatalf("%s: asked %d/%d, want %d/%d", when, len(*got), len(*rgot), want, rwant)
		}
	}
	p.probe(context.Background(), tg)
	asked(1, 1, "first probe")
	now = now.Add(time.Minute)
	if res := p.probe(context.Background(), tg); res.TrackerErrorKind != TrackerNotRegistered {
		t.Fatalf("cached probe %+v", res)
	}
	asked(1, 1, "a minute later (cached)")
	now = now.Add(28 * time.Minute) // 29 min: cache over, intervals not
	if res := p.probe(context.Background(), tg); res.skipped != 2 {
		t.Fatalf("skipped %d", res.skipped)
	}
	asked(1, 1, "29 min")
	now = now.Add(2 * time.Minute) // 31 min: the refusing tracker is due
	p.probe(context.Background(), tg)
	asked(1, 2, "31 min")
	now = now.Add(26 * time.Minute) // 57 min: the other one (50 min) too
	p.probe(context.Background(), tg)
	asked(2, 2, "57 min")
}

// fakeDHT is a DhtServer whose get_peers returns peers (and counts calls).
type fakeDHT struct {
	calls atomic.Int32
	peers []dht.Peer
}

func (f *fakeDHT) Stats() any                  { return nil }
func (f *fakeDHT) ID() [20]byte                { return [20]byte{} }
func (f *fakeDHT) Addr() net.Addr              { return &net.UDPAddr{} }
func (f *fakeDHT) AddNode(krpc.NodeInfo) error { return nil }
func (f *fakeDHT) Ping(*net.UDPAddr)           {}
func (f *fakeDHT) WriteStatus(io.Writer)       {}
func (f *fakeDHT) Announce([20]byte, int, bool) (atorrent.DhtAnnounce, error) {
	f.calls.Add(1)
	ch := make(chan dht.PeersValues, 1)
	ch <- dht.PeersValues{Peers: f.peers}
	close(ch)
	return fakeAnnounce{ch}, nil
}

type fakeAnnounce struct{ ch chan dht.PeersValues }

func (a fakeAnnounce) Close()                        {}
func (a fakeAnnounce) Peers() <-chan dht.PeersValues { return a.ch }

// A private torrent's infohash never goes to the DHT.
func TestProbePrivateSkipsDHT(t *testing.T) {
	d := &fakeDHT{peers: []dht.Peer{{IP: net.ParseIP("198.51.100.9"), Port: 7000}}}
	p := testProber()
	p.dht = func() []atorrent.DhtServer { return []atorrent.DhtServer{d} }
	if res := p.probe(context.Background(), ProbeTarget{InfoHash: testIH, Private: true}); res.Alive() || d.calls.Load() != 0 {
		t.Fatalf("private probe %+v, dht calls %d", res, d.calls.Load())
	}
	other := "1123456789abcdef0123456789abcdef01234567"
	if res := p.probe(context.Background(), ProbeTarget{InfoHash: other}); !res.Alive() || d.calls.Load() != 1 {
		t.Fatalf("public probe %+v, dht calls %d", res, d.calls.Load())
	}
}

func TestTargetOfPrivate(t *testing.T) {
	yes := true
	info := metainfo.Info{Name: "x", PieceLength: 16384, Pieces: make([]byte, 20), Length: 10, Private: &yes}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mf, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: ib, Announce: "http://t.example/ann?passkey=x"})
	if err != nil {
		t.Fatal(err)
	}
	tg, err := TargetOf("", mf)
	if err != nil || !tg.Private || len(tg.Trackers) != 1 {
		t.Fatalf("target %+v %v", tg, err)
	}
}

// The keys of a torrent's own tracker URLs are hidden verbatim wherever a
// tracker repeats them.
func TestRedactKnownKeys(t *testing.T) {
	trackers := []string{"http://tr.example/announce.php?id=abcXYZ12&x=1", "udp://u.example:80/0123456789abcdefABCD/announce"}
	got := RedactTracker("bad id abcXYZ12 for 0123456789abcdefABCD", trackers...)
	if got != "bad id … for …" {
		t.Fatalf("redacted %q", got)
	}
}

// A peer that completes a handshake marks the torrent at once (no monitor
// tick), and OnPeer hooks hear of it.
func TestPeerSeenOnHandshake(t *testing.T) {
	e, h := stalledEngine(t)
	heard := make(chan string, 1)
	e.OnPeer(func(ih string) { heard <- ih })
	var cb atorrent.Callbacks
	e.installPeerSeen(&cb)
	var ih metainfo.Hash
	copy(ih[:], mustHex(t, h.InfoHash()))
	cb.CompletedHandshake(nil, ih)
	if !h.Status().HadPeers {
		t.Fatal("HadPeers false after a handshake")
	}
	select {
	case got := <-heard:
		if got != h.InfoHash() {
			t.Fatalf("hook got %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnPeer not called")
	}
	cb.CompletedHandshake(nil, ih)
	select {
	case <-heard:
		t.Fatal("OnPeer called twice")
	case <-time.After(100 * time.Millisecond):
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	var h metainfo.Hash
	if err := h.FromHexString(s); err != nil {
		t.Fatal(err)
	}
	return h[:]
}

// RemoveIfIdle keeps a torrent that has had a peer, and never deletes
// files (a magnet's display name may be another folder's).
func TestRemoveIfIdle(t *testing.T) {
	e, h := stalledEngine(t)
	e.mu.Lock()
	en := e.entries[h.InfoHash()]
	en.everPeer = true
	dir, name := en.rec.SaveDir, en.rec.Name
	e.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, name, "unrelated.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveIfIdle(context.Background(), h.InfoHash()); !errors.Is(err, ErrNotIdle) {
		t.Fatalf("removed a torrent that had a peer: %v", err)
	}
	e.mu.Lock()
	en.everPeer = false
	e.mu.Unlock()
	if err := e.RemoveIfIdle(context.Background(), h.InfoHash()); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.Get(h.InfoHash()); ok {
		t.Fatal("still there")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("files deleted: %v", err)
	}
}

// CheckLiveness of a torrent probed within reprobeEvery asks nobody;
// NoteProbe takes a probe made before the add as the check.
func TestCheckLivenessReusesRecentProbe(t *testing.T) {
	e, h := stalledEngine(t)
	var calls atomic.Int32
	e.mu.Lock()
	e.probe = func(context.Context, ProbeTarget) SwarmProbe { calls.Add(1); return SwarmProbe{Peers: 1} }
	e.mu.Unlock()
	e.NoteProbe(h.InfoHash(), SwarmProbe{Seeders: 2})
	if st := h.Status(); st.Liveness != LivenessAlive {
		t.Fatalf("liveness %q after NoteProbe", st.Liveness)
	}
	e.CheckLiveness(h.InfoHash())
	e.CheckLiveness(h.InfoHash())
	time.Sleep(100 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("probed %d times within reprobeEvery", calls.Load())
	}
}

// After a start, stalled torrents are not re-probed during reprobeGrace,
// and then each at its own offset.
func TestReprobeWaitsAfterStart(t *testing.T) {
	e, h := stalledEngine(t, true)
	var calls atomic.Int32
	e.mu.Lock()
	e.probe = func(context.Context, ProbeTarget) SwarmProbe { calls.Add(1); return SwarmProbe{} }
	e.reprobeGrace, e.reprobeJitter = time.Hour, 0
	e.mu.Unlock()
	eventually(t, func() bool { return h.Status().Stalled == StalledNoPeers })
	time.Sleep(500 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("re-probed during the grace after start")
	}
	e.mu.Lock()
	en := e.entries[h.InfoHash()]
	now := time.Now()
	e.started = now.Add(-2 * time.Hour)
	e.reprobeJitter = time.Hour
	j := jitterOf(en.ih, e.reprobeJitter)
	e.maybeReprobe(en, e.started.Add(time.Hour+j-time.Second))
	before := en.probing
	e.maybeReprobe(en, e.started.Add(time.Hour+j))
	after := en.probing
	e.mu.Unlock()
	if before || !after {
		t.Fatalf("re-probe before its offset %v, at it %v", before, after)
	}
}
