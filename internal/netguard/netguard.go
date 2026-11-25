// Package netguard keeps the addon's network clients away from the local
// network when the address they were given comes from someone else (a
// torrent's webseed, a magnet's xs=, a tracker URL, a tracker's redirect, an
// indexer's result link): such a name may resolve to loopback, a LAN device
// or a cloud metadata address, and the server would fetch it blind.
//
// The check runs on the IP actually connected (the dialer's Control hook),
// after DNS and on every redirect and every re-dial, so a name that
// resolves into the home network is refused as well. What an admin
// configures on purpose (a LAN Jackett or Prowlarr, an OPDS catalog on the
// home network) does not go through here, or goes through with
// allowPrivate. The address rules are addonkit's (BlockedAddr,
// DialControl); this package is the plumbing of the engine's and the
// indexers' clients around them.
package netguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// ErrBlocked: the address is not one a third-party URL may reach.
var ErrBlocked = addonkit.ErrBlocked

var (
	// publicOnly is the dialers' Control for what a third party named; named
	// is the one for the hosts the admin named, which may be on the home
	// network (a name that is made to resolve to cloud metadata or another
	// never-reachable address is refused all the same).
	publicOnly = addonkit.DialControl(false)
	named      = addonkit.DialControl(true)
)

// DialContext dials like net.Dialer, refusing blocked addresses.
func DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: publicOnly}
	return d.DialContext(ctx, network, addr)
}

// ListenPacket opens a UDP socket whose WriteTo refuses blocked addresses
// (a UDP tracker is written to by address, not dialled).
func ListenPacket(network, addr string) (net.PacketConn, error) {
	pc, err := net.ListenPacket(network, addr)
	if err != nil {
		return nil, err
	}
	return guardedPacketConn{pc}, nil
}

type guardedPacketConn struct{ net.PacketConn }

func (c guardedPacketConn) WriteTo(b []byte, to net.Addr) (int, error) {
	host := ""
	if u, ok := to.(*net.UDPAddr); ok {
		host = u.IP.String()
	} else if h, _, err := net.SplitHostPort(to.String()); err == nil {
		host = h
	}
	if addonkit.BlockedAddr(host) {
		return 0, ErrBlocked
	}
	return c.PacketConn.WriteTo(b, to)
}

// HostBlocked reports whether a host (a name or an IP) is, or resolves to,
// a blocked address: for a fetch made on our behalf by something that
// resolves the name itself (a proxy, FlareSolverr's browser). A name that
// does not resolve here is not blocked: nothing can be said of it.
func HostBlocked(ctx context.Context, host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return addonkit.BlockedAddr(host)
	}
	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if addonkit.BlockedAddr(a) {
			return true
		}
	}
	return false
}

// Transport is a transport that refuses blocked addresses, except for the
// hosts allow names (nil: none): the site an admin pointed a source at may
// be on the home network, whatever a redirect or a result link of its pages
// names may not. Even those hosts never reach link-local (cloud metadata),
// unspecified or multicast addresses. An environment proxy is used as ever;
// as it connects for us, the target is judged by name before the request
// goes out (a name that resolves only at the proxy is trusted), and the
// connection to the proxy itself is the admin's own, keyed by its host:port
// (a target with the same host:port, kept off the proxy by NO_PROXY, would
// be dialled unchecked).
func Transport(allow func(host string) bool) *http.Transport {
	return transport(allow, http.ProxyFromEnvironment)
}

// transport is Transport with the proxy lookup a parameter (tests).
func transport(allow func(host string) bool, proxyFor func(*http.Request) (*url.URL, error)) *http.Transport {
	allowed := func(host string) bool { return allow != nil && allow(host) }
	var proxies sync.Map // host:port of the proxies in use
	return &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			pu, err := proxyFor(req)
			if err != nil || pu == nil {
				return nil, err
			}
			if h := req.URL.Hostname(); !allowed(h) && HostBlocked(req.Context(), h) {
				return nil, ErrBlocked
			}
			proxies.Store(proxyAddr(pu), true)
			return pu, nil
		},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			if _, isProxy := proxies.Load(addr); isProxy || allowed(host) {
				d := net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second, Control: named}
				return d.DialContext(ctx, network, addr)
			}
			return DialContext(ctx, network, addr)
		},
		// a custom dialer would turn HTTP/2 off; sites expect it
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	}
}

// proxyAddr is the host:port a transport dials for proxy pu.
func proxyAddr(pu *url.URL) string {
	port := pu.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443", "socks5": "1080", "socks5h": "1080"}[pu.Scheme]
	}
	return net.JoinHostPort(pu.Hostname(), port)
}

// HTTPClient is a client that never connects to a blocked address, whatever
// a redirect names (see Transport). It follows at most 3 redirects.
func HTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: Transport(nil),
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("netguard: too many redirects")
			}
			return nil
		},
	}
}
