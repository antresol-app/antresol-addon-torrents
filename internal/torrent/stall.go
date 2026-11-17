package torrent

import (
	"context"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	atorrent "github.com/anacrolix/torrent"
	pp "github.com/anacrolix/torrent/peer_protocol"
)

// reqTracker follows our outstanding block requests per peer connection via
// anacrolix callbacks. anacrolix v1.61 occasionally leaves a request
// unanswered forever (seen with one peer under load); with no other event
// the torrent then never requests again. A connection is "stuck" only when
// it has requests in flight and none of them was resolved (data, reject or
// cancel) for StallTimeout. Peers that are merely choking us, or lack the
// pieces we want, have no requests in flight and are never touched.
type reqTracker struct {
	mu    sync.Mutex
	peers map[*atorrent.Peer]*peerReqs
}

type peerReqs struct {
	outstanding int
	since       time.Time // last request resolution, or first request in flight
	// Captured in callbacks, i.e. under the client lock, so the watchdog
	// never reads PeerConn fields unlocked.
	remote     string
	listenPort int
	unchoked   bool // the peer unchoked us (and has not choked us since)
}

// stuckPeer is a connection reported by stuck, with its address snapshot.
type stuckPeer struct {
	p          *atorrent.Peer
	remote     string
	listenPort int
}

func newReqTracker() *reqTracker { return &reqTracker{peers: map[*atorrent.Peer]*peerReqs{}} }

// install hooks the tracker into a client config. Callbacks run with the
// client lock held, so they only touch the tracker's own mutex.
func (rt *reqTracker) install(cb *atorrent.Callbacks) {
	cb.SentRequest = append(cb.SentRequest, func(ev atorrent.PeerRequestEvent) { rt.sent(ev.Peer, time.Now()) })
	cb.DeletedRequest = append(cb.DeletedRequest, func(ev atorrent.PeerRequestEvent) { rt.resolved(ev.Peer, time.Now()) })
	cb.PeerClosed = append(cb.PeerClosed, func(p *atorrent.Peer) { rt.closed(p) })
	cb.PeerConnAdded = append(cb.PeerConnAdded, func(pc *atorrent.PeerConn) {
		remote := ""
		if pc.RemoteAddr != nil {
			remote = pc.RemoteAddr.String()
		}
		rt.mu.Lock()
		rt.get(&pc.Peer).remote = remote
		rt.mu.Unlock()
	})
	prevMsg := cb.ReadMessage
	cb.ReadMessage = func(pc *atorrent.PeerConn, m *pp.Message) {
		if prevMsg != nil {
			prevMsg(pc, m)
		}
		if m.Type != pp.Choke && m.Type != pp.Unchoke {
			return
		}
		rt.mu.Lock()
		if pr := rt.peers[&pc.Peer]; pr != nil {
			pr.unchoked = m.Type == pp.Unchoke
		}
		rt.mu.Unlock()
	}
	prev := cb.ReadExtendedHandshake
	cb.ReadExtendedHandshake = func(pc *atorrent.PeerConn, m *pp.ExtendedHandshakeMessage) {
		if prev != nil {
			prev(pc, m)
		}
		rt.mu.Lock()
		// Only update a live conn's record; never resurrect a closed one.
		if pr := rt.peers[&pc.Peer]; pr != nil {
			pr.listenPort = m.Port
		}
		rt.mu.Unlock()
	}
}

// get returns the record for p, creating it. rt.mu must be held.
func (rt *reqTracker) get(p *atorrent.Peer) *peerReqs {
	pr := rt.peers[p]
	if pr == nil {
		pr = &peerReqs{}
		rt.peers[p] = pr
	}
	return pr
}

func (rt *reqTracker) sent(p *atorrent.Peer, now time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pr := rt.get(p)
	if pr.outstanding == 0 {
		pr.since = now
	}
	pr.outstanding++
}

func (rt *reqTracker) resolved(p *atorrent.Peer, now time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if pr := rt.peers[p]; pr != nil {
		if pr.outstanding > 0 {
			pr.outstanding--
		}
		pr.since = now
	}
}

// snapshot returns p's address record and whether it unchoked us.
func (rt *reqTracker) snapshot(p *atorrent.Peer) (sp stuckPeer, unchoked, ok bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	pr := rt.peers[p]
	if pr == nil {
		return stuckPeer{}, false, false
	}
	return stuckPeer{p, pr.remote, pr.listenPort}, pr.unchoked, true
}

func (rt *reqTracker) closed(p *atorrent.Peer) {
	rt.mu.Lock()
	delete(rt.peers, p)
	rt.mu.Unlock()
}

// stuck returns the connections with requests in flight and nothing
// resolved for timeout. Their timers restart, so each is reported once per
// timeout.
func (rt *reqTracker) stuck(now time.Time, timeout time.Duration) []stuckPeer {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var out []stuckPeer
	for p, pr := range rt.peers {
		if pr.outstanding > 0 && now.Sub(pr.since) >= timeout {
			pr.since = now
			out = append(out, stuckPeer{p, pr.remote, pr.listenPort})
		}
	}
	return out
}

// inFlight returns how many of our requests to t's peers are outstanding.
func (rt *reqTracker) inFlight(t *atorrent.Torrent) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for p, pr := range rt.peers {
		if pr.outstanding > 0 && p.Torrent() == t {
			n += pr.outstanding
		}
	}
	return n
}

// checkIdle (monitor, every tick, e.mu held) revives a torrent that is
// downloading but idle: incomplete (its selection, or the whole torrent),
// with connected peers, nothing requested and no byte written for
// StallTimeout. anacrolix v1.61 can end up there after a piece fails its
// hash: nothing in flight and no event to re-plan. A spell gets actions
// after StallTimeout, then 2×, 4×, … (capped at 16×) more: first the
// torrent's requests are re-planned (e.replan); from the second action on,
// the connections to peers that unchoked us and have a piece we want are
// also reconnected, untrusted (a fresh connection drops whatever request
// state anacrolix got stuck in). Peers that lack what we want, or choke us,
// are left alone. Logged once per spell; progress ends the spell.
func (e *Engine) checkIdle(en *entry, now time.Time) {
	t := en.t
	if t == nil || en.moving || t.Info() == nil || en.hold != "" || en.dupOf != "" || en.rec.Error != "" ||
		en.dataMissing || en.rec.Mode == ModeStream {
		en.idleSince, en.idleLogged, en.idleTries = time.Time{}, false, 0
		return
	}
	done := t.BytesCompleted() // dirty chunks count: any write is progress
	if done != en.idleDone || en.idleSince.IsZero() {
		en.idleDone, en.idleSince, en.idleLogged, en.idleTries = done, now, false, 0
		return
	}
	// Back off within a spell: StallTimeout, then 2×, 4×, … up to 16×
	// (a swarm that just chokes us, or lacks what we want, is not re-planned
	// every minute forever).
	if now.Sub(en.idleSince) < e.cfg.StallTimeout<<min(en.idleTries, 4) {
		return
	}
	en.idleSince = now
	complete := false
	if en.rec.Files == nil {
		complete = t.Complete().Bool()
	} else {
		complete = selectionComplete(t, en.rec.Files)
	}
	if complete || t.Stats().ActivePeers == 0 || e.reqs.inFlight(t) > 0 {
		en.idleTries = 0
		return
	}
	if !en.idleLogged {
		en.idleLogged = true
		e.log.Warn("torrent idle with peers and nothing requested, re-planning requests", "infohash", en.ih)
	}
	en.idleTries++
	e.idleNudges.Add(1)
	// Under e.mu, so no hold, write error or mode change comes in between
	// (writeFailed takes e.mu before it disallows).
	e.replan(t, en.rec.Files)
	if en.idleTries < 2 {
		return
	}
	wanted := wantedPieces(t, en.rec.Files)
	if len(wanted) == 0 {
		return
	}
	for _, pc := range t.PeerConns() {
		sp, unchoked, ok := e.reqs.snapshot(&pc.Peer)
		if !ok || !unchoked || !hasAny(pc, wanted) {
			continue
		}
		e.idleKicks.Add(1)
		e.reconnectPeer(sp, "torrent idle, peer has wanted pieces")
	}
}

// wantedPieces returns the pieces of the selection (every piece without
// one) that are not complete and not being checked: known incomplete,
// not hashing, not queued for a hash.
func wantedPieces(t *atorrent.Torrent, files []int) []uint32 {
	want := func(i int) bool {
		ps := t.PieceState(i)
		return ps.Ok && !ps.Complete && !ps.Hashing && !ps.QueuedForHash && !ps.Marking
	}
	var out []uint32
	if files == nil {
		for i := range t.NumPieces() {
			if want(i) {
				out = append(out, uint32(i))
			}
		}
		return out
	}
	seen := map[int]bool{}
	for i, f := range t.Files() {
		if !isSelected(files, i) || f.Length() <= 0 {
			continue
		}
		for p := f.BeginPieceIndex(); p < f.EndPieceIndex(); p++ {
			if !seen[p] {
				seen[p] = true
				if want(p) {
					out = append(out, uint32(p))
				}
			}
		}
	}
	return out
}

// hasAny reports whether the peer claims any of pieces.
func hasAny(pc *atorrent.PeerConn, pieces []uint32) bool {
	have := pc.PeerPieces()
	for _, i := range pieces {
		if have.Contains(i) {
			return true
		}
	}
	return false
}

// reconnectPeer closes one stuck connection and re-adds the peer's listen
// address (IP + the port from its extended handshake when known, so it also
// works for incoming connections). Other connections are left alone.
func (e *Engine) reconnectPeer(sp stuckPeer, why string) {
	p := sp.p
	t := p.Torrent()
	addr := sp.remote
	if sp.listenPort > 0 {
		if host, _, err := net.SplitHostPort(sp.remote); err == nil {
			addr = net.JoinHostPort(host, strconv.Itoa(sp.listenPort))
		}
	}
	e.log.Warn("reconnecting peer", "reason", why, "infohash", t.InfoHash().HexString(), "peer", sp.remote)
	p.Close() // takes the client lock itself
	if addr == "" {
		return
	}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		// Let the closed conn go away, or the address is skipped as already connected.
		select {
		case <-time.After(500 * time.Millisecond):
		case <-e.closing:
			return
		}
		t.AddPeers([]atorrent.PeerInfo{redialInfo(addr)})
	}()
}

// redialInfo is the peer re-added after a reconnect. Never Trusted: a
// trusted peer skips anacrolix's banned-address check and is never banned
// for bad data, and a peer reconnected here may be the one that sent it.
func redialInfo(addr string) atorrent.PeerInfo {
	return atorrent.PeerInfo{Addr: atorrent.StringAddr(addr), Source: atorrent.PeerSourceDirect}
}

// quietHandler drops anacrolix's per-read error logs for reads that ended
// because their context expired (ReadTimeout or a closed HTTP client); those
// are normal for streaming and reported to the caller as errors anyway.
type quietHandler struct{ slog.Handler }

func (h quietHandler) Handle(ctx context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "read failed") {
		quiet := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "err" {
				v := a.Value.String()
				quiet = strings.Contains(v, "context deadline exceeded") || strings.Contains(v, "context canceled")
				return false
			}
			return true
		})
		if quiet {
			return nil
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h quietHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return quietHandler{h.Handler.WithAttrs(as)}
}

func (h quietHandler) WithGroup(name string) slog.Handler {
	return quietHandler{h.Handler.WithGroup(name)}
}
