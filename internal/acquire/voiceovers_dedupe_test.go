package acquire

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/extaudio"
)

// voPack makes the source answer link with a pack of E01–E02 LostFilm
// tracks whose files lie in the torrents folder, and runs the download up
// to its organize.
func voPack(t *testing.T, e *env, show int64, link, content string) string {
	t.Helper()
	const name = "Billions.S03.RUS.Sound.LostFilm"
	mf := planRelease(t, e, link, name, seasonPack("Billions.S03E%02d.LostFilm.ac3", 1, 2)...)
	e.eng.nextIH = metafileIH(t, mf)
	dir := filepath.Join(e.root, "torrents/series", strings.TrimPrefix(link, "https://t/"))
	files := torrentFiles(t, dir, seasonPack(name+"/Billions.S03E%02d.LostFilm.ac3", 1, 2)...)
	for _, f := range files {
		if err := os.WriteFile(f.FullPath, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.eng.nextFiles = files
	c := voFind(t, e, show, link, voRel("Миллиарды / Billions [S03] Звуковая дорожка LostFilm", link, 5, 1<<30))
	res, err := e.svc.DownloadVoiceOver(context.Background(), VoiceOverRequest{ItemID: show, ID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.organize(context.Background(), res.InfoHash, false, false)
	return res.InfoHash
}

func lostfilmTracks(video string) []string {
	var out []string
	for _, tr := range extaudio.Find(video) {
		if tr.Studio == "LostFilm" {
			out = append(out, filepath.Base(tr.Path))
		}
	}
	return out
}

// A LostFilm track already beside the episode (the video release brought
// it, stage A) keeps that episode from getting a second one; the sheet
// says it is there. Unless that one is a misfit: then the download's
// track goes beside it.
func TestOrganizeVoiceOverSkipsStudioThere(t *testing.T) {
	e := newEnv(t)
	show, eps := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 2)
	had := strings.TrimSuffix(eps[1], ".mkv") + ".ru.LostFilm.mka"
	if err := os.WriteFile(had, []byte("stage A"), 0o644); err != nil {
		t.Fatal(err)
	}
	var placed []string
	e.svc.SetVoiceOverFit(func(_ context.Context, p PlacedVoiceOver) error { placed = append(placed, p.Path); return nil }, nil)
	voPack(t, e, show, "https://t/a", "audio")
	if got := lostfilmTracks(eps[1]); len(got) != 1 || got[0] != filepath.Base(had) {
		t.Fatalf("E01 tracks %v", got)
	}
	if got := lostfilmTracks(eps[2]); len(got) != 1 {
		t.Fatalf("E02 tracks %v", got)
	}
	if len(placed) != 1 {
		t.Fatalf("placed %v", placed)
	}
	ds, err := e.svc.VoiceOverDownloads(context.Background(), show)
	if err != nil || len(ds) != 1 || ds[0].Episodes[0].State != VoiceHad || ds[0].Episodes[1].State != VoicePlaced {
		t.Fatalf("downloads %+v %v", ds, err)
	}

	// the track there is of another cut: the download's goes beside it
	e2 := newEnv(t)
	show2, eps2 := voSeries(t, e2, filepath.Join(e2.root, "media/series/Billions/Season 3"), 1, 1)
	misfit := strings.TrimSuffix(eps2[1], ".mkv") + ".ru.LostFilm.mka"
	if err := os.WriteFile(misfit, []byte("other cut"), 0o644); err != nil {
		t.Fatal(err)
	}
	e2.svc.SetVoiceOverFit(func(context.Context, PlacedVoiceOver) error { return nil },
		func(_ context.Context, p string) (string, float64, float64, bool) {
			if p == misfit {
				return VoiceMisfit, 0, 0, true
			}
			return "", 0, 0, false
		})
	voPack(t, e2, show2, "https://t/b", "audio")
	if got := lostfilmTracks(eps2[1]); len(got) != 2 {
		t.Fatalf("beside a misfit: %v", got)
	}

	// nor one never offered: unmeasured, or a failed fit given up; one
	// still being fitted does hold its place
	for _, c := range []struct {
		state string
		want  int
	}{{VoiceUnmeasured, 2}, {VoicePlaced, 1}, {VoiceFitted, 1}} {
		e3 := newEnv(t)
		show3, eps3 := voSeries(t, e3, filepath.Join(e3.root, "media/series/Billions/Season 3"), 1, 1)
		there := strings.TrimSuffix(eps3[1], ".mkv") + ".ru.LostFilm.mka"
		if err := os.WriteFile(there, []byte("earlier download"), 0o644); err != nil {
			t.Fatal(err)
		}
		e3.svc.SetVoiceOverFit(func(context.Context, PlacedVoiceOver) error { return nil },
			func(_ context.Context, p string) (string, float64, float64, bool) {
				if p == there {
					return c.state, 0, 0, true
				}
				return "", 0, 0, false
			})
		voPack(t, e3, show3, "https://t/c", "audio")
		if got := lostfilmTracks(eps3[1]); len(got) != c.want {
			t.Fatalf("beside a %s one: %v", c.state, got)
		}
	}
}

// The same release downloaded again after it was removed in «Загрузки»
// (new files, the links still in the library): no second copy, the links
// are reused.
func TestOrganizeVoiceOverReusesLinks(t *testing.T) {
	e := newEnv(t)
	show, eps := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 2)
	fits := map[string]bool{}
	e.svc.SetVoiceOverFit(func(_ context.Context, p PlacedVoiceOver) error { fits[p.Path] = true; return nil },
		func(_ context.Context, p string) (string, float64, float64, bool) {
			if fits[p] {
				return VoiceFitted, 200, 1, true
			}
			return "", 0, 0, false
		})
	ih := voPack(t, e, show, "https://t/a", "audio")
	if err := e.svc.Remove(context.Background(), ih, true); err != nil {
		t.Fatal(err)
	}
	e.eng.nextIH = "" // planRelease below sets it again
	voPack(t, e, show, "https://t/a2", "audio")
	for n := 1; n <= 2; n++ {
		if got := lostfilmTracks(eps[n]); len(got) != 1 {
			t.Fatalf("E%02d tracks %v", n, got)
		}
	}
	if len(fits) != 2 {
		t.Fatalf("queued %v", fits)
	}
	ds, err := e.svc.VoiceOverDownloads(context.Background(), show)
	if err != nil || len(ds) != 1 || ds[0].Episodes[0].State != VoiceFitted {
		t.Fatalf("downloads %+v %v", ds, err)
	}
}
