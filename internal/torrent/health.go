package torrent

import (
	"time"

	atorrent "github.com/anacrolix/torrent"
)

// Stalled hints (Status.Stalled): why a running download makes no headway,
// so the apps can say so instead of a silent «0 %».
const (
	// StalledNoPeers: no peer has been connected for StalledAfter since the
	// torrent started (or resumed), or since the last peer left. A magnet
	// in this state is also still without its metadata.
	StalledNoPeers = "no_peers"
	// StalledNoMetadata: a magnet has peers but still no metadata after
	// StalledAfter.
	StalledNoMetadata = "no_metadata"
)

// Defaults of EngineConfig.StalledAfter and IncomingCheckAfter.
const (
	defaultStalledAfter       = 2 * time.Minute
	defaultIncomingCheckAfter = 15 * time.Minute
)

// stallHint decides Status.Stalled. runSince: when the torrent last started
// running (attach or resume); peersAt: the last tick it had a peer (zero =
// never); peers: connected now; hasInfo: the metadata is there.
func stallHint(now, runSince, peersAt time.Time, after time.Duration, peers int, hasInfo bool) string {
	if runSince.IsZero() {
		return ""
	}
	if peers == 0 {
		since := runSince
		if peersAt.After(since) {
			since = peersAt
		}
		if now.Sub(since) >= after {
			return StalledNoPeers
		}
		return ""
	}
	if !hasInfo && now.Sub(runSince) >= after {
		return StalledNoMetadata
	}
	return ""
}

// checkStalled (monitor, every tick, e.mu held) updates en.stalled. Only a
// torrent that should be downloading counts: not held, failed, blocked,
// moving or complete (a seeding torrent without peers is just idle).
func (e *Engine) checkStalled(en *entry, now time.Time) {
	t := en.t
	running := t != nil && !en.moving && en.hold == "" && en.dupOf == "" && en.rec.Error == "" && !en.dataMissing
	if running && t.Info() != nil {
		if en.rec.Files == nil {
			running = !t.Complete().Bool()
		} else {
			running = !selectionComplete(t, en.rec.Files)
		}
	}
	if !running {
		en.runSince, en.peersAt, en.stalled = time.Time{}, time.Time{}, ""
		return
	}
	if en.runSince.IsZero() {
		en.runSince = now
	}
	peers := t.Stats().ActivePeers
	if peers > 0 {
		en.peersAt = now
		en.everPeer = true
	}
	en.stalled = stallHint(now, en.runSince, en.peersAt, e.cfg.StalledAfter, peers, t.Info() != nil)
}

// Connectivity is the engine's view of whether peers on the internet can
// reach it. It is worked out from what the engine sees itself (incoming
// peer handshakes); no outside service is asked.
type Connectivity struct {
	// ListenPort is the BitTorrent port (TCP and UDP) peers connect to.
	ListenPort int `json:"listenPort"`
	// PortForwarding: the engine asks the router for the port by UPnP
	// (off with VAULT_TORRENT_UPNP=off and in tests).
	PortForwarding bool `json:"portForwarding"`
	// IncomingConns: peer handshakes received since start.
	IncomingConns int64 `json:"incomingConns"`
	// IncomingBlocked: torrents have been running for IncomingCheckAfter
	// and not one peer has connected in: the port is most likely closed
	// (no router forward, Docker port not published).
	IncomingBlocked bool `json:"incomingBlocked"`
}

// installIncoming counts incoming peer handshakes (any torrent, even one
// the engine does not have: the peer reached us either way).
func (e *Engine) installIncoming(cb *atorrent.Callbacks) {
	prev := cb.CompletedHandshake
	cb.CompletedHandshake = func(pc *atorrent.PeerConn, ih atorrent.InfoHash) {
		if prev != nil {
			prev(pc, ih)
		}
		if pc.Discovery == atorrent.PeerSourceIncoming {
			e.incoming.Add(1)
		}
	}
}

// checkActive (monitor, every tick, e.mu held) follows since when torrents
// have been running without a break: what IncomingBlocked waits for.
func (e *Engine) checkActive(now time.Time) {
	for _, en := range e.entries {
		if en.t != nil && !en.moving && en.hold == "" && en.rec.Error == "" && en.dupOf == "" {
			if e.activeSince.IsZero() {
				e.activeSince = now
			}
			return
		}
	}
	e.activeSince = time.Time{}
}

// incomingBlocked: running for at least after with no incoming handshake.
func incomingBlocked(now, activeSince time.Time, after time.Duration, incoming int64) bool {
	return incoming == 0 && !activeSince.IsZero() && now.Sub(activeSince) >= after
}

// Connectivity reports whether peers can reach the engine (admin status).
func (e *Engine) Connectivity() Connectivity {
	if e.cfg.Off || e.client == nil {
		return Connectivity{}
	}
	c := Connectivity{ListenPort: e.client.LocalPort(), PortForwarding: !e.cfg.LocalOnly && !e.cfg.NoUPnP, IncomingConns: e.incoming.Load()}
	if c.ListenPort == 0 {
		c.ListenPort = e.cfg.ListenPort
	}
	e.mu.Lock()
	since := e.activeSince
	e.mu.Unlock()
	c.IncomingBlocked = incomingBlocked(time.Now(), since, e.cfg.IncomingCheckAfter, c.IncomingConns)
	return c
}
