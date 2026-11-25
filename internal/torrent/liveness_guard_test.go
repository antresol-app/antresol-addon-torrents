package torrent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	atorrent "github.com/anacrolix/torrent"
)

// A swarm probe asks the trackers of a release nobody has picked yet: their
// URLs are a stranger's. The prober the engine builds (outside LocalOnly)
// never connects to a local address, over HTTP or UDP, and so gives a
// stranger no status-code oracle for internal services.
func TestProbeRefusesLocalTrackers(t *testing.T) {
	cc := atorrent.NewDefaultClientConfig()
	cc.DefaultStorage = nil
	cc.DataDir = t.TempDir()
	cc.ListenPort = 0
	cc.ListenHost = func(string) string { return "127.0.0.1" }
	cc.NoDHT, cc.DisablePEX, cc.NoDefaultPortForwarding, cc.DisableIPv6, cc.DisableUTP, cc.DisableTrackers = true, true, true, true, true, true
	cl, err := newClient(cc)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	e := &Engine{cfg: EngineConfig{}, self: &selfAddrs{}}
	e.initProbe(cl)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r) // what would show as «HTTP 404» to the user
	}))
	defer srv.Close()
	res := e.ProbeSwarm(context.Background(), ProbeTarget{
		InfoHash: testIH,
		Trackers: []string{srv.URL + "/announce", udpTracker(t, 5, []string{compact("198.51.100.4", 6881)}, "")},
	})
	if res.Answered || res.Alive() || res.TrackerError != "" || hits.Load() != 0 {
		t.Fatalf("probe %+v, local server got %d requests", res, hits.Load())
	}

	// the same trackers through the test prober (no guard) do answer
	p := testProber()
	if res = p.probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{srv.URL + "/announce"}}); !res.Answered || hits.Load() == 0 {
		t.Fatalf("control probe of HTTP %+v, hits %d: the test proves nothing", res, hits.Load())
	}
	if res = p.probe(context.Background(), ProbeTarget{InfoHash: "fedcba9876543210fedcba9876543210fedcba98", Trackers: []string{udpTracker(t, 5, nil, "")}}); res.Seeders != 5 {
		t.Fatalf("control probe of UDP %+v: the test proves nothing", res)
	}
}

// A prober left without a client of its own is guarded too.
func TestProbeWithoutClientIsGuarded(t *testing.T) {
	srv, got := httpTracker(t, 200, map[string]any{"complete": 3})
	p := testProber()
	p.hc = nil
	if res := p.probe(context.Background(), ProbeTarget{InfoHash: testIH, Trackers: []string{srv.URL + "/a"}}); res.Answered || len(*got) != 0 {
		t.Fatalf("probe %+v, tracker saw %v", res, *got)
	}
}
