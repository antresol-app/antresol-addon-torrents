package acquire

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol/addonkit"
)

// The state names coreadapt mirrors (it cannot import acquire) are
// acquire's own.
func TestVoiceStates_Mirrored(t *testing.T) {
	for got, want := range map[string]string{
		coreadapt.VoicePlaced: VoicePlaced, coreadapt.VoiceFitted: VoiceFitted,
		coreadapt.VoiceMisfit: VoiceMisfit, coreadapt.VoiceUnmeasured: VoiceUnmeasured,
	} {
		if got != want {
			t.Errorf("coreadapt %q, acquire %q", got, want)
		}
	}
}

// TestVoiceOverDownload_QueuesFit: a placed voice-over track goes to the
// core's fit queue (POST /voiceover/fit) with the video's library file id,
// through coreadapt.VoiceOver as boot wires it; the sheet reads the
// core's state back.
func TestVoiceOverDownload_QueuesFit(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var queued []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/addon-core/v1/voiceover/fit" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodPost {
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			queued = append(queued, b)
			mu.Unlock()
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Write([]byte(`{"state":"ok","offsetMs":-1500,"tempo":1}`))
	}))
	defer srv.Close()
	v := coreadapt.VoiceOver{C: addonkit.NewClient(addonkit.Env{CoreURL: srv.URL, Token: "tok"})}

	e := newEnv(t)
	show, eps := voSeries(t, e, filepath.Join(e.root, "media/series/Billions/Season 3"), 1, 1)
	e.svc.SetVoiceOverFit(func(ctx context.Context, p PlacedVoiceOver) error {
		return v.Placed(ctx, p.Path, p.FileID, p.OrigLang)
	}, v.State)
	const name = "Billions.S03.RUS.Sound.LostFilm"
	mf := planRelease(t, e, "https://t/lf", name, seasonPack("Billions.S03E%02d.LostFilm.ac3", 1, 1)...)
	e.eng.nextIH = metafileIH(t, mf)
	e.eng.nextFiles = torrentFiles(t, filepath.Join(e.root, "torrents/series"), seasonPack(name+"/Billions.S03E%02d.LostFilm.ac3", 1, 1)...)
	c := voFind(t, e, show, "https://t/lf", voRel("Миллиарды / Billions [S03] Звуковая дорожка LostFilm", "https://t/lf", 5, 1<<30))
	res, err := e.svc.DownloadVoiceOver(ctx, VoiceOverRequest{ItemID: show, ID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	e.svc.organize(ctx, res.InfoHash, false, false)
	track := strings.TrimSuffix(eps[1], ".mkv") + ".ru.LostFilm.ac3"
	mu.Lock()
	got := queued
	mu.Unlock()
	if len(got) != 1 || got[0]["path"] != track || got[0]["fileId"] != float64(e.lib.fileID(eps[1])) || got[0]["origLang"] != "en" {
		t.Fatalf("queued %v, want %s beside file %d", got, track, e.lib.fileID(eps[1]))
	}
	ds, err := e.svc.VoiceOverDownloads(ctx, show)
	if err != nil || len(ds) != 1 || len(ds[0].Episodes) != 1 || ds[0].Episodes[0].State != VoiceFitted || ds[0].Episodes[0].OffsetMs != -1500 {
		t.Fatalf("downloads %+v %v", ds, err)
	}
}
