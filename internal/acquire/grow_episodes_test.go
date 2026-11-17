package acquire

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// TestFollowResolutionPreferred: a request with a resolution («Докачивать
// новые» of 1080p) takes the best release of that resolution over a
// better scored one of another; with none of it, the best of the others,
// with a note.
func TestFollowResolutionPreferred(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{
		relx("Дюна 2024 2160p", "https://t/uhd", 2, 90),
		relx("Дюна 2024 1080p", "https://t/fhd", 2, 50),
	}}
	res, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload, Resolution: "1080p"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(e.idx.downloads) != "[2 https://t/fhd]" || res.Note != "" {
		t.Fatalf("downloads %v, note %q", e.idx.downloads, res.Note)
	}

	e2 := newEnv(t)
	e2.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024 2160p", "https://t/uhd", 2, 90)}}
	res, err = e2.svc.Acquire(ctx, AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload, Resolution: "1080p"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(e2.idx.downloads) != "[2 https://t/uhd]" || res.Note != fmt.Sprintf(msgNoResolution, "1080p") {
		t.Fatalf("downloads %v, note %q", e2.idx.downloads, res.Note)
	}

	// no resolution asked: the best scored, as before
	e3 := newEnv(t)
	e3.idx.resp = e.idx.resp
	if _, err := e3.svc.Acquire(ctx, AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(e3.idx.downloads) != "[2 https://t/uhd]" {
		t.Fatalf("downloads %v", e3.idx.downloads)
	}
}

// growEnv: a series download of a pinned release (season 1, three
// episodes and an external track of E03) with E01 and E02 selected,
// organized.
func growEnv(t *testing.T) (*env, string) {
	t.Helper()
	ctx := context.Background()
	e := newEnv(t)
	ih := strings.Repeat("b", 40)
	h := e.eng.put(torrent.Status{InfoHash: ih, Mode: ModeDownload, Status: torrent.StatusDownloading})
	h.files = torrentFiles(t, filepath.Join(e.root, "torrents/series"),
		"Show.S01/Show.S01E01.mkv", "Show.S01/Show.S01E02.mkv", "Show.S01/Show.S01E03.mkv", "Show.S01/RUS/Show.S01E03.mka")
	h.selection = []int{0, 1}
	if err := e.svc.saveAcquisition(ctx, acquisition{InfoHash: ih, Kind: KindSeries, TMDBID: 126308, Title: "Сёгун", Year: 2024, Season: 1}, 1, "uid-1", false); err != nil {
		t.Fatal(err)
	}
	e.eng.complete(ih)
	e.svc.wait()
	a, err := e.svc.acquisition(ctx, ih)
	if err != nil || !a.OrganizedAt.Valid {
		t.Fatalf("first organize: %+v %v", a, err)
	}
	return e, ih
}

// TestGrowEpisodesSelectsAndReorganizes: new episodes' files join the
// download's selection, the acquisition is organized afresh once they are
// complete, and the episodes placed before stay as they are (no copies).
func TestGrowEpisodesSelectsAndReorganizes(t *testing.T) {
	ctx := context.Background()
	e, ih := growEnv(t)
	if got := placedVideos(t, e); fmt.Sprint(got) != "[Сёгун/Season 01/Сёгун - S01E01.mkv Сёгун/Season 01/Сёгун - S01E02.mkv]" {
		t.Fatalf("placed first %v", got)
	}
	res, err := e.svc.GrowEpisodes(ctx, ih, []int{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.InfoHash != ih || fmt.Sprint(e.eng.grown) != "[[2 3]]" {
		t.Fatalf("result %+v, grown %v", res, e.eng.grown)
	}
	a, _ := e.svc.acquisition(ctx, ih)
	if a.OrganizedAt.Valid || a.Meta.SelRev != 1 {
		t.Fatalf("after grow %+v", a)
	}
	e.eng.complete(ih)
	e.svc.wait()
	a, _ = e.svc.acquisition(ctx, ih)
	if !a.OrganizedAt.Valid || a.Error != "" {
		t.Fatalf("second organize %+v", a)
	}
	if got := placedVideos(t, e); fmt.Sprint(got) != "[Сёгун/Season 01/Сёгун - S01E01.mkv Сёгун/Season 01/Сёгун - S01E02.mkv Сёгун/Season 01/Сёгун - S01E03.mkv]" {
		t.Fatalf("placed after grow %v", got)
	}
	// the same files again: nothing changes
	if _, err := e.svc.GrowEpisodes(ctx, ih, []int{2, 3}); err != nil || len(e.eng.grown) != 1 {
		t.Fatalf("again: %v, grown %v", err, e.eng.grown)
	}
}

// TestGrowEpisodesPackReopensSeason: a pack whose season was placed
// (meta organized) places that season again once it grows by an episode;
// a season it was not asked for joins it.
func TestGrowEpisodesPackReopensSeason(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	ih := strings.Repeat("c", 40)
	h := e.eng.put(torrent.Status{InfoHash: ih, Mode: ModeDownload, Status: torrent.StatusSeeding})
	h.files = torrentFiles(t, filepath.Join(e.root, "torrents/series"),
		"Show/Season 1/Show.S01E01.mkv", "Show/Season 1/Show.S01E02.mkv", "Show/Season 2/Show.S02E01.mkv")
	h.selection = []int{0}
	a := acquisition{InfoHash: ih, Kind: KindSeries, TMDBID: 126308, Title: "Сёгун", Year: 2024, Season: 1,
		Meta: acquisitionMeta{Seasons: []int{1}, Organized: []int{1}}}
	if err := e.svc.saveAcquisition(ctx, a, 1, "uid-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.GrowEpisodes(ctx, ih, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	got, _ := e.svc.acquisition(ctx, ih)
	if fmt.Sprint(got.Meta.Seasons) != "[1 2]" || len(got.Meta.Organized) != 0 || got.Meta.SelRev != 1 {
		t.Fatalf("meta %+v", got.Meta)
	}
}

// TestGrowEpisodesRefused: a torrent the engine lacks, a stream, or one
// added outside «На сервер» is not grown.
func TestGrowEpisodesRefused(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if _, err := e.svc.GrowEpisodes(ctx, strings.Repeat("d", 40), []int{0}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not in the engine: %v", err)
	}
	ih := strings.Repeat("e", 40)
	h := e.eng.put(torrent.Status{InfoHash: ih, Mode: ModeStream})
	h.files = torrentFiles(t, filepath.Join(e.root, "stream"), "S/Show.S01E01.mkv", "S/Show.S01E02.mkv")
	h.selection = []int{0}
	if _, err := e.svc.GrowEpisodes(ctx, ih, []int{1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("stream: %v", err)
	}
	e.eng.setMode(ih, ModeDownload)
	if _, err := e.svc.GrowEpisodes(ctx, ih, []int{1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no acquisition: %v", err)
	}
	if _, err := e.svc.GrowEpisodes(ctx, ih, []int{7}); err == nil {
		t.Fatal("a file the torrent lacks is accepted")
	}
	if len(e.eng.grown) != 0 {
		t.Fatalf("grown %v", e.eng.grown)
	}
}

// TestRekeyMovesAcquisition: a pinned release replaced in the engine keeps
// its acquisition (organize and «Загрузки» find it under the new infohash).
func TestRekeyMovesAcquisition(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	old, nw := strings.Repeat("1", 40), strings.Repeat("2", 40)
	if err := e.svc.saveAcquisition(ctx, acquisition{InfoHash: old, Kind: KindSeries, TMDBID: 126308, Title: "Сёгун", Season: 1}, 1, "uid-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.o.DB.Exec(`UPDATE acquisitions SET organized_at='2026-11-01T00:00:00Z' WHERE infohash=?`, old); err != nil {
		t.Fatal(err)
	}
	if at, ok, err := e.svc.Imported(ctx, old); err != nil || !ok || at.Format(time.DateOnly) != "2026-11-01" {
		t.Fatalf("imported %v %v %v", at, ok, err)
	}
	if err := e.svc.Rekey(ctx, old, nw); err != nil {
		t.Fatal(err)
	}
	a, err := e.svc.acquisition(ctx, nw)
	if err != nil || a.TMDBID != 126308 || a.Season != 1 || a.OrganizedAt.Valid {
		t.Fatalf("new %+v %v", a, err)
	}
	if _, ok, _ := e.svc.Imported(ctx, nw); ok {
		t.Fatal("imported before its organize")
	}
	if _, err := e.svc.acquisition(ctx, old); err == nil {
		t.Fatal("old row left")
	}
	// a row of the new infohash already there is kept
	third := strings.Repeat("3", 40)
	if err := e.svc.saveAcquisition(ctx, acquisition{InfoHash: third, Kind: KindMovie, TMDBID: 1, Title: "x"}, 1, "uid-1", false); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Rekey(ctx, nw, third); err != nil {
		t.Fatal(err)
	}
	if a, _ := e.svc.acquisition(ctx, third); a.Kind != KindMovie {
		t.Fatalf("overwritten %+v", a)
	}
}

// TestAcquirePinnedFilesSelectsOnly: the pinned release not in the engine
// is added in download mode with only the asked files selected (never the
// whole season); a streamed one is narrowed to them and downloaded; a
// download grows.
func TestAcquirePinnedFilesSelectsOnly(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := planRelease(t, e, "https://t/pin", "Show.S01", "Show.S01E01.mkv", "Show.S01E02.mkv", "Show.S01E03.mkv", "RUS/Show.S01E03.mka")
	e.eng.nextIH = metafileIH(t, mf)
	e.eng.nextFiles = torrentFiles(t, filepath.Join(e.root, "torrents/series"), "Show.S01/Show.S01E01.mkv", "Show.S01/Show.S01E02.mkv", "Show.S01/Show.S01E03.mkv", "Show.S01/RUS/Show.S01E03.mka")
	req := AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, ReleaseLink: "https://t/pin", SourceID: 3, UserID: 1}
	res, err := e.svc.AcquirePinnedFiles(ctx, e.eng.nextIH, req, []int{2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.eng.added) != 1 || fmt.Sprint(e.eng.added[0].Files) != "[2 3]" || e.eng.added[0].Mode != ModeDownload || res.InfoHash != e.eng.nextIH {
		t.Fatalf("added %+v, result %+v", e.eng.added, res)
	}
	a, err := e.svc.acquisition(ctx, res.InfoHash)
	if err != nil || a.Kind != KindSeries || a.TMDBID != 126308 || a.Season != 1 || a.AddedBy != 1 {
		t.Fatalf("acquisition %+v %v", a, err)
	}
	// again, the engine has it downloading: it grows
	if _, err := e.svc.AcquirePinnedFiles(ctx, res.InfoHash, req, []int{1}); err != nil {
		t.Fatal(err)
	}
	if len(e.eng.added) != 1 || fmt.Sprint(e.eng.grown) != "[[1]]" {
		t.Fatalf("grow: added %d, grown %v", len(e.eng.added), e.eng.grown)
	}

	// a stream (no acquisition yet): narrowed, then downloaded
	s := newEnv(t)
	ih := strings.Repeat("f", 40)
	h := s.eng.put(torrent.Status{InfoHash: ih, Mode: ModeStream, Status: torrent.StatusDownloading})
	h.files = torrentFiles(t, filepath.Join(s.root, "stream"), "S/Show.S01E01.mkv", "S/Show.S01E02.mkv", "S/Show.S01E03.mkv")
	if _, err := s.svc.AcquirePinnedFiles(ctx, ih, req, []int{2}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(s.eng.shrunk) != "[[0 1]]" || fmt.Sprint(h.selection) != "[2]" || len(s.eng.modes) != 1 || !strings.HasPrefix(s.eng.modes[0], ih+" download") {
		t.Fatalf("shrunk %v, selection %v, modes %v", s.eng.shrunk, h.selection, s.eng.modes)
	}
	if a, err := s.svc.acquisition(ctx, ih); err != nil || a.TMDBID != 126308 {
		t.Fatalf("acquisition %+v %v", a, err)
	}
	if s.eng.mayEmpty[h.files[0].FullPath] {
		t.Fatal("a stream's data may be emptied")
	}
	// refused: not a series, no files, no link
	for _, r := range []AcquireRequest{{TMDBID: 1, Kind: KindMovie, ReleaseLink: "x"}, {TMDBID: 1, Kind: KindSeries}} {
		if _, err := s.svc.AcquirePinnedFiles(ctx, ih, r, []int{0}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%+v: %v", r, err)
		}
	}
	if _, err := s.svc.AcquirePinnedFiles(ctx, ih, req, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no files: %v", err)
	}
}
