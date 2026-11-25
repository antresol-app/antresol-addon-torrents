package torrent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	pp "github.com/anacrolix/torrent/peer_protocol"
	"github.com/anacrolix/torrent/tracker/udp"
	"github.com/antresol-app/antresol-addon-torrents/internal/netguard"
)

// Liveness of a torrent's swarm (Status.Liveness): what a short probe of
// its trackers and the DHT found right after it was picked. Advisory only:
// a NATed seeder may still connect in later, so nothing is cancelled on a
// probe alone. A torrent that gets a peer or a byte is alive regardless.
const (
	LivenessChecking = "checking"
	LivenessAlive    = "alive"
	// LivenessUnseen: no tracker and no DHT node knew a peer of it (other
	// than this server) — «Сидов пока не видно».
	LivenessUnseen = "unseen"
)

// Kinds of Status.TrackerErrorKind: what a tracker's refusal means.
const (
	// TrackerBadPasskey: the tracker refused our (or the release's) key.
	TrackerBadPasskey = "bad_passkey"
	// TrackerNotRegistered: the tracker does not know the torrent.
	TrackerNotRegistered = "not_registered"
	// TrackerRejected: any other refusal (failure reason or HTTP error).
	TrackerRejected = "rejected"
)

const (
	// probeTimeout bounds a whole probe (trackers and DHT together).
	probeTimeout = 15 * time.Second
	// trackerTimeout bounds one tracker's announce.
	trackerTimeout = 10 * time.Second
	// reprobeEvery: a torrent stalled without peers is probed again this
	// often (at most), so Status.TrackerError follows what the trackers
	// answer now; CheckLiveness probes a torrent probed this recently not
	// again. A tracker is asked again only once the interval of its last
	// answer has passed (prober.due), whatever the probe; the DHT each time.
	reprobeEvery = 30 * time.Minute
	// probeCacheFor: a probe of an infohash this recent is the answer of
	// the next one (the switch's probe of a release is the new download's
	// liveness; a second pick asks nobody).
	probeCacheFor = 25 * time.Minute
	// defaultTrackerInterval: a tracker that refused, or named no
	// interval, is asked again after this (as a re-probe).
	defaultTrackerInterval = 30 * time.Minute
	// maxProbeTrackers: trackers asked at most per probe.
	maxProbeTrackers = 20
)

// SwarmProbe is what a liveness probe found.
type SwarmProbe struct {
	// Peers: distinct peer addresses the trackers and the DHT returned,
	// this server left out.
	Peers int
	// Seeders: the most seeders a tracker reported.
	Seeders int
	// Answered: at least one tracker or DHT node answered.
	Answered bool
	// TrackerError (redacted) and TrackerErrorKind: the most telling
	// refusal of a tracker ("" = none refused).
	TrackerError, TrackerErrorKind string
	// skipped: trackers not asked, their interval not over yet.
	skipped int
}

// Alive reports whether anyone but us is in the swarm.
func (p SwarmProbe) Alive() bool { return p.Peers > 0 || p.Seeders > 0 }

// ProbeTarget is a swarm to probe: an infohash (hex) and its trackers.
type ProbeTarget struct {
	InfoHash string
	Trackers []string
	// Private: the metafile says private (BEP 27): the DHT is not asked.
	Private bool
}

// TargetOf reads a probe target from a magnet or a metafile.
func TargetOf(magnet string, metafile []byte) (ProbeTarget, error) {
	var spec *atorrent.TorrentSpec
	var err error
	if magnet != "" {
		spec, err = atorrent.TorrentSpecFromMagnetUri(magnet)
	} else {
		var mi *metainfo.MetaInfo
		mi, err = metainfo.Load(bytes.NewReader(metafile))
		if err == nil {
			spec, err = atorrent.TorrentSpecFromMetaInfoErr(mi)
		}
	}
	if err != nil {
		return ProbeTarget{}, err
	}
	if spec.InfoHash.IsZero() {
		return ProbeTarget{}, errors.New("torrent: v2-only torrents are not supported")
	}
	return ProbeTarget{InfoHash: spec.InfoHash.HexString(), Trackers: flatTrackers(spec.Trackers), Private: privateInfo(spec.InfoBytes)}, nil
}

// privateInfo reports whether info bytes say private (unknown: false).
func privateInfo(infoBytes []byte) bool {
	if infoBytes == nil {
		return false
	}
	inf, err := (&metainfo.MetaInfo{InfoBytes: infoBytes}).UnmarshalInfo()
	return err == nil && inf.Private != nil && *inf.Private
}

// flatTrackers flattens tiers, deduplicated, in order.
func flatTrackers(tiers [][]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, tier := range tiers {
		for _, u := range tier {
			u = strings.TrimSpace(u)
			if u != "" && !seen[u] {
				seen[u] = true
				out = append(out, u)
			}
		}
	}
	return out
}

// selfAddrs are the IPs peers see this server at: the configured public
// IPs, the local interfaces, the «yourip» peers tell us in the extended
// handshake and the «external ip» HTTP trackers report.
type selfAddrs struct {
	mu  sync.Mutex
	ips map[string]bool
}

func (s *selfAddrs) add(ip net.IP) {
	if ip == nil || ip.IsUnspecified() {
		return
	}
	s.mu.Lock()
	if s.ips == nil {
		s.ips = map[string]bool{}
	}
	if len(s.ips) < 256 {
		s.ips[ip.String()] = true
	}
	s.mu.Unlock()
}

func (s *selfAddrs) has(ip net.IP) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ips[ip.String()]
}

// prober runs liveness probes. Its fields are seams for the tests.
type prober struct {
	// hc asks HTTP trackers, listen opens the socket UDP trackers are
	// asked from (nil: net.ListenPacket). The tracker URLs come from
	// torrents anyone can publish: outside tests both refuse local
	// addresses (netguard), as the client's own announces do.
	hc     *http.Client
	listen func(network, addr string) (net.PacketConn, error)
	peerID [20]byte
	port   func() int
	self   *selfAddrs
	dht    func() []atorrent.DhtServer
	// timeout and perTracker default to probeTimeout and trackerTimeout.
	timeout, perTracker time.Duration
	// next: when each tracker may be asked again about an infohash (key
	// infohash+" "+URL), from the interval of its last answer.
	mu   sync.Mutex
	next map[string]time.Time
	// cache: the last probe of each infohash (probeCacheFor).
	cache map[string]cachedProbe
	now   func() time.Time // nil = time.Now
}

type cachedProbe struct {
	at  time.Time
	res SwarmProbe
}

func (p *prober) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// due reports whether a re-probe may ask tracker u about ih now.
func (p *prober) due(ih, u string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.clock().Before(p.next[ih+" "+u])
}

// answered records a tracker's answer: it is not asked again (by a
// re-probe) before interval, or defaultTrackerInterval when it named none.
func (p *prober) answered(ih, u string, interval time.Duration) {
	if interval <= 0 {
		interval = defaultTrackerInterval
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	if p.next == nil {
		p.next = map[string]time.Time{}
	}
	if len(p.next) > 4096 {
		for k, t := range p.next {
			if now.After(t) {
				delete(p.next, k)
			}
		}
	}
	p.next[ih+" "+u] = now.Add(interval)
}

// peerAddr is one peer a source returned.
type peerAddr struct {
	ip     net.IP
	port   int
	peerID string // HTTP non-compact lists only
}

// probeMsg is one source's answer (several for the DHT: one per batch).
type probeMsg struct {
	peers    []peerAddr
	seeders  int
	answered bool
	failure  string // a tracker's refusal, raw
	done     bool   // the source is finished
	// interval: how long the tracker asked us to wait before the next
	// announce (the larger of interval and min interval; 0 = not said).
	interval time.Duration
}

func (p *prober) isSelf(a peerAddr) bool {
	if a.peerID != "" && a.peerID == string(p.peerID[:]) {
		return true
	}
	return a.port == p.port() && p.self.has(a.ip)
}

// probe answers from the cache when the infohash was probed within
// probeCacheFor, else probes (probeNow) and caches what it found.
func (p *prober) probe(ctx context.Context, tg ProbeTarget) SwarmProbe {
	p.mu.Lock()
	c, ok := p.cache[tg.InfoHash]
	p.mu.Unlock()
	if ok && p.clock().Sub(c.at) < probeCacheFor {
		return c.res
	}
	res := p.probeNow(ctx, tg)
	if ctx.Err() == nil && res.skipped == 0 { // a partial probe is no answer to keep
		p.mu.Lock()
		if p.cache == nil || len(p.cache) > 4096 {
			p.cache = map[string]cachedProbe{}
		}
		p.cache[tg.InfoHash] = cachedProbe{at: p.clock(), res: res}
		p.mu.Unlock()
	}
	return res
}

// probeNow asks the target's trackers (HTTP and UDP; each only once the
// interval of its last answer has passed) and, unless the torrent is
// private, the DHT for peers. It returns as soon as anyone else is found,
// else when every source is done or the timeout ends.
func (p *prober) probeNow(ctx context.Context, tg ProbeTarget) SwarmProbe {
	var ih [20]byte
	if b, err := hex.DecodeString(tg.InfoHash); err != nil || len(b) != 20 {
		return SwarmProbe{}
	} else {
		copy(ih[:], b)
	}
	timeout := p.timeout
	if timeout <= 0 {
		timeout = probeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	msgs := make(chan probeMsg, 64)
	send := func(m probeMsg) {
		select {
		case msgs <- m:
		case <-ctx.Done():
		}
	}
	sources := 0
	trackers := tg.Trackers
	if len(trackers) > maxProbeTrackers {
		trackers = trackers[:maxProbeTrackers]
	}
	skipped := 0
	for _, u := range trackers {
		if !p.due(tg.InfoHash, u) {
			skipped++
			continue
		}
		sources++
		go func() {
			m := p.announce(ctx, u, ih)
			if m.answered {
				p.answered(tg.InfoHash, u, m.interval)
			}
			m.done = true
			send(m)
		}()
	}
	if p.dht != nil && !tg.Private {
		for _, s := range p.dht() {
			sources++
			go p.dhtPeers(ctx, s, ih, send)
		}
	}
	out := SwarmProbe{skipped: skipped}
	seen := map[string]bool{}
	rank := -1
	for sources > 0 {
		var m probeMsg
		select {
		case m = <-msgs:
		case <-ctx.Done():
			return out
		}
		if m.done {
			sources--
		}
		out.Answered = out.Answered || m.answered
		out.Seeders = max(out.Seeders, m.seeders)
		for _, a := range m.peers {
			if a.ip == nil || a.port <= 0 || p.isSelf(a) {
				continue
			}
			k := net.JoinHostPort(a.ip.String(), strconv.Itoa(a.port))
			if !seen[k] {
				seen[k] = true
				out.Peers++
			}
		}
		if m.failure != "" {
			kind := TrackerErrorKind(m.failure)
			if r := kindRank[kind]; r > rank {
				rank = r
				out.TrackerError, out.TrackerErrorKind = RedactTracker(m.failure, tg.Trackers...), kind
			}
		}
		if out.Alive() {
			return out
		}
	}
	return out
}

var kindRank = map[string]int{TrackerRejected: 0, TrackerNotRegistered: 1, TrackerBadPasskey: 2}

// dhtPeers runs a get_peers traversal (no announce: port 0) on one of the
// client's own DHT servers and sends each batch of peers.
func (p *prober) dhtPeers(ctx context.Context, s atorrent.DhtServer, ih [20]byte, send func(probeMsg)) {
	defer send(probeMsg{done: true})
	a, err := s.Announce(ih, 0, false)
	if err != nil {
		return
	}
	defer a.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case pv, ok := <-a.Peers():
			if !ok {
				return
			}
			m := probeMsg{answered: true}
			for _, np := range pv.Peers {
				m.peers = append(m.peers, peerAddr{ip: np.IP, port: np.Port})
			}
			send(m)
		}
	}
}

// announce asks one tracker. Network failures are no answer; a refusal is
// returned as failure.
func (p *prober) announce(ctx context.Context, raw string, ih [20]byte) probeMsg {
	per := p.perTracker
	if per <= 0 {
		per = trackerTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, per)
	defer cancel()
	u, err := url.Parse(raw)
	if err != nil {
		return probeMsg{}
	}
	switch u.Scheme {
	case "http", "https":
		return p.httpAnnounce(ctx, raw, ih)
	case "udp", "udp4", "udp6":
		return p.udpAnnounce(ctx, u, ih)
	}
	return probeMsg{}
}

// httpAnnounce is a regular announce (no event) with our peer id and port,
// as the client's own announces are, read by hand so the tracker's answer
// (its failure reason, or an HTTP error) is seen as is. The URL (it may
// hold a passkey) is never logged.
func (p *prober) httpAnnounce(ctx context.Context, raw string, ih [20]byte) probeMsg {
	q := url.Values{}
	q.Set("info_hash", string(ih[:]))
	q.Set("peer_id", string(p.peerID[:]))
	q.Set("port", strconv.Itoa(p.port()))
	q.Set("uploaded", "0")
	q.Set("downloaded", "0")
	q.Set("left", strconv.FormatInt(1<<40, 10))
	q.Set("compact", "1")
	q.Set("numwant", "50")
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw+sep+q.Encode(), nil)
	if err != nil {
		return probeMsg{}
	}
	req.Header.Set("User-Agent", "Antresol")
	hc := p.hc
	if hc == nil {
		hc = guardedClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return probeMsg{}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return probeMsg{}
	}
	var r struct {
		Failure    string        `bencode:"failure reason"`
		Complete   int           `bencode:"complete"`
		Interval   int           `bencode:"interval"`
		MinIntvl   int           `bencode:"min interval"`
		Incomplete int           `bencode:"incomplete"`
		Peers      bencode.Bytes `bencode:"peers"`
		Peers6     string        `bencode:"peers6"`
		ExternalIP string        `bencode:"external ip"`
	}
	derr := bencode.Unmarshal(body, &r)
	var trailing bencode.ErrUnusedTrailingBytes
	if errors.As(derr, &trailing) {
		derr = nil
	}
	interval := time.Duration(max(r.Interval, r.MinIntvl)) * time.Second
	if r.Failure != "" {
		return probeMsg{answered: true, failure: r.Failure, interval: interval}
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return probeMsg{answered: true, failure: fmt.Sprintf("HTTP %d", resp.StatusCode)}
		}
		return probeMsg{}
	}
	if derr != nil {
		return probeMsg{}
	}
	if ip := net.IP([]byte(r.ExternalIP)); len(r.ExternalIP) == 4 || len(r.ExternalIP) == 16 {
		p.self.add(ip)
	}
	m := probeMsg{answered: true, seeders: r.Complete, peers: httpPeers(r.Peers), interval: interval}
	m.peers = append(m.peers, compactPeers([]byte(r.Peers6), 16)...)
	return m
}

// httpPeers decodes «peers»: compact (a string) or a list of dicts.
func httpPeers(b bencode.Bytes) []peerAddr {
	if len(b) == 0 {
		return nil
	}
	if b[0] == 'l' {
		var list []struct {
			IP     string `bencode:"ip"`
			Port   int    `bencode:"port"`
			PeerID string `bencode:"peer id"`
		}
		if bencode.Unmarshal(b, &list) != nil {
			return nil
		}
		var out []peerAddr
		for _, x := range list {
			if ip := net.ParseIP(x.IP); ip != nil {
				out = append(out, peerAddr{ip: ip, port: x.Port, peerID: x.PeerID})
			}
		}
		return out
	}
	var s string
	if bencode.Unmarshal(b, &s) != nil {
		return nil
	}
	return compactPeers([]byte(s), 4)
}

// compactPeers decodes compact peers of ipLen-byte addresses.
func compactPeers(b []byte, ipLen int) []peerAddr {
	n := ipLen + 2
	var out []peerAddr
	for i := 0; i+n <= len(b); i += n {
		ip := make(net.IP, ipLen)
		copy(ip, b[i:i+ipLen])
		out = append(out, peerAddr{ip: ip, port: int(binary.BigEndian.Uint16(b[i+ipLen:]))})
	}
	return out
}

var discardLog = slog.New(slog.DiscardHandler)

// guardedClient: what a prober without a client of its own asks with.
var guardedClient = netguard.HTTPClient(trackerTimeout)

// udpAnnounce is a BEP 15 announce with our peer id and port.
func (p *prober) udpAnnounce(ctx context.Context, u *url.URL, ih [20]byte) probeMsg {
	network := "udp"
	if u.Scheme != "udp" {
		network = u.Scheme
	}
	cc, err := udp.NewConnClient(udp.NewConnClientOpts{Network: network, Host: u.Host, Logger: discardLog, ListenPacket: p.listen})
	if err != nil {
		return probeMsg{}
	}
	defer cc.Close()
	req := udp.AnnounceRequest{InfoHash: ih, PeerId: p.peerID, Left: 1 << 40, NumWant: 50, Port: uint16(p.port()), Key: rand.Int32()}
	h, nas, err := cc.Announce(ctx, req, udp.Options{RequestUri: u.RequestURI()})
	if err != nil {
		var er udp.ErrorResponse
		if errors.As(err, &er) {
			msg := strings.TrimRight(er.Message, "\x00")
			if msg != "" && !strings.Contains(strings.ToLower(msg), "connection id") {
				return probeMsg{answered: true, failure: msg}
			}
		}
		return probeMsg{}
	}
	m := probeMsg{answered: true, seeders: int(h.Seeders), interval: time.Duration(h.Interval) * time.Second}
	if nas != nil {
		for _, a := range nas.NodeAddrs() {
			m.peers = append(m.peers, peerAddr{ip: a.IP, port: a.Port})
		}
	}
	return m
}

// TrackerErrorKind classifies a tracker's refusal.
func TrackerErrorKind(msg string) string {
	l := strings.ToLower(msg)
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(l, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has("passkey", "пасскей", "пасккей", "пас-кей", "authkey", "auth key", "неверный ключ", "invalid key", "unknown key"):
		return TrackerBadPasskey
	case has("not registered", "unregistered", "не зарегистрирован", "torrent not found", "unknown torrent",
		"info_hash not found", "infohash not found", "раздача не найдена", "торрент не найден", "not authorized torrent",
		"requested download is not authorized"):
		return TrackerNotRegistered
	}
	return TrackerRejected
}

// secretParam: tracker keys in URLs and texts: passkey=…, pk=…, uk=…,
// ak=…, key/secret/token/t, also as «passkey: …».
var secretParam = regexp.MustCompile(`(?i)\b(passkey|pk|uk|ak|authkey|torrent_pass|key|secret|token|t)(\s*[=:]\s*)([^&\s"'<>,;]+)`)

// secretRun: a run of 16 or more hex/base64-like characters with a digit
// in it (a bare key); words are left alone.
var secretRun = regexp.MustCompile(`[A-Za-z0-9+_=-]{16,}`)

// RedactTracker hides tracker keys in a text: the key-like values of the
// trackers' own URLs (query values, long path segments) verbatim, values
// of passkey, pk, uk, ak, key, secret, token and t, and any long run of
// key-like characters with a digit. The result is bounded to 200 runes.
func RedactTracker(s string, trackers ...string) string {
	for _, k := range trackerSecrets(trackers) {
		s = strings.ReplaceAll(s, k, "…")
	}
	s = secretParam.ReplaceAllString(s, "$1$2…")
	s = secretRun.ReplaceAllStringFunc(s, func(r string) string {
		if strings.ContainsAny(r, "0123456789") {
			return "…"
		}
		return r
	})
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

// trackerSecrets: the parts of tracker URLs that may be keys — query
// values of 6 characters or more and path segments of 16 or more —
// longest first.
func trackerSecrets(trackers []string) []string {
	var out []string
	for _, raw := range trackers {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		for _, vs := range u.Query() {
			for _, v := range vs {
				if len(v) >= 6 {
					out = append(out, v)
				}
			}
		}
		for _, seg := range strings.Split(u.Path, "/") {
			if len(seg) >= 16 {
				out = append(out, seg)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// peerSeen is the set of infohashes a peer completed a handshake for.
type peerSeen struct {
	mu  sync.Mutex
	set map[string]bool
	// hooks: Engine.OnPeer callbacks (run in their own goroutine).
	hooks []func(infohash string)
}

// add marks ih and returns the hooks to run when it is new.
func (p *peerSeen) add(ih string) []func(string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.set[ih] {
		return nil
	}
	if p.set == nil || len(p.set) > 100000 {
		p.set = map[string]bool{}
	}
	p.set[ih] = true
	return append([]func(string){}, p.hooks...)
}

func (p *peerSeen) has(ih string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.set[ih]
}

func (p *peerSeen) forget(ih string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.set, ih)
}

// installPeerSeen marks a torrent as having had a peer the moment a peer
// completes a handshake for it (not only on a monitor tick).
func (e *Engine) installPeerSeen(cb *atorrent.Callbacks) {
	prev := cb.CompletedHandshake
	cb.CompletedHandshake = func(pc *atorrent.PeerConn, ih atorrent.InfoHash) {
		if prev != nil {
			prev(pc, ih)
		}
		hex := ih.HexString()
		for _, f := range e.peered.add(hex) {
			go f(hex) // the client lock is held here
		}
	}
}

// OnPeer registers f, called (in its own goroutine) the first time a
// peer completes a handshake for a torrent: Status.HadPeers turns true.
func (e *Engine) OnPeer(f func(infohash string)) {
	e.peered.mu.Lock()
	e.peered.hooks = append(e.peered.hooks, f)
	e.peered.mu.Unlock()
}

// ErrNotIdle: RemoveIfIdle found the torrent has had a peer or a byte, or
// is not running.
var ErrNotIdle = errors.New("torrent: not idle")

// RemoveIfIdle removes a torrent, keeping its files, only if — checked
// under the engine lock — it is queued or downloading, has never had a
// peer and has no byte; else ErrNotIdle and it stays.
func (e *Engine) RemoveIfIdle(ctx context.Context, infohash string) error {
	return e.removeIf(ctx, infohash, "", false, func(en *entry) error {
		st := e.statusOf(en)
		if (st.Status != StatusQueued && st.Status != StatusDownloading) || st.HadPeers || st.Done > 0 || st.Downloaded > 0 {
			return ErrNotIdle
		}
		return nil
	}, nil)
}

// initProbe wires the prober to the client: its peer id, listen port and
// DHT servers (no DHT node of its own), and the configured public IPs and
// local interfaces as our own addresses.
func (e *Engine) initProbe(cl *atorrent.Client) {
	for _, ip := range cl.PublicIPs() {
		e.self.add(ip)
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				e.self.add(n.IP)
			}
		}
	}
	pr := &prober{
		hc:     guardedClient,
		listen: netguard.ListenPacket,
		peerID: cl.PeerID(),
		port:   cl.LocalPort,
		self:   e.self,
		dht:    cl.DhtServers,
	}
	if e.cfg.LocalOnly { // tests: their trackers are on loopback
		pr.hc, pr.listen = &http.Client{Timeout: trackerTimeout}, nil
	}
	e.probe = pr.probe
}

// installYourIP records the address peers say they see us at («yourip»
// of the extended handshake): the probe leaves that address out.
func (e *Engine) installYourIP(cb *atorrent.Callbacks) {
	prev := cb.ReadExtendedHandshake
	cb.ReadExtendedHandshake = func(pc *atorrent.PeerConn, m *pp.ExtendedHandshakeMessage) {
		if prev != nil {
			prev(pc, m)
		}
		if ip := net.IP(m.YourIp); len(ip) == net.IPv4len || len(ip) == net.IPv6len {
			e.self.add(ip)
		}
	}
}

// ProbeSwarm probes a swarm that need not be in the engine (a release
// about to be picked): its trackers and the DHT, at most probeTimeout.
func (e *Engine) ProbeSwarm(ctx context.Context, tg ProbeTarget) SwarmProbe {
	if e.cfg.Off {
		return SwarmProbe{}
	}
	return e.probe(ctx, tg)
}

// CheckLiveness probes a torrent's swarm in the background (Status.Liveness
// is LivenessChecking until it ends). It never blocks and never changes
// what the torrent does. A torrent probed within reprobeEvery is not
// probed again: the last probe's answer is its liveness.
func (e *Engine) CheckLiveness(infohash string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	en, err := e.entry(infohash)
	if err != nil || en.removed || e.cfg.Off || en.parked {
		return
	}
	if !en.probedAt.IsZero() && time.Since(en.probedAt) < reprobeEvery {
		if en.liveness == "" || en.liveness == LivenessChecking {
			en.liveness = livenessOf(en.lastAlive || en.everPeer)
		}
		return
	}
	if en.liveness == "" {
		en.liveness = LivenessChecking
	}
	e.startProbe(en)
}

// NoteProbe takes a probe made of the swarm before the torrent was added
// (the automatic switch's) as its liveness check: no second probe.
func (e *Engine) NoteProbe(infohash string, res SwarmProbe) {
	e.mu.Lock()
	defer e.mu.Unlock()
	en, err := e.entry(infohash)
	if err != nil || en.removed {
		return
	}
	e.noteProbe(en, res, time.Now())
	en.liveness = livenessOf(res.Alive() || en.everPeer)
}

func livenessOf(alive bool) string {
	if alive {
		return LivenessAlive
	}
	return LivenessUnseen
}

// noteProbe records a probe's answer. e.mu must be held.
func (e *Engine) noteProbe(en *entry, res SwarmProbe, now time.Time) {
	en.probedAt, en.lastAlive = now, res.Alive()
	if res.TrackerErrorKind != "" || res.skipped == 0 {
		// trackers not asked this time keep their last refusal
		en.trackerErr, en.trackerErrKind = res.TrackerError, res.TrackerErrorKind
	}
}

// maybeReprobe (monitor, e.mu held) probes a torrent stalled without peers
// again every reprobeEvery (not in the first reprobeGrace after start, and
// each torrent put off by its own share of reprobeJitter): its tracker
// refusal (Status.TrackerError) then says what the trackers answer now.
func (e *Engine) maybeReprobe(en *entry, now time.Time) {
	if en.stalled != StalledNoPeers || en.probing {
		return
	}
	due := en.probedAt.Add(reprobeEvery)
	if en.probedAt.IsZero() {
		due = e.started.Add(e.reprobeGrace)
	}
	if now.Before(due.Add(jitterOf(en.ih, e.reprobeJitter))) {
		return
	}
	e.startProbe(en)
}

// jitterOf is ih's own delay in [0, max).
func jitterOf(ih string, max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	h := fnv.New64a()
	h.Write([]byte(ih))
	return time.Duration(h.Sum64() % uint64(max))
}

// startProbe runs one probe of en. e.mu must be held.
func (e *Engine) startProbe(en *entry) {
	if en.probing {
		return
	}
	select {
	case <-e.closing:
		return
	default:
	}
	en.probing = true
	tg := ProbeTarget{InfoHash: en.ih, Trackers: append([]string(nil), en.trackers...)}
	if t := en.t; t != nil && t.Info() != nil && t.Info().Private != nil {
		tg.Private = *t.Info().Private
	} else if en.private {
		tg.Private = true
	}
	probe := e.probe
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-e.closing:
				cancel()
			case <-ctx.Done():
			}
		}()
		res := probe(ctx, tg)
		e.mu.Lock()
		defer e.mu.Unlock()
		en.probing = false
		e.noteProbe(en, res, time.Now())
		if en.liveness == "" {
			return // a reprobe of a torrent never checked: the hint alone
		}
		en.liveness = livenessOf(res.Alive() || en.everPeer)
	}()
}
