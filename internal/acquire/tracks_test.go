package acquire

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

func TestMatchTrackFiles(t *testing.T) {
	files := []ReleaseFile{
		{Index: 0, Path: "Кино - Группа крови/01 - Группа крови.flac", Size: 30, Kind: fileKindTrack, Checked: true},
		{Index: 1, Path: "Кино - Группа крови/02 - Закрой за мной дверь.flac", Size: 30, Kind: fileKindTrack, Checked: true},
		{Index: 2, Path: "Кино - Группа крови/cover.jpg", Size: 1, Kind: fileKindExtra, Checked: true},
	}
	want := []WantedTrack{{Key: "1", Title: "Группа крови"}}
	idx, matched, missing := matchTrackFiles(files, want, false)
	if !slices.Equal(idx, []int{0}) || matched["1"] != 0 || len(missing) != 0 {
		t.Fatalf("existing album: %v %v %v", idx, matched, missing)
	}
	idx, _, _ = matchTrackFiles(files, want, true)
	if !slices.Equal(idx, []int{0, 2}) {
		t.Fatalf("new album: %v", idx)
	}

	// «1-03. », «A1 », «Artist - » prefixes; an edition suffix
	files = []ReleaseFile{
		{Index: 0, Path: "Metallica/CD1/1-03. Sad But True.flac", Kind: fileKindTrack, Checked: true},
		{Index: 1, Path: "Metallica/A1 Enter Sandman (Remastered).flac", Kind: fileKindTrack, Checked: true},
		{Index: 2, Path: "Metallica/Metallica - The Unforgiven.mp3", Kind: fileKindTrack, Checked: true},
		{Index: 3, Path: "Metallica/99 Problems.mp3", Kind: fileKindTrack, Checked: true},
	}
	want = []WantedTrack{{Key: "a", Title: "Sad But True"}, {Key: "b", Title: "Enter Sandman"}, {Key: "c", Title: "The Unforgiven"},
		{Key: "d", Title: "Nothing Else Matters"}, {Key: "e", Title: "99 Problems"}}
	idx, matched, missing = matchTrackFiles(files, want, false)
	if !slices.Equal(idx, []int{0, 1, 2, 3}) || matched["a"] != 0 || matched["b"] != 1 || matched["c"] != 2 || matched["e"] != 3 {
		t.Fatalf("prefixes: %v %v", idx, matched)
	}
	if !slices.Equal(missing, []string{"d"}) {
		t.Fatalf("missing %v", missing)
	}

	// two «Intro» files: the one with the wanted number, else the
	// requested album's
	files = []ReleaseFile{
		{Index: 0, Path: "Disco/1990 - A/01 - Intro.flac", Kind: fileKindTrack, Checked: false},
		{Index: 1, Path: "Disco/1991 - B/05 - Intro.flac", Kind: fileKindTrack, Checked: true},
		{Index: 2, Path: "Disco/1991 - B/07 - Intro.flac", Kind: fileKindTrack, Checked: true},
		{Index: 3, Path: "Disco/1990 - A/02 - Live Song.flac", Kind: fileKindTrack, Checked: false},
	}
	_, matched, _ = matchTrackFiles(files, []WantedTrack{{Key: "x", Title: "Intro", No: 7}}, false)
	if matched["x"] != 2 {
		t.Fatalf("by number: %v", matched)
	}
	_, matched, _ = matchTrackFiles(files, []WantedTrack{{Key: "x", Title: "Intro"}}, false)
	if matched["x"] != 1 {
		t.Fatalf("by requested root: %v", matched)
	}
	// into an album the library has, only the requested album's files: a
	// song only another album of the torrent holds is missing
	_, matched, missing = matchTrackFiles(files, []WantedTrack{{Key: "y", Title: "Live Song"}}, false)
	if len(matched) != 0 || !slices.Equal(missing, []string{"y"}) {
		t.Fatalf("other root into an existing album: %v %v", matched, missing)
	}
	// several albums and none of them the requested one: nothing
	all := slices.Clone(files)
	for i := range all {
		all[i].Checked = true
	}
	if _, matched, _ := matchTrackFiles(all, []WantedTrack{{Key: "x", Title: "Intro"}}, false); len(matched) != 0 {
		t.Fatalf("no requested root: %v", matched)
	}
	// a new album may take it (it is organized as that album)
	if _, matched, _ := matchTrackFiles(files, []WantedTrack{{Key: "y", Title: "Live Song"}}, true); matched["y"] != 3 {
		t.Fatalf("new album: %v", matched)
	}
	// a disc number counts too
	files = []ReleaseFile{
		{Index: 0, Path: "Album/CD1/01 - Intro.flac", Kind: fileKindTrack, Checked: true},
		{Index: 1, Path: "Album/CD2/01 - Intro.flac", Kind: fileKindTrack, Checked: true},
	}
	_, matched, _ = matchTrackFiles(files, []WantedTrack{{Key: "x", Title: "Intro", Disc: 2, No: 1}}, false)
	if matched["x"] != 1 {
		t.Fatalf("by disc: %v", matched)
	}
}

// release links of the plan tests
const (
	linkA = "https://t/a"
	linkB = "https://t/b"
	linkC = "https://t/c"
)

// planRelease makes the source answer link with a metafile named name.
func planRelease(t *testing.T, e *env, link, name string, paths ...string) []byte {
	t.Helper()
	mf := testMetafile(t, name, paths...)
	if e.idx.byLink == nil {
		e.idx.byLink = map[string]cardigann.DownloadResult{}
	}
	e.idx.byLink[link] = cardigann.DownloadResult{Torrent: mf}
	return mf
}

func kinoTracks() TrackAcquireRequest {
	return TrackAcquireRequest{ExtID: "rg-kino", Title: "Группа крови", Artist: "Кино", Year: 1988, UserID: 1,
		Tracks: []WantedTrack{{Key: "1", Title: "Группа крови"}, {Key: "2", Title: "Закрой за мной дверь"}}}
}

func TestPlanTracksPicksBestCoverage(t *testing.T) {
	ctx := context.Background()
	both := []string{"01 - Группа крови.flac", "02 - Закрой за мной дверь.flac"}
	one := []string{"01 - Группа крови.flac", "02 - Война.flac"}

	// A (better score) covers 1 of 2, B covers 2 of 2 → B
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("A", linkA, 1, 90), relx("B", linkB, 1, 50)}}
	planRelease(t, e, linkA, "Кино - Группа крови", one...)
	planRelease(t, e, linkB, "Кино - Группа крови (1988)", both...)
	p, err := e.svc.PlanTracks(ctx, kinoTracks())
	if err != nil {
		t.Fatal(err)
	}
	if p.ReleaseLink != linkB || p.ReleaseTitle != "B" || p.SourceID != 1 || !slices.Equal(p.Files, []int{0, 1}) ||
		len(p.Matched) != 2 || len(p.Missing) != 0 || p.Bytes != 2000 {
		t.Fatalf("plan %+v", p)
	}
	if q := e.idx.requests[0]; q.Kind != KindMusic || q.Year != 1988 {
		t.Fatalf("search %+v", q)
	}

	// equal coverage → A
	e = newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("A", linkA, 1, 90), relx("B", linkB, 1, 50)}}
	planRelease(t, e, linkA, "Кино - Группа крови", both...)
	planRelease(t, e, linkB, "Кино - Группа крови (1988)", both...)
	if p, err := e.svc.PlanTracks(ctx, kinoTracks()); err != nil || p.ReleaseLink != linkA {
		t.Fatalf("equal: %+v %v", p, err)
	}

	// C is in the engine already: skipped
	e = newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("C", linkC, 1, 99), relx("A", linkA, 1, 90)}}
	mfC := planRelease(t, e, linkC, "Кино - Группа крови FLAC", both...)
	e.eng.put(torrent.Status{InfoHash: metafileIH(t, mfC), Mode: ModeDownload})
	planRelease(t, e, linkA, "Кино - Группа крови", one...)
	p, err = e.svc.PlanTracks(ctx, kinoTracks())
	if err != nil || p.ReleaseLink != linkA || !slices.Equal(p.Missing, []string{"2"}) {
		t.Fatalf("added skipped: %+v %v", p, err)
	}

	// the only release holding the songs is in «Загрузки» already (a
	// follow-up of an album downloaded before): a conflict, not «not found»
	e = newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("C", linkC, 1, 99), relx("A", linkA, 1, 90)}}
	mfC = planRelease(t, e, linkC, "Кино - Группа крови FLAC", both...)
	h := e.eng.put(torrent.Status{InfoHash: metafileIH(t, mfC), Mode: ModeDownload})
	h.files = torrentFiles(t, t.TempDir(), "Кино - Группа крови FLAC/01 - Группа крови.flac", "Кино - Группа крови FLAC/02 - Закрой за мной дверь.flac")
	planRelease(t, e, linkA, "Кино - Ночь", "01 - Ночь.flac")
	_, err = e.svc.PlanTracks(ctx, kinoTracks())
	if msg, _ := UserMessage(err); !errors.Is(err, ErrConflict) || msg != msgReleaseAdded {
		t.Fatalf("added holds the songs: %v", err)
	}
	// an added release without the songs is no reason
	e = newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("C", linkC, 1, 99)}}
	mfC = planRelease(t, e, linkC, "Кино - Ночь", "01 - Ночь.flac")
	h = e.eng.put(torrent.Status{InfoHash: metafileIH(t, mfC), Mode: ModeDownload})
	h.files = torrentFiles(t, t.TempDir(), "Кино - Ночь/01 - Ночь.flac")
	if _, err := e.svc.PlanTracks(ctx, kinoTracks()); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("added without the songs: %v", err)
	}

	// only direct releases → ErrNoRelease
	e = newEnv(t)
	d := relx("OPDS", "https://opds/1", 2, 90)
	d.Direct = true
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{d}}
	_, err = e.svc.PlanTracks(ctx, kinoTracks())
	if msg, _ := UserMessage(err); !errors.Is(err, ErrNoRelease) || msg != "Подходящих вариантов не нашлось" {
		t.Fatalf("direct only: %v", err)
	}
	if len(e.idx.downloads) != 0 {
		t.Fatalf("downloads %v", e.idx.downloads)
	}

	// no release holds any of the songs → ErrNoRelease
	e = newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("A", linkA, 1, 90)}}
	planRelease(t, e, linkA, "Кино - Ночь", "01 - Ночь.flac")
	if _, err := e.svc.PlanTracks(ctx, kinoTracks()); !errors.Is(err, ErrNoRelease) {
		t.Fatalf("no coverage: %v", err)
	}

	// at most 3 releases are opened
	e = newEnv(t)
	var rels []indexer.ScoredRelease
	for i, l := range []string{"https://t/1", "https://t/2", "https://t/3", "https://t/4"} {
		rels = append(rels, relx("R", l, 1, 90-i))
		planRelease(t, e, l, "R"+l[len(l)-1:], "01 - Ночь.flac")
	}
	e.idx.resp = indexer.SearchResponse{Releases: rels}
	_, _ = e.svc.PlanTracks(ctx, kinoTracks())
	if len(e.idx.downloads) != 3 {
		t.Fatalf("downloads %v", e.idx.downloads)
	}
}

// albumDirLib is a library that knows album folders.
type albumDirLib struct {
	*fakeLib
	dirs map[int64]string
}

func (l albumDirLib) AlbumDir(_ context.Context, id int64) (string, error) {
	if d, ok := l.dirs[id]; ok {
		return d, nil
	}
	return "", library.ErrNotFound
}

func TestAcquireTracksPassesFiles(t *testing.T) {
	ctx := context.Background()
	paths := []string{"01 - Группа крови.flac", "02 - Война.flac", "03 - Спокойная ночь.flac", "04 - Мама, мы все тяжело больны.flac",
		"05 - Закрой за мной дверь.flac"}
	for _, existing := range []bool{false, true} {
		e := newEnv(t)
		e.svc.o.Library = albumDirLib{fakeLib: e.lib, dirs: map[int64]string{42: "/music/Кино/Группа крови"}}
		e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Кино - Группа крови", linkA, 1, 90)}}
		mf := planRelease(t, e, linkA, "Кино - Группа крови (1988)", paths...)
		e.eng.nextIH = metafileIH(t, mf)
		req := kinoTracks()
		if existing {
			req.AlbumID = 42
		}
		p, err := e.svc.PlanTracks(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		res, err := e.svc.AcquireTracks(ctx, req, p)
		if err != nil {
			t.Fatal(err)
		}
		if len(e.eng.added) != 1 || !slices.Equal(e.eng.added[0].Files, []int{0, 4}) || e.eng.added[0].Mode != ModeDownload {
			t.Fatalf("added %+v", e.eng.added)
		}
		if res.InfoHash != e.eng.nextIH || res.Files["1"] != 0 || res.Files["2"] != 4 || res.Bytes != 2000 || len(res.Missing) != 0 {
			t.Fatalf("result %+v", res)
		}
		a, err := e.svc.acquisition(ctx, res.InfoHash)
		if err != nil || !a.Meta.Picked || a.Meta.Artist != "Кино" || a.ExtID != "rg-kino" || a.Meta.Release != "Кино - Группа крови" {
			t.Fatalf("meta %+v %v", a, err)
		}
		want := ""
		if existing {
			want = "/music/Кино/Группа крови"
		}
		if len(e.svc.forced) != 0 || e.eng.added[0].WaitingSpace {
			t.Fatalf("forced %v", e.svc.forced)
		}
		if a.Meta.AlbumDir != want {
			t.Fatalf("albumDir %q, want %q", a.Meta.AlbumDir, want)
		}
	}
}

// A second download of an album that is downloading (the same release
// group, or into the same library album) is never started: Acquire
// returns the running one, PlanTracks and AcquireTracks say it runs.
func TestMusicDownloadNotDoubled(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.svc.o.Library = albumDirLib{fakeLib: e.lib, dirs: map[int64]string{42: "/music/Кино/Группа крови"}}
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Кино - Группа крови (1988) FLAC", "https://t/1", 1, 50)}}
	const first = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	e.eng.nextIH = first
	req := AcquireRequest{Kind: KindMusic, ExtID: "rg-kino", Title: "Группа крови", Artist: "Кино", Mode: ModeDownload, UserID: 1}
	res, err := e.svc.Acquire(ctx, req)
	if err != nil || res.InfoHash != first || res.Existing {
		t.Fatalf("first %+v %v", res, err)
	}
	e.eng.nextIH = "cccccccccccccccccccccccccccccccccccccccc"
	again, err := e.svc.Acquire(ctx, req)
	if err != nil || again.InfoHash != first || !again.Existing || len(e.eng.added) != 1 || len(e.idx.requests) != 1 {
		t.Fatalf("second %+v %v (%d added, %d searches)", again, err, len(e.eng.added), len(e.idx.requests))
	}
	if ih, ok, err := e.svc.InProgress(ctx, "rg-kino", 0, ""); err != nil || !ok || ih != first {
		t.Fatalf("in progress %q %v %v", ih, ok, err)
	}
	if _, err := e.svc.PlanTracks(ctx, kinoTracks()); !errors.Is(err, ErrInProgress) {
		t.Fatalf("plan: %v", err)
	}
	if _, err := e.svc.AcquireTracks(ctx, kinoTracks(), TrackPlan{SourceID: 1, ReleaseLink: "https://t/2", Files: []int{0}}); !errors.Is(err, ErrInProgress) {
		t.Fatalf("acquire tracks: %v", err)
	}
	if len(e.eng.added) != 1 {
		t.Fatalf("added %d", len(e.eng.added))
	}
	// another album is not held up
	if _, ok, _ := e.svc.InProgress(ctx, "rg-other", 0, ""); ok {
		t.Fatal("another release group in progress")
	}
	// organized: a new download may start
	if _, err := e.svc.o.DB.Exec(`UPDATE acquisitions SET organized_at = '2026-10-27T00:00:00Z' WHERE infohash = ?`, first); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.svc.InProgress(ctx, "rg-kino", 0, ""); ok {
		t.Fatal("an organized download is in progress")
	}

	// the same library album under another release group id
	const into = "dddddddddddddddddddddddddddddddddddddddd"
	e.eng.put(torrent.Status{InfoHash: into, Mode: ModeDownload})
	if err := e.svc.saveAcquisition(ctx, acquisition{InfoHash: into, Kind: KindMusic, Title: "Группа крови", ExtID: "rg-edition",
		Meta: acquisitionMeta{AlbumDir: "/music/Кино/Группа крови"}}, 1, "uid-1", false); err != nil {
		t.Fatal(err)
	}
	if ih, ok, err := e.svc.InProgress(ctx, "rg-kino", 42, ""); err != nil || !ok || ih != into {
		t.Fatalf("same album %q %v %v", ih, ok, err)
	}
	// a download the engine no longer has is not in progress
	if ih, ok, _ := e.svc.InProgress(ctx, "rg-edition", 0, ""); !ok || ih != into {
		t.Fatalf("by id %q", ih)
	}
	e.eng.mu.Lock()
	delete(e.eng.handles, into)
	e.eng.mu.Unlock()
	if _, ok, _ := e.svc.InProgress(ctx, "rg-edition", 42, ""); ok {
		t.Fatal("removed torrent in progress")
	}
}

// Two downloads of one album started at the same time: the second is
// refused while the first starts.
func TestStartAlbumOnceAtATime(t *testing.T) {
	e := newEnv(t)
	release, ok := e.svc.startAlbum("rg-kino", "")
	if !ok {
		t.Fatal("first start refused")
	}
	_, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindMusic, ExtID: "rg-kino", Title: "Группа крови", Artist: "Кино",
		Mode: ModeDownload, UserID: 1})
	if msg, _ := UserMessage(err); !errors.Is(err, ErrInProgress) || msg != "Альбом уже скачивается" {
		t.Fatalf("concurrent: %v", err)
	}
	if _, ok := e.svc.startAlbum("", "/m/x"); !ok {
		t.Fatal("another album refused")
	}
	release()
	if r, ok := e.svc.startAlbum("rg-kino", ""); !ok {
		t.Fatal("released album refused")
	} else {
		r()
	}
}

// An explicit pick («Выбрать релиз…», a track selection) of an album that
// is downloading: the same release is that download; another release is
// started as asked (the admin's choice). Only automatic starts return the
// running one.
func TestExplicitPickOfDownloadingAlbum(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	both := []string{"01 - Группа крови.flac", "02 - Закрой за мной дверь.flac"}
	mfA := planRelease(t, e, linkA, "Кино - Группа крови", both...)
	mfB := planRelease(t, e, linkB, "Кино - Группа крови (1988) FLAC", both...)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("A", linkA, 1, 90)}}
	ihA, ihB := metafileIH(t, mfA), metafileIH(t, mfB)
	e.eng.nextIH = ihA
	base := AcquireRequest{Kind: KindMusic, ExtID: "rg-kino", Title: "Группа крови", Artist: "Кино", Mode: ModeDownload, UserID: 1}
	if res, err := e.svc.Acquire(ctx, base); err != nil || res.InfoHash != ihA {
		t.Fatalf("first %+v %v", res, err)
	}

	// the same release picked by hand: that download
	same := base
	same.ReleaseLink, same.SourceID, same.ReleaseTitle = linkA, 1, "A"
	if res, err := e.svc.Acquire(ctx, same); err != nil || res.InfoHash != ihA || !res.Existing || len(e.eng.added) != 1 {
		t.Fatalf("same release %+v %v (%d added)", res, err, len(e.eng.added))
	}
	// the same release with a track selection: that download too
	if _, err := e.svc.ReleaseFiles(ctx, ReleaseFilesRequest{SourceID: 1, ReleaseLink: linkA, Title: "Группа крови", Artist: "Кино"}); err != nil {
		t.Fatal(err)
	}
	picked := same
	picked.Files = []int{0}
	if res, err := e.svc.Acquire(ctx, picked); err != nil || res.InfoHash != ihA || !res.Existing || len(e.eng.added) != 1 {
		t.Fatalf("same release, picked tracks %+v %v", res, err)
	}

	// another release picked by hand: started as asked
	other := base
	other.ReleaseLink, other.SourceID, other.ReleaseTitle = linkB, 1, "B"
	e.eng.nextIH = ihB
	res, err := e.svc.Acquire(ctx, other)
	if err != nil || res.InfoHash != ihB || res.Existing || len(e.eng.added) != 2 {
		t.Fatalf("other release %+v %v (%d added)", res, err, len(e.eng.added))
	}
	// a one-tap «На сервер» meanwhile: still the running one, nothing new
	if res, err := e.svc.Acquire(ctx, base); err != nil || !res.Existing || len(e.eng.added) != 2 {
		t.Fatalf("one tap %+v %v", res, err)
	}
}

// The import's own search after it gave up waiting on a download: that
// download no longer holds the album.
func TestPlanTracksIgnoresGivenUpDownload(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	both := []string{"01 - Группа крови.flac", "02 - Закрой за мной дверь.flac"}
	mfA := planRelease(t, e, linkA, "Кино - Группа крови", both...)
	planRelease(t, e, linkB, "Кино - Группа крови (1988)", both...)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("A", linkA, 1, 90), relx("B", linkB, 1, 50)}}
	e.eng.nextIH = metafileIH(t, mfA)
	if _, err := e.svc.Acquire(ctx, AcquireRequest{Kind: KindMusic, ExtID: "rg-kino", Title: "Группа крови", Artist: "Кино", Mode: ModeDownload, UserID: 1}); err != nil {
		t.Fatal(err)
	}
	req := kinoTracks()
	if _, err := e.svc.PlanTracks(ctx, req); !errors.Is(err, ErrInProgress) {
		t.Fatalf("waiting: %v", err)
	}
	req.IgnoreDownload = metafileIH(t, mfA)
	p, err := e.svc.PlanTracks(ctx, req)
	if err != nil || p.ReleaseLink != linkB {
		t.Fatalf("given up: %+v %v", p, err)
	}
	e.eng.nextIH = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	if res, err := e.svc.AcquireTracks(ctx, req, p); err != nil || res.InfoHash == req.IgnoreDownload {
		t.Fatalf("own download %+v %v", res, err)
	}
}
