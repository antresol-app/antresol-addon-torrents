package indexer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// A source FlareSolverr failed on is not asked again by each search: it is
// paused for 30 minutes, then for twice as long after each failure in a row,
// and a solved page ends the run. The searches during the pause answer at
// once with the time they may come back, and send nothing.
func TestSolverFailurePausesTheSource(t *testing.T) {
	c := newCFEnv(t, 1)
	s := c.add("cf1")
	if s.Status != StatusOK {
		t.Fatalf("source after Add: %q %q", s.Status, s.LastError)
	}
	c.expire()
	c.fsFails.Store(true)
	calls := c.fsCalls.Load()

	msg := failedMessage(t, c.search())
	if !strings.Contains(msg, "FlareSolverr не справился, повтор после 10:30") {
		t.Fatalf("first failure: %q", msg)
	}
	if got := c.fsCalls.Load(); got != calls+1 {
		t.Fatalf("solver calls = %d, want %d", got, calls+1)
	}
	if got := c.source(s.ID); got.Status != StatusError || !strings.Contains(got.LastError, "повтор после 10:30") {
		t.Fatalf("source: %q %q", got.Status, got.LastError)
	}

	// during the pause: an answer at once, nothing sent
	for range 3 {
		began := time.Now()
		if msg := failedMessage(t, c.search()); !strings.Contains(msg, "повтор после 10:30") {
			t.Fatalf("paused: %q", msg)
		}
		if time.Since(began) > time.Second {
			t.Fatal("a search of a paused source was slow")
		}
	}
	if got := c.fsCalls.Load(); got != calls+1 {
		t.Fatalf("solver calls = %d during the pause", got)
	}

	// after it: asked once more; failing again doubles the pause
	c.now = c.now.Add(30*time.Minute + time.Second) // 10:30:01
	if msg := failedMessage(t, c.search()); !strings.Contains(msg, "повтор после 11:30") {
		t.Fatalf("second failure: %q", msg)
	}
	if got := c.fsCalls.Load(); got != calls+2 {
		t.Fatalf("solver calls = %d, want %d", got, calls+2)
	}

	// FlareSolverr is back: the source is asked when the pause ends, and
	// the run of failures ends with the answer
	c.fsFails.Store(false)
	c.now = c.now.Add(61 * time.Minute) // 11:31:01
	resp := c.search()
	if len(resp.Failed) != 0 || len(resp.Releases) != 1 {
		t.Fatalf("after the pause: releases %d, failed %+v", len(resp.Releases), resp.Failed)
	}
	if got := c.source(s.ID); got.Status != StatusOK {
		t.Fatalf("source after an answer: %q %q", got.Status, got.LastError)
	}
	c.expire()
	c.fsFails.Store(true)
	if msg := failedMessage(t, c.search()); !strings.Contains(msg, "повтор после 12:01") {
		t.Fatalf("failure after a success starts over at 30 minutes: %q", msg)
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
// one says until when, too.
func TestAdminTestIgnoresAndClearsThePause(t *testing.T) {
	c := newCFEnv(t, 1)
	s := c.add("cf1")
	c.expire()
	c.fsFails.Store(true)
	failedMessage(t, c.search()) // paused until 10:30
	calls := c.fsCalls.Load()

	err := c.m.Test(context.Background(), s.ID)
	if err == nil || !strings.Contains(err.Error(), "повтор после 10:30") {
		t.Fatalf("failing test: %v", err) // not a new failure in a row: the pause stays as it was
	}
	if got := c.fsCalls.Load(); got != calls+1 {
		t.Fatalf("the test did not ask FlareSolverr (%d calls)", got)
	}

	c.fsFails.Store(false)
	if err := c.m.Test(context.Background(), s.ID); err != nil {
		t.Fatalf("passing test: %v", err)
	}
	if _, paused := c.m.solverPausedUntil(s.ID); paused {
		t.Fatal("a passing test left the source paused")
	}
	if resp := c.search(); len(resp.Failed) != 0 || len(resp.Releases) != 1 {
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
	failedMessage(t, c.search())
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

	failedMessage(t, c.search()) // paused again
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
// the failure still pauses the source when the answer comes, and says so in
// its status.
func TestAbandonedSolveStillPausesTheSource(t *testing.T) {
	c := newCFEnv(t, 1)
	s := c.add("cf1")
	c.expire()
	c.fsFails.Store(true)
	c.fsDelay.Store(int64(400 * time.Millisecond))
	c.m.searchTimeout = 100 * time.Millisecond
	calls := c.fsCalls.Load()

	began := time.Now()
	msg := failedMessage(t, c.search())
	if !strings.Contains(msg, "не ответил вовремя") || time.Since(began) > 350*time.Millisecond {
		t.Fatalf("search: %q after %v", msg, time.Since(began))
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
	if got := c.source(s.ID); got.Status != StatusError || !strings.Contains(got.LastError, "повтор после 10:30") {
		t.Fatalf("source: %q %q", got.Status, got.LastError)
	}
	if msg := failedMessage(t, c.search()); !strings.Contains(msg, "повтор после 10:30") {
		t.Fatalf("next search: %q", msg)
	}
	if got := c.fsCalls.Load(); got != calls+1 {
		t.Fatalf("solver calls = %d, want %d", got, calls+1)
	}
}

// The messages are in the language of the request.
func TestSolverPauseMessagesAreTranslated(t *testing.T) {
	until := time.Date(2026, 11, 24, 10, 30, 0, 0, time.UTC)
	en := i18n.WithLanguage(context.Background(), i18n.EN)
	got := humanErrorIn(en, &solverPausedError{until: until})
	if !strings.Contains(got, "FlareSolverr failed; the next try is after 10:30") {
		t.Errorf("paused: %q", got)
	}
	// the status is stored in Russian and shown in the viewer's language
	stored := humanError(&solverPausedError{until: until})
	if got := i18n.Localize(en, stored); !strings.Contains(got, "FlareSolverr failed; the next try is after 10:30") {
		t.Errorf("stored status %q shows as %q", stored, got)
	}
}
