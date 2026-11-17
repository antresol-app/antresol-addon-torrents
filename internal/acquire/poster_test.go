package acquire

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

// fakeImages is the image cache: it records the URLs asked for and names
// the file the way metadata.Images does (sha1 of the URL).
type fakeImages struct {
	mu   sync.Mutex
	urls []string
	fail bool
}

func (f *fakeImages) Fetch(_ context.Context, url string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, url)
	if f.fail {
		return "", context.DeadlineExceeded
	}
	return cacheNameOf(url), nil
}

func cacheNameOf(url string) string {
	sum := sha1.Sum([]byte(url))
	return hex.EncodeToString(sum[:]) + ".jpg"
}

func (f *fakeImages) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.urls...)
}

// posterOf waits for the background cover fetch and returns the poster the
// downloads list shows for an infohash.
func posterOf(t *testing.T, e *env, ih string) string {
	t.Helper()
	var got string
	for i := 0; i < 200; i++ {
		for _, tv := range e.svc.Torrents() {
			if tv.InfoHash == ih {
				got = tv.Poster
			}
		}
		if got != "" {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return ""
}

func TestDownloadsCarryPosterCapturedAtAcquire(t *testing.T) {
	cases := []struct {
		name string
		req  AcquireRequest
		url  string // the third-party cover the server fetches into its cache
	}{
		{"movie: TMDB poster", AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload}, "https://img/w342/dune.jpg"},
		{"series: TMDB poster", AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload}, "https://img/w342/dune.jpg"},
		{"album: Cover Art Archive", AcquireRequest{Kind: KindMusic, Title: "Группа крови", Artist: "Кино", ExtID: "rg-1", Mode: ModeDownload}, "https://caa/rg-1"},
		{"book: Open Library cover the client saw", AcquireRequest{Kind: KindBook, Title: "Мастер и Маргарита", Artist: "Булгаков", Poster: "https://covers/42", Mode: ModeDownload}, "https://covers/42"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			imgs := &fakeImages{}
			e.svc.o.Images = imgs
			e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Релиз 2024 1080p FLAC", "https://t/1", 1, 50)}}
			res, err := e.svc.Acquire(context.Background(), c.req)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := posterOf(t, e, res.InfoHash), cacheNameOf(c.url); got != want {
				t.Fatalf("poster %q, want %q (fetched %v)", got, want, imgs.asked())
			}
		})
	}
}

func TestPosterOfBookIgnoresForeignHosts(t *testing.T) {
	e := newEnv(t)
	imgs := &fakeImages{}
	e.svc.o.Images = imgs
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Булгаков - Мастер и Маргарита fb2", "https://t/1", 1, 50)}}
	// the client names a poster: only an Open Library cover may be fetched (no SSRF)
	if _, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Мастер и Маргарита", Artist: "Булгаков", Poster: "http://127.0.0.1:8420/api/me", Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := imgs.asked(); len(got) != 0 {
		t.Fatalf("fetched %v", got)
	}
}

func TestBackfillPostersForExistingRows(t *testing.T) {
	e := newEnv(t)
	imgs := &fakeImages{}
	e.svc.o.Images = imgs
	ctx := context.Background()
	rows := []acquisition{
		{InfoHash: "aa", Kind: KindMovie, TMDBID: 693134, Title: "Дюна"},
		{InfoHash: "bb", Kind: KindMusic, Title: "Группа крови", ExtID: "rg-9", Meta: acquisitionMeta{Artist: "Кино"}},
		// in the library already: its cached poster is reused, nothing is fetched
		{InfoHash: "cc", Kind: KindSeries, TMDBID: 126308, Title: "Сёгун"},
	}
	e.lib.items[126308] = library.Item{Kind: KindSeries, Poster: cacheNameOf("https://img/w342/lib.jpg")}
	for _, a := range rows {
		if err := e.svc.saveAcquisition(ctx, a, 0, "", false); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.backfillPosters(ctx)
	got := map[string]string{}
	for ih, a := range mustAcqs(t, e) {
		got[ih] = a.Poster
	}
	want := map[string]string{"aa": cacheNameOf("https://img/w342/dune.jpg"), "bb": cacheNameOf("https://caa/rg-9"), "cc": cacheNameOf("https://img/w342/lib.jpg")}
	for ih, w := range want {
		if got[ih] != w {
			t.Errorf("%s: poster %q, want %q", ih, got[ih], w)
		}
	}
	if len(imgs.asked()) != 2 {
		t.Errorf("fetched %v", imgs.asked())
	}
}

func mustAcqs(t *testing.T, e *env) map[string]acquisition {
	t.Helper()
	m, err := e.svc.acquisitions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return m
}
