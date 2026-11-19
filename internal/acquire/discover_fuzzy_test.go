package acquire

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// tmdbLike answers TMDB's way: a query matches a title when its words are
// the title's (the last one by its beginning too). No typo is forgiven.
type tmdbLike struct {
	fakeMeta
	mu     sync.Mutex
	movies []tmdb.Result
	shows  []tmdb.Result
	asked  []string
	langs  []string
}

func tmdbMatch(q, title string) bool {
	qw := strings.Fields(release.Normalize(q))
	tw := strings.Fields(release.Normalize(title))
	for i, w := range qw {
		hit := false
		for _, t := range tw {
			if t == w || (i == len(qw)-1 && strings.HasPrefix(t, w)) {
				hit = true
			}
		}
		if !hit {
			return false
		}
	}
	return len(qw) > 0
}

func (f *tmdbLike) find(ctx context.Context, q string, from []tmdb.Result) []tmdb.Result {
	f.mu.Lock()
	f.asked = append(f.asked, q)
	f.langs = append(f.langs, tmdb.ContextLanguage(ctx))
	f.mu.Unlock()
	var out []tmdb.Result
	for _, r := range from {
		if tmdbMatch(q, r.Title) || tmdbMatch(q, r.OriginalTitle) {
			out = append(out, r)
		}
	}
	return out
}
func (f *tmdbLike) SearchMovie(ctx context.Context, q string, _ int) ([]tmdb.Result, error) {
	return f.find(ctx, q, f.movies), nil
}
func (f *tmdbLike) SearchTV(ctx context.Context, q string, _ int) ([]tmdb.Result, error) {
	return f.find(ctx, q, f.shows), nil
}

var gentlemen = &tmdbLike{
	movies: []tmdb.Result{
		{ID: 522627, Title: "Джентльмены", OriginalTitle: "The Gentlemen", Year: 2020},
		{ID: 20871, Title: "Джентльмены удачи", OriginalTitle: "Джентльмены удачи", Year: 1971},
		{ID: 2623, Title: "Офицер и джентльмен", OriginalTitle: "An Officer and a Gentleman", Year: 1982},
		{ID: 1, Title: "Джентри", OriginalTitle: "Gentry", Year: 2001},
	},
	shows: []tmdb.Result{
		{ID: 236235, Title: "Джентльмены", OriginalTitle: "The Gentlemen", Year: 2024},
		{ID: 2349, Title: "Лига джентльменов", OriginalTitle: "The League of Gentlemen", Year: 1999},
	},
}

func TestDiscoverForgivesATypo(t *testing.T) {
	e := newEnv(t)
	meta := &tmdbLike{movies: gentlemen.movies, shows: gentlemen.shows}
	e.svc.o.Metadata = meta
	ctx := tmdb.WithLanguage(context.Background(), "en-US")
	cs, err := e.svc.Discover(ctx, "Джентельмены")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range cs {
		if !c.Approx {
			t.Fatalf("not marked approximate: %+v", c)
		}
		got = append(got, c.Kind+":"+c.Title)
	}
	// the film first (fewest edits), the series, then the titles with a word two edits away;
	// not «Джентри» nor «Лига джентльменов» (three)
	want := "movie:Джентльмены series:Джентльмены movie:Джентльмены удачи movie:Офицер и джентльмен"
	if strings.Join(got, " ") != want {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	// the second look asked the word's beginning, in Russian (a Cyrillic query)
	if !slices.Contains(meta.asked, "джент") || meta.langs[len(meta.langs)-1] != "ru-RU" {
		t.Fatalf("asked %v in %v", meta.asked, meta.langs)
	}
}

func TestDiscoverExactNeedsNoSecondLook(t *testing.T) {
	e := newEnv(t)
	meta := &tmdbLike{movies: gentlemen.movies, shows: gentlemen.shows}
	e.svc.o.Metadata = meta
	cs, err := e.svc.Discover(context.Background(), "The Gentlemen")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 || cs[0].Kind != KindMovie || cs[0].TMDBID != 522627 || cs[1].TMDBID != 236235 || cs[0].Approx {
		t.Fatalf("%+v", cs)
	}
	if len(meta.asked) != 2 {
		t.Fatalf("asked again: %v", meta.asked)
	}
}

func TestDiscoverNoNeighbours(t *testing.T) {
	e := newEnv(t)
	e.svc.o.Metadata = &tmdbLike{movies: gentlemen.movies, shows: gentlemen.shows}
	for _, q := range []string{"Мадагаскар", "кот"} {
		cs, err := e.svc.Discover(context.Background(), q)
		if err != nil || len(cs) != 0 {
			t.Fatalf("%q: %+v %v", q, cs, err)
		}
	}
}

func TestFuzzyPrefixes(t *testing.T) {
	for q, want := range map[string]string{
		"Джентельмены": "джент",
		"Гари Потер":   "пот гар",
		"Брат 2":       "бра",
		"Он и я":       "",
		"Interstelar":  "inter",
	} {
		got := strings.Join(fuzzyPrefixes(strings.Fields(release.Normalize(q))), " ")
		if got != want {
			t.Errorf("%q: %q, want %q", q, got, want)
		}
	}
}
