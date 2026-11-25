package cardigann

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// seen records the requests a test server got (headers and form field).
type seen struct {
	mu   sync.Mutex
	hits []map[string]string
}

func (s *seen) handler(next string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		s.mu.Lock()
		s.hits = append(s.hits, map[string]string{"path": r.URL.Path, "key": r.Header.Get("X-Api-Key"), "pass": r.PostForm.Get("password")})
		s.mu.Unlock()
		if next != "" && r.URL.Path == "/start" {
			http.Redirect(w, r, next, http.StatusTemporaryRedirect)
		}
	}
}

func (s *seen) got() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string(nil), s.hits...)
}

func redirectRunner(t *testing.T, site string) *Runner {
	t.Helper()
	r := &Runner{Def: mustParse(t, "id: rd\nname: Rd\nlinks: [\""+site+"/\"]\nsettings: []\nsearch:\n  path: s\n  rows: {selector: tr}\n  fields: {title: {selector: td}}\n"), HTTP: newClient(t)}
	if err := r.setup(); err != nil {
		t.Fatal(err)
	}
	return r
}

// elsewhere: the same server under another host name (localhost, not
// 127.0.0.1): another site to the runner.
func elsewhere(u string) string { return strings.Replace(u, "127.0.0.1", "localhost", 1) }

// A redirect that leaves the site takes neither the definition's headers (API
// keys) nor a posted form (a login) along; one that stays keeps them.
func TestRedirectKeepsSecretsOnTheSite(t *testing.T) {
	var other, site seen
	otherSrv := httptest.NewServer(other.handler(""))
	defer otherSrv.Close()
	siteSrv := httptest.NewServer(site.handler(""))
	defer siteSrv.Close()
	leave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere(otherSrv.URL)+"/landed", http.StatusFound)
	}))
	defer leave.Close()

	r := redirectRunner(t, leave.URL)
	resp, err := r.do(context.Background(), &request{method: http.MethodGet, url: leave.URL + "/start", headers: map[string]string{"X-API-KEY": "secret"}, follow: true})
	if err != nil || resp.status != http.StatusOK {
		t.Fatalf("redirect off the site: %+v, %v", resp, err)
	}
	if h := other.got(); len(h) != 1 || h[0]["key"] != "" {
		t.Fatalf("the other site got %v: the API key must stay home", h)
	}

	// the same hop inside the site keeps the header
	siteSrv.Config.Handler = site.handler(siteSrv.URL + "/landed")
	r = redirectRunner(t, siteSrv.URL)
	if _, err := r.do(context.Background(), &request{method: http.MethodGet, url: siteSrv.URL + "/start", headers: map[string]string{"X-API-KEY": "secret"}, follow: true}); err != nil {
		t.Fatal(err)
	}
	if h := site.got(); len(h) != 2 || h[1]["path"] != "/landed" || h[1]["key"] != "secret" {
		t.Fatalf("the site got %v", h)
	}
}

// A 307 on a login POST would replay the password to the new host: refused.
func TestRedirectDoesNotReplayAFormToAnotherSite(t *testing.T) {
	var other seen
	otherSrv := httptest.NewServer(other.handler(""))
	defer otherSrv.Close()
	login := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere(otherSrv.URL)+"/steal", http.StatusTemporaryRedirect)
	}))
	defer login.Close()
	r := redirectRunner(t, login.URL)
	_, err := r.do(context.Background(), &request{method: http.MethodPost, url: login.URL + "/login", form: []formPair{{key: "password", value: "hunter2"}}, follow: true})
	if !errors.Is(err, ErrBadResponse) {
		t.Fatalf("err = %v, want ErrBadResponse", err)
	}
	if h := other.got(); len(h) != 0 {
		t.Fatalf("the other site got %v", h)
	}
}

// With credentials on the request an https to http hop is refused even
// outside Strict; a bare page request may still take it.
func TestRedirectWithSecretsNeverDowngrades(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
	}))
	defer tls.Close()
	r := redirectRunner(t, tls.URL)
	r.client = tls.Client()
	if _, err := r.do(context.Background(), &request{method: http.MethodGet, url: tls.URL + "/a", headers: map[string]string{"X-API-KEY": "k"}, follow: true}); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("with a key: err = %v, want ErrBadResponse", err)
	}
	if _, err := r.do(context.Background(), &request{method: http.MethodGet, url: tls.URL + "/a", follow: true}); err != nil {
		t.Fatalf("bare page: %v", err)
	}
}
