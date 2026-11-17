package follow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol/addonkit"
)

// The event types coreadapt mirrors (follow imports acquire, which
// imports coreadapt) are follow's own.
func TestFollowEvents_Mirrored(t *testing.T) {
	if coreadapt.EventFollowQueued != EventFollowQueued || coreadapt.EventFollowChecked != EventFollowChecked {
		t.Fatalf("coreadapt %q %q", coreadapt.EventFollowQueued, coreadapt.EventFollowChecked)
	}
}

// TestFollowChecked_Event: a check's follow.queued goes to the core
// as admin.follow-queued with its episode codes, its follow.checked as
// follow-checked, which reaches every adult profile and so carries only
// the follow's id (no codes, no note naming library titles).
func TestFollowChecked_Event(t *testing.T) {
	type posted struct {
		Name string          `json:"name"`
		Data json.RawMessage `json:"data"`
	}
	var mu sync.Mutex
	var got []posted
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/addon-core/v1/events" {
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
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { coreadapt.Forward(ctx, e.bus, addonkit.NewClient(addonkit.Env{CoreURL: srv.URL})); close(done) }()
	defer func() { cancel(); <-done }()
	for wait := time.Now().Add(5 * time.Second); !e.bus.Wants(EventFollowChecked); time.Sleep(time.Millisecond) {
		if time.Now().After(wait) {
			t.Fatal("Forward does not listen to follow.checked")
		}
	}
	f := e.add("series", 42)
	if _, err := e.svc.Check(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("posted %+v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	id := string(mustJSON(t, f.ID))
	if got[0].Name != "admin.follow-queued" || string(got[0].Data) != `{"followId":`+id+`,"queued":["S01E10"]}` {
		t.Fatalf("queued %s %s", got[0].Name, got[0].Data)
	}
	if got[1].Name != "follow-checked" || string(got[1].Data) != `{"followId":`+id+`}` {
		t.Fatalf("checked %s %s", got[1].Name, got[1].Data)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
