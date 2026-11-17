package pinned

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// fakeFollows: the active follows, "kind:tmdb" → their author.
type fakeFollows map[string]int64

func (f fakeFollows) Following(_ context.Context, kind string, tmdbID int64) (addonkit.User, bool, error) {
	id, ok := f[fmt.Sprintf("%s:%d", kind, tmdbID)]
	if !ok || id == 0 {
		return addonkit.User{}, ok, nil
	}
	return by(id), ok, nil
}

type pinnedCall struct {
	ih        string
	req       acquire.AcquireRequest
	files     []int
	cancelled bool // the ctx could end (not a background one)
}

type fakeGrower struct {
	calls    []pinnedCall
	acquires []acquire.AcquireRequest
	// fail: the next calls fail with this
	fail    error
	pending []acquire.PendingAcquisition
	// onCall runs inside each AcquirePinnedFiles (the engine takes it)
	onCall func(ih string, files []int)
}

func (f *fakeGrower) Pending(context.Context) ([]acquire.PendingAcquisition, error) {
	return f.pending, nil
}

func (f *fakeGrower) AcquirePinnedFiles(ctx context.Context, ih string, req acquire.AcquireRequest, files []int) (acquire.AcquireResult, error) {
	f.calls = append(f.calls, pinnedCall{ih: ih, req: req, files: slices.Clone(files), cancelled: ctx.Done() != nil})
	if f.onCall != nil {
		f.onCall(ih, files)
	}
	if f.fail != nil {
		return acquire.AcquireResult{}, f.fail
	}
	return acquire.AcquireResult{InfoHash: ih}, nil
}

func (f *fakeGrower) Acquire(_ context.Context, req acquire.AcquireRequest) (acquire.AcquireResult, error) {
	f.acquires = append(f.acquires, req)
	return acquire.AcquireResult{}, nil
}

// actionsEnv: the title (TMDB 100) pinned to a release of S01E01–E<n> on
// source 3, refreshed by the fake core with the pinned scanner and the
// actions after each scan; the episode list is E01–E09 (E09 airs later);
// follows: none.
type actionsEnv struct {
	*env
	follows fakeFollows
	grower  *fakeGrower
	eps     []tracking.Episode
	pin     Pin
	link    string
}

func episodeFiles(n int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("Show.S01E%02d.1080p.mkv", i), fmt.Sprintf("RUS Sound/Show.S01E%02d.mka", i))
	}
	return out
}

func newActionsEnv(t *testing.T, n int) *actionsEnv {
	t.Helper()
	ctx := context.Background()
	a := &actionsEnv{env: newEnv(t), follows: fakeFollows{}, grower: &fakeGrower{}, link: "https://tracker.test/dl/1"}
	a.idx.downloads[a.link] = cardigann.DownloadResult{Torrent: metafile(t, releaseName, episodeFiles(n)...)}
	p, err := a.svc.PinRelease(ctx, a.title.ID, "3", a.link, "", releaseName, by(a.users[0]))
	if err != nil {
		t.Fatal(err)
	}
	a.pin = p
	var eps []tracking.Episode
	for i := 1; i <= 8; i++ {
		eps = append(eps, tracking.Episode{Season: 1, Number: i, AirDate: "2026-11-01"})
	}
	eps = append(eps, tracking.Episode{Season: 1, Number: 9, AirDate: "2026-12-01"})
	acts := &Actions{Pins: a.svc, Follows: a.follows, Acquire: a.grower}
	a.svc.AfterScan = acts.Sync
	a.eps = eps
	a.refresh() // the baseline
	return a
}

func (a *actionsEnv) refresh() {
	if err := a.track.Refresh(context.Background(), a.title.ID, a.eps, a.now, a.svc.Scanner()); err != nil {
		panic(err)
	}
}

// library puts S01E<eps> into the library (its tracking sources).
func (a *actionsEnv) library(t *testing.T, eps ...int) {
	t.Helper()
	var srcs []tracking.Source
	for _, n := range eps {
		srcs = append(srcs, tracking.Source{TitleID: a.title.ID, Season: 1, Episode: n, Kind: "library", Ref: fmt.Sprintf("item:%d", n)})
	}
	a.track.UpsertSources(a.title.ID, "library", srcs, a.now)
}

// reupload: the tracker's topic now holds E01–E<n>; the pin is due again.
func (a *actionsEnv) reupload(t *testing.T, n int) string {
	t.Helper()
	mf := metafile(t, releaseName, episodeFiles(n)...)
	a.idx.downloads[a.link] = cardigann.DownloadResult{Torrent: mf}
	a.now = a.now.Add(7 * time.Hour)
	return ihOf(t, mf)
}

func (a *actionsEnv) calls() string {
	return fmt.Sprintf("pinned files %+v, acquires %+v", a.grower.calls, a.grower.acquires)
}

// TestMarkedOnlyNeverDownloads: the owner's rule. A title only marked
// (watching, no «Докачивать новые») whose pinned release gets E08 is told
// about, never downloaded — also when the pin's torrent is in the engine,
// and with nothing of it in the library.
func TestMarkedOnlyNeverDownloads(t *testing.T) {
	ctx := context.Background()
	a := newActionsEnv(t, 7)
	// (the mark is the core's: no follow is all the actions see)
	a.eng.has[a.pin.InfoHash] = true
	a.eng.modes = map[string]string{a.pin.InfoHash: torrent.ModeStream}
	newIH := a.reupload(t, 8)
	a.refresh()
	srcs, _ := a.svc.Tracking.Sources(ctx, a.title.ID)
	if len(srcs) != 8 || !a.eng.has[newIH] {
		t.Fatalf("sources %d, engine %v", len(srcs), a.eng.has)
	}
	if len(a.grower.calls) != 0 || len(a.grower.acquires) != 0 {
		t.Fatalf("a marked-only title downloaded: %s", a.calls())
	}
	// a follow of another title, or of the other kind, changes nothing
	a.follows["movie:100"], a.follows["series:101"] = 1, 1
	if err := (&Actions{Pins: a.svc, Follows: a.follows, Acquire: a.grower}).Sync(ctx, a.title); err != nil {
		t.Fatal(err)
	}
	if len(a.grower.calls) != 0 || len(a.grower.acquires) != 0 {
		t.Fatalf("downloaded for another follow: %s", a.calls())
	}
}

// TestFollowOnDownloadsOnlyMissingPinnedEpisodes: with «Докачивать новые»
// on, the pin (E01–E08) and the library holding E01–E06, only E07 and E08
// are downloaded from the pin (videos and tracks), never the season; the
// pin's download selecting E07 already leaves E08; a stream selects
// nothing yet.
func TestFollowOnDownloadsOnlyMissingPinnedEpisodes(t *testing.T) {
	a := newActionsEnv(t, 8)
	a.library(t, 1, 2, 3, 4, 5, 6)
	a.follows["series:100"] = a.users[1]
	a.refresh()
	if len(a.grower.calls) != 1 || len(a.grower.acquires) != 0 {
		t.Fatalf("%s", a.calls())
	}
	c := a.grower.calls[0]
	want := acquire.AcquireRequest{TMDBID: 100, Kind: acquire.KindSeries, Season: 1, Mode: acquire.ModeDownload,
		ReleaseLink: a.link, SourceID: 3, UserID: a.users[1], UserUID: by(a.users[1]).UID, Background: true}
	if c.ih != a.pin.InfoHash || fmt.Sprint(c.files) != "[12 13 14 15]" || c.cancelled || fmt.Sprintf("%+v", c.req) != fmt.Sprintf("%+v", want) {
		t.Fatalf("call %+v\nwant req %+v", c, want)
	}

	// downloading in the engine with E07 selected: E08 only
	a.eng.has[a.pin.InfoHash] = true
	a.eng.modes = map[string]string{a.pin.InfoHash: torrent.ModeDownload}
	var files []torrent.FileInfo
	for i := range 16 {
		files = append(files, torrent.FileInfo{Index: i, Selected: i == 12 || i == 13})
	}
	a.eng.data = map[string]*fakeData{a.pin.InfoHash: {files: files}}
	a.refresh()
	if len(a.grower.calls) != 2 || fmt.Sprint(a.grower.calls[1].files) != "[14 15]" {
		t.Fatalf("download: %s", a.calls())
	}
	// both selected: nothing more
	files[14].Selected, files[15].Selected = true, true
	a.refresh()
	if len(a.grower.calls) != 2 {
		t.Fatalf("all selected: %s", a.calls())
	}
	// streamed: a stream's files are no download, E07 and E08 again
	a.eng.modes[a.pin.InfoHash] = torrent.ModeStream
	a.refresh()
	if len(a.grower.calls) != 3 || fmt.Sprint(a.grower.calls[2].files) != "[12 13 14 15]" {
		t.Fatalf("stream: %s", a.calls())
	}
	// in the library at last: nothing
	a.library(t, 1, 2, 3, 4, 5, 6, 7, 8)
	a.refresh()
	if len(a.grower.calls) != 3 {
		t.Fatalf("in the library: %s", a.calls())
	}
}

// TestFailedPinnedEpisodeRetriedNextRefresh: a download of the missing
// episodes that failed is asked again at the next refresh.
func TestFailedPinnedEpisodeRetriedNextRefresh(t *testing.T) {
	a := newActionsEnv(t, 8)
	a.library(t, 1, 2, 3, 4, 5, 6, 7)
	a.follows["series:100"] = 1
	a.grower.fail = fmt.Errorf("tracker down")
	a.refresh()
	a.now = a.now.Add(time.Hour)
	a.grower.fail = nil
	a.refresh()
	if len(a.grower.calls) != 2 || fmt.Sprint(a.grower.calls[0].files) != "[14 15]" || fmt.Sprint(a.grower.calls[1].files) != "[14 15]" {
		t.Fatalf("%s", a.calls())
	}
}

// TestFollowSkipsUnairedPinnedEpisode: an episode the pin holds before its
// air date is not downloaded yet.
func TestFollowSkipsUnairedPinnedEpisode(t *testing.T) {
	a := newActionsEnv(t, 9)
	a.library(t, 1, 2, 3, 4, 5, 6, 7, 8)
	a.follows["series:100"] = 1
	a.refresh()
	if len(a.grower.calls) != 0 {
		t.Fatalf("%s", a.calls())
	}
}

// TestCoveredIsPinEpisodes: the episodes the title's pin holds; none for a
// title without a pin or not tracked.
func TestCoveredIsPinEpisodes(t *testing.T) {
	ctx := context.Background()
	a := newActionsEnv(t, 7)
	got, err := a.svc.Covered(ctx, 100)
	if err != nil || len(got) != 7 || !got[[2]int{1, 7}] || got[[2]int{1, 8}] {
		t.Fatalf("covered %v %v", got, err)
	}
	if got, err := a.svc.Covered(ctx, 555); err != nil || len(got) != 0 {
		t.Fatalf("untracked %v %v", got, err)
	}
}

// TestReplaceTellsReplaced: a refresh that replaces the engine's torrent
// tells Replaced (acquire moves its download record to the new infohash).
func TestReplaceTellsReplaced(t *testing.T) {
	a := newActionsEnv(t, 7)
	var moved []string
	a.svc.Replaced = func(_ context.Context, oldIH, newIH string) error {
		moved = append(moved, oldIH+">"+newIH)
		return nil
	}
	a.eng.has[a.pin.InfoHash] = true
	newIH := a.reupload(t, 8)
	a.refresh()
	if fmt.Sprint(moved) != "["+a.pin.InfoHash+">"+newIH+"]" {
		t.Fatalf("moved %v", moved)
	}
}

// TestFollowedTitles_FromFollows: the tracking.titles hook's answer is
// the active follows of series and films, with their names and posters
// (the core makes and refreshes the titles; no core table is touched).
func TestFollowedTitles_FromFollows(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	if got, err := FollowedTitles(e.svc.DB)(ctx); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no follows: %#v %v", got, err)
	}
	if _, err := e.svc.DB.Exec(`INSERT INTO follows (kind, tmdb_id, title, poster, active, created_at) VALUES
		  ('series', 100, 'Show', '', 1, 'x'), ('movie', 7, 'Film', 'https://img/w342/f.jpg', 1, 'x'), ('series', 8, 'Off', '', 0, 'x'),
		  ('album', 9, 'Album', '', 1, 'x')`); err != nil {
		t.Fatal(err)
	}
	got, err := FollowedTitles(e.svc.DB)(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []addonkit.ExtraTitle{{Kind: "series", TMDBID: 100, Name: "Show"}, {Kind: "movie", TMDBID: 7, Name: "Film", Poster: "https://img/w342/f.jpg"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("titles %+v, want %+v", got, want)
	}
	if len(e.track.Titles) != 1 {
		t.Fatalf("the hook made titles itself: %+v", e.track.Titles)
	}
}

// TestPinnedFilmNotDoubledWithFollow: a followed film whose download is
// pending (the follow's own pick) is not downloaded again from its pin;
// with none pending, the pin is.
func TestPinnedFilmNotDoubledWithFollow(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	film, err := e.track.EnsureTitle(ctx, "movie", 200, "Film", "")
	if err != nil {
		t.Fatal(err)
	}
	link := "https://tracker.test/dl/film"
	e.idx.downloads[link] = cardigann.DownloadResult{Torrent: metafile(t, "Film.2024.1080p", "Film.2024.1080p.mkv")}
	if _, err := e.svc.PinRelease(ctx, film.ID, "3", link, "", "Film 2024 1080p", by(e.users[0])); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.svc.FilmPinned(ctx, 200); err != nil || !ok {
		t.Fatalf("film pinned %v %v", ok, err)
	}
	if ok, _ := e.svc.FilmPinned(ctx, 201); ok {
		t.Fatal("another film pinned")
	}
	g := &fakeGrower{pending: []acquire.PendingAcquisition{{Kind: acquire.KindMovie, TMDBID: 200, InfoHash: strings.Repeat("9", 40)}}}
	acts := &Actions{Pins: e.svc, Follows: fakeFollows{"movie:200": 1}, Acquire: g}
	if err := acts.Sync(ctx, film); err != nil {
		t.Fatal(err)
	}
	if len(g.acquires) != 0 {
		t.Fatalf("doubled: %+v", g.acquires)
	}
	g.pending = nil
	if err := acts.Sync(ctx, film); err != nil {
		t.Fatal(err)
	}
	if len(g.acquires) != 1 || g.acquires[0].Kind != acquire.KindMovie || g.acquires[0].ReleaseLink != link {
		t.Fatalf("acquires %+v", g.acquires)
	}
}

// TestPinnedEpisodeNotDoubledWithPendingFollow: an episode (or a season)
// another download not imported yet holds is not downloaded from the pin;
// the pin's own download does not count.
func TestPinnedEpisodeNotDoubledWithPendingFollow(t *testing.T) {
	ctx := context.Background()
	a := newActionsEnv(t, 8)
	a.library(t, 1, 2, 3, 4, 5, 6)
	a.follows["series:100"] = 1
	other := strings.Repeat("8", 40)
	acts := &Actions{Pins: a.svc, Follows: a.follows, Acquire: a.grower}
	for _, tc := range []struct {
		pending []acquire.PendingAcquisition
		want    string
	}{
		{[]acquire.PendingAcquisition{{Kind: acquire.KindSeries, TMDBID: 100, Season: 1, Episode: 7, InfoHash: other}}, "[14 15]"},
		{[]acquire.PendingAcquisition{{Kind: acquire.KindSeries, TMDBID: 100, Season: 1, Episode: 7, EpisodeEnd: 8, InfoHash: other}}, "none"},
		{[]acquire.PendingAcquisition{{Kind: acquire.KindSeries, TMDBID: 100, Season: 1, InfoHash: other}}, "none"},
		// a whole-series pack (no season known): every season is in it
		{[]acquire.PendingAcquisition{{Kind: acquire.KindSeries, TMDBID: 100, InfoHash: other}}, "none"},
		// a multi-season pack, one row per season: only its seasons
		{[]acquire.PendingAcquisition{{Kind: acquire.KindSeries, TMDBID: 100, Season: 2, InfoHash: other}, {Kind: acquire.KindSeries, TMDBID: 100, Season: 3, InfoHash: other}}, "[12 13 14 15]"},
		{[]acquire.PendingAcquisition{{Kind: acquire.KindSeries, TMDBID: 100, Season: 1, InfoHash: other}, {Kind: acquire.KindSeries, TMDBID: 100, Season: 2, InfoHash: other}}, "none"},
		{[]acquire.PendingAcquisition{{Kind: acquire.KindSeries, TMDBID: 100, Season: 1, InfoHash: a.pin.InfoHash}}, "[12 13 14 15]"},
		{[]acquire.PendingAcquisition{{Kind: acquire.KindSeries, TMDBID: 101, Season: 1, InfoHash: other}, {Kind: acquire.KindMovie, TMDBID: 100, InfoHash: other}}, "[12 13 14 15]"},
	} {
		a.grower.calls, a.grower.pending = nil, tc.pending
		if err := acts.Sync(ctx, a.title); err != nil {
			t.Fatal(err)
		}
		got := "none"
		if len(a.grower.calls) == 1 {
			got = fmt.Sprint(a.grower.calls[0].files)
		}
		if got != tc.want || len(a.grower.calls) > 1 {
			t.Fatalf("pending %+v: %s, want %s", tc.pending, a.calls(), tc.want)
		}
	}
}

// TestSyncSerializedPerTitle: two Syncs of a title at once add the pin
// once (the second sees the first's download).
func TestSyncSerializedPerTitle(t *testing.T) {
	a := newActionsEnv(t, 8)
	a.library(t, 1, 2, 3, 4, 5, 6)
	a.follows["series:100"] = 1
	a.eng.modes, a.eng.data = map[string]string{}, map[string]*fakeData{}
	a.grower.onCall = func(ih string, files []int) {
		time.Sleep(30 * time.Millisecond)
		var fi []torrent.FileInfo
		for i := range 16 {
			fi = append(fi, torrent.FileInfo{Index: i, Selected: slices.Contains(files, i)})
		}
		a.eng.data[ih] = &fakeData{files: fi}
		a.eng.modes[ih] = torrent.ModeDownload
		a.eng.has[ih] = true
	}
	acts := &Actions{Pins: a.svc, Follows: a.follows, Acquire: a.grower}
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- acts.Sync(context.Background(), a.title) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if len(a.grower.calls) != 1 {
		t.Fatalf("%s", a.calls())
	}
	if len(acts.locks) != 0 {
		t.Fatalf("locks left %v", acts.locks)
	}
}

// TestFailedPinDownloadRetried: the files of a pin download that failed
// are not counted as selected.
func TestFailedPinDownloadRetried(t *testing.T) {
	a := newActionsEnv(t, 8)
	a.library(t, 1, 2, 3, 4, 5, 6, 7)
	a.follows["series:100"] = 1
	a.eng.has[a.pin.InfoHash] = true
	a.eng.modes = map[string]string{a.pin.InfoHash: torrent.ModeDownload}
	var fi []torrent.FileInfo
	for i := range 16 {
		fi = append(fi, torrent.FileInfo{Index: i, Selected: true})
	}
	a.eng.data = map[string]*fakeData{a.pin.InfoHash: {files: fi, status: torrent.StatusError}}
	a.refresh()
	if len(a.grower.calls) != 1 || fmt.Sprint(a.grower.calls[0].files) != "[14 15]" {
		t.Fatalf("%s", a.calls())
	}
}

// TestPinnedSync_UsesTrackingClient: Sync reads the title's episode list
// and its library sources from the core's tracking API (the fake): an
// episode that airs later is not wanted, one the library has is not; the
// core unreachable fails the Sync and downloads nothing.
func TestPinnedSync_UsesTrackingClient(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	link := "https://tracker.test/dl/1"
	e.idx.downloads[link] = cardigann.DownloadResult{Torrent: metafile(t, releaseName, episodeFiles(3)...)}
	if _, err := e.svc.PinRelease(ctx, e.title.ID, "3", link, "", releaseName, by(e.users[0])); err != nil {
		t.Fatal(err)
	}
	e.track.Episodes = map[int64][]tracking.Episode{e.title.ID: {
		{Season: 1, Number: 1, AirDate: "2026-11-01"}, {Season: 1, Number: 2, AirDate: "2026-11-01"}, {Season: 1, Number: 3, AirDate: "2026-12-01"}}}
	e.track.UpsertSources(e.title.ID, "library", []tracking.Source{{Season: 1, Episode: 1, Ref: "file:1"}}, e.now)
	g := &fakeGrower{}
	acts := &Actions{Pins: e.svc, Follows: fakeFollows{"series:100": e.users[1]}, Acquire: g}
	if err := acts.Sync(ctx, e.title); err != nil {
		t.Fatal(err)
	}
	if len(g.calls) != 1 || fmt.Sprint(g.calls[0].files) != "[2 3]" || g.calls[0].req.UserID != e.users[1] {
		t.Fatalf("calls %+v", g.calls)
	}
	e.track.Err = errors.New("core down")
	g.calls = nil
	if err := acts.Sync(ctx, e.title); err == nil || len(g.calls) != 0 {
		t.Fatalf("core down: %v, calls %+v", err, g.calls)
	}
}
