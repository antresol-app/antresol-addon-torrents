package coreadapt_test

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/antresol-app/antresol/addonkit"
	"github.com/antresol-app/antresol/addonkit/addonkittest"
)

// The replay helper: the core's recorded answers (addonkit's
// testdata/golden, through addonkittest.ReplayGolden) behind a client, and
// a note of each (adapter method, golden) pair the package's tests make,
// for TestAdapterCoverage (coverage_test.go). The note is taken in the
// client's own goroutine (http.Transport's RoundTrip runs in it): every
// coreadapt method on the stack of a request a golden answered is paired
// with that golden. No comment convention.

// goldenSrc is the golden folder: the core checkout's (ANTRESOL_CORE, as
// make sets it, relative to the module root), else addonkit's own.
func goldenSrc() string {
	if core := os.Getenv("ANTRESOL_CORE"); core != "" {
		if !filepath.IsAbs(core) {
			core = filepath.Join("..", "..", core) // relative to the module root (make), not this package
		}
		return filepath.Join(core, "addonkit", "testdata", "golden")
	}
	return addonkittest.GoldenDir()
}

type replayRoute struct {
	method, path string
	query        url.Values
	name         string
}

var replays = struct {
	sync.Mutex
	hosts map[string][]replayRoute // a replay server's host → its goldens
	pairs map[string]map[string]bool
}{hosts: map[string][]replayRoute{}, pairs: map[string]map[string]bool{}}

// replay is a client of the core's recorded answers.
func replay(t *testing.T) *addonkit.Client {
	t.Helper()
	dir := goldenSrc()
	srv := addonkittest.ReplayGolden(t, dir)
	var routes []replayRoute
	for name, g := range addonkittest.LoadGoldens(t, dir) {
		u, err := url.Parse(g.Path)
		if err != nil {
			t.Fatal(err)
		}
		routes = append(routes, replayRoute{g.Method, u.EscapedPath(), u.Query(), name})
	}
	host := strings.TrimPrefix(srv.URL, "http://")
	replays.Lock()
	replays.hosts[host] = routes
	replays.Unlock()
	t.Cleanup(func() {
		replays.Lock()
		delete(replays.hosts, host)
		replays.Unlock()
	})
	return addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})
}

// recording notes the pairs of a request to a replay server, then sends it.
type recording struct{ next http.RoundTripper }

func (r recording) RoundTrip(req *http.Request) (*http.Response, error) {
	replays.Lock()
	routes := replays.hosts[req.URL.Host]
	replays.Unlock()
	if routes != nil {
		q := req.URL.Query()
		for _, rt := range routes {
			if rt.method == req.Method && rt.path == req.URL.EscapedPath() && reflect.DeepEqual(rt.query, q) {
				notePair(rt.name)
				break
			}
		}
	}
	return r.next.RoundTrip(req)
}

const adaptPkg = "github.com/antresol-app/antresol-addon-torrents/internal/coreadapt."

// notePair pairs golden with each coreadapt method on the caller's stack.
func notePair(golden string) {
	pc := make([]uintptr, 64)
	frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
	for {
		f, more := frames.Next()
		if m, ok := adapterMethod(f.Function); ok {
			replays.Lock()
			if replays.pairs[m] == nil {
				replays.pairs[m] = map[string]bool{}
			}
			replays.pairs[m][golden] = true
			replays.Unlock()
		}
		if !more {
			return
		}
	}
}

// adapterMethod is "Type.Method" of a coreadapt method's frame
// ("…/coreadapt.Library.Item", "…/coreadapt.(*Profiles).Languages", a
// closure inside one); a function or a test's frame is none.
func adapterMethod(fn string) (string, bool) {
	rest, ok := strings.CutPrefix(fn, adaptPkg)
	if !ok {
		return "", false
	}
	rest = strings.NewReplacer("(*", "", ")", "").Replace(rest)
	parts := strings.Split(rest, ".")
	if len(parts) < 2 {
		return "", false
	}
	return parts[0] + "." + parts[1], true
}
