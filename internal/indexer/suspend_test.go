package indexer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// loginDef is fakeDef with a cookie sign-in whose check looks for a logout link.
func loginDef(id, name, link string) string {
	return fmt.Sprintf(`---
id: %s
name: %s
description: test tracker with a sign-in
language: ru-RU
type: semi-private
links: [%q]
caps:
  categorymappings:
    - {id: 1, cat: Movies, desc: Movies}
  modes: {search: [q]}
settings: []
login:
  method: cookie
  inputs:
    cookie: "sid=old"
  test:
    path: index
    selector: a.logout
search:
  paths:
    - path: search
  inputs:
    q: "{{ .Keywords }}"
  rows:
    selector: tr
  fields:
    title: {selector: td.t}
    size: {selector: td.s}
    seeders: {selector: td.se}
`, id, name, link+"/")
}

// A source whose sign-in is refused (an expired cookie) is not asked again
// on every search: it rests until its settings change, a test passes, or
// a while has gone by — each refused search is a login attempt the tracker
// sees.
func TestSearchRestsASourceWithARefusedSignIn(t *testing.T) {
	var hits atomic.Int32
	loggedIn := atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if loggedIn.Load() {
			w.Write([]byte(`<html><body><a class="logout" href="/out">Выход</a><table></table></body></html>`))
			return
		}
		w.Write([]byte(`<html><body><form>login</form></body></html>`))
	}))
	defer srv.Close()
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.dir, "definitions", "lt.yml"), []byte(loginDef("lt", "LoginTracker", srv.URL)), 0o644); err != nil {
		t.Fatal(err)
	}
	e.reload()
	now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	e.m.now = func() time.Time { return now }
	s := e.add("lt")
	ctx := context.Background()
	search := func() SearchResponse {
		resp, err := e.m.Search(ctx, SearchRequest{Text: "Дюна", Kind: "movie"})
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := search()
	if len(resp.Failed) != 1 {
		t.Fatalf("first search: failed %+v", resp.Failed)
	}
	first := hits.Load()
	resp = search()
	if hits.Load() != first {
		t.Fatalf("a refused sign-in was tried again on the next search (%d → %d requests)", first, hits.Load())
	}
	if len(resp.Failed) != 1 {
		t.Fatalf("a resting source still counts as not answering (the search is incomplete): %+v", resp.Failed)
	}
	if got, _ := e.m.Get(ctx, s.ID); got.Status != StatusError {
		t.Fatalf("the source keeps showing its error: %q", got.Status)
	}
	// six hours later it is tried once more
	now = now.Add(6*time.Hour + time.Minute)
	search()
	if hits.Load() == first {
		t.Fatal("not tried again after the rest")
	}
	// new settings (a fresh cookie): tried at once
	second := hits.Load()
	loggedIn.Store(true)
	name := "LoginTracker"
	if _, err := e.m.Update(ctx, s.ID, SourcePatch{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if resp := search(); hits.Load() == second || len(resp.Failed) != 0 {
		t.Fatalf("after new settings: requests %d → %d, failed %+v", second, hits.Load(), resp.Failed)
	}
}
