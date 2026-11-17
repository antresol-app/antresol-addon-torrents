package acquire

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// The original language of a library file's title, asked again by the fit
// when the download stored none: an episode's series (TMDB), a film; none
// without a TMDB id.
func TestOriginalLanguageOf(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	_, eps := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 1)
	ep := e.lib.fileID(eps[1])
	if got := e.svc.OriginalLanguageOf(ctx, ep); got != "en" {
		t.Fatalf("episode: %q", got)
	}
	film := e.lib.addItem(library.Item{Kind: "movie", Title: "Без TMDB"})
	f := e.lib.addFile(library.File{ItemID: film, Path: "/m/x.mkv"})
	if got := e.svc.OriginalLanguageOf(ctx, f); got != "" {
		t.Fatalf("film without TMDB: %q", got)
	}
}

// A voice-over download nobody seeds is no title to switch: the
// automatic switch leaves it alone (no attempt, no note).
func TestAutoReplaceSkipsVoiceOvers(t *testing.T) {
	shortWindow(t)
	e := newEnv(t)
	p := withProbe(e)
	show, _ := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 1)
	mf := planRelease(t, e, "https://t/lf", "Billions.S03.RUS.Sound", "Billions.S03E01.ac3")
	ih := metafileIH(t, mf)
	e.eng.nextIH = ih
	c := voFind(t, e, show, "https://t/lf", voRel("Billions S03 RUS Sound LostFilm", "https://t/lf", 0, 1<<20))
	if _, err := e.svc.DownloadVoiceOver(context.Background(), VoiceOverRequest{ItemID: show, ID: c.ID}); err != nil {
		t.Fatal(err)
	}
	e.eng.mu.Lock()
	st := e.eng.st[ih]
	st.Stalled = torrent.StalledNoPeers
	e.eng.st[ih] = st
	e.eng.mu.Unlock()
	searches := len(e.idx.requests)
	t0 := time.Now()
	checkAt(e, t0)
	checkAt(e, t0.Add(2*time.Minute))
	checkAt(e, t0.Add(time.Hour))
	e.svc.auto.mu.Lock()
	_, watched := e.svc.auto.quiet[ih]
	e.svc.auto.mu.Unlock()
	if watched {
		t.Fatal("watched for a switch")
	}
	if len(e.idx.requests) != searches || len(p.probed) != 0 {
		t.Fatalf("searched %d, probed %v", len(e.idx.requests)-searches, p.probed)
	}
	for _, v := range e.svc.Torrents() {
		if v.InfoHash == ih && v.AutoReplace != "" {
			t.Fatalf("note %q", v.AutoReplace)
		}
	}
}
