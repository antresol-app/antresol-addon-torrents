package acquire

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// The release titles «Найти озвучку…» takes as voice-over releases: an
// audio-only release named so after its title part, its studio parsed.
func TestVoiceOverCandidateTitles(t *testing.T) {
	for _, c := range []struct {
		title  string
		ok     bool
		studio string
	}{
		{"Миллиарды / Billions [S03] (2018) Звуковая дорожка LostFilm", true, "LostFilm"},
		{"Миллиарды / Billions [S03] WEB-DL 1080p | LostFilm", false, ""},
		{"Billions S03 RUS Sound (Кубик в Кубе) [AC3 5.1]", true, "Кубик в Кубе"},
		{"Миллиарды / Billions [S01-03] Звуковые дорожки (HDRezka Studio)", true, "HDRezka Studio"},
		{"Billions S03 [AC3 5.1] NewStudio", true, "NewStudio"},
		{"Billions S03 1080p WEB-DL AC3 NewStudio", false, ""},
		{"Billions S03 Soundtrack", false, ""},
		{"Billions.S03.Rus.Audio.mka", true, ""},
		// a video release naming its audio: «Multi Audio», «RUS Sound» beside 1080p
		{"Billions S03 1080p WEB-DL Multi Audio", false, ""},
		{"Billions S03 2160p RUS Sound LostFilm", false, ""},
		{"Billions S03 RUS Sound LostFilm", true, "LostFilm"},
	} {
		ok, studio := voiceOverRelease(c.title)
		if ok != c.ok || ok && studio != c.studio {
			t.Errorf("%q: %v %q, want %v %q", c.title, ok, studio, c.ok, c.studio)
		}
	}
}

// voSeries adds the series «Миллиарды» (Billions) with season 3 episodes
// first..last on disk under dir (each a small file) and returns its id
// and the episodes' file paths by number.
func voSeries(t *testing.T, e *env, dir string, first, last int) (int64, map[int]string) {
	t.Helper()
	show := e.lib.addItem(library.Item{Kind: "series", Title: "Миллиарды", OriginalTitle: "Billions", Year: 2016, TmdbID: 62852})
	season := e.lib.addItem(library.Item{Kind: "season", ParentID: show, Title: "Сезон 3", SeasonNo: 3})
	paths := map[int]string{}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for n := first; n <= last; n++ {
		ep := e.lib.addItem(library.Item{Kind: "episode", ParentID: season, Title: fmt.Sprintf("E%d", n), SeasonNo: 3, EpisodeNo: n})
		p := filepath.Join(dir, fmt.Sprintf("Billions.S03E%02d.1080p.mkv", n))
		if err := os.WriteFile(p, []byte("video"), 0o644); err != nil {
			t.Fatal(err)
		}
		e.lib.addFile(library.File{ItemID: ep, Path: p, Size: 5, Mtime: 1, DurationMs: 3300000})
		paths[n] = p
	}
	return show, paths
}

func voRel(title, link string, seeders int, size int64) indexer.ScoredRelease {
	return indexer.ScoredRelease{Result: cardigann.Result{Title: title, Link: link, Seeders: seeders, Size: size}, SourceID: 1, SourceName: "RuTracker"}
}

// One search for the title; the voice-over releases of the title, the
// favourite studios first, then by seeders; video releases naming a studio
// apart (their files may hold separate tracks); other titles left out.
func TestFindVoiceOvers(t *testing.T) {
	e := newEnv(t)
	show, _ := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 10)
	e.idx.resp = indexer.SearchResponse{
		Releases: []indexer.ScoredRelease{
			voRel("Миллиарды / Billions [S03] WEB-DL 1080p | LostFilm", "https://t/video", 90, 40<<30),
			voRel("Billions S03 RUS Sound (Кубик в Кубе) [AC3 5.1]", "https://t/kk", 30, 3<<30),
			voRel("Миллиарды / Billions [S03] (2018) Звуковая дорожка LostFilm", "https://t/lf", 5, 2<<30),
			voRel("Billions S03 Звуковая дорожка HDRezka Studio", "https://t/hdr", 50, 2<<30),
		},
		Rejected: []indexer.ScoredRelease{
			voRel("Миллионы / Millions S01 Звуковая дорожка LostFilm", "https://t/other", 99, 1<<30),
		},
		Prefs: &indexer.Prefs{Studios: []string{"LostFilm", "Кубик в Кубе"}},
	}
	res, err := e.svc.FindVoiceOvers(context.Background(), show)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.idx.requests) != 1 {
		t.Fatalf("searches %d", len(e.idx.requests))
	}
	q := e.idx.requests[0]
	if q.Text != "Миллиарды" || !contains(q.AltTitles, "Billions") || q.Kind != KindSeries || q.Season != 0 || !q.VoiceOvers {
		t.Fatalf("request %+v", q)
	}
	var got []string
	for _, c := range res.Candidates {
		got = append(got, c.Studio)
		if c.ID == "" || c.Coverage.Total != 10 || c.Coverage.Have != 10 || !c.Coverage.Known || c.Coverage.Exact {
			t.Errorf("%s: %+v", c.Title, c)
		}
	}
	if fmt.Sprint(got) != "[LostFilm Кубик в Кубе HDRezka Studio]" {
		t.Fatalf("order %v", got)
	}
	if len(res.VideoReleases) != 1 || res.VideoReleases[0].Studio != "LostFilm" || !res.VideoReleases[0].Video {
		t.Fatalf("video releases %+v", res.VideoReleases)
	}
	if res.Candidates[0].Size != 2<<30 || res.Candidates[0].Seeders != 5 || res.Candidates[0].SourceName != "RuTracker" {
		t.Fatalf("first %+v", res.Candidates[0])
	}
}

// A film: coverage is the film; a title that is no film or series of the
// library is not found.
func TestFindVoiceOversFilmAndUnknown(t *testing.T) {
	e := newEnv(t)
	film := e.lib.addItem(library.Item{Kind: "movie", Title: "Дюна", OriginalTitle: "Dune", Year: 2021, TmdbID: 438631})
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{voRel("Дюна / Dune (2021) Звуковая дорожка Дубляж", "https://t/d", 3, 1<<30)}}
	res, err := e.svc.FindVoiceOvers(context.Background(), film)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) != 1 || !res.Candidates[0].Coverage.Film || res.Candidates[0].Coverage.Have != 1 {
		t.Fatalf("%+v", res.Candidates)
	}
	if e.idx.requests[0].Kind != KindMovie || e.idx.requests[0].Year != 2021 {
		t.Fatalf("request %+v", e.idx.requests[0])
	}
	if _, err := e.svc.FindVoiceOvers(context.Background(), 999); err == nil {
		t.Fatal("unknown item found")
	}
}

// TestVoiceOvers_MatchAlias: the library's alias titles of a
// series are searched and matched again, as the old build's
// item_titles query gave them: «Миллиарды» / «Billions» with the alias
// «Деньги» finds a voice-over release named only «Деньги».
func TestVoiceOvers_MatchAlias(t *testing.T) {
	e := newEnv(t)
	show, _ := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 2)
	e.lib.ItemAliases = map[int64][]string{show: {"Деньги"}}
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{
		voRel("Деньги S03 Звуковая дорожка LostFilm", "https://t/money", 7, 1<<30),
	}}
	res, err := e.svc.FindVoiceOvers(context.Background(), show)
	if err != nil {
		t.Fatal(err)
	}
	if q := e.idx.requests[0]; !contains(q.AltTitles, "Деньги") || !contains(q.AltTitles, "Billions") {
		t.Fatalf("request %+v", q)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].Studio != "LostFilm" {
		t.Fatalf("candidates %+v", res.Candidates)
	}
}
