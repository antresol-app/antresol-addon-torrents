package indexer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// A source FlareSolverr failed on is not asked again by each background
// search (a round of tracked titles): it is paused for 30 minutes, then for
// twice as long after each failure in a row, and a solved page ends the run.
// The background searches during the pause answer at once with the time
// automatic checks resume, and send nothing.
func TestSolverFailurePausesTheSource(t *testing.T) {
	c := newCFEnv(t, 1)
	s := c.add("cf1")
	if s.Status != StatusOK {
		t.Fatalf("source after Add: %q %q", s.Status, s.LastError)
	}
	c.expire()
	c.fsFails.Store(true)
	calls := c.fsCalls.Load()

	msg := failedMessage(t, c.bgSearch())
	if !strings.Contains(msg, "FlareSolverr не справился. Автоматические проверки возобновятся после 10:30") {
		t.Fatalf("first failure: %q", msg)
	}
	if got := c.fsCalls.Load(); got != calls+1 {
		t.Fatalf("solver calls = %d, want %d", got, calls+1)
	}
	if got := c.source(s.ID); got.Status != StatusError || !strings.Contains(got.LastError, "возобновятся после 10:30") {
		t.Fatalf("source: %q %q", got.Status, got.LastError)
	}

	// during the pause: an answer at once, nothing sent
	for range 3 {
		began := time.Now()
		if msg := failedMessage(t, c.bgSearch()); !strings.Contains(msg, "возобновятся после 10:30") {
			t.Fatalf("paused: %q", msg)
		}
		if time.Since(began) > time.Second {
			t.Fatal("a background search of a paused source was slow")
		}
	}
	if got := c.fsCalls.Load(); got != calls+1 {
		t.Fatalf("solver calls = %d during the pause", got)
	}

	// after it: asked once more; failing again doubles the pause
	c.now = c.now.Add(30*time.Minute + time.Second) // 10:30:01
	if msg := failedMessage(t, c.bgSearch()); !strings.Contains(msg, "возобновятся после 11:30") {
		t.Fatalf("second failure: %q", msg)
	}
	if got := c.fsCalls.Load(); got != calls+2 {
		t.Fatalf("solver calls = %d, want %d", got, calls+2)
	}

	// FlareSolverr is back: the source is asked when the pause ends, and
	// the run of failures ends with the answer
	c.fsFails.Store(false)
	c.now = c.now.Add(61 * time.Minute) // 11:31:01
	resp := c.bgSearch()
	if len(resp.Failed) != 0 || len(resp.Releases) != 1 {
		t.Fatalf("after the pause: releases %d, failed %+v", len(resp.Releases), resp.Failed)
	}
	if got := c.source(s.ID); got.Status != StatusOK {
		t.Fatalf("source after an answer: %q %q", got.Status, got.LastError)
	}
	c.expire()
	c.fsFails.Store(true)
	if msg := failedMessage(t, c.bgSearch()); !strings.Contains(msg, "возобновятся после 12:01") {
		t.Fatalf("failure after a success starts over at 30 minutes: %q", msg)
	}
}

// The pause holds only background searches: a search a person started still
// asks FlareSolverr about the paused source, one at a time through the gate.
// While FlareSolverr keeps failing it reports the failure itself, changes
// neither the pause nor the source's status (the failures in a row stay
// those of the background checks), and never starts a pause of its own.
func TestUserSearchTriesAPausedSourceAndNeverLengthensThePause(t *testing.T) {
	c := newCFEnv(t, 1)
	s := c.add("cf1")
	c.expire()
	c.fsFails.Store(true)

	// a person's failure on a source nothing paused: no pause
	calls := c.fsCalls.Load()
	if msg := failedMessage(t, c.search()); strings.Contains(msg, "возобновятся") {
		t.Fatalf("a person's failure: %q", msg)
	}
	if got := c.fsCalls.Load(); got != calls+1 {
		t.Fatalf("solver calls = %d, want %d", got, calls+1)
	}
	if _, paused := c.m.solverPausedUntil(s.ID); paused {
		t.Fatal("a person's failed search paused the source")
	}

	// the background's failure does (10:30, first in a row)
	failedMessage(t, c.bgSearch())
	until, paused := c.m.solverPausedUntil(s.ID)
	if !paused || until.Format("15:04") != "10:30" {
		t.Fatalf("pause: %v %v", until, paused)
	}
	status := c.source(s.ID)

	// a person's search tries all the same, and fails the way FlareSolverr did
	calls = c.fsCalls.Load()
	for range 2 {
		msg := failedMessage(t, c.search())
		if strings.Contains(msg, "возобновятся") || !strings.Contains(msg, "FlareSolverr её не прошёл") {
			t.Fatalf("a person's search of a paused source: %q", msg)
		}
	}
	if got := c.fsCalls.Load(); got != calls+2 {
		t.Fatalf("solver calls = %d, want %d: a person's search must try", got, calls+2)
	}
	if got, ok := c.m.solverPausedUntil(s.ID); !ok || !got.Equal(until) {
		t.Fatalf("pause after a person's failures: %v %v, want until %v", got, ok, until)
	}
	if got := c.source(s.ID); got.Status != status.Status || got.LastError != status.LastError {
		t.Fatalf("status after a person's failures: %q %q, want %q %q", got.Status, got.LastError, status.Status, status.LastError)
	}
	// the background still waits
	calls = c.fsCalls.Load()
	if msg := failedMessage(t, c.bgSearch()); !strings.Contains(msg, "возобновятся после 10:30") {
		t.Fatalf("background search: %q", msg)
	}
	if got := c.fsCalls.Load(); got != calls {
		t.Fatalf("a background search sent %d requests to a paused source", got-calls)
	}
	// and the next failure of its own is the second in a row (one hour),
	// not the fourth: a person's failures were not counted
	c.now = c.now.Add(30*time.Minute + time.Second) // 10:30:01
	if msg := failedMessage(t, c.bgSearch()); !strings.Contains(msg, "возобновятся после 11:30") {
		t.Fatalf("second failure of the background: %q", msg)
	}
}

// A search a person started that gets an answer ends the pause: the
// background checks resume at once. It does with a solved page and with a
// site that needs no solve (the clearance of an earlier one still holds).
func TestUserSearchAnswerEndsThePause(t *testing.T) {
	for _, tc := range []struct {
		name string
		heal func(c *cfEnv)
	}{
		{"solved", func(c *cfEnv) { c.fsFails.Store(false) }},
		{"no solve needed", func(c *cfEnv) { c.gen.Store(0) }}, // the cookie of the first solve is good again
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCFEnv(t, 1)
			s := c.add("cf1")
			c.expire()
			c.fsFails.Store(true)
			failedMessage(t, c.bgSearch())
			if _, paused := c.m.solverPausedUntil(s.ID); !paused {
				t.Fatal("not paused")
			}
			tc.heal(c)

			resp := c.search()
			if len(resp.Failed) != 0 || len(resp.Releases) != 1 {
				t.Fatalf("a person's search: releases %d, failed %+v", len(resp.Releases), resp.Failed)
			}
			if _, paused := c.m.solverPausedUntil(s.ID); paused {
				t.Fatal("an answer left the source paused")
			}
			if got := c.source(s.ID); got.Status != StatusOK {
				t.Fatalf("source: %q %q", got.Status, got.LastError)
			}
			// the run of failures ended too: the next failure is the first
			c.expire()
			c.fsFails.Store(true)
			c.now = c.now.Add(time.Minute)
			if msg := failedMessage(t, c.bgSearch()); !strings.Contains(msg, "возобновятся после 10:31") {
				t.Fatalf("background after the answer: %q", msg)
			}
			// the next background search after a solved one runs at once
			c.fsFails.Store(false)
			c.now = c.now.Add(31 * time.Minute)
			if resp := c.bgSearch(); len(resp.Failed) != 0 || len(resp.Releases) != 1 {
				t.Fatalf("background after the pause: releases %d, failed %+v", len(resp.Releases), resp.Failed)
			}
		})
	}
}

// A marked context is background; the marks of Background are idempotent and
// the plain and the derived contexts of a person's request are not.
func TestBackgroundMarker(t *testing.T) {
	ctx := context.Background()
	if IsBackground(ctx) {
		t.Fatal("a plain context is a person's")
	}
	bg := Background(ctx)
	if !IsBackground(bg) || Background(bg) != bg {
		t.Fatal("Background")
	}
	derived, cancel := context.WithTimeout(WithSettle(bg, time.Second, time.Second), time.Hour)
	defer cancel()
	if !IsBackground(derived) || !IsBackground(context.WithoutCancel(derived)) {
		t.Fatal("a context derived from a background one is background")
	}
	if IsBackground(WithSettle(ctx, time.Second, time.Second)) {
		t.Fatal("a quick search of a person is not background")
	}
}

func TestBackoffAfter(t *testing.T) {
	for fails, want := range map[int]time.Duration{
		1: 30 * time.Minute, 2: time.Hour, 3: 2 * time.Hour, 4: 4 * time.Hour, 5: 6 * time.Hour, 6: 6 * time.Hour, 40: 6 * time.Hour,
	} {
		if got := cardigann.BackoffAfter(fails); got != want {
			t.Errorf("BackoffAfter(%d) = %v, want %v", fails, got, want)
		}
	}
}

// The admin's test of a source still asks FlareSolverr while it is paused
// (it is how a fix is checked), and a passing one ends the pause; a failing
// one reports the failure, and starts or lengthens no pause.
func TestAdminTestIgnoresAndClearsThePause(t *testing.T) {
	c := newCFEnv(t, 1)
	s := c.add("cf1")
	c.expire()
	c.fsFails.Store(true)

	// no pause: a failing test starts none
	if err := c.m.Test(context.Background(), s.ID); err == nil {
		t.Fatal("failing test passed")
	}
	if _, paused := c.m.solverPausedUntil(s.ID); paused {
		t.Fatal("a failed test paused the source")
	}

	failedMessage(t, c.bgSearch()) // paused until 10:30
	calls := c.fsCalls.Load()
	status := c.source(s.ID)

	err := c.m.Test(context.Background(), s.ID)
	if err == nil || strings.Contains(err.Error(), "возобновятся") {
		t.Fatalf("failing test: %v", err) // the failure itself
	}
	if got := c.fsCalls.Load(); got != calls+1 {
		t.Fatalf("the test did not ask FlareSolverr (%d calls)", got)
	}
	if until, ok := c.m.solverPausedUntil(s.ID); !ok || until.Format("15:04") != "10:30" {
		t.Fatalf("pause after a failed test: %v %v", until, ok)
	}
	if got := c.source(s.ID); got.LastError != status.LastError || !strings.Contains(got.LastError, "возобновятся после 10:30") {
		t.Fatalf("status after a failed test: %q, want %q", got.LastError, status.LastError)
	}

	c.fsFails.Store(false)
	if err := c.m.Test(context.Background(), s.ID); err != nil {
		t.Fatalf("passing test: %v", err)
	}
	if _, paused := c.m.solverPausedUntil(s.ID); paused {
		t.Fatal("a passing test left the source paused")
	}
	if resp := c.bgSearch(); len(resp.Failed) != 0 || len(resp.Releases) != 1 {
		t.Fatalf("search after the test: releases %d, failed %+v", len(resp.Releases), resp.Failed)
	}
}

// New settings of the source, or another FlareSolverr, end the pause: the
// failure was of what changed.
func TestSettingsOrSolverChangeEndsThePause(t *testing.T) {
	c := newCFEnv(t, 1)
	s := c.add("cf1")
	c.expire()
	c.fsFails.Store(true)
	failedMessage(t, c.bgSearch())
	if _, paused := c.m.solverPausedUntil(s.ID); !paused {
		t.Fatal("not paused")
	}
	name := "Сайт 1"
	if _, err := c.m.Update(context.Background(), s.ID, SourcePatch{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if _, paused := c.m.solverPausedUntil(s.ID); paused {
		t.Fatal("still paused after the source was saved")
	}

	failedMessage(t, c.bgSearch()) // paused again
	c.m.SetFlareSolverr(c.fs.URL)
	if _, paused := c.m.solverPausedUntil(s.ID); !paused {
		t.Fatal("the same address ended the pause")
	}
	c.m.SetFlareSolverr(c.fs.URL + "/")
	if _, paused := c.m.solverPausedUntil(s.ID); paused {
		t.Fatal("still paused after another address")
	}
}

// A search that gave up before FlareSolverr answered only saw a timeout, but
// the failure still pauses the source when the answer comes, if the search
// was a background one, and says so in the source's status. The failure of
// a request a person started pauses nothing.
func TestAbandonedSolveStillPausesTheSource(t *testing.T) {
	c := newCFEnv(t, 1)
	s := c.add("cf1")
	c.expire()
	c.fsFails.Store(true)
	c.fsDelay.Store(int64(400 * time.Millisecond))
	c.m.searchTimeout = 100 * time.Millisecond
	calls := c.fsCalls.Load()

	// a person's: gives up, FlareSolverr fails later, nothing is paused
	began := time.Now()
	msg := failedMessage(t, c.search())
	if !strings.Contains(msg, "не ответил вовремя") || time.Since(began) > 350*time.Millisecond {
		t.Fatalf("search: %q after %v", msg, time.Since(began))
	}
	time.Sleep(700 * time.Millisecond) // FlareSolverr answers
	if _, paused := c.m.solverPausedUntil(s.ID); paused {
		t.Fatal("the failure of a person's abandoned request paused the source")
	}
	if got := c.fsCalls.Load(); got != calls+1 {
		t.Fatalf("solver calls = %d, want %d", got, calls+1)
	}

	began = time.Now()
	msg = failedMessage(t, c.bgSearch())
	if !strings.Contains(msg, "не ответил вовремя") || time.Since(began) > 350*time.Millisecond {
		t.Fatalf("background search: %q after %v", msg, time.Since(began))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, paused := c.m.solverPausedUntil(s.ID); paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the failure of the abandoned request did not pause the source")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := c.source(s.ID); got.Status != StatusError || !strings.Contains(got.LastError, "возобновятся после 10:30") {
		t.Fatalf("source: %q %q", got.Status, got.LastError)
	}
	if msg := failedMessage(t, c.bgSearch()); !strings.Contains(msg, "возобновятся после 10:30") {
		t.Fatalf("next search: %q", msg)
	}
	if got := c.fsCalls.Load(); got != calls+2 {
		t.Fatalf("solver calls = %d, want %d", got, calls+2)
	}
}

// The messages are in the language of the request.
func TestSolverPauseMessagesAreTranslated(t *testing.T) {
	until := time.Date(2026, 11, 24, 10, 30, 0, 0, time.UTC)
	en := i18n.WithLanguage(context.Background(), i18n.EN)
	got := humanErrorIn(en, &solverPausedError{until: until})
	if !strings.Contains(got, "FlareSolverr failed. Automatic checks resume after 10:30") {
		t.Errorf("paused: %q", got)
	}
	// the status is stored in Russian and shown in the viewer's language
	stored := humanError(&solverPausedError{until: until})
	if got := i18n.Localize(en, stored); !strings.Contains(got, "FlareSolverr failed. Automatic checks resume after 10:30") {
		t.Errorf("stored status %q shows as %q", stored, got)
	}
}
