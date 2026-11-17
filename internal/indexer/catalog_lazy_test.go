package indexer

import (
	"sync"
	"testing"
)

// The catalog parses its definitions on first use, not when made (the
// server's start), and once however many ask at the same time.
func TestCatalogLoadsOnFirstUse(t *testing.T) {
	c := NewCatalog(t.TempDir())
	c.mu.RLock()
	loaded := c.defs != nil
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
