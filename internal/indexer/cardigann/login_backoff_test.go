package cardigann

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// cookieSite is the cookie-login tracker of TestLoginCookieMethod with a
// switch: it accepts the session only while good is set, and counts the
// requests that check it (each is one sign-in attempt the tracker sees).
func cookieSite(t *testing.T) (srv *httptest.Server, good *atomic.Bool, checks *atomic.Int32) {
	t.Helper()
	good, checks = new(atomic.Bool), new(atomic.Int32)
	mux := http.NewServeMux()
	mux.HandleFunc("/forum/index.php", func(w http.ResponseWriter, r *http.Request) {
		checks.Add(1)
		if !good.Load() {
			w.Write([]byte(`<html><body><a href="login.php">Вход</a></body></html>`))
			return
		}
		w.Write([]byte(`<html><body><a id="logged-in-username">bob</a></body></html>`))
	})
	mux.HandleFunc("/forum/tracker.php", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html><body><a id="logged-in-username">bob</a><div id="search-results"><table>
<tr><td><a href="tracker.php?f=313">HD Video</a></td><td><a href="viewtopic.php?t=1">Сёгун / Shogun [2024, WEB-DL 1080p]</a></td>
<td><a class="tr-dl" href="dl.php?t=1">19.6 GB</a></td><td><b class="seedmed">250</b></td><td class="leechmed">31</td><td><p>2024-04-23 12:00</p></td></tr>
</table></div></body></html>`))
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, good, checks
}

// A sign-in that failed is not tried again by every search: the next
// automatic one waits 30 minutes, then twice as long after each failure in
// a row, and the searches in between answer with the same error at once.
// The admin's test always tries, and a good one ends the backoff.
func TestFailedSignInIsBackedOff(t *testing.T) {
	srv, good, checks := cookieSite(t)
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	r := &Runner{Def: mustParse(t, cookieLoginDef), Settings: map[string]string{"sitelink": srv.URL + "/", "cookie": "bb_session=old"},
		HTTP: newClient(t), Now: func() time.Time { return now }}
	ctx := context.Background()
	search := func() error {
		_, err := r.Search(ctx, Query{Keywords: "x"})
		return err
	}

	if err := search(); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("first search: %v", err)
	}
	if checks.Load() != 1 {
		t.Fatalf("sign-in attempts = %d", checks.Load())
	}
	for range 3 {
		if err := search(); !errors.Is(err, ErrLoginFailed) {
			t.Fatalf("backed off: %v", err)
		}
	}
	if checks.Load() != 1 {
		t.Fatalf("sign-in attempts = %d during the backoff, want 1", checks.Load())
	}

	// 30 minutes: tried once more; failing again, the wait is an hour
	now = now.Add(30*time.Minute + time.Second)
	if err := search(); !errors.Is(err, ErrLoginFailed) || checks.Load() != 2 {
		t.Fatalf("after 30 min: %v, attempts %d", err, checks.Load())
	}
	now = now.Add(59 * time.Minute)
	search()
	if checks.Load() != 2 {
		t.Fatalf("attempts = %d, want 2 within the hour", checks.Load())
	}
	now = now.Add(2 * time.Minute)
	search()
	if checks.Load() != 3 {
		t.Fatalf("attempts = %d, want 3 after the hour", checks.Load())
	}

	// the admin's test tries at once, whatever the backoff; a failing one
	// does not push it further, a passing one ends it
	before := checks.Load()
	if err := r.Login(ctx); !errors.Is(err, ErrLoginFailed) || checks.Load() != before+1 {
		t.Fatalf("forced login: %v, attempts %d", err, checks.Load())
	}
	now = now.Add(2*time.Hour + time.Second) // the backoff after three failures was 2 h
	if err := search(); !errors.Is(err, ErrLoginFailed) || checks.Load() != before+2 {
		t.Fatalf("after the 2 h backoff: %v, attempts %d", err, checks.Load())
	}
	good.Store(true)
	if err := r.Login(ctx); err != nil {
		t.Fatalf("a good login: %v", err)
	}
	if err := search(); err != nil {
		t.Fatalf("search after a good login: %v", err)
	}
	// the run is over: a new failure starts at 30 minutes again
	good.Store(false)
	r.mu.Lock()
	r.loggedIn = false
	r.mu.Unlock()
	if err := search(); !errors.Is(err, ErrLoginFailed) {
		t.Fatalf("a new failure: %v", err)
	}
	failed := checks.Load()
	now = now.Add(31 * time.Minute)
	search()
	if checks.Load() != failed+1 {
		t.Fatalf("attempts = %d, want %d: the backoff did not start over at 30 minutes", checks.Load(), failed+1)
	}
	// looked at (the manager's wake): the next search signs in at once
	search()
	if checks.Load() != failed+1 {
		t.Fatalf("attempts = %d, want %d: tried during the backoff", checks.Load(), failed+1)
	}
	r.ResetSignIn()
	search()
	if checks.Load() != failed+2 {
		t.Fatalf("attempts = %d, want %d: not tried at once after the reset", checks.Load(), failed+2)
	}
}

// A sign-in that failed because its caller ran out of time says nothing of
// the site: the next search tries again.
func TestSignInCutShortIsNotBackedOff(t *testing.T) {
	srv, good, checks := cookieSite(t)
	good.Store(true)
	r := &Runner{Def: mustParse(t, cookieLoginDef), Settings: map[string]string{"sitelink": srv.URL + "/", "cookie": "bb_session=x"}, HTTP: newClient(t)}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Search(gone, Query{Keywords: "x"}); err == nil {
		t.Fatal("a search with no time left succeeded")
	}
	if _, err := r.Search(context.Background(), Query{Keywords: "x"}); err != nil {
		t.Fatalf("the next search: %v", err)
	}
	if checks.Load() == 0 {
		t.Fatal("never signed in")
	}
}
