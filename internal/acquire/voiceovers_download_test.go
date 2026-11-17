package acquire

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/extaudio"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

// voFind runs the search with the releases and returns the candidate (or
// video release) of link.
func voFind(t *testing.T, e *env, item int64, link string, rels ...indexer.ScoredRelease) VoiceOverCandidate {
	t.Helper()
	e.idx.resp = indexer.SearchResponse{Releases: rels}
	res, err := e.svc.FindVoiceOvers(context.Background(), item)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append(res.Candidates, res.VideoReleases...) {
		if r := e.svc.voSearch.m[item].rels[c.ID]; r.Link == link {
			return c
		}
	}
	t.Fatalf("no candidate of %s in %+v", link, res)
	return VoiceOverCandidate{}
}

func seasonPack(format string, from, to int) []string {
	var out []string
	for n := from; n <= to; n++ {
		out = append(out, fmt.Sprintf(format, n))
	}
	return out
}

// A season pack of tracks of episodes 1–12 while the server has 1–10: only
// those ten download, each meant for its episode's file.
func TestDownloadVoiceOverSelectsEpisodes(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	show, eps := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 10)
	const name = "Billions.S03.RUS.Sound.LostFilm"
	files := append([]string{"Описание.txt"}, seasonPack("Billions.S03E%02d.LostFilm.ac3", 1, 12)...)
	mf := planRelease(t, e, "https://t/lf", name, files...)
	e.eng.nextIH = metafileIH(t, mf)
	c := voFind(t, e, show, "https://t/lf", voRel("Миллиарды / Billions [S03] Звуковая дорожка LostFilm", "https://t/lf", 5, 1<<30))
	res, err := e.svc.DownloadVoiceOver(ctx, VoiceOverRequest{ItemID: show, ID: c.ID, UserID: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.eng.added) != 1 {
		t.Fatalf("added %d", len(e.eng.added))
	}
	opt := e.eng.added[0]
	if fmt.Sprint(opt.Files) != "[1 2 3 4 5 6 7 8 9 10]" || opt.SaveDir != filepath.Join(e.root, "torrents", "series") || opt.Mode != ModeDownload || opt.AddedBy != 2 {
		t.Fatalf("options %+v", opt)
	}
	if res.InfoHash != e.eng.nextIH || len(res.Episodes) != 10 || res.Episodes[0].Season != 3 || res.Episodes[0].Episode != 1 {
		t.Fatalf("result %+v", res)
	}
	a, err := e.svc.acquisition(ctx, res.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	vo := a.Meta.VoiceOver
	if a.Kind != KindVoiceOver || vo == nil || vo.ItemID != show || vo.Studio != "LostFilm" || len(vo.Picks) != 10 || vo.Picks[4].Path != eps[4] {
		t.Fatalf("acquisition %+v %+v", a, vo)
	}
	// the same release again: no second torrent, nothing new to select
	again, err := e.svc.DownloadVoiceOver(ctx, VoiceOverRequest{ItemID: show, ID: c.ID, UserID: 2})
	if err != nil || !again.Existing || len(e.eng.added) != 1 || len(e.eng.grown) != 0 {
		t.Fatalf("again %+v %v, added %d grown %v", again, err, len(e.eng.added), e.eng.grown)
	}
}

// The same release after the server got episode 11: that track joins the
// download (one torrent, a grown selection).
func TestDownloadVoiceOverGrows(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	show, _ := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 2)
	mf := planRelease(t, e, "https://t/lf", "Billions.S03.RUS.Sound", seasonPack("Billions.S03E%02d.ac3", 1, 3)...)
	e.eng.nextIH = metafileIH(t, mf)
	c := voFind(t, e, show, "https://t/lf", voRel("Billions S03 RUS Sound LostFilm", "https://t/lf", 5, 1<<30))
	if _, err := e.svc.DownloadVoiceOver(ctx, VoiceOverRequest{ItemID: show, ID: c.ID}); err != nil {
		t.Fatal(err)
	}
	e.eng.handles[e.eng.nextIH].files = torrentFiles(t, filepath.Join(e.root, "torrents/series"),
		seasonPack("Billions.S03.RUS.Sound/Billions.S03E%02d.ac3", 1, 3)...)
	// E03 arrives on the server
	season := show + 1 // voSeries' season
	ep := e.lib.addItem(library.Item{Kind: "episode", ParentID: season, Title: "E3", SeasonNo: 3, EpisodeNo: 3})
	e.lib.addFile(library.File{ItemID: ep, Path: filepath.Join(e.root, "media/series/Billions/Season 3/Billions.S03E03.mkv"), Size: 5, Mtime: 1})
	c = voFind(t, e, show, "https://t/lf", voRel("Billions S03 RUS Sound LostFilm", "https://t/lf", 5, 1<<30))
	res, err := e.svc.DownloadVoiceOver(ctx, VoiceOverRequest{ItemID: show, ID: c.ID})
	if err != nil || !res.Existing || len(e.eng.added) != 1 || fmt.Sprint(e.eng.grown) != "[[2]]" || len(res.Episodes) != 3 {
		t.Fatalf("%+v %v, grown %v", res, err, e.eng.grown)
	}
	a, _ := e.svc.acquisition(ctx, res.InfoHash)
	if len(a.Meta.VoiceOver.Picks) != 3 || a.OrganizedAt.Valid {
		t.Fatalf("meta %+v", a.Meta.VoiceOver)
	}
}

// A video release with a «RUS Sound» folder: only its audio files.
func TestDownloadVoiceOverFromVideoRelease(t *testing.T) {
	e := newEnv(t)
	show, _ := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 2)
	const name = "Billions.S03.1080p.WEB-DL"
	files := []string{"Billions.S03E01.1080p.mkv", "Billions.S03E02.1080p.mkv", "RUS Sound/LostFilm/Billions.S03E01.mka", "RUS Sound/LostFilm/Billions.S03E02.mka"}
	mf := planRelease(t, e, "https://t/v", name, files...)
	e.eng.nextIH = metafileIH(t, mf)
	c := voFind(t, e, show, "https://t/v", voRel("Миллиарды / Billions [S03] WEB-DL 1080p | LostFilm", "https://t/v", 50, 20<<30))
	if !c.Video {
		t.Fatalf("%+v", c)
	}
	prev, err := e.svc.PreviewVoiceOver(context.Background(), show, c.ID)
	if err != nil || prev.Coverage.Have != 2 || prev.Coverage.Total != 2 || !prev.Coverage.Exact || len(prev.Episodes) != 2 {
		t.Fatalf("preview %+v %v", prev, err)
	}
	if _, err := e.svc.DownloadVoiceOver(context.Background(), VoiceOverRequest{ItemID: show, ID: c.ID}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(e.eng.added[0].Files) != "[2 3]" {
		t.Fatalf("files %v", e.eng.added[0].Files)
	}
	// the file list was fetched once for the preview and the download
	if len(e.idx.downloads) != 1 {
		t.Fatalf("downloads %v", e.idx.downloads)
	}
}

// File lists of at most five candidates per search.
func TestPreviewVoiceOverLimit(t *testing.T) {
	e := newEnv(t)
	show, _ := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 1)
	var rels []indexer.ScoredRelease
	for i := range 6 {
		link := fmt.Sprintf("https://t/%d", i)
		planRelease(t, e, link, fmt.Sprintf("Billions.S03.Sound.%d", i), "Billions.S03E01.ac3")
		rels = append(rels, voRel(fmt.Sprintf("Billions S03 RUS Sound %d", i), link, 5, 1<<30))
	}
	e.idx.resp = indexer.SearchResponse{Releases: rels}
	res, err := e.svc.FindVoiceOvers(context.Background(), show)
	if err != nil || len(res.Candidates) != 6 {
		t.Fatalf("%+v %v", res, err)
	}
	for i, c := range res.Candidates {
		_, err := e.svc.PreviewVoiceOver(context.Background(), show, c.ID)
		if i < voMaxLists && err != nil || i >= voMaxLists && err == nil {
			t.Fatalf("preview %d: %v", i, err)
		}
	}
	if len(e.idx.downloads) != voMaxLists {
		t.Fatalf("downloads %v", e.idx.downloads)
	}
}

// Tracks named «01.ac3»: matched by number only inside a folder naming the
// season — exactly one: a root or folder of a season range or several
// seasons names none; otherwise nothing is selected and nothing added.
func TestDownloadVoiceOverNumberedFiles(t *testing.T) {
	for _, c := range []struct {
		name  string
		root  string
		files []string
		want  string // selected indexes, or "" for the refusal
	}{
		{"season folder", "Billions RUS Sound", []string{"Сезон 3/01.ac3", "Сезон 3/02.ac3"}, "[0 1]"},
		{"season root", "Billions S03 RUS Sound", []string{"01.ac3", "02.ac3"}, "[0 1]"},
		{"no season", "Billions RUS Sound", []string{"LostFilm/01.ac3", "LostFilm/02.ac3"}, ""},
		{"season range root", "Миллиарды S01-S03 Звуковые дорожки LostFilm", []string{"1/01.ac3", "2/01.ac3", "3/01.ac3", "3/02.ac3"}, ""},
		{"seasons folder", "Billions RUS Sound", []string{"1-3 сезон/01.ac3", "1-3 сезон/02.ac3"}, ""},
		{"one season inside a range root", "Billions S01-S03 RUS Sound", []string{"Season 3/01.ac3", "Season 3/02.ac3"}, "[0 1]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			show, _ := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 2)
			mf := planRelease(t, e, "https://t/n", c.root, c.files...)
			e.eng.nextIH = metafileIH(t, mf)
			cand := voFind(t, e, show, "https://t/n", voRel("Billions (2016) RUS Sound LostFilm", "https://t/n", 5, 1<<30))
			_, err := e.svc.DownloadVoiceOver(context.Background(), VoiceOverRequest{ItemID: show, ID: cand.ID})
			if c.want == "" {
				if msg, _ := UserMessage(err); msg != msgVoiceOverNoFiles || len(e.eng.added) != 0 {
					t.Fatalf("err %v, added %d", err, len(e.eng.added))
				}
				return
			}
			if err != nil || fmt.Sprint(e.eng.added[0].Files) != c.want {
				t.Fatalf("err %v added %+v", err, e.eng.added)
			}
		})
	}
}

// Once complete, each track is linked beside its episode as
// «<stem>.ru.LostFilm.ac3», where the library's player finds it.
func TestOrganizeVoiceOver(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	show, eps := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 2)
	const name = "Billions.S03.RUS.Sound.LostFilm"
	mf := planRelease(t, e, "https://t/lf", name, seasonPack("Billions.S03E%02d.LostFilm.ac3", 1, 3)...)
	e.eng.nextIH = metafileIH(t, mf)
	e.eng.nextFiles = torrentFiles(t, filepath.Join(e.root, "torrents/series"), seasonPack(name+"/Billions.S03E%02d.LostFilm.ac3", 1, 3)...)
	var placed []PlacedVoiceOver
	e.svc.o.VoiceOverPlaced = func(_ context.Context, p PlacedVoiceOver) error { placed = append(placed, p); return nil }
	c := voFind(t, e, show, "https://t/lf", voRel("Миллиарды / Billions [S03] Звуковая дорожка LostFilm", "https://t/lf", 5, 1<<30))
	res, err := e.svc.DownloadVoiceOver(ctx, VoiceOverRequest{ItemID: show, ID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.organize(ctx, res.InfoHash, false, false)
	for n := 1; n <= 2; n++ {
		want := strings.TrimSuffix(eps[n], ".mkv") + ".ru.LostFilm.ac3"
		if _, err := os.Stat(want); err != nil {
			t.Fatalf("episode %d: %v", n, err)
		}
		if tr := extaudio.Find(eps[n]); len(tr) != 1 || tr[0].Studio != "LostFilm" || tr[0].Lang != "ru" {
			t.Fatalf("episode %d tracks %+v", n, tr)
		}
	}
	if len(placed) != 2 || placed[0].FileID == 0 || placed[0].Path != strings.TrimSuffix(eps[1], ".mkv")+".ru.LostFilm.ac3" || placed[0].OrigLang != "en" {
		t.Fatalf("placed %+v", placed)
	}
	a, _ := e.svc.acquisition(ctx, res.InfoHash)
	if !a.OrganizedAt.Valid || a.Error != "" {
		t.Fatalf("acquisition %+v", a)
	}
	// organized again (a grown selection): no second copy
	if _, err := e.svc.o.DB.Exec(`UPDATE acquisitions SET organized_at=NULL`); err != nil {
		t.Fatal(err)
	}
	e.svc.organize(ctx, res.InfoHash, false, false)
	if tr := extaudio.Find(eps[1]); len(tr) != 1 || len(placed) != 2 {
		t.Fatalf("tracks after a second organize %+v, placed %d", tr, len(placed))
	}
	ds, err := e.svc.VoiceOverDownloads(ctx, show)
	if err != nil || len(ds) != 1 || len(ds[0].Episodes) != 2 || ds[0].Episodes[0].State != VoicePlaced || ds[0].Studio != "LostFilm" {
		t.Fatalf("downloads %+v %v", ds, err)
	}
}
