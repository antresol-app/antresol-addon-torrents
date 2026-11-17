package acquire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

const suitsPack = "Suits - S1-9E1-134 - 2011-2019 DVO (NewStudio, TVShows) Blu-Ray Remux 1080p"

// suitsFiles: a complete-series pack, seasons in folders, plus files of
// no season.
var suitsFiles = []string{
	"Suits.nfo",
	"Season 1/Suits.S01E01.mkv",
	"Season 1/Suits.S01E02.mkv",
	"Season 2/Suits.S02E01.mkv",
	"Season 7/Suits.S07E01.mkv",
	"Season 7/Suits.S07E02.mkv",
	"Season 7/Rus Sound/Suits.S07E01.ac3",
}

func TestSeasonFiles(t *testing.T) {
	entries := func(name string, paths ...string) []torrent.FileEntry {
		out := make([]torrent.FileEntry, len(paths))
		for i, p := range paths {
			out[i] = torrent.FileEntry{Index: i, Path: name + "/" + p, Size: 1000}
		}
		return out
	}
	for _, tc := range []struct {
		name    string
		entries []torrent.FileEntry
		season  int
		want    string // fmt of the selection; "error" = refused
	}{
		{"pack in season folders", entries(suitsPack, suitsFiles...), 7, "[4 5 6]"},
		{"pack by episode codes", entries("Suits", "Suits.S01E01.mkv", "Suits.S02E01.mkv", "Suits.S02E02.mkv"), 2, "[1 2]"},
		{"numbered files in season folders", entries("Suits S01-S03", "S01/01.mkv", "S02/01.mkv", "S03/01.mkv"), 3, "[2]"},
		{"one season: the whole torrent", entries("Suits.S03.1080p", "Suits.S03E01.mkv", "Suits.S03E02.mkv", "Suits.nfo"), 3, "[]"},
		{"no season named: the whole torrent", entries("Suits", "01.mkv", "02.mkv"), 3, "[]"},
		{"single file", []torrent.FileEntry{{Index: 0, Path: "Suits.S03E01.mkv", Size: 1}}, 3, "[]"},
		{"the season is not there", entries(suitsPack, suitsFiles...), 4, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := seasonFiles(tc.entries, tc.season)
			switch {
			case tc.want == "error":
				if err == nil || !errors.Is(err, ErrNoFiles) {
					t.Fatalf("got %v, %v; want ErrNoFiles", got, err)
				}
			case err != nil:
				t.Fatal(err)
			case fmt.Sprint(got) != tc.want:
				t.Fatalf("got %v, want %s", got, tc.want)
			}
		})
	}
}

// packFiles is suitsFiles as the engine lists them (under the torrents dir).
func packFiles(t *testing.T, e *env) []torrent.FileInfo {
	t.Helper()
	paths := make([]string, len(suitsFiles))
	for i, p := range suitsFiles {
		paths[i] = suitsPack + "/" + p
	}
	return torrentFiles(t, filepath.Join(e.root, "torrents/series"), paths...)
}

// Live (Suits): a season request that picks a complete-series pack
// downloads only that season's files; a later request of another season
// the pack has grows its selection instead of failing, and the title's
// progress and «Следить» see both seasons.
func TestSeasonRequestTakesSeasonOfPack(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := planRelease(t, e, "https://t/pack", suitsPack, suitsFiles...)
	e.eng.nextIH = metafileIH(t, mf)
	e.eng.nextFiles = packFiles(t, e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(suitsPack, "https://t/pack", 1, 50)}}
	res, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload, UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.eng.added) != 1 || fmt.Sprint(e.eng.added[0].Files) != "[1 2]" {
		t.Fatalf("added %+v", e.eng.added)
	}
	a, err := e.svc.acquisition(ctx, res.InfoHash)
	if err != nil || a.Season != 1 || fmt.Sprint(a.Meta.Seasons) != "[1]" {
		t.Fatalf("acquisition %+v %v", a, err)
	}
	if _, err := e.svc.o.DB.Exec(`UPDATE acquisitions SET organized_at='2026-11-03T00:00:00Z' WHERE infohash=?`, res.InfoHash); err != nil {
		t.Fatal(err)
	}

	// season 7: the same pack again
	res7, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 7, Mode: ModeDownload, UserID: 1})
	if err != nil {
		t.Fatalf("season 7: %v", err)
	}
	if !res7.Existing || res7.InfoHash != res.InfoHash || len(e.eng.added) != 1 || fmt.Sprint(e.eng.grown) != "[[4 5 6]]" {
		t.Fatalf("season 7: %+v, added %d, grown %v", res7, len(e.eng.added), e.eng.grown)
	}
	a, _ = e.svc.acquisition(ctx, res.InfoHash)
	if fmt.Sprint(a.Meta.Seasons) != "[1 7]" || a.OrganizedAt.Valid || a.Meta.SelRev != 1 {
		t.Fatalf("after season 7: %+v", a)
	}
	// season 7 again: nothing more
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 7, Mode: ModeDownload}); err != nil || len(e.eng.grown) != 1 {
		t.Fatalf("season 7 again: %v, grown %v", err, e.eng.grown)
	}

	if _, ok := e.svc.TitleProgress(ctx, KindSeries, 126308, 7); !ok {
		t.Error("season 7 has no progress")
	}
	// the engine's row (the fake engine keeps none)
	if _, err := e.svc.o.DB.Exec(`INSERT INTO torrents (infohash, name, mode, save_dir) VALUES (?,?,'download','/t')`, res.InfoHash, suitsPack); err != nil {
		t.Fatal(err)
	}
	pending, err := e.svc.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seasons := map[int]bool{}
	for _, p := range pending {
		if p.Episode == 0 {
			seasons[p.Season] = true
		}
	}
	if !seasons[1] || !seasons[7] || len(seasons) != 2 {
		t.Errorf("pending %+v", pending)
	}

	// complete: only the selected seasons go into the library
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	var placed []string
	filepath.WalkDir(filepath.Join(e.root, "media/series"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".mkv") {
			placed = append(placed, filepath.Base(p))
		}
		return nil
	})
	slices.Sort(placed)
	if fmt.Sprint(placed) != "[Сёгун - S01E01.mkv Сёгун - S01E02.mkv Сёгун - S07E01.mkv Сёгун - S07E02.mkv]" {
		t.Errorf("placed %v", placed)
	}
}

// sameFilesElsewhere is metafile mf with another source field: the same
// files under another infohash.
func sameFilesElsewhere(t *testing.T, mf []byte) []byte {
	t.Helper()
	mi, err := metainfo.Load(bytes.NewReader(mf))
	if err != nil {
		t.Fatal(err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	info.Source = "another tracker"
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	out, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: ib})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// placedVideos lists the video files organize put into the series library.
func placedVideos(t *testing.T, e *env) []string {
	t.Helper()
	var placed []string
	filepath.WalkDir(filepath.Join(e.root, "media/series"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".mkv") {
			rel, _ := filepath.Rel(filepath.Join(e.root, "media/series"), p)
			placed = append(placed, filepath.ToSlash(rel))
		}
		return nil
	})
	slices.Sort(placed)
	return placed
}

// a pack grown from a placed season 1 to season 7 keeps season
// 1 done; cancelling season 7 (or «Выбрать другую раздачу» for it) drops
// only season 7's files, and the torrent goes only with its last season.
func TestCancelSeasonOfGrownPack(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := planRelease(t, e, "https://t/pack", suitsPack, suitsFiles...)
	ih := metafileIH(t, mf)
	e.eng.nextIH = ih
	e.eng.nextFiles = packFiles(t, e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(suitsPack, "https://t/pack", 1, 50)}}
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	e.eng.complete(ih)
	e.svc.wait()
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 7, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	a, _ := e.svc.acquisition(ctx, ih)
	if fmt.Sprint(a.Meta.Seasons, a.Meta.Organized) != "[1 7] [1]" {
		t.Fatalf("after growing: %+v", a.Meta)
	}
	// season 1 stays done; season 7's ring counts season 7's files only
	if p, ok := e.svc.TitleProgress(ctx, KindSeries, 126308, 1); !ok || p.Status != "done" {
		t.Errorf("season 1: %+v %v", p, ok)
	}
	files := e.eng.handles[ih].Files()
	var s7 int64
	for _, f := range files[4:] {
		s7 += f.Size
	}
	if p, ok := e.svc.TitleProgress(ctx, KindSeries, 126308, 7); !ok || p.Status != "active" || p.Size != s7 {
		t.Errorf("season 7: %+v %v (want size %d)", p, ok, s7)
	}
	// «Следить»: only season 7 is pending
	if _, err := e.svc.o.DB.Exec(`INSERT INTO torrents (infohash, name, mode, save_dir) VALUES (?,?,'download','/t')`, ih, suitsPack); err != nil {
		t.Fatal(err)
	}
	pending, _ := e.svc.Pending(ctx)
	if len(pending) != 1 || pending[0].Season != 7 || pending[0].Episode != 0 {
		t.Errorf("pending %+v", pending)
	}
	// season 1 is placed: cancelling it changes nothing
	if n, err := e.svc.CancelTitle(ctx, KindSeries, 126308, 1, ""); err != nil || n != 0 || len(e.eng.shrunk) != 0 {
		t.Fatalf("cancel season 1: %d %v, shrunk %v", n, err, e.eng.shrunk)
	}
	// «Выбрать другую раздачу» for season 7: its files leave the pack
	if n, err := e.svc.CancelTitle(ctx, KindSeries, 126308, 7, strings.Repeat("f", 40)); err != nil || n != 1 {
		t.Fatalf("cancel season 7: %d %v", n, err)
	}
	if e.eng.removed[ih] || fmt.Sprint(e.eng.shrunk) != "[[4 5 6]]" {
		t.Fatalf("removed %v, shrunk %v", e.eng.removed, e.eng.shrunk)
	}
	a, _ = e.svc.acquisition(ctx, ih)
	if fmt.Sprint(a.Meta.Seasons) != "[1]" || !a.OrganizedAt.Valid {
		t.Fatalf("after cancelling season 7: %+v %v", a.Meta, a.OrganizedAt)
	}
}

// a season joining a pack on a full disk waits for space at
// once, as a new download would.
func TestGrowSeasonOnFullDiskWaits(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := planRelease(t, e, "https://t/pack", suitsPack, suitsFiles...)
	ih := metafileIH(t, mf)
	e.eng.nextIH = ih
	e.eng.nextFiles = packFiles(t, e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(suitsPack, "https://t/pack", 1, 50)}}
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	e.eng.complete(ih)
	e.svc.wait()
	full := diskAt(97)
	full.Engine = e.eng
	e.svc.o.Disk = full
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 7, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.eng.held, ih) {
		t.Fatalf("held %v", e.eng.held)
	}
}

// a release added whole (its files name no season) that
// another season's request picks again records no seasons and is
// organized whole, as before.
func TestWholeTorrentJoinedStaysWhole(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const name = "Suits 1080p"
	mf := planRelease(t, e, "https://t/whole", name, "01.mkv", "02.mkv")
	ih := metafileIH(t, mf)
	e.eng.nextIH = ih
	e.eng.nextFiles = torrentFiles(t, filepath.Join(e.root, "torrents/series"), name+"/01.mkv", name+"/02.mkv")
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(name, "https://t/whole", 1, 50)}}
	for _, season := range []int{1, 2} {
		if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: season, Mode: ModeDownload}); err != nil {
			t.Fatal(err)
		}
	}
	if e.eng.added[0].Files != nil || len(e.eng.grown) != 0 {
		t.Fatalf("added %+v grown %v", e.eng.added, e.eng.grown)
	}
	a, _ := e.svc.acquisition(ctx, ih)
	if a.isPack() {
		t.Fatalf("a whole torrent records seasons: %+v", a.Meta)
	}
	e.eng.complete(ih)
	e.svc.wait()
	if got := fmt.Sprint(placedVideos(t, e)); got != "[Сёгун/Season 01/Сёгун - S01E01.mkv Сёгун/Season 01/Сёгун - S01E02.mkv]" {
		t.Fatalf("placed %s", got)
	}
}

// a season of a pack is not taken out while an organize of the
// pack runs; and the engine may empty only files no library row names.
func TestCancelSeasonWaitsForOrganize(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := planRelease(t, e, "https://t/pack", suitsPack, suitsFiles...)
	ih := metafileIH(t, mf)
	e.eng.nextIH = ih
	e.eng.nextFiles = packFiles(t, e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(suitsPack, "https://t/pack", 1, 50)}}
	for _, season := range []int{1, 7} {
		if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: season, Mode: ModeDownload}); err != nil {
			t.Fatal(err)
		}
	}
	e.svc.mu.Lock()
	e.svc.organizing[ih] = true
	e.svc.mu.Unlock()
	_, err := e.svc.CancelTitle(ctx, KindSeries, 126308, 1, "")
	if msg, _ := UserMessage(err); msg != msgOrganizingWait || len(e.eng.shrunk) != 0 {
		t.Fatalf("cancel while organizing: %v, shrunk %v", err, e.eng.shrunk)
	}
	e.svc.mu.Lock()
	delete(e.svc.organizing, ih)
	e.svc.mu.Unlock()
	// a library row names one of season 1's torrent files: it is kept
	named := e.eng.handles[ih].files[1].FullPath
	e.lib.addFile(library.File{ItemID: e.lib.addItem(library.Item{Kind: "episode", Title: "E1"}), Path: named, Size: 5, Mtime: 1})
	if n, err := e.svc.CancelTitle(ctx, KindSeries, 126308, 1, ""); err != nil || n != 1 {
		t.Fatalf("cancel: %d %v", n, err)
	}
	other := e.eng.handles[ih].files[2].FullPath
	if e.eng.mayEmpty[named] || !e.eng.mayEmpty[other] {
		t.Fatalf("may empty %v", e.eng.mayEmpty)
	}
}

// The last season asked of a pack takes the torrent with it, as before.
func TestCancelLastSeasonOfPackRemovesIt(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := planRelease(t, e, "https://t/pack", suitsPack, suitsFiles...)
	ih := metafileIH(t, mf)
	e.eng.nextIH = ih
	e.eng.nextFiles = packFiles(t, e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(suitsPack, "https://t/pack", 1, 50)}}
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 7, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.svc.CancelTitle(ctx, KindSeries, 126308, 7, ""); err != nil || n != 1 || !e.eng.removed[ih] || !e.eng.removedData[ih] {
		t.Fatalf("cancel: %d %v, removed %v", n, err, e.eng.removed)
	}
}

// a pack whose files are bare numbers in «Season N» folders,
// season 1 asked first and season 3 joining: each file goes to the season
// its folder names, not to the first season asked for.
func TestGrownPackSeasonFolders(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	const name = "Suits Complete 1080p"
	files := []string{"Season 1/01.mkv", "Season 1/02.mkv", "Season 2/01.mkv", "Season 3/01.mkv", "Season 3/02.mkv"}
	mf := planRelease(t, e, "https://t/pack", name, files...)
	e.eng.nextIH = metafileIH(t, mf)
	paths := make([]string, len(files))
	for i, p := range files {
		paths[i] = name + "/" + p
	}
	e.eng.nextFiles = torrentFiles(t, filepath.Join(e.root, "torrents/series"), paths...)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(name, "https://t/pack", 1, 50)}}
	res, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 3, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(e.eng.grown) != "[[3 4]]" {
		t.Fatalf("grown %v", e.eng.grown)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	want := "[Сёгун/Season 01/Сёгун - S01E01.mkv Сёгун/Season 01/Сёгун - S01E02.mkv Сёгун/Season 03/Сёгун - S03E01.mkv Сёгун/Season 03/Сёгун - S03E02.mkv]"
	if got := fmt.Sprint(placedVideos(t, e)); got != want {
		t.Fatalf("placed %s\nwant   %s", got, want)
	}
}

// Live (Suits S7): the pack under another infohash (the same files from
// another source) is refused by the engine; the season joins the pack the
// engine has.
func TestSeasonRequestJoinsSameFilesUnderOtherInfohash(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := planRelease(t, e, "https://t/pack", suitsPack, suitsFiles...)
	owner := metafileIH(t, mf)
	e.eng.nextIH = owner
	e.eng.nextFiles = packFiles(t, e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(suitsPack, "https://t/pack", 1, 50)}}
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	// the same files from another tracker (another source field), as the
	// engine refuses them (torrent.TestNamesakeGetsOwnPath)
	other := sameFilesElsewhere(t, mf)
	e.idx.byLink["https://t/other"] = cardigann.DownloadResult{Torrent: other}
	if metafileIH(t, other) == owner {
		t.Fatal("same infohash")
	}
	e.eng.nextIH = metafileIH(t, other)
	e.eng.addErr = &torrent.PathInUseError{Path: "/torrents/series/" + suitsPack, Owner: owner}
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(suitsPack, "https://t/other", 2, 50)}}
	res, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 7, Mode: ModeDownload})
	if err != nil {
		t.Fatalf("season 7: %v", err)
	}
	if res.InfoHash != owner || !res.Existing || fmt.Sprint(e.eng.grown) != "[[4 5 6]]" {
		t.Fatalf("season 7: %+v, grown %v", res, e.eng.grown)
	}
	// another title's download of those files stays a conflict
	e.eng.grown = nil
	if _, err := e.svc.o.DB.Exec(`UPDATE acquisitions SET tmdb_id=1 WHERE infohash=?`, owner); err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 2, Mode: ModeDownload})
	if msg, _ := UserMessage(err); msg != "Эти файлы уже скачиваются другой раздачей" || len(e.eng.grown) != 0 {
		t.Fatalf("other title: %v, grown %v", err, e.eng.grown)
	}
}

// sizedMagnet is a magnet release of the search of size gigabytes.
func sizedMagnet(title, ih string, gb int64, score int) indexer.ScoredRelease {
	r := relx(title, "magnet:?xt=urn:btih:"+ih, 1, score)
	r.Size = gb << 30
	return r
}

// a pack magnet whose file list does not come is never added
// whole: the next candidate is taken.
func TestSeasonRequestPackMagnetWithoutFilesFallsBack(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.eng.fetchInfo = func(context.Context, string) ([]byte, error) { return nil, torrent.ErrNoInfo }
	single := strings.Repeat("b", 40)
	e.eng.nextIH = single
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{
		sizedMagnet("Suits Season 1-9 Remux 1080p", strings.Repeat("a", 40), 1100, 60),
		sizedMagnet("Suits S01 1080p", single, 40, 50),
	}}
	res, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	if e.eng.fetches != 1 || len(e.eng.added) != 1 || res.InfoHash != single || e.eng.added[0].Files != nil {
		t.Fatalf("fetches %d added %+v res %+v", e.eng.fetches, e.eng.added, res)
	}
	if !strings.Contains(string(e.eng.added[0].Release), "Suits S01 1080p") {
		t.Fatalf("release %s", e.eng.added[0].Release)
	}
	// nothing else to take: the file list's error
	e2 := newEnv(t)
	e2.eng.fetchInfo = e.eng.fetchInfo
	e2.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{sizedMagnet("Suits Complete Series 1080p", strings.Repeat("a", 40), 1100, 60)}}
	if _, err := e2.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload}); !errors.Is(err, ErrNoFiles) || len(e2.eng.added) != 0 {
		t.Fatalf("only a pack: %v, added %+v", err, e2.eng.added)
	}
}

// A request from the web stays under the proxy's 60 s: the pack file
// lists of one request share webFetchBudget, each fetch getting what is
// left of it; «Следить» (Background) keeps three full fetches.
func TestSeasonRequestWebFetchBudget(t *testing.T) {
	ctx := context.Background()
	defer func(d, b time.Duration) { packFetchTimeout, webFetchBudget = d, b }(packFetchTimeout, webFetchBudget)
	packFetchTimeout, webFetchBudget = 300*time.Millisecond, 450*time.Millisecond
	for _, tc := range []struct {
		background bool
		fetches    int
	}{{false, 2}, {true, maxPackFetches}} {
		e := newEnv(t)
		var mu sync.Mutex
		var left []time.Duration
		e.eng.fetchInfo = func(fctx context.Context, _ string) ([]byte, error) {
			d, _ := fctx.Deadline()
			mu.Lock()
			left = append(left, time.Until(d))
			mu.Unlock()
			<-fctx.Done() // a magnet that never answers
			return nil, torrent.ErrNoInfo
		}
		single := strings.Repeat("e", 40)
		e.eng.nextIH = single
		var rels []indexer.ScoredRelease
		for i, c := range "abcd" {
			rels = append(rels, sizedMagnet(fmt.Sprintf("Suits Seasons 1-%d", i+2), strings.Repeat(string(c), 40), 1000, 60-i))
		}
		e.idx.resp = indexer.SearchResponse{Releases: append(rels, sizedMagnet("Suits S01 1080p", single, 40, 40))}
		start := time.Now()
		res, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload, Background: tc.background})
		took := time.Since(start)
		if err != nil || res.InfoHash != single || e.eng.fetches != tc.fetches {
			t.Fatalf("background %v: %+v %v, fetches %d", tc.background, res, err, e.eng.fetches)
		}
		if !tc.background && (took > 700*time.Millisecond || left[1] > 200*time.Millisecond) {
			t.Fatalf("web request took %v, fetch time left %v", took, left)
		}
	}
}

// a magnet whose name names no season, with no release
// of the season alone to compare its size with, is read before it is
// added.
func TestSeasonRequestSeasonlessMagnetIsRead(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := testMetafile(t, "Suits", suitsFiles...)
	ih := metafileIH(t, mf)
	e.eng.nextIH = ih
	e.eng.fetchInfo = func(context.Context, string) ([]byte, error) { return mf, nil }
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{sizedMagnet("Suits Remux 1080p", ih, 1100, 60)}}
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 2, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	if e.eng.fetches != 1 || len(e.eng.added) != 1 || fmt.Sprint(e.eng.added[0].Files) != "[3]" {
		t.Fatalf("fetches %d added %+v", e.eng.fetches, e.eng.added)
	}
}

// each pack's file list gets its own time (one slow magnet does
// not fail the next), and at most maxPackFetches are read per request.
func TestSeasonRequestPackFetchesEachOwnDeadline(t *testing.T) {
	ctx := context.Background()
	defer func(d time.Duration) { packFetchTimeout = d }(packFetchTimeout)
	packFetchTimeout = 300 * time.Millisecond
	e := newEnv(t)
	var mu sync.Mutex
	var left []time.Duration // time left to each fetch when it starts
	e.eng.fetchInfo = func(fctx context.Context, _ string) ([]byte, error) {
		d, _ := fctx.Deadline()
		mu.Lock()
		left = append(left, time.Until(d))
		mu.Unlock()
		time.Sleep(200 * time.Millisecond) // a slow magnet
		return nil, torrent.ErrNoInfo
	}
	single := strings.Repeat("e", 40)
	e.eng.nextIH = single
	var rels []indexer.ScoredRelease
	for i, c := range "abcd" {
		rels = append(rels, sizedMagnet(fmt.Sprintf("Suits Seasons 1-%d", i+2), strings.Repeat(string(c), 40), 1000, 60-i))
	}
	e.idx.resp = indexer.SearchResponse{Releases: append(rels, sizedMagnet("Suits S01 1080p", single, 40, 40))}
	res, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	if res.InfoHash != single || e.eng.fetches != maxPackFetches {
		t.Fatalf("result %+v, fetches %d", res, e.eng.fetches)
	}
	for i, l := range left {
		if l < 250*time.Millisecond || l > packFetchTimeout {
			t.Errorf("fetch %d started with %v left", i, l)
		}
	}
}

// a magnet whose name says no season but which is far bigger
// than the season's other releases is read before it is added.
func TestSeasonRequestHugeMagnetIsRead(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := testMetafile(t, "Suits", suitsFiles...)
	ih := metafileIH(t, mf)
	e.eng.nextIH = ih
	e.eng.fetchInfo = func(context.Context, string) ([]byte, error) { return mf, nil }
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{
		sizedMagnet("Suits Remux 1080p", ih, 1100, 60),
		sizedMagnet("Suits S07 1080p", strings.Repeat("b", 40), 40, 50),
		sizedMagnet("Suits S07 720p", strings.Repeat("c", 40), 20, 40),
	}}
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 7, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	if e.eng.fetches != 1 || len(e.eng.added) != 1 || fmt.Sprint(e.eng.added[0].Files) != "[4 5 6]" {
		t.Fatalf("fetches %d added %+v", e.eng.fetches, e.eng.added)
	}
}

// two seasons of one new pack asked at the same time: one
// download with both seasons, none lost.
func TestSeasonRequestsAtOnceJoin(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := testMetafile(t, suitsPack, suitsFiles...)
	ih := metafileIH(t, mf)
	e.eng.nextIH = ih
	e.eng.nextFiles = packFiles(t, e)
	e.eng.fetchInfo = func(context.Context, string) ([]byte, error) {
		time.Sleep(100 * time.Millisecond) // both requests are under way
		return mf, nil
	}
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{sizedMagnet(suitsPack, ih, 1100, 50)}}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, season := range []int{1, 7} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: season, Mode: ModeDownload})
		}()
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil {
		t.Fatal(errs)
	}
	a, err := e.svc.acquisition(ctx, ih)
	if err != nil || fmt.Sprint(a.Meta.Seasons) != "[1 7]" || len(e.eng.added) != 1 || len(e.eng.grown) != 1 {
		t.Fatalf("acquisition %+v %v, added %d, grown %v", a.Meta, err, len(e.eng.added), e.eng.grown)
	}
}

// A magnet whose name says several seasons: its file list is fetched first
// and only the season is downloaded.
func TestSeasonRequestMagnetPack(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	mf := testMetafile(t, suitsPack, suitsFiles...)
	ih := metafileIH(t, mf)
	e.eng.nextIH = ih
	e.eng.fetchInfo = func(context.Context, string) ([]byte, error) { return mf, nil }
	e.idx.dl = &cardigann.DownloadResult{Magnet: "magnet:?xt=urn:btih:" + ih}
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx(suitsPack, "https://t/pack", 1, 50)}}
	if _, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 2, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	if len(e.eng.added) != 1 || fmt.Sprint(e.eng.added[0].Files) != "[3]" || string(e.eng.addedData[0]) != string(mf) {
		t.Fatalf("added %+v", e.eng.added)
	}
	// a single season's magnet is added as before, without a fetch
	e2 := newEnv(t)
	e2.idx.dl = &cardigann.DownloadResult{Magnet: "magnet:?xt=urn:btih:" + ih}
	e2.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Suits S02 1080p", "https://t/s2", 1, 50)}}
	if _, err := e2.svc.Acquire(ctx, AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 2, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	if e2.eng.fetches != 0 || len(e2.eng.added) != 1 || e2.eng.added[0].Files != nil {
		t.Fatalf("single season: fetches %d added %+v", e2.eng.fetches, e2.eng.added)
	}
}
