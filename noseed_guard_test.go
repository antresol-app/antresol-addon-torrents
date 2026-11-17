package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// engineNoSeedingCall is any use of a SetNoSeeding method through a value
// (x.SetNoSeeding(…), or the method value x.SetNoSeeding). The engine's
// definition (func (e *Engine) SetNoSeeding) has no dot before the name;
// the store's own method is SaveNoSeeding, so it is not confused with it.
var engineNoSeedingCall = regexp.MustCompile(`\.SetNoSeeding\b`)

// noSeedingCallers: the only files that may hold it: boot.NoSeed, the
// one owner of the effective value, and the engine's definition file.
var noSeedingCallers = map[string]bool{
	"internal/boot/noseed.go":    true,
	"internal/torrent/noseed.go": true,
}

// TestNoSeedingSingleCaller: no .go file of the module (every package,
// build-tagged and legacy/ included; tests left out) but noSeedingCallers
// reaches the engine's SetNoSeeding: every settings save goes through
// NoSeed.Apply.
func TestNoSeedingSingleCaller(t *testing.T) {
	walked := 0
	found := map[string]bool{}
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		walked++
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(p)
		for i, line := range strings.Split(string(body), "\n") {
			if engineNoSeedingCall.MatchString(line) {
				found[rel] = true
				if !noSeedingCallers[rel] {
					t.Errorf("%s:%d: %s — only boot.NoSeed sets the engine's no-seeding; call NoSeed.Apply", rel, i+1, strings.TrimSpace(line))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked < 100 {
		t.Fatalf("walked %d .go files: not the module", walked)
	}
	if !found["internal/boot/noseed.go"] {
		t.Fatal("internal/boot/noseed.go does not call SetNoSeeding: the guard is looking at the wrong thing")
	}
}

// storeSaveNoSeeding is a write of the stored «Не раздавать» setting.
var storeSaveNoSeeding = regexp.MustCompile(`\.SaveNoSeeding\(`)

// TestSaveNoSeedingOnlyInBoot: the stored setting is written only by
// boot.NoSeed (and Boot's sticky forced value) — never by a route, the
// importer or another package straight to the store. Tests count
// too: only boot's and the store's own may write it directly.
func TestSaveNoSeedingOnlyInBoot(t *testing.T) {
	allowed := func(rel string) bool {
		return strings.HasPrefix(rel, "internal/boot/") || rel == "internal/store/settings.go" ||
			strings.HasPrefix(rel, "internal/store/") && strings.HasSuffix(rel, "_test.go")
	}
	walked, seen := 0, false
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		walked++
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(p)
		for i, line := range strings.Split(string(body), "\n") {
			if !storeSaveNoSeeding.MatchString(line) || rel == "noseed_guard_test.go" {
				continue
			}
			if rel == "internal/boot/noseed.go" {
				seen = true
			}
			if !allowed(rel) {
				t.Errorf("%s:%d: %s — write «Не раздавать» through boot.NoSeed.Set", rel, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked < 100 || !seen {
		t.Fatalf("walked %d .go files, boot's write seen %v: the guard is looking at the wrong thing", walked, seen)
	}
}
