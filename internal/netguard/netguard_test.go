package netguard

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// A guarded client never reaches a server on loopback, nor through a
// redirect from anywhere.
func TestHTTPClientRefusesLoopback(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	if _, err := HTTPClient(2 * time.Second).Get(srv.URL); err == nil || !errors.Is(err, ErrBlocked) {
		t.Fatalf("GET on loopback: err %v, want ErrBlocked", err)
	}
	if hits != 0 {
		t.Fatalf("the loopback server saw %d requests", hits)
	}
}

func TestListenPacketRefusesBlockedTarget(t *testing.T) {
	srv, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer srv.Close()
	pc, err := ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.WriteTo([]byte("x"), srv.LocalAddr()); !errors.Is(err, ErrBlocked) {
		t.Fatalf("WriteTo loopback: %v, want ErrBlocked", err)
	}
	srv.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if n, _, err := srv.ReadFrom(make([]byte, 8)); err == nil {
		t.Fatalf("the loopback socket received %d bytes", n)
	}
}

func get(t *testing.T, tr *http.Transport, u string) (string, error) {
	t.Helper()
	c := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	resp, err := c.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

// Transport refuses every blocked address but the hosts it is told are the
// admin's own (a source pointed at a LAN site), and a hop from such a host
// to any other is judged anew.
func TestTransportAllowsOnlyNamedHosts(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "other") }))
	defer other.Close()
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/out" {
			http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1), http.StatusFound)
			return
		}
		io.WriteString(w, "site")
	}))
	defer site.Close()

	if _, err := get(t, Transport(nil), site.URL); !errors.Is(err, ErrBlocked) {
		t.Fatalf("nothing allowed: err = %v", err)
	}
	only := func(h string) bool { return h == "127.0.0.1" }
	if body, err := get(t, Transport(only), site.URL); err != nil || body != "site" {
		t.Fatalf("the named host: %q, %v", body, err)
	}
	if _, err := get(t, Transport(only), site.URL+"/out"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("a redirect to another local host: err = %v, want ErrBlocked", err)
	}
}

// With a proxy the target is judged by name (the proxy resolves it) and the
// connection to the proxy itself is always allowed.
func TestTransportThroughAProxy(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "via proxy "+r.Host) }))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	via := func(*http.Request) (*url.URL, error) { return pu, nil }

	if body, err := get(t, transport(nil, via), "http://tracker.example.test/x"); err != nil || body != "via proxy tracker.example.test" {
		t.Fatalf("public name: %q, %v", body, err)
	}
	if _, err := get(t, transport(nil, via), "http://10.1.2.3/x"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("private literal through a proxy: err = %v, want ErrBlocked", err)
	}
	if body, err := get(t, transport(func(h string) bool { return h == "10.1.2.3" }, via), "http://10.1.2.3/x"); err != nil || body != "via proxy 10.1.2.3" {
		t.Fatalf("allowed literal: %q, %v", body, err)
	}
}

func TestHostBlocked(t *testing.T) {
	ctx := context.Background()
	for h, want := range map[string]bool{"127.0.0.1": true, "::1": true, "93.184.216.34": false, "localhost": true, "no-such-host.invalid": false} {
		if got := HostBlocked(ctx, h); got != want {
			t.Errorf("HostBlocked(%q) = %v, want %v", h, got, want)
		}
	}
}
