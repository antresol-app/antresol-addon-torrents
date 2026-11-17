package boot

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
)

// syncBuf is a log sink safe for the callback's goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The finish callback never waits for the core: the letter goes in the
// background under notifyTimeout, a failure is logged, and the later
// hooks run at once.
func TestFinished_NotifyFailureLogged(t *testing.T) {
	var buf syncBuf
	log := slog.New(slog.NewTextHandler(&buf, nil))
	old := notifyTimeout
	notifyTimeout = 100 * time.Millisecond
	t.Cleanup(func() { notifyTimeout = old })
	var later []string
	var mu sync.Mutex
	cb := finishedCallback(func() *slog.Logger { return log },
		func(ctx context.Context, f acquire.Finished) error {
			<-ctx.Done() // a core that does not answer
			return errors.New("core down")
		},
		func(f acquire.Finished) { mu.Lock(); later = append(later, f.Key); mu.Unlock() })
	start := time.Now()
	cb(acquire.Finished{Key: "abc", Title: "Фильм"})
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("the callback waited %v for the core", d)
	}
	mu.Lock()
	if len(later) != 1 || later[0] != "abc" {
		t.Fatalf("later hooks %v", later)
	}
	mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "core down") {
		if time.Now().After(deadline) {
			t.Fatalf("not logged: %q", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s := buf.String(); !strings.Contains(s, "abc") || !strings.Contains(s, "level=WARN") {
		t.Fatalf("log %q", s)
	}
}
