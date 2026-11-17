package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

func hasCyrillic(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Cyrillic, r) {
			return true
		}
	}
	return false
}

func TestCatalogHelpRussian(t *testing.T) {
	cat := NewCatalog(t.TempDir())
	e, ok := cat.Get("rutracker")
	if !ok {
		t.Fatal("rutracker is not in the catalog")
	}
	spec := map[string]SettingSpec{}
	for _, s := range e.Settings {
		spec[s.Name] = s
	}
	pw, ok := spec["password"]
	if !ok || !hasCyrillic(pw.Help) || !hasCyrillic(pw.Label) || !pw.Required {
		t.Fatalf("rutracker password spec = %+v", pw)
	}
	if u := spec["username"]; u.Help != "Логин на сайте" || !u.Required {
		t.Fatalf("username spec = %+v", u)
	}
	// technical settings of the definition are hidden from the simple form
	if f, ok := spec["format-movie"]; ok && !f.Advanced {
		t.Fatalf("format-movie is not advanced: %+v", f)
	}
	if _, ok := spec["format"]; ok {
		t.Fatal("info setting leaked into the form")
	}

	c, ok := cat.Get("rutracker-org-cookie")
	if !ok {
		t.Fatal("rutracker-org-cookie missing")
	}
	var cookie SettingSpec
	for _, s := range c.Settings {
		if s.Name == "cookie" {
			cookie = s
		}
	}
	if !cookie.Required || !strings.Contains(cookie.Help, "войдите") || !strings.Contains(cookie.Help, "Network") {
		t.Fatalf("cookie help = %q", cookie.Help)
	}
}

func TestCatalogEmbedsRequiredTrackers(t *testing.T) {
	cat := NewCatalog(t.TempDir())
	for _, id := range []string{"rutracker", "kinozal", "noname-club", "rutor", "megapeer", "bigfangroup", "newstudio", "thepiratebay"} {
		if _, ok := cat.Get(id); !ok {
			t.Errorf("%s missing", id)
		}
	}
	if n := len(cat.List("")); n < 500 {
		t.Errorf("catalog has %d entries, want the full Prowlarr set", n)
	}
	got := cat.List("кинозал")
	if len(got) == 0 {
		got = cat.List("kinozal")
	}
	if len(got) == 0 || got[0].ID != "kinozal" && got[0].ID != "kinozal-magnet" {
		t.Errorf("List(kinozal) = %+v", got)
	}
	// Russian trackers come first in an empty search
	if l := cat.List(""); l[0].Language != "ru-RU" {
		t.Errorf("first entry language = %q", l[0].Language)
	}
}

func TestCatalogUserDefinitionOverrides(t *testing.T) {
	dir := t.TempDir()
	def := strings.Replace(fakeDef("rutor", "Мой RuTor", "http://127.0.0.1:1"), "description: test tracker", "description: свой", 1)
	if err := os.WriteFile(filepath.Join(dir, "rutor.yml"), []byte(def), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "broken.yml"), []byte("::: not yaml"), 0o644)
	cat := NewCatalog(dir)
	e, ok := cat.Get("rutor")
	if !ok || e.Name != "Мой RuTor" || !e.Custom {
		t.Fatalf("rutor = %+v", e)
	}
}

// Every embedded definition must parse (scripts/update-definitions.sh runs
// this before keeping a new set).
func TestEmbeddedDefinitionsParse(t *testing.T) {
	files, _ := fs.Glob(embeddedFS, "definitions/*.yml")
	extra, _ := fs.Glob(embeddedFS, "definitions/extra/*.yml")
	for _, f := range append(files, extra...) {
		data, _ := embeddedFS.ReadFile(f)
		if _, err := cardigann.Parse(data); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if len(files) < 500 {
		t.Errorf("only %d embedded definitions", len(files))
	}
}
