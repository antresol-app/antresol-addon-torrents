package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The catalog parses its definitions on first use, not when made (the
// server's start), and once however many ask at the same time.
func TestCatalogLoadsOnFirstUse(t *testing.T) {
	c := NewCatalog(t.TempDir())
	c.mu.RLock()
	loaded := c.user != nil
	c.mu.RUnlock()
	if loaded {
		t.Fatal("parsed when made")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Definition("0magnet"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, ok := c.Get("0magnet"); !ok || len(c.List("")) < 100 {
		t.Fatal("not loaded")
	}
}

// An embedded definition is found by its id even where its file is named
// otherwise, and an unknown id is no definition.
func TestEmbeddedDefinitionsAreFoundByID(t *testing.T) {
	c := NewCatalog("")
	if n := len(embeddedFiles()); n < 500 {
		t.Fatalf("%d embedded definitions indexed", n)
	}
	for _, id := range []string{"nordicquality-api", "bluebirdhd", "showrss-yml", "rutor"} {
		d, err := c.Definition(id)
		if err != nil || d.ID != id {
			t.Fatalf("Definition(%q) = %v, %v", id, d, err)
		}
		if e, ok := c.Get(id); !ok || e.ID != id || e.Custom {
			t.Fatalf("Get(%q) = %+v, %v", id, e, ok)
		}
	}
	if _, err := c.Definition("no-such-tracker"); !errors.Is(err, ErrNoDefinition) {
		t.Fatalf("unknown id: %v", err)
	}
	// the index (a line scan) names every file's id as the parser does
	for id := range embeddedFiles() {
		if d, ok := embeddedDef(id); ok && d.def.ID != id {
			t.Errorf("indexed as %q, parsed as %q", id, d.def.ID)
		}
	}
	// parsed once: the same pointer every time
	a, _ := c.Definition("rutor")
	b, _ := c.Definition("rutor")
	if a != b {
		t.Fatal("an embedded definition was parsed twice")
	}
}

// Reading the user directory again keeps the parsed definition of a file
// that did not change (the runner of its source keeps its login session),
// takes up a changed one and drops a removed one.
func TestReloadKeepsUnchangedUserDefinitions(t *testing.T) {
	dir := t.TempDir()
	write := func(name, id string) {
		t.Helper()
		yml := fakeDef(id, name, "http://"+id+".example")
		if err := os.WriteFile(filepath.Join(dir, id+".yml"), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("One", "one")
	write("Two", "two")
	c := NewCatalog(dir)
	one, _ := c.Definition("one")
	two, _ := c.Definition("two")
	if one == nil || two == nil {
		t.Fatal("user definitions not loaded")
	}
	// another definition saved: the others are the same objects
	if _, err := c.SaveUserDefinition([]byte(fakeDef("three", "Three", "http://three.example"))); err != nil {
		t.Fatal(err)
	}
	if again, _ := c.Definition("one"); again != one {
		t.Fatal("an unchanged definition was parsed anew")
	}
	// one rewritten (a later modification time, another size), one removed
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(dir, "two.yml"), []byte(fakeDef("two", "Two, renamed", "http://two.example")), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "one.yml"))
	c.Reload()
	if d, _ := c.Definition("two"); d == two || d.Name != "Two, renamed" {
		t.Fatalf("a changed definition: %+v", d)
	}
	if _, err := c.Definition("one"); !errors.Is(err, ErrNoDefinition) {
		t.Fatalf("a removed definition: %v", err)
	}
	if e, ok := c.Get("two"); !ok || !e.Custom {
		t.Fatalf("entry %+v", e)
	}
}

// A user definition replaces the embedded one of its id in the list, once.
func TestListUserDefinitionOverridesEmbedded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rutor.yml"), []byte(fakeDef("rutor", "My Rutor", "http://rutor.lan")), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewCatalog(dir)
	n := 0
	for _, e := range c.List("") {
		if e.ID == "rutor" {
			n++
			if !e.Custom || e.Name != "My Rutor" {
				t.Fatalf("entry %+v", e)
			}
		}
	}
	if n != 1 {
		t.Fatalf("rutor listed %d times", n)
	}
}
