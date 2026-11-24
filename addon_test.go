package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/trackinghooks"
	"github.com/antresol-app/antresol/addonkit"
)

func TestHealth(t *testing.T) {
	ready := &addonkit.Ready{}
	h, _ := newHandler(addonkit.Env{}, nil, ready)
	for _, want := range []bool{false, true} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
		var got map[string]bool
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK || got["ready"] != want {
			t.Fatalf("health: %d %s, want ready %v", rec.Code, rec.Body, want)
		}
		ready.Set()
	}
}

func TestManifest(t *testing.T) {
	raw, err := os.ReadFile("antresol-addon.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ID          string            `json:"id"`
		Version     string            `json:"version"`
		Contract    int               `json:"contract"`
		Access      string            `json:"access"`
		Permissions []string          `json:"permissions"`
		Hooks       []string          `json:"hooks"`
		Notify      json.RawMessage   `json:"notify"`
		Name        map[string]string `json:"name"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// play, notify and requests, the library hooks and the
	// downloads letters' topic; tracking and voiceover, the tracking
	// hooks (trackinghooks.Names)
	perms := []string{"library.read", "library.write", "files.holds", "metadata", "images", "users.read", "events", "play", "notify", "requests",
		"tracking", "voiceover"}
	hooks := append([]string{"files.plan", "files.pause", "files.resume", "files.stop", "files.detach", "files.attach",
		"library.pending", "disk.usage", "requests.open", "requests.title", "requests.search"}, trackinghooks.Names...)
	// «Переезд»'s leftovers for the core's screen, and 0.4.0; the
	// music import stays fenced: no imports permission, no import.* hook
	hooks = append(hooks, "migrate.leftovers")
	// TV channels that download their programmes ahead of air
	hooks = append(hooks, "channels.fetch")
	// «Состояние» of the core: FlareSolverr as the sources use it
	hooks = append(hooks, "helpers.status")
	var topics []struct {
		Topic    string            `json:"topic"`
		Title    map[string]string `json:"title"`
		Audience string            `json:"audience"`
	}
	json.Unmarshal(m.Notify, &topics)
	if m.ID != "torrents" || m.Version != "0.4.16" || m.Contract != 1 || m.Access != "adults" ||
		!slices.Equal(m.Permissions, perms) || !slices.Equal(m.Hooks, hooks) ||
		len(topics) != 1 || topics[0].Topic != "downloads" || topics[0].Audience != "admins" ||
		topics[0].Title["ru"] != "Загрузки" || topics[0].Title["en"] != "Downloads" ||
		m.Name["ru"] == "" || m.Name["en"] == "" {
		t.Fatalf("manifest: %+v %s", m, m.Notify)
	}
}

// TestHealth_BootError: a failed start answers not ready with its error.
func TestHealth_BootError(t *testing.T) {
	ready := &addonkit.Ready{}
	status := &bootStatus{}
	inner, _ := newHandler(addonkit.Env{}, nil, ready)
	h := status.wrap(inner)
	get := func() map[string]any {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("health: %d %s", rec.Code, rec.Body)
		}
		return got
	}
	if got := get(); got["ok"] != true || got["ready"] != false || got["error"] != nil {
		t.Fatalf("starting: %v", got)
	}
	status.fail(errors.New("boom"))
	if got := get(); got["ok"] != true || got["ready"] != false || got["error"] != "boom" {
		t.Fatalf("failed: %v", got)
	}
}

// TestManifest_NoImportWhileFenced: while the music import is fenced,
// the manifest claims none of its contract — no import.* hook, no imports
// permission — so the core finds no import addon and degrades to the file
// import; «Переезд»'s leftovers are declared.
func TestManifest_NoImportWhileFenced(t *testing.T) {
	fenced, err := os.ReadFile("tools/fenced.txt")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("antresol-addon.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Permissions []string `json:"permissions"`
		Hooks       []string `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Hooks, "migrate.leftovers") {
		t.Errorf("hooks %v: want migrate.leftovers", m.Hooks)
	}
	if !strings.Contains(string(fenced), "\ninternal/musicimport/") {
		return // the music import is wired: its own tests own the manifest
	}
	for _, h := range m.Hooks {
		if strings.HasPrefix(h, "import.") {
			t.Errorf("hook %s declared while internal/musicimport is fenced", h)
		}
	}
	if slices.Contains(m.Permissions, "imports") {
		t.Error("permission imports declared while internal/musicimport is fenced")
	}
}

// TestManifest_HelperTitle: the FlareSolverr helper has a human title in
// both languages («Состояние» shows it instead of the service name).
func TestManifest_HelperTitle(t *testing.T) {
	raw, err := os.ReadFile("antresol-addon.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Helpers []struct {
			Name  string            `json:"name"`
			Title map[string]string `json:"title"`
		} `json:"helpers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Helpers) != 1 || m.Helpers[0].Name != "flaresolverr" {
		t.Fatalf("helpers %+v", m.Helpers)
	}
	for _, lang := range []string{"ru", "en"} {
		if !strings.Contains(m.Helpers[0].Title[lang], "FlareSolverr") {
			t.Errorf("title %s: %q", lang, m.Helpers[0].Title[lang])
		}
	}
}
