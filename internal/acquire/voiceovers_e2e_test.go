package acquire

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// fitQueue stands in for the core's fit worker: Queue records the placed
// track, Pass fits each queued one at offset.
type fitQueue struct {
	mu     sync.Mutex
	queued map[string]PlacedVoiceOver
	fitted map[string]float64
	offset float64
}

func (q *fitQueue) Queue(_ context.Context, p PlacedVoiceOver) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.queued == nil {
		q.queued = map[string]PlacedVoiceOver{}
	}
	q.queued[p.Path] = p
	return nil
}

func (q *fitQueue) Pass() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fitted == nil {
		q.fitted = map[string]float64{}
	}
	for p := range q.queued {
		q.fitted[p] = q.offset
	}
}

func (q *fitQueue) State(_ context.Context, path string) (string, float64, float64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if off, ok := q.fitted[path]; ok {
		return VoiceFitted, off, 1, true
	}
	if _, ok := q.queued[path]; ok {
		return VoicePlaced, 0, 0, true
	}
	return "", 0, 0, false
}

// soundtrackWAV writes secs of noise bursts (a soundtrack's envelope) as
// mono 16 kHz PCM.
func soundtrackWAV(t *testing.T, p string, secs int) {
	t.Helper()
	const rate = 16000
	r := rand.New(rand.NewPCG(3, 5))
	n := secs * rate
	b := make([]byte, 44+2*n)
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+2*n))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], 1)
	binary.LittleEndian.PutUint32(b[24:], rate)
	binary.LittleEndian.PutUint32(b[28:], rate*2)
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(2*n))
	for i := 0; i < n; {
		k := (10 + r.IntN(290)) * rate / 1000
		level := r.Float64()
		if r.IntN(4) == 0 {
			level = 0
		}
		for j := 0; j < k && i < n; j++ {
			v := level * (2*r.Float64() - 1) * 0.5
			binary.LittleEndian.PutUint16(b[44+2*i:], uint16(int16(math.Round(v*32000))))
			i++
		}
	}
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The whole of «Найти озвучку…» with real files: the search's candidate,
// the season pack's track of the one episode on the server selected, the
// finished track linked beside the episode (pending, not offered), queued
// for the core's fit with the episode's file, fitted (+1.2 s: its first
// 1.2 s cut off), and the sheet's state says so.
func TestVoiceOverEndToEnd(t *testing.T) {
	ff, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("no ffmpeg")
	}
	ctx := context.Background()
	e := newEnv(t)
	dir := filepath.Join(e.root, "media/series/Billions/Season 3")
	show, eps := voSeries(t, e, dir, 1, 1)
	wav := filepath.Join(t.TempDir(), "orig.wav")
	soundtrackWAV(t, wav, 150)
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ff, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
	}
	// the episode: its English track (stream 0)
	run("-i", wav, "-c:a", "flac", "-metadata:s:a:0", "language=eng", "-f", "matroska", eps[1])
	for i := range e.lib.Library.Files {
		if f := &e.lib.Library.Files[i]; f.Path == eps[1] {
			f.DurationMs, f.ProbeJSON = 150000, `{"durationMs":150000,"audio":[{"index":0,"codec":"flac","lang":"eng","channels":1,"default":true}]}`
		}
	}
	// the release: tracks of E01 and E02 (the server has only E01)
	const name = "Billions.S03.RUS.Sound.LostFilm"
	mf := planRelease(t, e, "https://t/lf", name, "Billions.S03E01.LostFilm.mka", "Billions.S03E02.LostFilm.mka")
	e.eng.nextIH = metafileIH(t, mf)
	tdir := filepath.Join(e.root, "torrents/series", name)
	if err := os.MkdirAll(tdir, 0o755); err != nil {
		t.Fatal(err)
	}
	track := filepath.Join(tdir, "Billions.S03E01.LostFilm.mka")
	run("-i", wav, "-af", "atrim=start=1.2,asetpts=PTS-STARTPTS", "-c:a", "flac", track)
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: name + "/Billions.S03E01.LostFilm.mka", FullPath: track, Size: 1, Progress: 1},
		{Index: 1, Path: name + "/Billions.S03E02.LostFilm.mka", FullPath: filepath.Join(tdir, "Billions.S03E02.LostFilm.mka")}}

	w := &fitQueue{offset: 1200}
	e.svc.SetVoiceOverFit(w.Queue, w.State)
	c := voFind(t, e, show, "https://t/lf", voRel("Миллиарды / Billions [S03] Звуковая дорожка LostFilm", "https://t/lf", 5, 1<<30))
	res, err := e.svc.DownloadVoiceOver(ctx, VoiceOverRequest{ItemID: show, ID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.eng.added[0].Files) != 1 || e.eng.added[0].Files[0] != 0 {
		t.Fatalf("selected %v", e.eng.added[0].Files)
	}
	e.svc.organize(ctx, res.InfoHash, false, false)
	placed := strings.TrimSuffix(eps[1], ".mkv") + ".ru.LostFilm.mka"
	var fileID int64
	for _, f := range e.lib.Library.Files {
		if f.Path == eps[1] {
			fileID = f.ID
		}
	}
	if st, _, _, ok := w.State(ctx, placed); !ok || st != VoicePlaced {
		t.Fatalf("placed, before its fit: %q %v", st, ok)
	}
	if p := w.queued[placed]; p.FileID != fileID || fileID == 0 {
		t.Fatalf("queued %+v, want file %d", p, fileID)
	}
	w.Pass()
	ds, err := e.svc.VoiceOverDownloads(ctx, show)
	if err != nil || len(ds) != 1 || len(ds[0].Episodes) != 1 || ds[0].Episodes[0].State != VoiceFitted || math.Abs(ds[0].Episodes[0].OffsetMs-1200) > 5 {
		t.Fatalf("downloads %+v %v", ds, err)
	}
}
