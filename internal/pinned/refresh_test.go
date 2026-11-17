package pinned

import (
	"context"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

const dl = "https://tracker.test/dl/1"

func pinTwo(t *testing.T, e *env) Pin {
	t.Helper()
	e.idx.downloads[dl] = cardigann.DownloadResult{Torrent: twoEpisodes(t)}
	p, err := e.svc.PinRelease(context.Background(), e.title.ID, "3", dl, "", releaseName, by(e.users[0]))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRefreshSupersetReplacesInEngine(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	p := pinTwo(t, e)
	e.eng.has[p.InfoHash] = true
	three := metafile(t, releaseName, "Show.S01E01.1080p.mkv", "Show.S01E02.1080p.mkv", "Show.S01E03.1080p.mkv")
	e.idx.downloads[dl] = cardigann.DownloadResult{Torrent: three}
	e.now = e.now.Add(7 * time.Hour)
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	changed, err := e.svc.Refresh(rctx, p)
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if len(e.eng.replaced) != 1 {
		t.Fatalf("replaced %+v", e.eng.replaced)
	}
	r := e.eng.replaced[0]
	if r.old != p.InfoHash || r.newIH != ihOf(t, three) || r.cancelled || r.keepsNew {
		t.Fatalf("replace %+v", r)
	}
	got, _, _ := e.svc.ByTitle(ctx, e.title.ID)
	if got.InfoHash != ihOf(t, three) || len(got.Files) != 3 || got.LastError != "" || !got.LastChecked.Equal(e.now) || !got.PinnedAt.Equal(p.PinnedAt) {
		t.Fatalf("pin %+v", got)
	}

	// the same release again: only last_checked moves
	e.now = e.now.Add(7 * time.Hour)
	if changed, err := e.svc.Refresh(ctx, got); err != nil || changed {
		t.Fatal(changed, err)
	}
	again, _, _ := e.svc.ByTitle(ctx, e.title.ID)
	if again.InfoHash != got.InfoHash || !again.LastChecked.Equal(e.now) || len(e.eng.replaced) != 1 {
		t.Fatalf("pin %+v", again)
	}

	// not in the engine: the row moves on, nothing to replace
	delete(e.eng.has, again.InfoHash)
	four := metafile(t, releaseName, "Show.S01E01.1080p.mkv", "Show.S01E02.1080p.mkv", "Show.S01E03.1080p.mkv", "Show.S01E04.1080p.mkv")
	e.idx.downloads[dl] = cardigann.DownloadResult{Torrent: four}
	if changed, err := e.svc.Refresh(ctx, again); err != nil || !changed || len(e.eng.replaced) != 1 {
		t.Fatal(changed, err, e.eng.replaced)
	}
}

func TestRefreshKeepsPinOnBadOrSmaller(t *testing.T) {
	for name, body := range map[string]func(t *testing.T) cardigann.DownloadResult{
		"html": func(*testing.T) cardigann.DownloadResult {
			return cardigann.DownloadResult{Torrent: []byte("<!DOCTYPE html><html>503</html>")}
		},
		"login": func(*testing.T) cardigann.DownloadResult {
			return cardigann.DownloadResult{Torrent: []byte(`<form action="login.php"><input name="login_password"></form>`)}
		},
		"subset": func(t *testing.T) cardigann.DownloadResult {
			return cardigann.DownloadResult{Torrent: metafile(t, releaseName, "Show.S01E01.1080p.mkv")}
		},
		"empty": func(*testing.T) cardigann.DownloadResult { return cardigann.DownloadResult{} },
	} {
		t.Run(name, func(t *testing.T) {
			e, ctx := newEnv(t), context.Background()
			p := pinTwo(t, e)
			e.eng.has[p.InfoHash] = true
			e.idx.downloads[dl] = body(t)
			e.now = e.now.Add(7 * time.Hour)
			changed, err := e.svc.Refresh(ctx, p)
			if changed || err == nil {
				t.Fatal(changed, err)
			}
			got, _, _ := e.svc.ByTitle(ctx, e.title.ID)
			if got.InfoHash != p.InfoHash || len(got.Files) != 2 || got.LastError == "" || !got.LastChecked.Equal(e.now) || len(e.eng.replaced) != 0 {
				t.Fatalf("pin %+v, replaced %v", got, e.eng.replaced)
			}
		})
	}
}

func TestRefreshMagnetPinNotFetched(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	mf := twoEpisodes(t)
	mag := "magnet:?xt=urn:btih:" + ihOf(t, mf)
	e.eng.magnets[mag] = mf
	p, err := e.svc.PinOwn(ctx, e.title.ID, mag, nil, by(e.users[0]))
	if err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(7 * time.Hour)
	if changed, err := e.svc.Refresh(ctx, p); err != nil || changed || len(e.idx.calls) != 0 {
		t.Fatal(changed, err, e.idx.calls)
	}
	got, _, _ := e.svc.ByTitle(ctx, e.title.ID)
	if !got.LastChecked.Equal(e.now) {
		t.Fatalf("pin %+v", got)
	}
}

func TestAcceptSuggestion(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	p := pinTwo(t, e)
	e.eng.has[p.InfoHash] = true
	if _, err := e.svc.AcceptSuggestion(ctx, e.title.ID); err == nil {
		t.Fatal("accepted no suggestion")
	}
	three := metafile(t, "Show.S01.2160p", "Show.S01E01.mkv", "Show.S01E02.mkv", "Show.S01E03.mkv")
	mag := "magnet:?xt=urn:btih:" + ihOf(t, three)
	e.eng.magnets[mag] = three
	if err := e.svc.SetSuggestion(ctx, e.title.ID, &Suggestion{Link: mag, ReleaseTitle: "Show.S01.2160p", InfoHash: ihOf(t, three), Episodes: 3, Found: e.now}); err != nil {
		t.Fatal(err)
	}
	withSug, _, _ := e.svc.ByTitle(ctx, e.title.ID)
	if withSug.Suggestion == nil || withSug.Suggestion.Episodes != 3 {
		t.Fatalf("suggestion %+v", withSug.Suggestion)
	}
	got, err := e.svc.AcceptSuggestion(ctx, e.title.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.InfoHash != ihOf(t, three) || got.Suggestion != nil || got.Link != mag || got.SourceID != "" || got.Quality != "2160p" || len(e.eng.replaced) != 1 || e.eng.replaced[0].cancelled {
		t.Fatalf("pin %+v, replaced %+v", got, e.eng.replaced)
	}
}

// A suggestion is fetched again when accepted: a topic re-uploaded since
// with fewer episodes is refused as Refresh refuses it (the pin and its
// torrent stay, last_error says why), so SweepStale never deletes the
// episodes it dropped.
func TestAcceptSuggestionRefusesFewerEpisodes(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	p := pinTwo(t, e)
	e.eng.has[p.InfoHash] = true
	one := metafile(t, "Show.S01.2160p", "Show.S01E01.mkv")
	mag := "magnet:?xt=urn:btih:" + ihOf(t, one)
	e.eng.magnets[mag] = one
	if err := e.svc.SetSuggestion(ctx, e.title.ID, &Suggestion{Link: mag, ReleaseTitle: "Show.S01.2160p", InfoHash: ihOf(t, one), Episodes: 3, Found: e.now}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AcceptSuggestion(ctx, e.title.ID); err == nil {
		t.Fatal("accepted a release with fewer episodes")
	}
	got, _, _ := e.svc.ByTitle(ctx, e.title.ID)
	if got.InfoHash != p.InfoHash || len(got.Files) != 2 || got.LastError == "" || len(e.eng.replaced) != 0 {
		t.Fatalf("pin %+v, replaced %+v", got, e.eng.replaced)
	}
}

// sized builds a v1 metafile named name with files of the given sizes.
func sized(t *testing.T, name string, files ...metainfo.FileInfo) []byte {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: 16384, Pieces: make([]byte, 20), Files: files}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mf, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: ib})
	if err != nil {
		t.Fatal(err)
	}
	return mf
}

func file(p string, n int64) metainfo.FileInfo {
	return metainfo.FileInfo{Path: []string{p}, Length: n}
}

func TestRefreshMoviePinKeepsItsFiles(t *testing.T) {
	const name = "Film.2020.1080p"
	pin := sized(t, name, file("Film.2020.1080p.mkv", 50000), file("Film.rus.srt", 300))
	for _, c := range []struct {
		name    string
		mf      []byte
		replace bool
	}{
		{"one small file", sized(t, name, file("Film.2020.1080p.mkv", 100)), false},
		{"video smaller", sized(t, name, file("Film.2020.1080p.mkv", 100), file("Film.rus.srt", 300)), false},
		{"renamed", sized(t, name, file("Film.2020.2160p.mkv", 90000), file("Film.rus.srt", 300)), false},
		{"all kept, a track more", sized(t, name, file("Film.2020.1080p.mkv", 50000), file("Film.rus.srt", 300), file("Film.eng.srt", 200)), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, ctx := newEnv(t), context.Background()
			e.idx.downloads[dl] = cardigann.DownloadResult{Torrent: pin}
			p, err := e.svc.PinRelease(ctx, e.title.ID, "3", dl, "", name, by(e.users[0]))
			if err != nil {
				t.Fatal(err)
			}
			if len(EpisodesOf(p.Files, 1)) != 0 {
				t.Fatal("the film pin names episodes")
			}
			e.eng.has[p.InfoHash] = true
			e.idx.downloads[dl] = cardigann.DownloadResult{Torrent: c.mf}
			changed, err := e.svc.Refresh(ctx, p)
			got, _, _ := e.svc.ByTitle(ctx, e.title.ID)
			if c.replace {
				if err != nil || !changed || len(e.eng.replaced) != 1 || got.InfoHash != ihOf(t, c.mf) {
					t.Fatal(changed, err, e.eng.replaced, got)
				}
				return
			}
			if changed || err == nil || len(e.eng.replaced) != 0 || got.InfoHash != p.InfoHash || got.LastError == "" {
				t.Fatalf("changed %v err %v replaced %v pin %+v", changed, err, e.eng.replaced, got)
			}
		})
	}
}

func TestPinReleaseForeignLinkRefused(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.idx.downloads["https://elsewhere.test/dl/1"] = cardigann.DownloadResult{Torrent: twoEpisodes(t)}
	if _, err := e.svc.PinRelease(ctx, e.title.ID, "3", "https://elsewhere.test/dl/1", "", "X", by(e.users[0])); err == nil {
		t.Fatal("a link off the source's site pinned")
	}
	if len(e.idx.calls) != 0 {
		t.Fatalf("downloaded %v", e.idx.calls)
	}
	if _, ok, _ := e.svc.ByTitle(ctx, e.title.ID); ok {
		t.Fatal("pinned anyway")
	}
}
