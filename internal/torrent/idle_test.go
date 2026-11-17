package torrent

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	atorrent "github.com/anacrolix/torrent"
)

// stalledLeech returns a leecher of fx (selection files, nil = whole)
// that stopped requesting behind the engine's back with peers connected:
// the state anacrolix v1.61 can end up in after a piece fails its hash —
// nothing in flight, no event to re-plan. It gets file 0 from a partial
// seeder (so it has pieces and accepts connections without wanting any),
// then its remaining wanted pieces are dropped to priority None (a
// consistent anacrolix state, unlike DisallowDataDownload with pieces
// pending) and seeds connect. setup runs on the engine (under e.mu) before
// the add. The idle spell is restarted at the returned time.
func stalledLeech(t *testing.T, fx multiFixture, files []int, hooks *hookCount, stall time.Duration, setup func(*Engine), seeds ...*Engine) (*Engine, time.Time) {
	t.Helper()
	ctx := context.Background()
	partial := multiSeeder(t, fx, []int{0})
	leech := openEngine(t, t.TempDir(), engOpts{stall: stall})
	if setup != nil {
		leech.mu.Lock()
		setup(leech)
		leech.mu.Unlock()
	}
	if hooks != nil {
		leech.OnComplete(hooks.add)
	}
	if _, err := leech.AddTorrentFile(ctx, fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir(), Files: files}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, partial, fx.ih)
	waitFor(t, leech, fx.ih, 10*time.Second, func(s Status) bool { return fileBytesCompleted(leech, fx.ih, 0) == 32<<10 })
	at := liveTorrent(leech, fx.ih)
	if files == nil {
		at.CancelPieces(0, at.NumPieces()) // undo DownloadAll
	} else {
		for _, i := range files {
			at.Files()[i].SetPriority(atorrent.PiecePriorityNone)
		}
	}
	for _, seed := range seeds {
		connect(t, leech, seed, fx.ih)
	}
	waitFor(t, leech, fx.ih, 5*time.Second, func(s Status) bool { return s.Peers == 1+len(seeds) })
	if k := leech.idleKicks.Load(); k != 0 {
		t.Fatalf("reconnects before the stall: %d", k)
	}
	// The partial-seeder phase may already have started an idle spell
	// (that peer lacks the rest: re-plans only); start a fresh one.
	leech.mu.Lock()
	en := leech.entries[fx.ih]
	en.idleSince, en.idleTries = time.Time{}, 0
	leech.mu.Unlock()
	return leech, time.Now()
}

func liveTorrent(e *Engine, ih string) *atorrent.Torrent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.entries[ih].t
}

// waitCounter waits until c (an idle-check counter of e) reaches n and
// returns when it did. The counter moves at the start of an action; the
// action runs under e.mu, so taking e.mu waits for it to finish.
func waitCounter(t *testing.T, e *Engine, c *atomic.Int64, n int64, d time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(d)
	for c.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("counter %d, want %d", c.Load(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	at := time.Now()
	e.mu.Lock()
	e.mu.Unlock()
	return at
}

// An idle torrent with peers and nothing requested gets its requests
// re-planned by the monitor, no sooner than StallTimeout, and completes;
// whole torrent and selection.
func TestIdleTorrentRequestsReplanned(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	seed := multiSeeder(t, fx, nil)
	for _, files := range [][]int{{0, 2}, nil} {
		var hooks hookCount
		leech, from := stalledLeech(t, fx, files, &hooks, 500*time.Millisecond, nil, seed)
		n0 := leech.idleNudges.Load()
		first := waitCounter(t, leech, &leech.idleNudges, n0+1, 5*time.Second)
		if el := first.Sub(from); el < 450*time.Millisecond {
			t.Fatalf("files %v: re-planned %v into the spell, before StallTimeout", files, el)
		}
		s := waitFor(t, leech, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
		if s.Done != s.Size || s.Error != "" {
			t.Fatalf("files %v: status %+v", files, s)
		}
		waitHooks(t, &hooks, 1, 2*time.Second)
		if k := leech.idleKicks.Load(); k != 0 {
			t.Fatalf("files %v: the re-plan was enough, yet %d reconnects", files, k)
		}
		// Complete now: never nudged again.
		n := leech.idleNudges.Load()
		time.Sleep(2 * time.Second)
		if m := leech.idleNudges.Load(); m != n {
			t.Fatalf("files %v: complete torrent nudged %d more times", files, m-n)
		}
	}
}

// When re-planning does not help, the second action of a spell (not the
// first) reconnects the peers that unchoked us and have wanted pieces —
// untrusted — and never a choking peer; actions back off; then the
// download can finish.
func TestIdleEscalatesToReconnect(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	seedA, seedB := multiSeeder(t, fx, nil), multiSeeder(t, fx, nil)
	var replans atomic.Int64
	const stall = 300 * time.Millisecond
	noReplan := func(e *Engine) { e.replan = func(*atorrent.Torrent, []int) { replans.Add(1) } } // re-plans do not help
	leech, _ := stalledLeech(t, fx, []int{0, 2}, nil, stall, noReplan, seedA, seedB)
	lt := liveTorrent(leech, fx.ih)
	portOf := func(e *Engine) string { return port(e.client.ListenAddrs()[0].String()) }
	var pcA, pcB *atorrent.PeerConn
	for _, pc := range lt.PeerConns() {
		switch port(pc.RemoteAddr.String()) {
		case portOf(seedA):
			pcA = pc
		case portOf(seedB):
			pcB = pc
		}
	}
	if pcA == nil || pcB == nil {
		t.Fatalf("connections A %v B %v", pcA, pcB)
	}
	// B chokes us for good (paused: uploads disallowed); A counts as
	// having unchoked us (a seeder may or may not unchoke a leecher that is
	// not interested, so it is set by hand). The partial seeder lacks file
	// 2 and is never a candidate.
	if err := seedB.Pause(context.Background(), fx.ih); err != nil {
		t.Fatal(err)
	}
	leech.mu.Lock()
	leech.reqs.mu.Lock()
	leech.reqs.get(&pcA.Peer).unchoked = true
	leech.reqs.mu.Unlock()
	n0 := leech.idleNudges.Load()
	leech.mu.Unlock()

	waitCounter(t, leech, &leech.idleNudges, n0+1, 5*time.Second)
	if k := leech.idleKicks.Load(); k != 0 {
		t.Fatalf("reconnect on the first action: %d", k)
	}
	second := waitCounter(t, leech, &leech.idleNudges, n0+2, 5*time.Second)
	if k := leech.idleKicks.Load(); k != 1 {
		t.Fatalf("reconnects on the second action: %d, want 1 (A only)", k)
	}
	// Backoff: the third action comes 4× StallTimeout after the second.
	third := waitCounter(t, leech, &leech.idleNudges, n0+3, 5*time.Second)
	if gap := third.Sub(second); gap < 4*stall-50*time.Millisecond {
		t.Fatalf("third action %v after the second", gap)
	}
	// The re-dialled A may have unchoked us by now (then it is kicked
	// again, correctly); B never is.
	if k := leech.idleKicks.Load(); k < 1 || k > 2 {
		t.Fatalf("reconnects after the third action: %d", k)
	}
	if replans.Load() != n0+3 {
		t.Fatalf("replans %d", replans.Load())
	}
	stillB, newA := false, false
	for deadline := time.Now().Add(3 * time.Second); !(stillB && newA) && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		stillB, newA = false, false
		for _, pc := range lt.PeerConns() {
			stillB = stillB || pc == pcB
			newA = newA || pc != pcA && port(pc.RemoteAddr.String()) == portOf(seedA)
		}
	}
	if !stillB || !newA {
		t.Fatalf("choking peer kept %v, unchoked peer re-dialled %v", stillB, newA)
	}
	// The real re-plan on the next action lets it finish (from A).
	leech.mu.Lock()
	leech.replan = replan
	leech.mu.Unlock()
	waitFor(t, leech, fx.ih, 15*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
}

func port(addr string) string {
	_, p, _ := net.SplitHostPort(addr)
	return p
}

// A reconnected peer is never re-added as trusted: trusted peers skip
// anacrolix's banned-address check and are never banned for bad data.
func TestRedialNeverTrusted(t *testing.T) {
	if pi := redialInfo("127.0.0.1:6881"); pi.Trusted || pi.Addr == nil {
		t.Fatalf("redial %+v", pi)
	}
}

// Re-plans of a torrent that stays idle (its peer lacks what it wants)
// back off (StallTimeout, 2×, 4×, …), and that peer is never reconnected.
func TestIdleReplanBounded(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	partial := multiSeeder(t, fx, []int{0})
	leech := openEngine(t, t.TempDir(), engOpts{stall: 300 * time.Millisecond})
	if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir(), Files: []int{0, 2}}); err != nil {
		t.Fatal(err)
	}
	connect(t, leech, partial, fx.ih)
	waitFor(t, leech, fx.ih, 10*time.Second, func(s Status) bool { return s.Done == 32<<10 })
	start := time.Now()
	n0 := leech.idleNudges.Load()
	time.Sleep(4 * time.Second)
	n := leech.idleNudges.Load() - n0
	// Without backoff 4 s / 300 ms = 13; with it actions at +0.3, +0.9,
	// +2.1, +4.5 s from the spell's start.
	if n < 1 || n > 4 {
		t.Fatalf("re-plans in %v: %d", time.Since(start), n)
	}
	if s, _ := status(leech, fx.ih); s.Peers != 1 || s.Status != StatusDownloading {
		t.Fatalf("status %+v", s)
	}
	// The peer has nothing we want: never reconnected.
	if k := leech.idleKicks.Load() + leech.stallKicks.Load(); k != 0 {
		t.Fatalf("peer without wanted pieces reconnected %d times", k)
	}
}

// Healthy downloads, complete torrents (seeding with peers connected) and
// paused torrents are never nudged.
func TestIdleNeverNudgesHealthy(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10, 32<<10)
	src := t.TempDir()
	fx.writeFiles(t, src, nil)
	seed := openEngine(t, t.TempDir(), engOpts{stall: 300 * time.Millisecond})
	if _, err := seed.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, seed, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	var leeches []*Engine
	for _, files := range [][]int{nil, {0, 2}} {
		leech := openEngine(t, t.TempDir(), engOpts{stall: 300 * time.Millisecond})
		if _, err := leech.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir(), Files: files}); err != nil {
			t.Fatal(err)
		}
		connect(t, leech, seed, fx.ih)
		waitFor(t, leech, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
		leeches = append(leeches, leech)
	}
	paused := openEngine(t, t.TempDir(), engOpts{stall: 300 * time.Millisecond})
	if _, err := paused.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir(), Paused: true}); err != nil {
		t.Fatal(err)
	}
	connect(t, paused, seed, fx.ih)
	time.Sleep(2 * time.Second) // 6+ stall timeouts, peers connected
	for i, e := range append(leeches, seed, paused) {
		if n, k := e.idleNudges.Load(), e.idleKicks.Load(); n != 0 || k != 0 {
			t.Fatalf("engine %d nudged %d times, reconnected %d", i, n, k)
		}
	}
	// The partial (selection) leech stays connected to the seeder; the
	// whole one is dropped as mutually complete.
	if s, _ := status(leeches[1], fx.ih); s.Peers == 0 {
		t.Fatalf("no peer connected, the check proves nothing: %+v", s)
	}
}
