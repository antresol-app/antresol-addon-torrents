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

// rotatingTracker is a cookie-login site that, like phpBB trackers, hands
// out a new session id with Set-Cookie and stops accepting the old one.
// The values are fake.
type rotatingTracker struct {
	mu      sync.Mutex
	valid   string // the accepted bb_session
	next    []string
	rotates int
}

func (s *rotatingTracker) handler() http.Handler {
	mux := http.NewServeMux()
	signedIn := func(w http.ResponseWriter, r *http.Request) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		c, err := r.Cookie("bb_session")
		if err != nil || c.Value != s.valid {
			return false
		}
		if len(s.next) > 0 {
			s.valid, s.next = s.next[0], s.next[1:]
			s.rotates++
			http.SetCookie(w, &http.Cookie{Name: "bb_session", Value: s.valid, Path: "/"})
		}
		return true
	}
	mux.HandleFunc("/forum/index.php", func(w http.ResponseWriter, r *http.Request) {
		if !signedIn(w, r) {
			w.Write([]byte(`<html><body><a href="login.php">Вход</a></body></html>`))
			return
		}
		w.Write([]byte(`<html><body><a id="logged-in-username">bob</a></body></html>`))
	})
	mux.HandleFunc("/forum/tracker.php", func(w http.ResponseWriter, r *http.Request) {
		if !signedIn(w, r) {
			http.Redirect(w, r, "/forum/login.php", http.StatusFound)
			return
		}
		w.Write([]byte(`<html><body><a id="logged-in-username">bob</a><div id="search-results"><table></table></div></body></html>`))
	})
	return mux
}

// A refreshed session is reported to OnCookies (once per change, not for
// the configured cookie itself) and a new sign-in starts from it: the
// configured cookie is no longer accepted by then.
func TestCookieLoginKeepsRefreshedSession(t *testing.T) {
	site := &rotatingTracker{valid: "fake-1", next: []string{"fake-2"}}
	srv := httptest.NewServer(site.handler())
	defer srv.Close()
	var mu sync.Mutex
	var got []string
	r := &Runner{
		Def:      mustParse(t, cookieLoginDef),
		Settings: map[string]string{"sitelink": srv.URL + "/", "cookie": "bb_session=fake-1; bb_ssl=1"},
		HTTP:     newClient(t),
		OnCookies: func(c string) {
			mu.Lock()
			got = append(got, c)
			mu.Unlock()
		},
	}
	ctx := context.Background()
	if err := r.Test(ctx); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if len(got) != 1 || cookieSet(got[0]) != "bb_session=fake-2; bb_ssl=1" {
		t.Fatalf("reported %q", got)
	}
	// unchanged cookies are not reported again
	if _, err := r.Search(ctx, Query{Keywords: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("reported again: %q", got)
	}
	// signing in again (a test, a lost session) uses the refreshed cookie,
	// not the configured fake-1 the site has dropped
	if err := r.Login(ctx); err != nil {
		t.Fatalf("second sign-in: %v", err)
	}
}

// Changes within CookieInterval of the last report wait for a later
// signed-in response; responses that ask to sign in are not reported.
func TestCookieLoginReportsAtMostOncePerInterval(t *testing.T) {
	site := &rotatingTracker{valid: "fake-1", next: []string{"fake-2", "fake-3"}}
	srv := httptest.NewServer(site.handler())
	defer srv.Close()
	var got []string
	r := &Runner{
		Def:       mustParse(t, cookieLoginDef),
		Settings:  map[string]string{"sitelink": srv.URL + "/", "cookie": "bb_session=fake-1"},
		HTTP:      newClient(t),
		OnCookies: func(c string) { got = append(got, c) },
	}
	ctx := context.Background()
	if err := r.Login(ctx); err != nil { // fake-1 → fake-2, reported
		t.Fatal(err)
	}
	if _, err := r.Search(ctx, Query{Keywords: "x"}); err != nil { // fake-2 → fake-3, too soon
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "bb_session=fake-2" {
		t.Fatalf("reported %q", got)
	}
	old := CookieInterval
	CookieInterval = 0
	defer func() { CookieInterval = old }()
	if _, err := r.Search(ctx, Query{Keywords: "x"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "bb_session=fake-3" {
		t.Fatalf("reported %q", got)
	}

	// a dead session: nothing reported, the sign-in fails as Cardigann's
	// login test does (the test page lacks the selector)
	dead := &Runner{
		Def:       mustParse(t, cookieLoginDef),
		Settings:  map[string]string{"sitelink": srv.URL + "/", "cookie": "bb_session=stale"},
		HTTP:      newClient(t),
		OnCookies: func(c string) { t.Errorf("reported %q for a dead session", c) },
	}
	err := dead.Test(ctx)
	if !errors.Is(err, ErrLoginFailed) || !strings.Contains(err.Error(), "cookie") {
		t.Fatalf("stale cookie: %v", err)
	}
}
