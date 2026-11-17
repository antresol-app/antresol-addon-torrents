package coreadapt_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/events"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol/addonkit"
)

type posted struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data"`
}

func TestForward(t *testing.T) {
	var mu sync.Mutex
	var got []posted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/addon-core/v1/events" {
			http.NotFound(w, r)
			return
		}
		var p posted
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		got = append(got, p)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	bus := &events.Bus{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { coreadapt.Forward(ctx, bus, addonkit.NewClient(addonkit.Env{CoreURL: srv.URL})); close(done) }()
	for !bus.Wants(coreadapt.EventProgress) {
		time.Sleep(time.Millisecond)
	}
	if bus.Wants(events.LibraryUpdated) {
		t.Fatal("Forward listens to library.updated")
	}
	bus.Publish(events.Event{Type: events.LibraryUpdated, Data: "x"})
	bus.Publish(events.Event{Type: coreadapt.EventProgress, Data: []string{"a"}})
	bus.Publish(events.Event{Type: indexer.EventSourceStatus, Data: map[string]int{"id": 3}})
	bus.Publish(events.Event{Type: "other", Data: 1})
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // nothing else follows
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Name != "admin.progress" || string(got[0].Data) != `["a"]` ||
		got[1].Name != "admin.source-status" || string(got[1].Data) != `{"id":3}` {
		t.Fatalf("%+v", got)
	}
}

// The constants are the addon's own; acquire's is checked by acquire
// once it builds on this package (it imports coreadapt).
func TestForward_Constants(t *testing.T) {
	if coreadapt.EventSourceStatus != indexer.EventSourceStatus {
		t.Fatalf("%q != indexer's %q", coreadapt.EventSourceStatus, indexer.EventSourceStatus)
	}
	if coreadapt.EventProgress != "torrent.progress" {
		t.Fatal(coreadapt.EventProgress)
	}
}

// Forward ends when the bus closes.
func TestForward_BusClosed(t *testing.T) {
	bus := &events.Bus{}
	done := make(chan struct{})
	go func() {
		coreadapt.Forward(context.Background(), bus, addonkit.NewClient(addonkit.Env{CoreURL: "http://127.0.0.1:1"}))
		close(done)
	}()
	for bus.Subscribers() == 0 {
		time.Sleep(time.Millisecond)
	}
	bus.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Forward still running")
	}
}

// An event the core refuses as too big (413) is logged once per period
// with its size, not on every 2 s tick; other events still go out.
func TestForward_TooBigLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	var logMu sync.Mutex
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &logMu}, nil)))
	defer slog.SetDefault(prev)
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p posted
		json.NewDecoder(r.Body).Decode(&p)
		mu.Lock()
		calls++
		mu.Unlock()
		if p.Name == "admin.progress" {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			w.Write([]byte(`{"error":"too big"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	bus := &events.Bus{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { coreadapt.Forward(ctx, bus, addonkit.NewClient(addonkit.Env{CoreURL: srv.URL})); close(done) }()
	for !bus.Wants(coreadapt.EventProgress) {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		bus.Publish(events.Event{Type: coreadapt.EventProgress, Data: []string{"a"}})
		time.Sleep(5 * time.Millisecond)
	}
	bus.Publish(events.Event{Type: indexer.EventSourceStatus, Data: map[string]int{"id": 3}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 6 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	logMu.Lock()
	out := buf.String()
	logMu.Unlock()
	if calls != 6 {
		t.Fatalf("calls = %d", calls)
	}
	if n := strings.Count(out, "admin.progress"); n != 1 || !strings.Contains(out, "bytes=") || !strings.Contains(out, "413") {
		t.Fatalf("want one 413 line with the size, got %d:\n%s", n, out)
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
