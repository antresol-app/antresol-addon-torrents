package indexer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

const torrentBody = "d8:announce3:xxx4:infod4:name1:x12:piece lengthi16384e6:pieces20:01234567890123456789ee"

// local names the same loopback server under another host name: another
// site, as far as the guard goes.
func local(u string) string { return strings.Replace(u, "127.0.0.1", "localhost", 1) }

// A tracker's result link and its redirects name hosts of its own choosing:
// Download refuses a link off the source's sites (also for the background
// callers, which skip CheckReleaseLink), and the source's client refuses to
// connect to the local network off the admin's own site. The control, with
// a client the caller supplied, shows the redirect would be followed.
func TestTrackerSourceCannotReachTheHomeNetwork(t *testing.T) {
	var internal atomic.Int32
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internal.Add(1)
		w.Write([]byte(torrentBody))
	}))
	defer service.Close()
	tracker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/own.torrent":
			w.Write([]byte(torrentBody))
		case "/hop.torrent":
			http.Redirect(w, r, local(service.URL)+"/secret", http.StatusFound)
		}
	})
	ctx := context.Background()

	run := func(t *testing.T, own bool) (e *env, id int64) {
		e = newEnv(t)
		e.def("fake", "Fake", tracker)
		if own {
			e.m = NewManager(e.db, e.cat, Options{SearchTimeout: 2 * time.Second, TestTimeout: 2 * time.Second, HTTP: &http.Client{Timeout: 5 * time.Second}})
		}
		return e, e.add("fake").ID
	}
	site := func(e *env) string { d, _ := e.cat.Definition("fake"); return strings.TrimSuffix(d.Links[0], "/") }

	t.Run("a link off the site is refused", func(t *testing.T) {
		e, id := run(t, false)
		if _, err := e.m.Download(ctx, id, local(service.URL)+"/x.torrent"); !errors.Is(err, errForeignRelease) {
			t.Fatalf("err = %v, want errForeignRelease", err)
		}
		if n := internal.Load(); n != 0 {
			t.Fatalf("the home network service was asked %d times", n)
		}
		if res, err := e.m.Download(ctx, id, site(e)+"/own.torrent"); err != nil || len(res.Torrent) == 0 {
			t.Fatalf("a link on the site: %d bytes, %v", len(res.Torrent), err)
		}
	})

	t.Run("a redirect off the site is not followed to the home network", func(t *testing.T) {
		e, id := run(t, false)
		_, err := e.m.Download(ctx, id, site(e)+"/hop.torrent")
		if err == nil {
			t.Fatal("the redirect to a local host was followed")
		}
		// the admin is told why, in both languages, not that the site is down
		en := i18n.WithLanguage(ctx, i18n.EN)
		if ru := err.Error(); !strings.Contains(ru, "заблокировал") || !strings.Contains(ru, "«Адрес сайта»") || strings.Contains(ru, "недоступен") {
			t.Fatalf("ru: %q", ru)
		}
		if msg := i18n.Message(en, err); !strings.Contains(msg, "blocked the source") || !strings.Contains(msg, "Site Address") {
			t.Fatalf("en: %q", msg)
		}
		if n := internal.Load(); n != 0 {
			t.Fatalf("the home network service was asked %d times", n)
		}
	})

	t.Run("control: a caller's own client follows it", func(t *testing.T) {
		e, id := run(t, true)
		if _, err := e.m.Download(ctx, id, site(e)+"/hop.torrent"); err != nil {
			t.Fatal(err)
		}
		if internal.Load() == 0 {
			t.Fatal("the redirect was not followed: the test proves nothing")
		}
	})
}

// Which hosts a source may reach on the home network: what the admin typed
// (mirror, API host) and the links of a definition of his own; never the
// links of an embedded definition, and nothing of a direct-download source.
func TestLocalHostsAreWhatTheAdminConfigured(t *testing.T) {
	e := newEnv(t)
	e.def("mine", "Mine", serve(""))
	d, _ := e.cat.Definition("mine")
	own := cardigann.SiteHost(d.Links[0])

	mine := e.m.localHosts(Source{DefinitionID: "mine", Settings: map[string]string{"sitelink": "http://jackett.lan:9117/x"}})
	if !mine(own) || !mine("jackett.lan") || mine("example.org") {
		t.Fatalf("own definition: links %v", d.Links)
	}
	embedded := e.m.localHosts(Source{DefinitionID: "rutor", Settings: map[string]string{"sitelink": "http://10.20.30.40:8080", "apiurl": "api.lan"}})
	if !embedded("10.20.30.40") || !embedded("api.lan") || embedded("rutor.info") || embedded("127.0.0.1") {
		t.Fatal("embedded definition: only the typed hosts may be local")
	}
}

// A solve refused for a local address is not a FlareSolverr failure: it says
// so, and nothing pauses the source for it.
func TestSolverRefusalIsExplained(t *testing.T) {
	err := fmt.Errorf("%w: Fake: not solved through FlareSolverr", cardigann.ErrLocalAddress)
	en := i18n.WithLanguage(context.Background(), i18n.EN)
	if ru := humanError(err); !strings.Contains(ru, "не открывает адреса домашней сети") || strings.Contains(ru, "её не прошёл") {
		t.Fatalf("ru: %q", ru)
	}
	if msg := humanErrorIn(en, err); !strings.Contains(msg, "does not open home-network addresses") {
		t.Fatalf("en: %q", msg)
	}
	if errors.Is(err, cardigann.ErrCloudflare) {
		t.Fatal("a refusal is no Cloudflare failure: it would pause the source")
	}
}
