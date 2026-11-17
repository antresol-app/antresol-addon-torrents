package opds

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// blockedPrefixes are not reachable on the internet (besides what
// netip's predicates cover): CGNAT and Tailscale, "this network", IETF
// protocol assignments, benchmarking, reserved, IPv4-compatible IPv6,
// site-local IPv6.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"), netip.MustParsePrefix("fec0::/10"),
}

// embedsIPv4: NAT64 (64:ff9b::/96, 64:ff9b:1::/48) and 6to4 (2002::/16)
// addresses carry an IPv4 address that is checked as well.
var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour  = netip.MustParsePrefix("2002::/16")
)

// blockedAddr reports addresses an OPDS source may not reach unless the
// admin allows the home network: anything that is not global unicast
// (loopback, private, link-local and cloud metadata, multicast,
// unspecified), the non-global ranges above, and v6 forms embedding such
// an IPv4 address.
func blockedAddr(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return true
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	if a.Is6() {
		b := a.As16()
		var v4 [4]byte
		switch {
		case nat64.Contains(a):
			copy(v4[:], b[12:16])
		case nat64Local.Contains(a):
			copy(v4[:], b[12:16])
		case sixToFour.Contains(a):
			copy(v4[:], b[2:6])
		default:
			return false
		}
		return blockedAddr(netip.AddrFrom4(v4).String())
	}
	return false
}

// BlockedAddr is blockedAddr for other packages («Смотреть онлайн»'s
// upstream guard).
func BlockedAddr(s string) bool { return blockedAddr(s) }

// guardControl refuses a connection to a blocked address (the dialer's
// Control: it sees the IP actually dialled).
func guardControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	if blockedAddr(host) {
		return &Error{Msg: ErrPrivate.Msg, kind: ErrPrivate}
	}
	return nil
}

// NewHTTPClient returns the client OPDS sources are fetched with. The
// address check runs on the connected IP (after DNS, on every redirect),
// so a name that resolves into the home network is refused as well. Only
// http and https redirects are followed, at most 5; Go drops the
// Authorization header when a redirect leaves the host. Responses must
// start within 30 s; the caller's context bounds the whole request.
func NewHTTPClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = guardControl
	}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if !allowPrivate {
		// a proxy would connect on our behalf and dodge the address check
		tr.Proxy = nil
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("opds: too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return &Error{Msg: ErrBadURL.Msg, kind: ErrBadURL}
			}
			return nil
		},
	}
}

// withTimeout bounds one feed request.
func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 30*time.Second)
}

// errDowngrade: a redirect from https to http would send the login in the
// clear.
var errDowngrade = errors.New("opds: redirect from https to http")

// SameSite returns c with a stricter redirect policy: never from https to
// http, and the login (Authorization) only on hops to credHost — Go would
// keep it on a subdomain (example.org → www.example.org). When sameHost,
// no hop may leave credHost at all. Every other rule of c stays.
func SameSite(c *http.Client, credHost string, sameHost bool) *http.Client {
	cc := *c
	base := c.CheckRedirect
	cc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if base != nil {
			if err := base(req, via); err != nil {
				return err
			}
		}
		if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errDowngrade
		}
		if !strings.EqualFold(req.URL.Hostname(), credHost) {
			if sameHost {
				return ErrForeignHost
			}
			req.Header.Del("Authorization")
		}
		return nil
	}
	return &cc
}
