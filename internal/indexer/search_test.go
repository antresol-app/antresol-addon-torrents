package indexer

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

func TestScoredReleaseJSONDirect(t *testing.T) {
	b, err := json.Marshal(ScoredRelease{Result: cardigann.Result{Title: "x", Link: "https://c/b/1/epub"}, Direct: true})
	if err != nil || !strings.Contains(string(b), `"direct":true`) {
		t.Fatalf("%s %v", b, err)
	}
	b, _ = json.Marshal(ScoredRelease{Result: cardigann.Result{Title: "x"}})
	if strings.Contains(string(b), `"direct"`) {
		t.Fatalf("torrent marked direct: %s", b)
	}
}

func TestSearchSkipsOPDSForMusic(t *testing.T) {
	e := newEnv(t)
	var hits atomic.Int32
	srv := fakeOPDS(t, false, &hits)
	e.addOPDS(srv.URL+"/opds", nil)
	hits.Store(0)
	for _, kind := range []string{"music", "audiobook", "movie"} {
		resp, err := e.m.Search(context.Background(), SearchRequest{Kind: kind, Text: "Мастер и Маргарита"})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Releases)+len(resp.Rejected)+len(resp.Failed) != 0 {
			t.Fatalf("%s: %+v", kind, resp)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the OPDS catalog was asked %d times", hits.Load())
	}
	resp, err := e.m.Search(context.Background(), SearchRequest{Kind: "comic", Text: "Мастер и Маргарита"})
	if err != nil || hits.Load() == 0 {
		t.Fatalf("comic search skipped the catalog: %v %+v", err, resp)
	}
}

// TestDirectBeatsEqualTorrent: a direct EPUB scores like a torrent EPUB
// with its seeders' points replaced by the +15 bonus; on a tie the direct
// download comes first.
func TestDirectBeatsEqualTorrent(t *testing.T) {
	want := Wanted{Kind: "book", Titles: []string{"Михаил Булгаков Мастер и Маргарита", "Мастер и Маргарита"}}
	p := DefaultPrefs()
	direct, why := scoreDirect(cardigann.Result{Title: "Михаил Булгаков — Мастер и Маргарита"}, "EPUB", want, p)
	if direct == Excluded || !contains(why, "EPUB") || !contains(why, "прямая загрузка") {
		t.Fatalf("direct %d %v", direct, why)
	}
	noBonus, _ := ScoreMedia(cardigann.Result{Title: "Михаил Булгаков — Мастер и Маргарита [EPUB]"}, "book", want, Prefs{})
	if direct != noBonus+15 {
		t.Fatalf("direct %d, plain %d", direct, noBonus)
	}
	if s, why := scoreDirect(cardigann.Result{Title: "Лев Толстой — Война и мир"}, "EPUB", want, p); s != Excluded {
		t.Fatalf("other title %d %v", s, why)
	}
	rels := []ScoredRelease{
		{Result: cardigann.Result{Title: "t", Seeders: 500}, Score: direct},
		{Result: cardigann.Result{Title: "d"}, Score: direct, Direct: true},
		{Result: cardigann.Result{Title: "low"}, Score: direct - 1, Direct: true},
	}
	sortReleases(rels)
	if rels[0].Title != "d" || rels[1].Title != "t" || rels[2].Title != "low" {
		t.Fatalf("order %+v", rels)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
