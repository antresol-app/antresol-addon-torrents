package torrent

import (
	"testing"

	atorrent "github.com/anacrolix/torrent"
)

// A host without IPv6 (a kernel built without it, ipv6.disable=1, some
// containers) refuses tcp6/udp6 sockets with EAFNOSUPPORT. Outside
// LocalOnly anacrolix listens on both families, so the client must still
// start there, on IPv4 alone. (On a host with IPv6 this only checks that
// the client starts.)
func TestNewClientWithoutIPv6(t *testing.T) {
	cc := atorrent.NewDefaultClientConfig()
	cc.DataDir = t.TempDir()
	cc.ListenPort = 0
	cc.NoDHT = true
	cc.DisableTrackers = true
	cc.DisablePEX = true
	cc.NoDefaultPortForwarding = true
	cc.DisableUTP = true
	cc.DisableWebtorrent = true
	cc.DisableWebseeds = true
	cc.ListenHost = func(network string) string {
		if network == "tcp6" || network == "udp6" {
			return "::1"
		}
		return "127.0.0.1"
	}
	cl, err := newClient(cc)
	if err != nil {
		t.Fatalf("newClient: %v", err)
	}
	defer cl.Close()
	if cl.LocalPort() == 0 {
		t.Fatal("the client listens on no port")
	}
}
