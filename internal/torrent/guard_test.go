package torrent

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/antresol-app/antresol-addon-torrents/internal/netguard"
)

// A torrent anyone can publish names URLs the client fetches by itself:
// webseeds (ws=, url-list), metainfo sources (xs=, as=) and trackers. Outside
// LocalOnly guardNetwork keeps all of them off loopback, LAN and metadata
// addresses; the control run shows the same torrent does reach a server on
// loopback without it.
func TestGuardNetworkKeepsMetadataURLsOffLocalAddresses(t *testing.T) {
	fx := newFixtureSize(t, t.TempDir(), 1<<20)
	mi, err := metainfo.Load(bytes.NewReader(fx.metafile))
	if err != nil {
		t.Fatal(err)
	}

	// run returns the paths the "internal service" on loopback was asked for
	run := func(t *testing.T, guarded bool) []string {
		var mu sync.Mutex
		var got []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			got = append(got, r.URL.Path)
			mu.Unlock()
			http.NotFound(w, r)
		}))
		defer srv.Close()

		cc := atorrent.NewDefaultClientConfig()
		cc.DefaultStorage = storage.NewFile(t.TempDir())
		cc.ListenPort = 0
		cc.ListenHost = func(string) string { return "127.0.0.1" }
		cc.NoDHT, cc.DisablePEX, cc.NoDefaultPortForwarding, cc.DisableIPv6, cc.DisableUTP = true, true, true, true, true
		cc.DisableWebtorrent = true
		if guarded {
			guardNetwork(cc)
		}
		cl, err := newClient(cc)
		if err != nil {
			t.Fatal(err)
		}
		defer cl.Close()

		// a webseed (it is asked as soon as a piece is wanted) and a tracker
		spec, err := atorrent.TorrentSpecFromMetaInfoErr(mi)
		if err != nil {
			t.Fatal(err)
		}
		spec.Webseeds = []string{srv.URL + "/ws/"}
		spec.Trackers = [][]string{{srv.URL + "/announce"}}
		tw, _, err := cl.AddTorrentSpec(spec)
		if err != nil {
			t.Fatal(err)
		}
		tw.DownloadAll()

		// a magnet source (fetched at once, retried every minute or two)
		src := &atorrent.TorrentSpec{InfoHash: metainfo.HashBytes([]byte("another torrent")), Sources: []string{srv.URL + "/source.torrent"}}
		if _, _, err := cl.AddTorrentSpec(src); err != nil {
			t.Fatal(err)
		}

		time.Sleep(6 * time.Second)
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}

	// the webseed is first asked for after about 5 s: both runs wait for it
	t.Run("control", func(t *testing.T) {
		t.Parallel()
		got := run(t, false)
		t.Logf("the unguarded client asked the local server for %q", got)
		for _, want := range []string{"/announce", "/source.torrent", "/ws/" + fx.name} {
			if !slices.Contains(got, want) {
				t.Fatalf("the unguarded client never asked for %s: the test proves nothing", want)
			}
		}
	})
	t.Run("guarded", func(t *testing.T) {
		t.Parallel()
		if got := run(t, true); len(got) != 0 {
			t.Fatalf("the local server was asked for %q by torrent metadata", got)
		}
	})
}

// New builds its client from clientConfig: outside LocalOnly every network
// path torrent metadata can steer (webseeds and magnet sources, HTTP and
// websocket trackers, UDP trackers) is guarded there; LocalOnly keeps the
// loopback of the tests. Deleting the guardNetwork call fails this.
func TestClientConfigGuardsOutsideLocalOnly(t *testing.T) {
	build := func(cfg EngineConfig) *atorrent.ClientConfig {
		e := &Engine{cfg: cfg, reqs: newReqTracker(), self: &selfAddrs{}, peered: &peerSeen{}}
		return e.clientConfig()
	}
	ctx := context.Background()
	blocked := func(what string, dial func(context.Context, string, string) (net.Conn, error)) {
		t.Helper()
		if dial == nil {
			t.Errorf("%s: no dialer", what)
			return
		}
		if c, err := dial(ctx, "tcp", "127.0.0.1:1"); !errors.Is(err, netguard.ErrBlocked) {
			if c != nil {
				c.Close()
			}
			t.Errorf("%s: dialling loopback gave %v, want ErrBlocked", what, err)
		}
	}

	cc := build(EngineConfig{})
	blocked("HTTPDialContext", cc.HTTPDialContext)
	blocked("TrackerDialContext", cc.TrackerDialContext)
	web, ok := cc.WebTransport.(*http.Transport)
	if !ok {
		t.Fatalf("WebTransport = %T", cc.WebTransport)
	}
	blocked("WebTransport", web.DialContext)
	if !web.ForceAttemptHTTP2 || web.Proxy != nil || cc.HTTPProxy != nil {
		t.Errorf("web transport: h2 %v, proxy set %v / %v", web.ForceAttemptHTTP2, web.Proxy != nil, cc.HTTPProxy != nil)
	}
	if cc.TrackerListenPacket == nil {
		t.Fatal("UDP trackers are not guarded")
	}
	pc, err := cc.TrackerListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.WriteTo([]byte("x"), &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9}); !errors.Is(err, netguard.ErrBlocked) {
		t.Errorf("UDP write to loopback: %v, want ErrBlocked", err)
	}
	if cc.NoUpload {
		t.Error("NoUpload without the field")
	}
	if !build(EngineConfig{NoUpload: true}).NoUpload {
		t.Error("EngineConfig.NoUpload is not passed to the client")
	}

	local := build(EngineConfig{LocalOnly: true})
	if local.HTTPDialContext != nil || local.TrackerDialContext != nil || local.WebTransport != nil || local.TrackerListenPacket != nil || !local.DisableWebseeds {
		t.Error("LocalOnly must keep the tests' loopback and no webseeds")
	}
}

// LocalOnly fetches nothing from the network, a magnet's xs= included.
func TestLocalOnlyDropsMagnetSources(t *testing.T) {
	e := openEngine(t, t.TempDir(), engOpts{})
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&xs=" + url.QueryEscape(srv.URL+"/x.torrent")
	if _, err := e.AddMagnet(context.Background(), magnet, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if n := hits.Load(); n != 0 {
		t.Fatalf("the magnet's source was fetched %d times in LocalOnly", n)
	}
}
