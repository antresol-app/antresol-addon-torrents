package indexer

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestSearch_CoreStudiosFirst: a search scores with the core's
// studios (Options.Studios) ahead of the addon's own prefs, without
// duplicates, and hands that list on in Prefs; the stored prefs and the
// settings' read (Prefs) keep the addon's own list.
func TestSearch_CoreStudiosFirst(t *testing.T) {
	e := newEnv(t)
	e.reload()
	ctx := context.Background()
	p := DefaultPrefs()
	p.Studios = []string{"Кураж-Бамбей", "lostfilm"}
	if err := e.m.SavePrefs(ctx, p); err != nil {
		t.Fatal(err)
	}
	stored, _ := e.m.Prefs(ctx)
	asked := 0
	e.m = NewManager(e.db, e.cat, Options{SearchTimeout: 2 * time.Second, TestTimeout: 2 * time.Second,
		Studios: func(context.Context) []string { asked++; return []string{"LostFilm"} }})
	resp, err := e.m.Search(ctx, SearchRequest{Text: "Show", Kind: "series"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Prefs == nil || !slices.Equal(resp.Prefs.Studios, []string{"LostFilm", "Кураж-Бамбей"}) || asked != 1 {
		t.Fatalf("prefs %+v, asked %d", resp.Prefs, asked)
	}
	if got, _ := e.m.Prefs(ctx); !slices.Equal(got.Studios, stored.Studios) {
		t.Fatalf("stored prefs changed: %v", got.Studios)
	}
}
