package studios

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol/addonkit"
)

// The studios the core used to ship (its studios_full.go), as before.
func TestRegister(t *testing.T) {
	if _, ok := release.CanonicalStudio("rezka"); ok {
		t.Fatal("known before Register")
	}
	Register()
	Register()
	for title, want := range map[string][]string{
		"Something.S01.1080p.WEB-DL.LostFilm.TV": {"LostFilm"},
		"Сериал (2020) WEB-DL 720p | Dub + MVO (кубик³, Lost Film, LostFilm.TV, HDrezka Studio)":             {"Кубик в Кубе", "LostFilm", "HDRezka Studio"},
		"Сёгун / Shōgun / Сезон: 1 / Серии: 1-10 из 10 [2024, WEB-DL 1080p] MVO (HDRezka Studio) + Original": {"HDRezka Studio"},
		"Billions.S03E01.Rus.LostFilm.TV.1080p": {"LostFilm"},
	} {
		if got := release.Parse(title).Studios; !slices.Equal(got, want) {
			t.Errorf("%q: studios %v, want %v", title, got, want)
		}
	}
	for in, want := range map[string]string{" LostFilm.TV ": "LostFilm", "HDRezka": "HDRezka Studio", "rezka": "HDRezka Studio"} {
		if got, ok := release.CanonicalStudio(in); !ok || got != want {
			t.Errorf("%q → %q,%v want %q", in, got, ok, want)
		}
	}
	want := []string{"LostFilm", "Кубик в Кубе", "HDRezka Studio", "NewStudio", "AlexFilm", "Jaskier", "TVShows",
		"Кураж-Бамбей", "Red Head Sound", "BaibaKo", "Pifagor", "Novamedia", "SDI Media", "Amedia", "Coldfilm", "IdeaFilm"}
	if got := release.KnownStudios(); !slices.Equal(got, want) {
		t.Fatalf("%v", got)
	}
}

// fakeCore serves GET /settings/studios with list (or 503 when down) and
// counts the calls.
type fakeCore struct {
	mu    sync.Mutex
	list  []string
	down  bool
	calls int
}

func (f *fakeCore) client(t *testing.T) *addonkit.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/addon-core/v1/settings/studios" {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if f.down {
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":"busy"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"studios": f.list})
	}))
	t.Cleanup(srv.Close)
	return addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})
}

func (f *fakeCore) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

// TestStudios_MergedFromCore: the core's «Любимые озвучки» first,
// then the addon's own prefs without the ones the core named (by
// StudioKey).
func TestStudios_MergedFromCore(t *testing.T) {
	Register()
	fc := &fakeCore{list: []string{"LostFilm"}}
	c := &Core{Studios: fc.client(t).Studios}
	got := c.Merge(context.Background(), []string{"Кураж-Бамбей", "lostfilm"})
	if want := []string{"LostFilm", "Кураж-Бамбей"}; !slices.Equal(got, want) {
		t.Fatalf("merged %v, want %v", got, want)
	}
	// a nil Core is the addon's own list as it was
	var none *Core
	if got := none.Merge(context.Background(), []string{"x"}); !slices.Equal(got, []string{"x"}) {
		t.Fatalf("nil core: %v", got)
	}
}

// TestStudios_Cached: two searches within 60 s ask the core once;
// past it the core is asked again, and a core that is down keeps the last
// list.
func TestStudios_Cached(t *testing.T) {
	Register()
	fc := &fakeCore{list: []string{"Jaskier"}}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	c := &Core{Studios: fc.client(t).Studios, Now: func() time.Time { return now }}
	ctx := context.Background()
	for range 2 {
		if got := c.Merge(ctx, nil); !slices.Equal(got, []string{"Jaskier"}) {
			t.Fatalf("%v", got)
		}
	}
	if n := fc.count(); n != 1 {
		t.Fatalf("core calls %d within 60 s, want 1", n)
	}
	now = now.Add(61 * time.Second)
	fc.mu.Lock()
	fc.down = true
	fc.mu.Unlock()
	if got := c.Merge(ctx, []string{"Pifagor"}); !slices.Equal(got, []string{"Jaskier", "Pifagor"}) {
		t.Fatalf("core down: %v, want the last list kept", got)
	}
	if n := fc.count(); n != 2 {
		t.Fatalf("core calls %d, want 2", n)
	}
	// a failed read is not retried at every search either
	c.Merge(ctx, nil)
	if n := fc.count(); n != 2 {
		t.Fatalf("core calls %d after a failure, want 2", n)
	}
	// back: the new list once the minute is over
	now = now.Add(61 * time.Second)
	fc.mu.Lock()
	fc.down, fc.list = false, []string{"Amedia"}
	fc.mu.Unlock()
	if got := c.Merge(ctx, nil); !slices.Equal(got, []string{"Amedia"}) {
		t.Fatalf("back: %v", got)
	}
	// never read and down: none
	fd := &fakeCore{down: true}
	if got := (&Core{Studios: fd.client(t).Studios}).Merge(ctx, []string{"Pifagor"}); !slices.Equal(got, []string{"Pifagor"}) {
		t.Fatalf("never read: %v", got)
	}
}
