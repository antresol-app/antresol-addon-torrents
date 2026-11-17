package follow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/events"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol/addonkit"
)

type fakeAcq struct {
	mu      sync.Mutex
	calls   []acquire.AcquireRequest
	err     error
	pending []acquire.PendingAcquisition
	disk    acquire.DiskUsage
	diskErr error
	// errFor: an error for one {season, episode}
	errFor map[[2]int]error
	// active: Acquire calls running at once (max seen in maxActive)
	active, maxActive int
	delay             time.Duration
	note              string // AcquireResult.Note of each download
}

func (f *fakeAcq) Acquire(_ context.Context, r acquire.AcquireRequest) (acquire.AcquireResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, r)
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	err := f.err
	if e, ok := f.errFor[[2]int{r.Season, r.Episode}]; ok {
		err = e
	}
	d := f.delay
	f.mu.Unlock()
	time.Sleep(d)
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	if err != nil {
		return acquire.AcquireResult{}, err
	}
	return acquire.AcquireResult{InfoHash: "ih", Status: "downloading", Note: f.note}, nil
}

func (f *fakeAcq) Pending(context.Context) ([]acquire.PendingAcquisition, error) {
	return f.pending, nil
}

func (f *fakeAcq) Disk() (acquire.DiskUsage, error) {
	if f.diskErr != nil {
		return acquire.DiskUsage{}, f.diskErr
	}
	if f.disk.Level == "" {
		return acquire.DiskUsage{Level: acquire.LevelOK}, nil
	}
	return f.disk, nil
}

func (f *fakeAcq) requests() []acquire.AcquireRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]acquire.AcquireRequest(nil), f.calls...)
}

type fakeTMDB struct {
	offline bool
	tvErr   map[int]error // per series id
	tv      tmdb.Details
	seasons map[int]tmdb.SeasonDetails
	movie   tmdb.Details
	calls   int
}

var errDown = errors.New("dial tcp: connection refused")

func (f *fakeTMDB) TV(_ context.Context, id int) (tmdb.Details, error) {
	f.calls++
	if f.offline {
		return tmdb.Details{}, errDown
	}
	if err := f.tvErr[id]; err != nil {
		return tmdb.Details{}, err
	}
	d := f.tv
	d.ID = id
	return d, nil
}

func (f *fakeTMDB) Season(_ context.Context, _, n int) (tmdb.SeasonDetails, error) {
	f.calls++
	if f.offline {
		return tmdb.SeasonDetails{}, errDown
	}
	s, ok := f.seasons[n]
	if !ok {
		return s, tmdb.ErrNotFound
	}
	return s, nil
}

func (f *fakeTMDB) Movie(_ context.Context, id int) (tmdb.Details, error) {
	f.calls++
	if f.offline {
		return tmdb.Details{}, errDown
	}
	d := f.movie
	d.ID = id
	return d, nil
}

func (f *fakeTMDB) ImageURL(size, p string) string { return "https://img/" + size + p }

type env struct {
	t     *testing.T
	svc   *Service
	acq   *fakeAcq
	tm    *fakeTMDB
	lib   *coretest.Library
	users *coretest.Users
	now   time.Time
	bus   *events.Bus
}

func newEnv(t *testing.T) *env {
	d, err := db.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	e := &env{t: t, acq: &fakeAcq{}, lib: &coretest.Library{}, users: &coretest.Users{Names: map[int64]string{}}, now: today, bus: &events.Bus{}}
	e.tm = &fakeTMDB{
		tv:      tmdb.Details{Title: "Сёгун", OriginalTitle: "Shōgun", Year: 2024, Poster: "/p.jpg", Seasons: []tmdb.SeasonCount{{Number: 0, Episodes: 2}, {Number: 1, Episodes: 10}}},
		seasons: map[int]tmdb.SeasonDetails{0: season(0, 2, 0), 1: season(1, 10, 0)},
		movie:   tmdb.Details{Title: "Дюна", Year: 2024, ReleaseDate: "2024-02-27", Poster: "/d.jpg"},
	}
	e.svc = &Service{DB: d, Acquire: e.acq, TMDB: e.tm, Library: e.lib, Users: e.users.Adapter(), Titles: e.lib,
		Now: func() time.Time { return e.now }, Bus: e.bus, Pause: -1}
	return e
}

// item adds it to the fake library (the next id) and returns its id.
func (e *env) item(it library.Item) int64 {
	it.ID = int64(len(e.lib.Items) + 1)
	if it.MetaJSON == "" {
		it.MetaJSON = "{}"
	}
	e.lib.Items = append(e.lib.Items, it)
	return it.ID
}

// child is the library item of kind and number under parent, made when
// absent.
func (e *env) child(parent int64, kind string, n int) int64 {
	for _, it := range e.lib.Items {
		if it.ParentID == parent && it.Kind == kind && (it.SeasonNo == n && kind == library.KindSeason || it.EpisodeNo == n && kind == library.KindEpisode) {
			return it.ID
		}
	}
	it := library.Item{Kind: kind, ParentID: parent}
	if kind == library.KindSeason {
		it.SeasonNo = n
	} else {
		it.EpisodeNo = n
	}
	return e.item(it)
}

// episodeFile puts S<season>E<ep> of the series «Сёгун» (tmdbID; 0: not
// matched yet) into the library with the file at path.
func (e *env) episodeFile(tmdbID int64, s, ep int, path string) {
	series := int64(0)
	for _, it := range e.lib.Items {
		if it.Kind == library.KindSeries && it.TmdbID == tmdbID && it.ParentID == 0 {
			series = it.ID
		}
	}
	if series == 0 {
		series = e.item(library.Item{Kind: library.KindSeries, Title: "Сёгун", TmdbID: tmdbID})
	}
	id := e.child(e.child(series, library.KindSeason, s), library.KindEpisode, ep)
	e.lib.Files = append(e.lib.Files, library.File{ID: int64(len(e.lib.Files) + 1), ItemID: id, Path: path, Size: 1})
}

// episodes puts S<season>E<eps...> of the series into the library.
func (e *env) episodes(tmdbID int64, s int, eps ...int) {
	for _, n := range eps {
		e.episodeFile(tmdbID, s, n, filepath.Join(e.t.TempDir(), "e.mkv"))
	}
}

func (e *env) add(kind string, id int) Follow {
	f, err := e.svc.Add(context.Background(), addonkit.User{}, kind, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *env) get(id int64) Follow {
	f, err := e.svc.Get(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return f
}

func targets(rs []acquire.AcquireRequest) [][2]int {
	out := [][2]int{}
	for _, r := range rs {
		out = append(out, [2]int{r.Season, r.Episode})
	}
	return out
}

func TestSkipsPending(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7)
	e.acq.pending = []acquire.PendingAcquisition{{Kind: "series", TMDBID: 42, Season: 1, Episode: 8, InfoHash: "x"},
		{Kind: "series", TMDBID: 7, Season: 1, Episode: 9, InfoHash: "y"}} // another series
	f := e.add("series", 42)
	res, err := e.svc.Check(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := targets(e.acq.requests()), [][2]int{{1, 9}, {1, 10}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("acquired %v, want %v", got, want)
	}
	for _, r := range e.acq.requests() {
		if r.Kind != "series" || r.TMDBID != 42 || r.Mode != "download" {
			t.Fatalf("request %+v", r)
		}
	}
	if !reflect.DeepEqual(res.Queued, []string{"S01E09", "S01E10"}) || res.Missing != 2 {
		t.Fatalf("result %+v", res)
	}
	if g := e.get(f.ID); g.Missing != 2 || g.LastChecked == nil || g.LastError != "" {
		t.Fatalf("follow %+v", g)
	}
}

func TestSkipsPendingSeasonPack(t *testing.T) {
	e := newEnv(t)
	e.acq.pending = []acquire.PendingAcquisition{{Kind: "series", TMDBID: 42, Season: 1, InfoHash: "x"}}
	f := e.add("series", 42)
	if _, err := e.svc.Check(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.acq.requests()); n != 0 {
		t.Fatalf("season pack is queued, yet %v", e.acq.requests())
	}
}

func TestNeverRedownloadsLibrary(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	f := e.add("series", 42)
	if _, err := e.svc.Check(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.acq.requests()); n != 0 {
		t.Fatalf("everything is in the library, yet %v", e.acq.requests())
	}
	// a series scanned but not matched to TMDB yet: nothing is fetched
	e2 := newEnv(t)
	e2.episodes(0, 1, 1)
	f2 := e2.add("series", 42)
	if _, err := e2.svc.Check(context.Background(), f2.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(e2.acq.requests()); n != 0 {
		t.Fatalf("unmatched library series, yet %v", e2.acq.requests())
	}
	if g := e2.get(f2.ID); g.LastError == "" {
		t.Fatalf("no reason given: %+v", g)
	}
}

func TestInactiveNotChecked(t *testing.T) {
	e := newEnv(t)
	f := e.add("series", 42)
	if err := e.svc.SetActive(context.Background(), f.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.CheckAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(e.acq.requests()); n != 0 || e.get(f.ID).LastChecked != nil {
		t.Fatalf("inactive follow checked: %v", e.acq.requests())
	}
	if _, err := e.svc.Check(context.Background(), f.ID); !errors.Is(err, ErrInactive) {
		t.Fatalf("check of an inactive follow: %v", err)
	}
}

func TestBackoffAfterNoRelease(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	e.acq.err = errors.Join(acquire.ErrNoRelease, errors.New("Раздач не нашлось"))
	f := e.add("series", 42)
	ctx := context.Background()
	if _, err := e.svc.Check(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.acq.requests()); n != 1 {
		t.Fatalf("first check: %v", e.acq.requests())
	}
	if g := e.get(f.ID); g.LastError == "" {
		t.Fatalf("no release is not reported: %+v", g)
	}
	e.now = e.now.Add(23 * time.Hour)
	if _, err := e.svc.Check(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.acq.requests()); n != 1 {
		t.Fatalf("retried within 24 h: %v", e.acq.requests())
	}
	e.now = e.now.Add(2 * time.Hour) // 25 h after the miss
	e.acq.err = nil
	res, err := e.svc.Check(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.acq.requests()); n != 2 || !reflect.DeepEqual(res.Queued, []string{"S01E10"}) {
		t.Fatalf("not retried after 25 h: %v %+v", e.acq.requests(), res)
	}
}

func TestMovieFollowDoneWhenInLibrary(t *testing.T) {
	e := newEnv(t)
	f := e.add("movie", 693134)
	id := e.item(library.Item{Kind: library.KindMovie, Title: "Дюна", Year: 2024, TmdbID: 693134})
	e.lib.Files = append(e.lib.Files, library.File{ID: 1, ItemID: id, Path: filepath.Join(t.TempDir(), "d.mkv"), Size: 1})
	if _, err := e.svc.Check(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	if g := e.get(f.ID); g.Active || len(e.acq.requests()) != 0 {
		t.Fatalf("follow %+v, requests %v", g, e.acq.requests())
	}
}

func TestMovieAcquiredOnceReleased(t *testing.T) {
	e := newEnv(t)
	f := e.add("movie", 693134)
	res, err := e.svc.Check(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	rs := e.acq.requests()
	if len(rs) != 1 || rs[0].Kind != "movie" || rs[0].TMDBID != 693134 || rs[0].Mode != "download" || !reflect.DeepEqual(res.Queued, []string{"фильм"}) {
		t.Fatalf("%v %+v", rs, res)
	}
	// queued now: the next check does not search again
	e.acq.pending = []acquire.PendingAcquisition{{Kind: "movie", TMDBID: 693134, InfoHash: "ih"}}
	if _, err := e.svc.Check(context.Background(), f.ID); err != nil || len(e.acq.requests()) != 1 {
		t.Fatalf("%v %v", err, e.acq.requests())
	}
}

func TestMovieNotReleasedNotSearched(t *testing.T) {
	e := newEnv(t)
	e.tm.movie.ReleaseDate = "2024-12-18"
	f := e.add("movie", 1)
	if _, err := e.svc.Check(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	// no date: the year decides
	e.tm.movie.ReleaseDate, e.tm.movie.Year = "", 2025
	f2 := e.add("movie", 2)
	if _, err := e.svc.Check(context.Background(), f2.ID); err != nil {
		t.Fatal(err)
	}
	if n := len(e.acq.requests()); n != 0 {
		t.Fatalf("unreleased movie searched: %v", e.acq.requests())
	}
	if !e.get(f.ID).Active {
		t.Fatal("unreleased movie follow deactivated")
	}
}

func TestFollowOffline(t *testing.T) {
	e := newEnv(t)
	f := e.add("series", 42)
	e.tm.offline = true
	if _, err := e.svc.Check(context.Background(), f.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err %v", err)
	}
	if g := e.get(f.ID); g.LastError != "Нет связи с TMDB" || g.LastChecked == nil {
		t.Fatalf("follow %+v", g)
	}
	if n := len(e.acq.requests()); n != 0 {
		t.Fatalf("acquired offline: %v", e.acq.requests())
	}
	// adding needs TMDB for the title
	if _, err := e.svc.Add(context.Background(), addonkit.User{}, "movie", 5); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("add offline: %v", err)
	}
}

func TestFollowDiskFull(t *testing.T) {
	e := newEnv(t)
	e.acq.disk = acquire.DiskUsage{Level: acquire.LevelFull, Used: 96, Total: 100}
	f := e.add("series", 42)
	res, err := e.svc.Check(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.acq.requests()); n != 0 || res.Missing != 10 {
		t.Fatalf("disk full: %v %+v", e.acq.requests(), res)
	}
	if g := e.get(f.ID); g.LastError == "" {
		t.Fatalf("no reason given: %+v", g)
	}
}

func TestFollowUniquePerTitle(t *testing.T) {
	e := newEnv(t)
	a := e.add("series", 42)
	b := e.add("series", 42)
	c := e.add("movie", 42)
	if a.ID != b.ID || a.ID == c.ID {
		t.Fatalf("%+v %+v %+v", a, b, c)
	}
	if a.Title != "Сёгун" || a.Poster != "https://img/w342/p.jpg" || !a.Active {
		t.Fatalf("%+v", a)
	}
	list, err := e.svc.List(context.Background())
	if err != nil || len(list) != 2 {
		t.Fatalf("%v %v", list, err)
	}
	if _, err := e.svc.Add(context.Background(), addonkit.User{}, "album", 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("album: %v", err)
	}
	if err := e.svc.Remove(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Remove(context.Background(), a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
}

func TestFollowQueuedEvent(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	ch, stop := e.bus.Subscribe()
	defer stop()
	f := e.add("series", 42)
	if _, err := e.svc.Check(context.Background(), f.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		q, ok := ev.Data.(Queued)
		if ev.Type != EventFollowQueued || !ok || q.FollowID != f.ID || !reflect.DeepEqual(q.Queued, []string{"S01E10"}) {
			t.Fatalf("%+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no follow.queued")
	}
}

func TestPrefersSeasonPackAcquire(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1)
	f := e.add("series", 42)
	res, err := e.svc.Check(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := targets(e.acq.requests()); !reflect.DeepEqual(got, [][2]int{{1, 0}}) || !reflect.DeepEqual(res.Queued, []string{"S01"}) || res.Missing != 9 {
		t.Fatalf("%v %+v", got, res)
	}
}

// any adult may switch off or remove any follow, so
// the list says who added each one (the core's profiles).
func TestFollowAddedBy(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.users.Names[5] = "Мама"
	f, err := e.svc.Add(ctx, e.users.User(5), "series", 42)
	if err != nil {
		t.Fatal(err)
	}
	if f.AddedBy != "Мама" || f.UserID != 5 {
		t.Fatalf("addedBy %q %d", f.AddedBy, f.UserID)
	}
	if by, on, err := e.svc.Following(ctx, "series", 42); err != nil || !on || by.ID != 5 {
		t.Fatalf("following %+v %v %v", by, on, err)
	}
	anon := e.add("movie", 7)
	if anon.AddedBy != "" {
		t.Fatalf("no author: %q", anon.AddedBy)
	}
	// the core unreachable: the list still comes, its authors unnamed
	e.users.Err = errors.New("core down")
	list, err := e.svc.List(ctx)
	if err != nil || len(list) != 2 || list[1].AddedBy != "" || list[1].UserID != 5 {
		t.Fatalf("core down: %+v %v", list, err)
	}
}

// TestFollowList_DeletedProfile: a follow whose profile is gone
// stays, listed as «Профиль удалён» (English in English) with no author:
// its downloads and events name nobody, and nothing deletes it.
func TestFollowList_DeletedProfile(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.users.Names[5] = "Мама"
	f, err := e.svc.Add(ctx, e.users.User(5), "series", 42)
	if err != nil {
		t.Fatal(err)
	}
	delete(e.users.Names, 5) // the profile is deleted in the core
	list, err := e.svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("%v %v", list, err)
	}
	if g := list[0]; g.ID != f.ID || g.AddedBy != "Профиль удалён" || g.UserID != 0 {
		t.Fatalf("follow of a deleted profile %+v", g)
	}
	if g, err := e.svc.Get(i18n.WithLanguage(ctx, i18n.EN), f.ID); err != nil || g.AddedBy != "Profile deleted" {
		t.Fatalf("in English %q %v", g.AddedBy, err)
	}
	if by, on, err := e.svc.Following(ctx, "series", 42); err != nil || !on || by.ID != 0 {
		t.Fatalf("following %+v %v %v", by, on, err)
	}
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	e.check(f.ID)
	if rs := e.acq.requests(); len(rs) != 1 || rs[0].UserID != 0 {
		t.Fatalf("download of a deleted profile's follow %+v", rs)
	}
}

// TestFollowSkipsEpisodesCoveredByPinned: an episode the title's pinned
// release holds is the pin's to download («Докачивать новые» grows the
// pin's download): the follow's own search never queues it a second time.
func TestFollowSkipsEpisodesCoveredByPinned(t *testing.T) {
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7)
	var asked int64
	e.svc.Covered = func(_ context.Context, tmdbID int64) (map[[2]int]bool, error) {
		asked = tmdbID
		return map[[2]int]bool{{1, 8}: true}, nil
	}
	e.add("series", 42)
	if err := e.svc.CheckAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := targets(e.acq.requests()), [][2]int{{1, 9}, {1, 10}}; !reflect.DeepEqual(got, want) || asked != 42 {
		t.Fatalf("acquired %v (want %v), asked %d", got, want, asked)
	}
	// coverage that cannot be read: the follow goes on without it
	e2 := newEnv(t)
	e2.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7)
	e2.svc.Covered = func(context.Context, int64) (map[[2]int]bool, error) { return nil, errors.New("db gone") }
	e2.add("series", 42)
	if err := e2.svc.CheckAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := targets(e2.acq.requests()); len(got) != 3 {
		t.Fatalf("acquired %v", got)
	}
}

// TestFollowPassesResolution: the follow's resolution goes with each of its
// downloads; a note of the pick (another resolution taken) is kept.
func TestFollowPassesResolution(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	f := e.add("series", 42)
	if f.Resolution != "" {
		t.Fatalf("new follow resolution %q", f.Resolution)
	}
	if err := e.svc.SetResolution(ctx, f.ID, "1080p"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetResolution(ctx, f.ID, "1440p"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad resolution: %v", err)
	}
	if g := e.get(f.ID); g.Resolution != "1080p" {
		t.Fatalf("stored %q", g.Resolution)
	}
	e.acq.note = "В 1080p раздач нет — взята раздача другого качества"
	e.check(f.ID)
	rs := e.acq.requests()
	if len(rs) != 1 || rs[0].Resolution != "1080p" {
		t.Fatalf("requests %+v", rs)
	}
	if g := e.get(f.ID); !strings.Contains(g.LastError, "S01E10: "+e.acq.note) {
		t.Fatalf("note %q", g.LastError)
	}
}

// TestFollowingIsActiveFollow: Following answers for active follows only.
func TestFollowingIsActiveFollow(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	f := e.add("series", 42)
	for _, tc := range []struct {
		kind string
		id   int64
		want bool
	}{{"series", 42, true}, {"movie", 42, false}, {"series", 7, false}} {
		if _, got, err := e.svc.Following(ctx, tc.kind, tc.id); err != nil || got != tc.want {
			t.Fatalf("%s %d: %v %v", tc.kind, tc.id, got, err)
		}
	}
	if err := e.svc.SetActive(ctx, f.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, got, _ := e.svc.Following(ctx, "series", 42); got {
		t.Fatal("inactive follow counts")
	}
}

// TestOnFollowedTold: a follow added or turned on again is told (the
// pinned release's missing episodes are looked at at once); one turned
// off, or added twice, is not.
func TestOnFollowedTold(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	var told []int
	e.svc.OnFollowed = func(_ context.Context, f Follow) { told = append(told, f.TMDBID) }
	f := e.add("series", 42)
	e.add("series", 42)
	if err := e.svc.SetActive(ctx, f.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetActive(ctx, f.ID, true); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(told) != "[42 42]" {
		t.Fatalf("told %v", told)
	}
}

// TestFollowLeavesPinnedFilm: a released film with a pinned release is the
// pin's to download; the follow's own search waits (and searches when
// the pin cannot be told).
func TestFollowLeavesPinnedFilm(t *testing.T) {
	e := newEnv(t)
	var asked int64
	e.svc.FilmPinned = func(_ context.Context, id int64) (bool, error) { asked = id; return true, nil }
	f := e.add("movie", 7)
	res := e.check(f.ID)
	if n := len(e.acq.requests()); n != 0 || asked != 7 || res.Missing != 1 {
		t.Fatalf("pinned film: %d requests, asked %d, %+v", n, asked, res)
	}
	e.svc.FilmPinned = func(context.Context, int64) (bool, error) { return false, errors.New("db gone") }
	e.check(f.ID)
	if n := len(e.acq.requests()); n != 1 {
		t.Fatalf("unknown pin: %d requests", n)
	}
}

// TestFollow_DeletedProfileIDReused: the core reuses a deleted
// profile's id (users.id has no AUTOINCREMENT). Once a profile list was
// read without it, its follow stays the deleted profile's: a new profile
// with the same id neither owns it nor is told of it.
func TestFollow_DeletedProfileIDReused(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.users.Names[5] = "Мама"
	e.users.Names[1] = "Админ"
	f, err := e.svc.Add(ctx, e.users.User(5), "series", 42)
	if err != nil {
		t.Fatal(err)
	}
	delete(e.users.Names, 5) // deleted in the core
	if _, err := e.svc.List(ctx); err != nil {
		t.Fatal(err)
	}
	e.users.Names[5] = "Новый" // a new profile gets id 5
	g, err := e.svc.Get(ctx, f.ID)
	if err != nil || g.UserID != 0 || g.AddedBy != "Профиль удалён" {
		t.Fatalf("after the id came back: %+v %v", g, err)
	}
	if by, on, err := e.svc.Following(ctx, "series", 42); err != nil || !on || by.ID != 0 {
		t.Fatalf("following %+v %v %v", by, on, err)
	}
	// the core unreachable: a gone follow stays gone
	e.users.Err = errors.New("core down")
	if g, err := e.svc.Get(ctx, f.ID); err != nil || g.UserID != 0 || g.AddedBy != "Профиль удалён" {
		t.Fatalf("core down: %+v %v", g, err)
	}
	e.users.Err = nil
	// a follow with no author at all is not «deleted»
	anon := e.add("movie", 7)
	if g, _ := e.svc.Get(ctx, anon.ID); g.AddedBy != "" {
		t.Fatalf("no author: %q", g.AddedBy)
	}
	// an empty profile list (a broken answer) forgets nobody
	e2 := newEnv(t)
	e2.users.Names[3] = "Папа"
	f2, _ := e2.svc.Add(ctx, e2.users.User(3), "movie", 7)
	e2.users.Names = map[int64]string{}
	e2.svc.List(ctx)
	e2.users.Names[3] = "Папа"
	if g, _ := e2.svc.Get(ctx, f2.ID); g.UserID != 3 {
		t.Fatalf("an empty list forgot %+v", g)
	}
}
