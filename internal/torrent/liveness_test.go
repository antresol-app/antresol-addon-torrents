package torrent

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
)

const testIH = "0123456789abcdef0123456789abcdef01234567"

func testProber() *prober {
	var id [20]byte
	copy(id[:], "-AN0001-abcdefghijkl")
	return &prober{hc: &http.Client{}, peerID: id, port: func() int { return 42069 }, self: &selfAddrs{}, timeout: 3 * time.Second, perTracker: 2 * time.Second}
}

// httpTracker answers every announce with body and status, recording the
// query it got.
func httpTracker(t *testing.T, status int, body any) (*httptest.Server, *[]string) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RawQuery)
		b, err := bencode.Marshal(body)
		if err != nil {
			t.Error(err)
		}
		w.WriteHeader(status)
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func compact(ip string, port int) string {
	b := append([]byte(net.ParseIP(ip).To4()), byte(port>>8), byte(port))
	return string(b)
}

func TestProbeHTTPTrackerPeers(t *testing.T) {
	p := testProber()
	p.self.add(net.ParseIP("203.0.113.7"))
	// us (our external IP and port), and one real peer
	srv, got := httpTracker(t, 200, map[string]any{"interval": 1800, "complete": 0, "incomplete": 1,
		"peers": compact("203.0.113.7", 42069) + compact("198.51.100.2", 51413)})
	res := p.probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{srv.URL + "/announce.php?passkey=SECRET"}})
	if !res.Alive() || res.Peers != 1 || !res.Answered {
		t.Fatalf("probe %+v, want one peer (us left out)", res)
	}
	if len(*got) != 1 || !strings.Contains((*got)[0], "passkey=SECRET") || !strings.Contains((*got)[0], "port=42069") || !strings.Contains((*got)[0], "compact=1") {
		t.Fatalf("query %v", *got)
	}
}

func TestProbeHTTPTrackerSelfOnly(t *testing.T) {
	p := testProber()
	// the tracker tells us our external IP (BEP 24); the only peer is us
	srv, _ := httpTracker(t, 200, map[string]any{"complete": 0, "external ip": string(net.ParseIP("203.0.113.9").To4()),
		"peers": []map[string]any{{"ip": "203.0.113.9", "port": 42069, "peer id": "x"}}})
	srv2, _ := httpTracker(t, 200, map[string]any{"complete": 0, "peers": []map[string]any{{"ip": "198.51.100.3", "port": 1000, "peer id": string(p.peerID[:])}}})
	res := p.probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{srv.URL + "/a", srv2.URL + "/a"}})
	if res.Alive() || !res.Answered {
		t.Fatalf("probe %+v, want nobody but us", res)
	}
}

func TestProbeHTTPTrackerRefusals(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   any
		kind   string
		text   string
	}{
		{"bad passkey, 403", 403, map[string]any{"failure reason": "Неверный (34 - пасскей)"}, TrackerBadPasskey, "Неверный (34 - пасскей)"},
		{"not registered", 200, map[string]any{"failure reason": "Torrent not registered with this tracker"}, TrackerNotRegistered, "Torrent not registered with this tracker"},
		{"other reason with a key", 200, map[string]any{"failure reason": "go away http://t.example/announce?uk=abc123&x=1"}, TrackerRejected, "go away http://t.example/announce?uk=…&x=1"},
		{"bare 403", 403, "nope", TrackerRejected, "HTTP 403"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := httpTracker(t, c.status, c.body)
			res := testProber().probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{srv.URL + "/ann"}})
			if res.Alive() || res.TrackerErrorKind != c.kind || res.TrackerError != c.text {
				t.Fatalf("probe %+v, want %s %q", res, c.kind, c.text)
			}
		})
	}
}

func TestProbeRefusalKindsRank(t *testing.T) {
	a, _ := httpTracker(t, 200, map[string]any{"failure reason": "whatever"})
	b, _ := httpTracker(t, 403, map[string]any{"failure reason": "Invalid passkey"})
	res := testProber().probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{a.URL + "/x", b.URL + "/x"}})
	if res.TrackerErrorKind != TrackerBadPasskey {
		t.Fatalf("probe %+v, want the passkey refusal to win", res)
	}
}

func TestProbeUnreachableTrackerIsNoAnswer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	res := testProber().probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{u + "/announce", "wss://x/y", "::bad"}})
	if res.Answered || res.Alive() || res.TrackerError != "" {
		t.Fatalf("probe %+v", res)
	}
}

// udpTracker is a minimal BEP 15 tracker: connect, then announce answered
// with seeders and peers, or with an error message.
func udpTracker(t *testing.T, seeders int, peers []string, errMsg string) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		b := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			if n < 16 {
				continue
			}
			action := binary.BigEndian.Uint32(b[8:12])
			tx := b[12:16]
			var out []byte
			switch action {
			case 0: // connect
				out = binary.BigEndian.AppendUint32(nil, 0)
				out = append(out, tx...)
				out = binary.BigEndian.AppendUint64(out, 0x1122334455667788)
			case 1: // announce
				if errMsg != "" {
					out = binary.BigEndian.AppendUint32(nil, 3)
					out = append(out, tx...)
					out = append(out, errMsg...)
					break
				}
				out = binary.BigEndian.AppendUint32(nil, 1)
				out = append(out, tx...)
				out = binary.BigEndian.AppendUint32(out, 1800)
				out = binary.BigEndian.AppendUint32(out, 0)
				out = binary.BigEndian.AppendUint32(out, uint32(seeders))
				for _, p := range peers {
					out = append(out, p...)
				}
			default:
				continue
			}
			pc.WriteTo(out, addr)
		}
	}()
	return "udp://" + pc.LocalAddr().String() + "/announce"
}

func TestProbeUDPTracker(t *testing.T) {
	alive := udpTracker(t, 2, []string{compact("198.51.100.4", 6881)}, "")
	res := testProber().probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{alive}})
	if !res.Alive() || res.Seeders != 2 || res.Peers != 1 {
		t.Fatalf("probe %+v", res)
	}
	dead := udpTracker(t, 0, nil, "")
	if res := testProber().probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{dead}}); res.Alive() || !res.Answered {
		t.Fatalf("probe %+v, want an empty swarm", res)
	}
	refused := udpTracker(t, 0, nil, "Torrent not registered\x00")
	res = testProber().probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{refused}})
	if res.TrackerErrorKind != TrackerNotRegistered || res.TrackerError != "Torrent not registered" {
		t.Fatalf("probe %+v", res)
	}
}

func TestProbeStopsWhenAlive(t *testing.T) {
	// one tracker answers at once, another never: the probe ends early
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer hang.Close()
	ok, _ := httpTracker(t, 200, map[string]any{"complete": 3})
	p := testProber()
	p.timeout, p.perTracker = 10*time.Second, 10*time.Second
	start := time.Now()
	res := p.probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{hang.URL + "/a", ok.URL + "/a"}})
	if !res.Alive() || time.Since(start) > 3*time.Second {
		t.Fatalf("probe %+v took %v", res, time.Since(start))
	}
}

func TestRedactTracker(t *testing.T) {
	for in, want := range map[string]string{
		"http://bt.t-ru.org/ann?pk=0123abcd&info_hash=x":        "http://bt.t-ru.org/ann?pk=…&info_hash=x",
		"announce.php?passkey=deadbeef":                         "announce.php?passkey=…",
		"https://tr.example/AK=1&ak=zz uk=yy":                   "https://tr.example/AK=…&ak=… uk=…",
		"http://tr.example/0123456789abcdef0123456789abcdef/an": "http://tr.example/…/an",
		"Неверный (34 - пасскей)":                               "Неверный (34 - пасскей)",
		"passkey: 0f0f0f0f0f":                                   "passkey: …",
		"bad key=abcdef token=xyz secret: s3cr t=77":            "bad key=… token=… secret: … t=…",
		"your key 9f86d081884c7d659a2feaa0c55ad015 is wrong":    "your key … is wrong",
		"Torrent not registered with this tracker":              "Torrent not registered with this tracker",
	} {
		if got := RedactTracker(in); got != want {
			t.Errorf("RedactTracker(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTargetOf(t *testing.T) {
	tg, err := TargetOf("magnet:?xt=urn:btih:"+testIH+"&tr=udp%3A%2F%2Fa%3A1%2Fann&tr=http%3A%2F%2Fb%2Fann&tr=udp%3A%2F%2Fa%3A1%2Fann", nil)
	if err != nil || tg.InfoHash != testIH || len(tg.Trackers) != 2 {
		t.Fatalf("target %+v %v", tg, err)
	}
}

// CheckLiveness never blocks: the status says «checking» until the probe
// ends, then what it found; a peer makes it alive whatever the probe said.
func TestCheckLiveness(t *testing.T) {
	e, h := stalledEngine(t)
	release := make(chan SwarmProbe)
	e.mu.Lock()
	e.probe = func(ctx context.Context, tg ProbeTarget) SwarmProbe {
		select {
		case r := <-release:
			return r
		case <-ctx.Done():
			return SwarmProbe{}
		}
	}
	e.mu.Unlock()
	e.CheckLiveness(h.InfoHash())
	if st := h.Status(); st.Liveness != LivenessChecking {
		t.Fatalf("liveness %q, want checking", st.Liveness)
	}
	release <- SwarmProbe{Answered: true, TrackerError: "Неверный (34 - пасскей)", TrackerErrorKind: TrackerBadPasskey}
	eventually(t, func() bool { return h.Status().Liveness == LivenessUnseen })
	if st := h.Status(); st.TrackerErrorKind != TrackerBadPasskey || st.TrackerError == "" {
		t.Fatalf("status %+v, want the tracker refusal", st)
	}
	e.mu.Lock()
	e.entries[h.InfoHash()].everPeer = true
	e.mu.Unlock()
	if st := h.Status(); st.Liveness != LivenessAlive || !st.HadPeers {
		t.Fatalf("liveness %q after a peer, want alive", st.Liveness)
	}
}

// A torrent stalled without peers is probed again (tracker refusals stay
// current) even when it was never checked at pick, without a liveness.
func TestReprobeStalled(t *testing.T) {
	e, h := stalledEngine(t, true)
	calls := make(chan struct{}, 10)
	e.mu.Lock()
	e.probe = func(context.Context, ProbeTarget) SwarmProbe {
		calls <- struct{}{}
		return SwarmProbe{TrackerError: "Torrent not registered", TrackerErrorKind: TrackerNotRegistered}
	}
	e.mu.Unlock()
	select {
	case <-calls:
	case <-time.After(10 * time.Second):
		t.Fatal("no reprobe of a stalled torrent")
	}
	eventually(t, func() bool { return h.Status().TrackerErrorKind == TrackerNotRegistered })
	if st := h.Status(); st.Liveness != "" {
		t.Fatalf("liveness %q for a torrent never checked", st.Liveness)
	}
	select {
	case <-calls:
		t.Fatal("probed again before reprobeEvery")
	case <-time.After(500 * time.Millisecond):
	}
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// stalledEngine has one download nobody seeds; stalled after 200 ms with
// short, else after the default StalledAfter.
func stalledEngine(t *testing.T, short ...bool) (*Engine, Handle) {
	t.Helper()
	fx := newFixture(t, t.TempDir())
	o := engOpts{}
	if len(short) > 0 && short[0] {
		o.stalledAfter = 200 * time.Millisecond
	}
	e := openEngine(t, t.TempDir(), o)
	e.mu.Lock()
	e.reprobeGrace, e.reprobeJitter = 0, 0 // tests probe at once
	e.mu.Unlock()
	h, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return e, h
}
