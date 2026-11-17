//go:build ignore

package musicimport

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/music"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

const gb = int64(1) << 30

// fakeAcq is the acquire service of the queue tests: every plan covers
// all the wanted songs with 1 GB (plan overrides it); downloads never end
// unless their state is set.
type fakeAcq struct {
	mu       sync.Mutex
	now      func() time.Time
	planAt   []time.Time
	plans    []acquire.TrackAcquireRequest
	acquired []acquire.TrackAcquireRequest
	plan     func(req acquire.TrackAcquireRequest) (acquire.TrackPlan, error)
	disk     acquire.DiskUsage
	states   map[string]acquire.TrackDownload
	// running: the release group (and album, 0: a new one) of each
	// acquisition — the queue's and any other («На сервер»)
	running map[string]runningAcq
	ignored []string // IgnoreDownload of each AcquireTracks that had one
	removed []string // Remove calls: infohash, «with files» or «keeping files»
}

type runningAcq struct {
	extID string
	album int64
}

func newFakeAcq(now func() time.Time) *fakeAcq {
	return &fakeAcq{now: now, disk: acquire.DiskUsage{Used: 10 * gb, Total: 100 * gb, Level: acquire.LevelOK},
		states: map[string]acquire.TrackDownload{}, running: map[string]runningAcq{}}
}

func (f *fakeAcq) PlanTracks(_ context.Context, req acquire.TrackAcquireRequest) (acquire.TrackPlan, error) {
	f.mu.Lock()
	f.planAt = append(f.planAt, f.now())
	f.plans = append(f.plans, req)
	plan := f.plan
	f.mu.Unlock()
	if plan != nil {
		return plan(req)
	}
	p := acquire.TrackPlan{SourceID: 1, ReleaseLink: "https://t/" + req.ExtID, ReleaseTitle: req.Artist + " - " + req.Title,
		Matched: map[string]int{}, Bytes: gb}
	for i, w := range req.Tracks {
		p.Files = append(p.Files, i)
		p.Matched[w.Key] = i
	}
	return p, nil
}

func (f *fakeAcq) AcquireTracks(_ context.Context, req acquire.TrackAcquireRequest, p acquire.TrackPlan) (acquire.TrackAcquireResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, req)
	if req.IgnoreDownload != "" {
		f.ignored = append(f.ignored, req.IgnoreDownload)
	}
	ih := fmt.Sprintf("%040d", len(f.acquired))
	f.states[ih] = acquire.TrackDownload{Known: true, Present: true, Size: p.Bytes}
	f.running[ih] = runningAcq{req.ExtID, req.AlbumID}
	return acquire.TrackAcquireResult{InfoHash: ih, Files: p.Matched, Missing: p.Missing, Bytes: p.Bytes}, nil
}

func (f *fakeAcq) InProgress(_ context.Context, extID string, albumID int64, except string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ih, r := range f.running {
		d := f.states[ih]
		if ih != except && d.Present && !d.Organized && !(d.OrganizeError != "" && d.Exhausted) &&
			(extID != "" && r.extID == extID || albumID != 0 && r.album == albumID) {
			return ih, true, nil
		}
	}
	return "", false, nil
}

func (f *fakeAcq) Disk() (acquire.DiskUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.disk, nil
}

func (f *fakeAcq) TrackDownload(_ context.Context, ih string) (acquire.TrackDownload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.states[ih], nil
}

func (f *fakeAcq) Remove(_ context.Context, ih string, deleteData bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.states[ih]; !ok {
		return acquire.ErrNotFound
	}
	if deleteData {
		f.removed = append(f.removed, ih+" with files")
	} else {
		f.removed = append(f.removed, ih+" keeping files")
	}
	delete(f.states, ih)
	delete(f.running, ih)
	return nil
}

func (f *fakeAcq) set(fn func(f *fakeAcq)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// clock is a fake clock the tests move by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type queueEnv struct {
	t     *testing.T
	st    *playlistimport.Store
	lib   *library.Repo
	acq   *fakeAcq
	clk   *clock
	q     *Queue
	job   int64
	built []int64 // Rebuild calls
}

// newQueueEnv makes a downloading job with one matched song per album
// (titles[i] on album i, release group rg-i).
func newQueueEnv(t *testing.T, songs ...[2]string) *queueEnv {
	t.Helper()
	st, uid := newTestStore(t)
	e := &queueEnv{t: t, st: st, lib: library.NewRepo(st.DB), clk: &clock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}}
	st.Now = e.clk.now
	e.acq = newFakeAcq(e.clk.now)
	snap := playlistimport.Snapshot{Service: "file"}
	for i, s := range songs {
		snap.Tracks = append(snap.Tracks, playlistimport.SnapTrack{Key: "k" + strconv.Itoa(i), Artist: "Кино", Title: s[0], Album: s[1], Liked: true})
	}
	ctx := context.Background()
	id, err := st.CreateJob(ctx, uid, snap)
	if err != nil {
		t.Fatal(err)
	}
	e.job = id
	mustExec(t, st.DB, `UPDATE import_jobs SET status = 'downloading' WHERE id = ?`, id)
	tracks, err := st.NextPending(ctx, id, 1000)
	if err != nil {
		t.Fatal(err)
	}
	rgOf := map[string]string{}
	for _, tr := range tracks {
		rg, ok := rgOf[tr.Album]
		if !ok {
			rg = "rg-" + strconv.Itoa(len(rgOf))
			rgOf[tr.Album] = rg
		}
		if err := st.SetDecision(ctx, tr.ID, playlistimport.Decision{State: playlistimport.TrackMatched, ReleaseGroup: rg, Title: tr.Title, Artist: "Кино",
			Album: tr.Album, Year: 1988}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AssignGroups(ctx, id); err != nil {
		t.Fatal(err)
	}
	e.q = e.newQueue()
	return e
}

// newQueue is a queue as after a (re)start.
func (e *queueEnv) newQueue() *Queue {
	return &Queue{Acq: e.acq, Lib: e.lib, Store: e.st, Max: 3, Gap: 5 * time.Second, Now: e.clk.now,
		Rebuild: func(_ context.Context, jobID int64) error { e.built = append(e.built, jobID); return nil }}
}

func (e *queueEnv) step() {
	e.t.Helper()
	if err := e.q.Step(context.Background(), e.job); err != nil {
		e.t.Fatal(err)
	}
}

// states counts the job's groups per state.
func (e *queueEnv) states() map[string]int {
	e.t.Helper()
	rows, err := e.st.DB.Query(`SELECT state, COUNT(*) FROM import_groups WHERE job_id = ? GROUP BY state`, e.job)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		rows.Scan(&s, &n)
		out[s] = n
	}
	return out
}

// track returns a song's state and item id by title.
func (e *queueEnv) track(title string) (string, int64) {
	e.t.Helper()
	var state string
	var item int64
	if err := e.st.DB.QueryRow(`SELECT state, COALESCE(item_id, 0) FROM import_tracks WHERE job_id = ? AND title = ?`, e.job, title).
		Scan(&state, &item); err != nil {
		e.t.Fatal(err)
	}
	return state, item
}

func (e *queueEnv) group(rg string) playlistimport.Group {
	e.t.Helper()
	gs, err := e.st.Groups(context.Background(), e.job)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, g := range gs {
		if g.ReleaseGroup == rg {
			return g
		}
	}
	e.t.Fatalf("no group %s", rg)
	return playlistimport.Group{}
}

func TestQueueBoundedAndPaced(t *testing.T) {
	var songs [][2]string
	for i := range 10 {
		songs = append(songs, [2]string{"Песня " + strconv.Itoa(i), "Альбом " + strconv.Itoa(i)})
	}
	e := newQueueEnv(t, songs...)
	for range 60 {
		e.step()
		e.clk.add(time.Second)
	}
	if s := e.states(); s[playlistimport.GroupDownloading] != 3 || s[playlistimport.GroupWaiting] != 7 {
		t.Fatalf("states %v", s)
	}
	if len(e.acq.planAt) != 3 || len(e.acq.acquired) != 3 {
		t.Fatalf("%d plans, %d acquisitions", len(e.acq.planAt), len(e.acq.acquired))
	}
	for i := 1; i < len(e.acq.planAt); i++ {
		if d := e.acq.planAt[i].Sub(e.acq.planAt[i-1]); d < 5*time.Second {
			t.Fatalf("searches %v apart", d)
		}
	}
	// oldest first; the songs are downloading; TrackAcquireRequest has no
	// Force: the queue cannot bypass the space hold
	if e.acq.plans[0].ExtID != "rg-0" || e.acq.plans[0].Title != "Альбом 0" || e.acq.plans[0].Artist != "Кино" ||
		e.acq.plans[0].Year != 1988 || e.acq.plans[0].AlbumID != 0 || e.acq.plans[0].UserID == 0 {
		t.Fatalf("first plan %+v", e.acq.plans[0])
	}
	if s, _ := e.track("Песня 0"); s != playlistimport.TrackDownloading {
		t.Fatalf("song state %s", s)
	}
	if g := e.group("rg-0"); g.InfoHash == "" || g.Bytes != gb {
		t.Fatalf("group %+v", g)
	}
	// one finishes (organized, nothing in the library to link): a slot
	// frees and the next group starts
	e.acq.set(func(f *fakeAcq) {
		f.states[fmt.Sprintf("%040d", 1)] = acquire.TrackDownload{Known: true, Present: true, Organized: true}
	})
	for range 10 {
		e.step()
		e.clk.add(time.Second)
	}
	if s := e.states(); s[playlistimport.GroupDownloading] != 3 || s[playlistimport.GroupDone] != 1 || s[playlistimport.GroupWaiting] != 6 {
		t.Fatalf("states after one finished %v", s)
	}
	if s, _ := e.track("Песня 0"); s != playlistimport.TrackFailed {
		t.Fatalf("unlinked song %s (want failed)", s)
	}
	if len(e.built) == 0 {
		t.Fatal("no rebuild after a finished group")
	}
}

func TestQueueHoldsForSpace(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"}, [2]string{"Кукушка", "Чёрный альбом"})
	e.acq.set(func(f *fakeAcq) {
		f.disk = acquire.DiskUsage{Used: 94 * gb, Total: 100 * gb, Level: acquire.LevelWarn}
		f.plan = func(req acquire.TrackAcquireRequest) (acquire.TrackPlan, error) {
			return acquire.TrackPlan{SourceID: 1, ReleaseLink: "https://t/x", Files: []int{0}, Matched: map[string]int{req.Tracks[0].Key: 0},
				Bytes: 4 * gb}, nil
		}
	})
	e.step()
	if g := e.group("rg-0"); g.State != playlistimport.GroupWaitingSpace || len(e.acq.acquired) != 0 {
		t.Fatalf("group %+v, %d acquisitions", g, len(e.acq.acquired))
	}
	// the disk has not freed: not searched again for it
	for range 6 {
		e.clk.add(time.Second)
		e.step()
	}
	if len(e.acq.acquired) != 0 || e.group("rg-1").State != playlistimport.GroupWaitingSpace {
		t.Fatalf("%d acquisitions; rg-1 %+v", len(e.acq.acquired), e.group("rg-1"))
	}
	plans := len(e.acq.plans)
	for range 10 {
		e.clk.add(time.Second)
		e.step()
	}
	if len(e.acq.plans) != plans {
		t.Fatalf("re-planned on a full disk: %d → %d", plans, len(e.acq.plans))
	}
	// space frees: re-planned and started
	e.acq.set(func(f *fakeAcq) { f.disk = acquire.DiskUsage{Used: 50 * gb, Total: 100 * gb, Level: acquire.LevelOK} })
	for range 12 {
		e.clk.add(time.Second)
		e.step()
	}
	if s := e.states(); s[playlistimport.GroupDownloading] != 2 || len(e.acq.acquired) != 2 {
		t.Fatalf("after space freed %v, %d acquisitions", s, len(e.acq.acquired))
	}

	// a full disk: nothing starts, nothing is searched
	f := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	f.acq.set(func(a *fakeAcq) { a.disk = acquire.DiskUsage{Used: 96 * gb, Total: 100 * gb, Level: acquire.LevelFull} })
	for range 10 {
		f.clk.add(time.Second)
		f.step()
	}
	if len(f.acq.plans) != 0 || f.group("rg-0").State != playlistimport.GroupWaiting {
		t.Fatalf("full disk: %d plans, %+v", len(f.acq.plans), f.group("rg-0"))
	}
	// an offline disk neither
	f.acq.set(func(a *fakeAcq) {
		a.disk = acquire.DiskUsage{Level: acquire.LevelOK, Offline: "Медиадиск недоступен"}
	})
	f.step()
	if len(f.acq.plans) != 0 {
		t.Fatal("searched with the media disk offline")
	}
}

// kinoLibrary puts «Кино/Группа крови» with tracks 1–3 into the library
// (MusicBrainz id rg) and returns the album and its track ids.
func (e *queueEnv) kinoLibrary(rg string) (int64, []int64) {
	e.t.Helper()
	ctx := context.Background()
	var in []library.TrackInput
	for i, title := range []string{"Группа крови", "Закрой за мной дверь, я ухожу", "Война"} {
		in = append(in, library.TrackInput{File: library.FileInput{Path: fmt.Sprintf("/m/Кино/Группа крови/%02d.flac", i+1), Size: 1, Mtime: 1},
			AlbumDir: "/m/Кино/Группа крови", Album: "Группа крови", Title: title, Track: i + 1})
	}
	id, err := e.lib.UpsertAlbumTracks(ctx, "Кино", in)
	if err != nil {
		e.t.Fatal(err)
	}
	mustExec(e.t, e.st.DB, `UPDATE items SET meta_json = json_set(meta_json, '$.mb_id', ?) WHERE id = ?`, rg, id)
	tracks, err := e.lib.AlbumTracks(ctx, id)
	if err != nil || len(tracks) != 3 {
		e.t.Fatalf("%d tracks %v", len(tracks), err)
	}
	var ids []int64
	for _, tr := range tracks {
		ids = append(ids, tr.ID)
	}
	return id, ids
}

func TestGroupUsesExistingAlbum(t *testing.T) {
	// the import's album name differs from the library's; the MusicBrainz id
	// finds it
	e := newQueueEnv(t, [2]string{"Война", "Группа крови (Remastered)"}, [2]string{"Бошетунмай", "Группа крови (Remastered)"})
	album, ids := e.kinoLibrary("rg-0")
	e.step()
	if s, item := e.track("Война"); s != playlistimport.TrackHave || item != ids[2] {
		t.Fatalf("track 3: %s %d (want have %d)", s, item, ids[2])
	}
	if len(e.acq.plans) != 1 || len(e.acq.plans[0].Tracks) != 1 || e.acq.plans[0].Tracks[0].Title != "Бошетунмай" ||
		e.acq.plans[0].AlbumID != album {
		t.Fatalf("plan %+v", e.acq.plans)
	}
	if len(e.acq.acquired) != 1 || e.acq.acquired[0].AlbumID != album {
		t.Fatalf("acquired %+v", e.acq.acquired)
	}
	if s, _ := e.track("Бошетунмай"); s != playlistimport.TrackDownloading {
		t.Fatalf("track 5: %s", s)
	}

	// every song in the library: the group is have, nothing is searched
	f := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"}, [2]string{"Война", "Группа крови"})
	album, _ = f.kinoLibrary("rg-other") // found by artist and album name
	f.step()
	if g := f.group("rg-0"); g.State != playlistimport.GroupHave || g.AlbumItemID != album || len(f.acq.plans) != 0 {
		t.Fatalf("group %+v, %d plans", g, len(f.acq.plans))
	}
}

func TestQueueResumesDownloadingGroup(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"}, [2]string{"Война", "Группа крови"})
	const ih = "cccccccccccccccccccccccccccccccccccccccc"
	// the server stopped mid-download
	mustExec(t, e.st.DB, `UPDATE import_groups SET state = 'downloading', infohash = ?, bytes = ? WHERE job_id = ?`, ih, gb, e.job)
	mustExec(t, e.st.DB, `UPDATE import_tracks SET state = 'downloading' WHERE job_id = ?`, e.job)
	e.acq.set(func(f *fakeAcq) {
		f.states[ih] = acquire.TrackDownload{Known: true, Present: true, Size: gb, Done: gb / 2}
	})
	e.q = e.newQueue()
	for range 10 {
		e.clk.add(time.Second)
		e.step()
	}
	if len(e.acq.acquired) != 0 || len(e.acq.plans) != 0 || e.group("rg-0").State != playlistimport.GroupDownloading {
		t.Fatalf("restart: %d acquisitions, %d plans, %+v", len(e.acq.acquired), len(e.acq.plans), e.group("rg-0"))
	}
	// organize done: the songs link to the library tracks of the torrent
	album, ids := e.kinoLibrary("rg-x")
	for _, p := range []string{"/m/Кино/Группа крови/01.flac", "/m/Кино/Группа крови/03.flac"} {
		if err := e.lib.SetFileTorrent(context.Background(), p, ih); err != nil {
			t.Fatal(err)
		}
	}
	e.acq.set(func(f *fakeAcq) {
		f.states[ih] = acquire.TrackDownload{Known: true, Present: true, Organized: true, Size: gb, Done: gb}
	})
	e.step()
	if s, item := e.track("Группа крови"); s != playlistimport.TrackDone || item != ids[0] {
		t.Fatalf("track 1: %s %d", s, item)
	}
	if s, item := e.track("Война"); s != playlistimport.TrackDone || item != ids[2] {
		t.Fatalf("track 3: %s %d", s, item)
	}
	if g := e.group("rg-0"); g.State != playlistimport.GroupDone || g.AlbumItemID != album {
		t.Fatalf("group %+v", g)
	}
	if len(e.built) != 1 {
		t.Fatalf("rebuilds %v", e.built)
	}
}

func TestQueueFollowUpForSongAddedMidDownload(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.step()
	g := e.group("rg-0")
	if g.State != playlistimport.GroupDownloading {
		t.Fatalf("group %+v", g)
	}
	// a resolved review song joins the group while it downloads
	mustExec(t, e.st.DB, `INSERT INTO import_tracks(job_id, key, artist, title, liked, state, mb_release_group, mb_title, group_id)
		VALUES (?, 'late', 'Кино', 'Война', 1, 'matched', 'rg-0', 'Война', ?)`, e.job, g.ID)
	album, ids := e.kinoLibrary("rg-0")
	if err := e.lib.SetFileTorrent(context.Background(), "/m/Кино/Группа крови/01.flac", g.InfoHash); err != nil {
		t.Fatal(err)
	}
	// the library has «Война» already (another torrent): no second album,
	// it is linked as have
	e.acq.set(func(f *fakeAcq) {
		f.states[g.InfoHash] = acquire.TrackDownload{Known: true, Present: true, Organized: true}
	})
	e.clk.add(10 * time.Second)
	e.step()
	if s, item := e.track("Группа крови"); s != playlistimport.TrackDone || item != ids[0] {
		t.Fatalf("first song %s %d", s, item)
	}
	if s, item := e.track("Война"); s != playlistimport.TrackHave || item != ids[2] {
		t.Fatalf("late song %s %d", s, item)
	}
	if g := e.group("rg-0"); g.State != playlistimport.GroupHave && g.State != playlistimport.GroupDone || g.AlbumItemID != album {
		t.Fatalf("group %+v", g)
	}
	if len(e.acq.acquired) != 1 {
		t.Fatalf("acquisitions %d", len(e.acq.acquired))
	}

	// a song the library lacks: a follow-up acquisition into the album
	// folder of the first one
	f := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	f.step()
	g = f.group("rg-0")
	mustExec(t, f.st.DB, `INSERT INTO import_tracks(job_id, key, artist, title, liked, state, mb_release_group, mb_title, group_id)
		VALUES (?, 'late', 'Кино', 'Бошетунмай', 1, 'matched', 'rg-0', 'Бошетунмай', ?)`, f.job, g.ID)
	album, _ = f.kinoLibrary("rg-other")
	if err := f.lib.SetFileTorrent(context.Background(), "/m/Кино/Группа крови/01.flac", g.InfoHash); err != nil {
		t.Fatal(err)
	}
	f.acq.set(func(a *fakeAcq) {
		a.states[g.InfoHash] = acquire.TrackDownload{Known: true, Present: true, Organized: true}
	})
	f.clk.add(10 * time.Second)
	f.step()
	if len(f.acq.acquired) != 2 || f.acq.acquired[1].AlbumID != album || len(f.acq.acquired[1].Tracks) != 1 ||
		f.acq.acquired[1].Tracks[0].Title != "Бошетунмай" {
		t.Fatalf("follow-up %+v", f.acq.acquired)
	}
	if s, _ := f.track("Бошетунмай"); s != playlistimport.TrackDownloading {
		t.Fatalf("late song %s", s)
	}
}

func TestQueueReopenedGroupDownloadsAgain(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.step()
	g := e.group("rg-0")
	album, _ := e.kinoLibrary("rg-0")
	if err := e.lib.SetFileTorrent(context.Background(), "/m/Кино/Группа крови/01.flac", g.InfoHash); err != nil {
		t.Fatal(err)
	}
	e.acq.set(func(f *fakeAcq) {
		f.states[g.InfoHash] = acquire.TrackDownload{Known: true, Present: true, Organized: true}
	})
	e.step()
	if g := e.group("rg-0"); g.State != playlistimport.GroupDone {
		t.Fatalf("group %+v", g)
	}
	// a review song resolved into the finished album reopens it
	mustExec(t, e.st.DB, `INSERT INTO import_tracks(job_id, key, artist, title, liked, state, mb_release_group, mb_title)
		VALUES (?, 'late', 'Кино', 'Бошетунмай', 1, 'matched', 'rg-0', 'Бошетунмай')`, e.job)
	if err := e.st.AssignGroups(context.Background(), e.job); err != nil {
		t.Fatal(err)
	}
	if g := e.group("rg-0"); g.State != playlistimport.GroupWaiting {
		t.Fatalf("reopened %+v", g)
	}
	e.clk.add(10 * time.Second)
	e.step()
	if len(e.acq.acquired) != 2 || e.acq.acquired[1].AlbumID != album || len(e.acq.acquired[1].Tracks) != 1 {
		t.Fatalf("acquired %+v", e.acq.acquired)
	}
	if g2 := e.group("rg-0"); g2.State != playlistimport.GroupDownloading || g2.InfoHash == g.InfoHash {
		t.Fatalf("group %+v", g2)
	}
}

func TestQueueNotFoundAndRetry(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.acq.set(func(f *fakeAcq) {
		f.plan = func(acquire.TrackAcquireRequest) (acquire.TrackPlan, error) {
			return acquire.TrackPlan{}, fmt.Errorf("search: %w", acquire.ErrNoRelease)
		}
	})
	e.step()
	g := e.group("rg-0")
	if g.State != playlistimport.GroupNotFound || g.Error == "" {
		t.Fatalf("group %+v", g)
	}
	if s, _ := e.track("Группа крови"); s != playlistimport.TrackNotFound {
		t.Fatalf("song %s", s)
	}
	if busy, _ := e.st.Unfinished(ctx, e.job); busy {
		t.Fatal("a not-found group keeps the job open")
	}
	if err := e.st.RetryGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if s, _ := e.track("Группа крови"); s != playlistimport.TrackMatched || e.group("rg-0").State != playlistimport.GroupWaiting {
		t.Fatalf("after retry %s %+v", s, e.group("rg-0"))
	}
	if err := e.st.RetryGroup(ctx, g.ID); err != playlistimport.ErrNotRetryable {
		t.Fatalf("retry a waiting group: %v", err)
	}
}

// No release of «Формат загрузки»: the album waits as no_format until
// «Скачать в другом формате», whose search takes any format.
func TestQueueNoFormatAndOtherFormat(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.acq.set(func(f *fakeAcq) {
		f.plan = func(req acquire.TrackAcquireRequest) (acquire.TrackPlan, error) {
			if !req.AnyFormat {
				return acquire.TrackPlan{}, fmt.Errorf("search: %w", errors.Join(acquire.ErrNoRelease, acquire.ErrNoFormat))
			}
			return acquire.TrackPlan{SourceID: 1, ReleaseLink: "https://t/flac", Files: []int{0}, Matched: map[string]int{req.Tracks[0].Key: 0}, Bytes: gb}, nil
		}
	})
	e.step()
	g := e.group("rg-0")
	if g.State != playlistimport.GroupNoFormat || g.Error != msgNoFormat || g.AnyFormat {
		t.Fatalf("group %+v", g)
	}
	if busy, _ := e.st.Unfinished(ctx, e.job); busy {
		t.Fatal("an album of no format keeps the job open")
	}
	if err := e.st.RetryGroupAnyFormat(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if g := e.group("rg-0"); g.State != playlistimport.GroupWaiting || !g.AnyFormat {
		t.Fatalf("after other format %+v", g)
	}
	e.clk.add(time.Minute)
	e.step()
	if g := e.group("rg-0"); g.State != playlistimport.GroupDownloading {
		t.Fatalf("downloading %+v", g)
	}
	if last := e.acq.plans[len(e.acq.plans)-1]; !last.AnyFormat {
		t.Fatalf("plan %+v", last)
	}
}

func TestQueueFailedRetriesOnceAfter30Min(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.acq.set(func(f *fakeAcq) {
		f.plan = func(acquire.TrackAcquireRequest) (acquire.TrackPlan, error) {
			return acquire.TrackPlan{}, fmt.Errorf("tracker: %w", acquire.ErrSource)
		}
	})
	e.step()
	if g := e.group("rg-0"); g.State != playlistimport.GroupFailed || g.Attempts != 1 {
		t.Fatalf("group %+v", g)
	}
	if busy, _ := e.st.Unfinished(context.Background(), e.job); !busy {
		t.Fatal("a group waiting for its automatic retry must keep the job open")
	}
	e.clk.add(29 * time.Minute)
	e.step()
	if len(e.acq.plans) != 1 {
		t.Fatalf("retried early: %d plans", len(e.acq.plans))
	}
	e.clk.add(2 * time.Minute)
	e.step()
	if g := e.group("rg-0"); len(e.acq.plans) != 2 || g.State != playlistimport.GroupFailed || g.Attempts != 2 {
		t.Fatalf("%d plans, %+v", len(e.acq.plans), g)
	}
	e.clk.add(time.Hour)
	e.step()
	if len(e.acq.plans) != 2 {
		t.Fatalf("retried a second time: %d plans", len(e.acq.plans))
	}
	if busy, _ := e.st.Unfinished(context.Background(), e.job); busy {
		t.Fatal("only «Повторить» is left: the job may finish")
	}
}

func TestQueueRemovedTorrentAndOrganizeError(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"}, [2]string{"Кукушка", "Чёрный альбом"})
	e.step()
	e.clk.add(10 * time.Second)
	e.step()
	g0, g1 := e.group("rg-0"), e.group("rg-1")
	e.acq.set(func(f *fakeAcq) {
		delete(f.states, g0.InfoHash) // removed by an admin
		f.states[g1.InfoHash] = acquire.TrackDownload{Known: true, Present: true, OrganizeError: "Нет места", Exhausted: true}
	})
	e.step()
	if g := e.group("rg-0"); g.State != playlistimport.GroupFailed || g.Error == "" {
		t.Fatalf("removed: %+v", g)
	}
	if g := e.group("rg-1"); g.State != playlistimport.GroupFailed || g.Error != "Нет места" {
		t.Fatalf("organize error: %+v", g)
	}
	if s, _ := e.track("Кукушка"); s != playlistimport.TrackFailed {
		t.Fatalf("song %s", s)
	}
}

// «Отменить импорт» mid-download: the started downloads keep running
// (library content) but no longer hold the import's slots; the cancelled
// job starts nothing new, a later job downloads.
func TestQueueCancelledJobFreesSlots(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t, [2]string{"A", "Альбом A"}, [2]string{"B", "Альбом B"}, [2]string{"C", "Альбом C"}, [2]string{"D", "Альбом D"})
	for range 20 {
		e.clk.add(time.Second)
		e.step()
	}
	if s := e.states(); s[playlistimport.GroupDownloading] != 3 {
		t.Fatalf("states %v", s)
	}
	if err := e.st.CancelJob(ctx, e.job); err != nil {
		t.Fatal(err)
	}
	e.clk.add(time.Minute)
	e.step() // a cancelled job: nothing new
	if len(e.acq.acquired) != 3 {
		t.Fatalf("cancelled job acquired %d", len(e.acq.acquired))
	}
	uid := mustJobUser(t, e.st, e.job)
	id, err := e.st.CreateJob(ctx, uid, playlistimport.Snapshot{Service: "spotify", Tracks: []playlistimport.SnapTrack{{Key: "z", Artist: "Кино", Title: "Кукушка", Album: "Чёрный альбом", Liked: true}}})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.st.DB, `UPDATE import_jobs SET status = 'downloading' WHERE id = ?`, id)
	mustExec(t, e.st.DB, `UPDATE import_tracks SET state = 'matched', mb_release_group = 'rg-z', mb_title = title, mb_artist = artist,
		mb_album = album WHERE job_id = ?`, id)
	if err := e.st.AssignGroups(ctx, id); err != nil {
		t.Fatal(err)
	}
	e.job = id
	e.clk.add(time.Minute)
	e.step()
	if len(e.acq.acquired) != 4 || e.acq.acquired[3].ExtID != "rg-z" {
		t.Fatalf("new job: %d acquisitions", len(e.acq.acquired))
	}
}

func mustJobUser(t *testing.T, st *playlistimport.Store, id int64) int64 {
	t.Helper()
	j, err := st.Job(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j.UserID
}

// At most one search per tick, however short Gap is: a slow search does
// not keep the worker busy with the next ones.
func TestQueueOneSearchPerTick(t *testing.T) {
	e := newQueueEnv(t, [2]string{"A", "Альбом A"}, [2]string{"B", "Альбом B"}, [2]string{"C", "Альбом C"})
	e.q.Gap = time.Nanosecond
	e.acq.set(func(f *fakeAcq) {
		f.plan = func(req acquire.TrackAcquireRequest) (acquire.TrackPlan, error) {
			e.clk.add(time.Minute) // a slow search
			return acquire.TrackPlan{SourceID: 1, ReleaseLink: "https://t/x", Files: []int{0}, Matched: map[string]int{req.Tracks[0].Key: 0}, Bytes: gb}, nil
		}
	})
	e.step()
	if len(e.acq.plans) != 1 {
		t.Fatalf("%d searches in one tick", len(e.acq.plans))
	}
}

// importJob stores a snapshot of a service as a downloading job whose
// songs matched the release group of their album (rgOf[album]).
func (e *queueEnv) importJob(snap playlistimport.Snapshot, rgOf map[string]string) int64 {
	e.t.Helper()
	ctx := context.Background()
	uid := mustJobUser(e.t, e.st, e.job)
	id, err := e.st.CreateJob(ctx, uid, snap)
	if err != nil {
		e.t.Fatal(err)
	}
	mustExec(e.t, e.st.DB, `UPDATE import_jobs SET status = 'downloading' WHERE id = ?`, id)
	tracks, err := e.st.NextPending(ctx, id, 1000)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, tr := range tracks {
		if err := e.st.SetDecision(ctx, tr.ID, playlistimport.Decision{State: playlistimport.TrackMatched, ReleaseGroup: rgOf[tr.Album], Title: tr.Title, Artist: "Кино",
			Album: tr.Album, Year: 1988}); err != nil {
			e.t.Fatal(err)
		}
	}
	if err := e.st.AssignGroups(ctx, id); err != nil {
		e.t.Fatal(err)
	}
	e.job = id
	return id
}

// A re-run whose new songs need downloads: the albums the library has are
// linked before any download starts (they need no slot), so the playlist
// never loses them, neither when the first download ends nor on a cancel.
func TestRerunMidDownloadKeepsPlaylist(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t, [2]string{"Кукушка", "Чёрный альбом"})
	m := music.New(e.st.DB)
	e.q.Rebuild = func(ctx context.Context, jobID int64) error { return playlistimport.Rebuild(ctx, e.st, m, jobID) }
	_, ids := e.kinoLibrary("rg-kino")
	have := []playlistimport.SnapTrack{{Key: "k1", Artist: "Кино", Title: "Группа крови", Album: "Группа крови"},
		{Key: "k2", Artist: "Кино", Title: "Война", Album: "Группа крови"}}
	rgOf := map[string]string{"Группа крови": "rg-kino", "A": "rg-a", "B": "rg-b", "C": "rg-c", "D": "rg-d"}
	j1 := e.importJob(playlistimport.Snapshot{Service: "spotify", Tracks: have,
		Playlists: []playlistimport.SnapPlaylist{{ID: "p1", Name: "Дорога", Entries: []string{"k1", "k2"}}}}, rgOf)
	e.step()
	if err := e.q.Rebuild(ctx, j1); err != nil {
		t.Fatal(err)
	}
	var pid int64
	e.st.DB.QueryRow(`SELECT playlist_id FROM import_playlists WHERE external_id = 'p1'`).Scan(&pid)
	items := func() []int64 {
		t.Helper()
		_, got, err := m.Playlist(ctx, mustJobUser(t, e.st, e.job), pid)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := items(); !slices.Equal(got, []int64{ids[0], ids[2]}) {
		t.Fatalf("first run %v", got)
	}
	mustExec(t, e.st.DB, `UPDATE import_jobs SET status = 'done' WHERE id = ?`, j1)

	// the re-run: 4 new songs on albums to download come first
	var tracks []playlistimport.SnapTrack
	for _, a := range []string{"A", "B", "C", "D"} {
		tracks = append(tracks, playlistimport.SnapTrack{Key: "n" + a, Artist: "Кино", Title: "Песня " + a, Album: a})
	}
	tracks = append(tracks, have...)
	e.importJob(playlistimport.Snapshot{Service: "spotify", Tracks: tracks,
		Playlists: []playlistimport.SnapPlaylist{{ID: "p1", Name: "Дорога", Entries: []string{"nA", "nB", "nC", "nD", "k1", "k2"}}}}, rgOf)
	e.clk.add(time.Minute)
	e.step()
	if s, item := e.track("Война"); s != playlistimport.TrackHave || item != ids[2] || len(e.acq.acquired) != 1 {
		t.Fatalf("library song %s %d before downloads (acquired %d)", s, item, len(e.acq.acquired))
	}
	for range 20 {
		e.clk.add(time.Second)
		e.step()
	}
	if s := e.states(); s[playlistimport.GroupDownloading] != 3 || s[playlistimport.GroupHave] != 1 {
		t.Fatalf("states %v", s)
	}
	// the first download ends (its song does not link): the rebuild keeps
	// the library songs
	e.acq.set(func(f *fakeAcq) {
		f.states[e.group("rg-a").InfoHash] = acquire.TrackDownload{Known: true, Present: true, Organized: true}
	})
	e.step()
	if got := items(); !slices.Equal(got, []int64{ids[0], ids[2]}) {
		t.Fatalf("after the first download %v", got)
	}
	if err := e.st.CancelJob(ctx, e.job); err != nil {
		t.Fatal(err)
	}
	e.step()
	if got := items(); !slices.Equal(got, []int64{ids[0], ids[2]}) {
		t.Fatalf("after cancel %v", got)
	}
}

// Cancel, then a re-run of the same album while its download goes on: the
// re-run waits for that download instead of fetching a second copy, then
// links its songs.
func TestQueueCancelThenRerunWaitsForDownload(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.step()
	t1 := e.group("rg-0").InfoHash
	if t1 == "" {
		t.Fatal("not started")
	}
	if err := e.st.CancelJob(ctx, e.job); err != nil {
		t.Fatal(err)
	}
	e.importJob(playlistimport.Snapshot{Service: "spotify", Tracks: []playlistimport.SnapTrack{{Key: "z", Artist: "Кино", Title: "Группа крови", Album: "Группа крови", Liked: true}}},
		map[string]string{"Группа крови": "rg-0"})
	for range 10 {
		e.clk.add(time.Minute)
		e.step()
	}
	if g := e.group("rg-0"); len(e.acq.plans) != 1 || len(e.acq.acquired) != 1 || g.State != playlistimport.GroupWaitingDownload || g.InfoHash != t1 {
		t.Fatalf("re-run: %d plans, %d acquisitions, %+v", len(e.acq.plans), len(e.acq.acquired), g)
	}
	// the first download is organized: the re-run's song is linked to it
	_, ids := e.kinoLibrary("rg-x")
	if err := e.lib.SetFileTorrent(ctx, "/m/Кино/Группа крови/01.flac", t1); err != nil {
		t.Fatal(err)
	}
	e.acq.set(func(f *fakeAcq) { f.states[t1] = acquire.TrackDownload{Known: true, Present: true, Organized: true} })
	e.clk.add(time.Minute)
	e.step()
	if s, item := e.track("Группа крови"); s != playlistimport.TrackHave || item != ids[0] || len(e.acq.acquired) != 1 {
		t.Fatalf("linked %s %d, %d acquisitions", s, item, len(e.acq.acquired))
	}
}

// «На сервер» of an album the import has queued: the import waits for
// that download and links it, no second copy.
func TestQueueManualDownloadOfQueuedAlbum(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Кукушка", "Чёрный альбом"}, [2]string{"Группа крови", "Группа крови"})
	e.q.Max = 1
	e.step()
	if g := e.group("rg-1"); g.State != playlistimport.GroupWaiting {
		t.Fatalf("queued %+v", g)
	}
	const manual = "mmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmm"
	e.acq.set(func(f *fakeAcq) {
		f.states[manual] = acquire.TrackDownload{Known: true, Present: true}
		f.running[manual] = runningAcq{extID: "rg-1"}
		f.states[e.group("rg-0").InfoHash] = acquire.TrackDownload{Known: true, Present: true, Organized: true}
	})
	for range 10 {
		e.clk.add(time.Minute)
		e.step()
	}
	if g := e.group("rg-1"); len(e.acq.plans) != 1 || g.State != playlistimport.GroupWaitingDownload || g.InfoHash != manual {
		t.Fatalf("%d plans, %+v", len(e.acq.plans), g)
	}
	_, ids := e.kinoLibrary("rg-1")
	e.acq.set(func(f *fakeAcq) {
		f.states[manual] = acquire.TrackDownload{Known: true, Present: true, Organized: true}
	})
	e.clk.add(time.Minute)
	e.step()
	if s, item := e.track("Группа крови"); s != playlistimport.TrackHave || item != ids[0] || len(e.acq.plans) != 1 {
		t.Fatalf("linked %s %d, %d plans", s, item, len(e.acq.plans))
	}
	if g := e.group("rg-1"); g.State != playlistimport.GroupHave {
		t.Fatalf("group %+v", g)
	}
}

// A search or a start the acquire service refuses because the album began
// downloading meanwhile puts the group back to wait for it.
func TestQueueInProgressRefusalWaits(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.acq.set(func(f *fakeAcq) {
		f.plan = func(acquire.TrackAcquireRequest) (acquire.TrackPlan, error) {
			return acquire.TrackPlan{}, fmt.Errorf("plan: %w", acquire.ErrInProgress)
		}
	})
	e.step()
	if g := e.group("rg-0"); g.State != playlistimport.GroupWaiting || g.Attempts != 0 {
		t.Fatalf("group %+v", g)
	}
	if s, _ := e.track("Группа крови"); s != playlistimport.TrackMatched {
		t.Fatalf("song %s", s)
	}
}

// waitOnManual starts a «На сервер» download of rg-0 (named name) that
// the queue's album waits for.
func (e *queueEnv) waitOnManual(name string, done int64) string {
	const manual = "mmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmmm"
	e.acq.set(func(f *fakeAcq) {
		f.states[manual] = acquire.TrackDownload{Known: true, Present: true, Status: "downloading", Name: name, Size: gb, Done: done}
		f.running[manual] = runningAcq{extID: "rg-0"}
	})
	return manual
}

// An album waiting for another download says which one; a paused (or
// failed) one is given up: the album searches its own release, leaving
// that download out. A removed one is no reason to wait at all.
func TestQueueWaitShownThenGivenUpWhenPaused(t *testing.T) {
	ctx := context.Background()
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	manual := e.waitOnManual("Кино - Группа крови (1988) FLAC", 0)
	e.step()
	g := e.group("rg-0")
	if g.State != playlistimport.GroupWaitingDownload || g.InfoHash != manual || g.WaitingFor != "Кино - Группа крови (1988) FLAC" || len(e.acq.plans) != 0 {
		t.Fatalf("waiting %+v, %d plans", g, len(e.acq.plans))
	}
	if busy, _ := e.st.Unfinished(ctx, e.job); !busy {
		t.Fatal("a waiting album keeps the job open")
	}
	e.acq.set(func(f *fakeAcq) {
		d := f.states[manual]
		d.Status = "paused"
		f.states[manual] = d
	})
	for range 3 {
		e.clk.add(10 * time.Second)
		e.step()
	}
	if g := e.group("rg-0"); g.State != playlistimport.GroupDownloading || len(e.acq.acquired) != 1 || len(e.acq.ignored) != 1 || e.acq.ignored[0] != manual {
		t.Fatalf("given up %+v, acquired %d, ignored %v", g, len(e.acq.acquired), e.acq.ignored)
	}

	// removed in «Загрузки»: searched at once
	f := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	m := f.waitOnManual("Кино", 0)
	f.step()
	f.acq.set(func(a *fakeAcq) { delete(a.states, m) })
	f.clk.add(10 * time.Second)
	f.step()
	if g := f.group("rg-0"); g.State != playlistimport.GroupDownloading || len(f.acq.acquired) != 1 {
		t.Fatalf("removed %+v, %d acquired", g, len(f.acq.acquired))
	}
}

// A download that makes no progress for 6 h is given up too; progress
// restarts the clock.
func TestQueueWaitGivenUpAfterStall(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	manual := e.waitOnManual("Кино", 100)
	e.step()
	e.clk.add(5 * time.Hour)
	e.acq.set(func(f *fakeAcq) {
		d := f.states[manual]
		d.Done = 200 // progress
		f.states[manual] = d
	})
	e.step()
	e.clk.add(5 * time.Hour) // 10 h since the start, 5 h since the progress
	e.step()
	if g := e.group("rg-0"); g.State != playlistimport.GroupWaitingDownload || len(e.acq.plans) != 0 {
		t.Fatalf("still progressing %+v, %d plans", g, len(e.acq.plans))
	}
	e.clk.add(61 * time.Minute) // 6 h without progress
	e.step()
	e.clk.add(10 * time.Second)
	e.step()
	if g := e.group("rg-0"); g.State != playlistimport.GroupDownloading || len(e.acq.ignored) != 1 {
		t.Fatalf("stalled %+v, ignored %v", g, e.acq.ignored)
	}
}

// «Повторить» of an album waiting for another download: its own search.
func TestRetryWaitingDownloadSearchesOwn(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	manual := e.waitOnManual("Кино", 0)
	e.step()
	if err := e.st.RetryGroup(context.Background(), e.group("rg-0").ID); err != nil {
		t.Fatal(err)
	}
	if g := e.group("rg-0"); g.State != playlistimport.GroupWaiting || g.SkipInfoHash != manual {
		t.Fatalf("retried %+v", g)
	}
	e.clk.add(10 * time.Second)
	e.step()
	if len(e.acq.acquired) != 1 || len(e.acq.ignored) != 1 {
		t.Fatalf("acquired %d ignored %v", len(e.acq.acquired), e.acq.ignored)
	}
}

// setDownload changes how far the download ih is.
func (e *queueEnv) setDownload(ih, status string, done int64) {
	e.acq.set(func(f *fakeAcq) {
		d := f.states[ih]
		d.Status, d.Done = status, done
		f.states[ih] = d
	})
}

// run ticks the queue for d, a tick every 5 min (the clock counts ticks).
func (e *queueEnv) run(d time.Duration) {
	e.t.Helper()
	for end := e.clk.now().Add(d); e.clk.now().Before(end); {
		e.clk.add(5 * time.Minute)
		e.step()
	}
}

func (e *queueEnv) removed() []string {
	e.acq.mu.Lock()
	defer e.acq.mu.Unlock()
	return slices.Clone(e.acq.removed)
}

// An own download that runs an hour without a byte is given up: removed
// with its files, its release left out of the album's next search, which
// the freed slot starts at once.
func TestQueueGivesUpOwnDownloadWithoutBytes(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"}, [2]string{"Кукушка", "Чёрный альбом"})
	e.q.Max = 1
	e.step()
	dead := e.group("rg-0").InfoHash
	e.setDownload(dead, torrent.StatusDownloading, 0)
	e.run(55 * time.Minute)
	if g := e.group("rg-0"); g.InfoHash != dead || len(e.removed()) != 0 {
		t.Fatalf("given up early %+v", g)
	}
	e.run(10 * time.Minute)
	g := e.group("rg-0")
	if g.State != playlistimport.GroupDownloading || g.InfoHash == dead || !slices.Equal(g.Dead, []string{dead}) ||
		!slices.Equal(e.removed(), []string{dead + " with files"}) {
		t.Fatalf("given up %+v, removed %v", g, e.removed())
	}
	if last := e.acq.plans[len(e.acq.plans)-1]; last.ExtID != "rg-0" || !slices.Equal(last.Exclude, []string{dead}) {
		t.Fatalf("search again %+v", last)
	}
	if g := e.group("rg-1"); g.State != playlistimport.GroupWaiting {
		t.Fatalf("one slot: %+v", g)
	}
}

// Server downtime is no running time, and nothing is given up in the
// first hour after a start: a restart does not kill a download the
// engine has not even loaded yet (queued, no byte).
func TestQueueRestartDoesNotCountDowntime(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.step()
	ih := e.group("rg-0").InfoHash
	e.setDownload(ih, torrent.StatusDownloading, 0)
	e.run(50 * time.Minute)
	e.q = e.newQueue() // a restart, 10 h later
	e.clk.add(10 * time.Hour)
	e.setDownload(ih, torrent.StatusQueued, 0)
	e.run(55 * time.Minute)
	if g := e.group("rg-0"); g.InfoHash != ih || len(e.removed()) != 0 {
		t.Fatalf("given up after the restart %+v", g)
	}
	e.run(10 * time.Minute) // an hour up, 1 h 50 min running without a byte
	if g := e.group("rg-0"); !slices.Equal(g.Dead, []string{ih}) {
		t.Fatalf("not given up %+v", g)
	}
}

// Bytes restart the quiet clock; no progress for 6 h of running gives up
// as well. A paused download (an admin's pause) is never given up, and
// its pause does not count.
func TestQueueGivesUpStalledOwnDownload(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.step()
	ih := e.group("rg-0").InfoHash
	e.acq.set(func(f *fakeAcq) { d := f.states[ih]; d.Size = 500; f.states[ih] = d })
	e.setDownload(ih, torrent.StatusDownloading, 400)
	e.run(3 * time.Hour)
	e.setDownload(ih, torrent.StatusPaused, 400)
	e.run(10 * time.Hour)
	e.setDownload(ih, torrent.StatusDownloading, 400)
	e.run(150 * time.Minute) // 5 h 30 min running since the bytes
	if g := e.group("rg-0"); g.InfoHash != ih {
		t.Fatalf("paused time counted %+v", g)
	}
	e.run(35 * time.Minute)
	if g := e.group("rg-0"); !slices.Equal(g.Dead, []string{ih}) || !slices.Equal(e.removed(), []string{ih + " with files"}) {
		t.Fatalf("6 h without progress %+v", g)
	}
}

// A trickle (100 KB/h of 16 MB) is let run 12 h, then given up as slow —
// more than 3 days to go: removed keeping its files, not dead (the album
// may take it again when nothing else holds the songs, and it does not
// count toward the album's failure). 1 MB/h is left to finish.
func TestQueueGivesUpTooSlowOwnDownload(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"}, [2]string{"Кукушка", "Чёрный альбом"})
	e.step()
	e.clk.add(10 * time.Second)
	e.step()
	slow, fast := e.group("rg-0").InfoHash, e.group("rg-1").InfoHash
	e.acq.set(func(f *fakeAcq) {
		for _, ih := range []string{slow, fast} {
			d := f.states[ih]
			d.Size = 16 << 20
			f.states[ih] = d
		}
	})
	for h := int64(1); h <= 11; h++ {
		e.setDownload(slow, torrent.StatusDownloading, h*100<<10)
		e.setDownload(fast, torrent.StatusDownloading, h*1<<20)
		e.run(time.Hour)
	}
	if g := e.group("rg-0"); g.InfoHash != slow {
		t.Fatalf("given up before 12 h %+v", g)
	}
	e.setDownload(slow, torrent.StatusDownloading, 12*100<<10)
	e.setDownload(fast, torrent.StatusDownloading, 12*1<<20)
	e.run(time.Hour)
	g := e.group("rg-0")
	if !slices.Equal(g.Slow, []string{slow}) || len(g.Dead) != 0 || !slices.Equal(e.removed(), []string{slow + " keeping files"}) {
		t.Fatalf("slow %+v removed %v", g, e.removed())
	}
	if last := e.acq.plans[len(e.acq.plans)-1]; !slices.Equal(last.Avoid, []string{slow}) || len(last.Exclude) != 0 {
		t.Fatalf("search again %+v", last)
	}
	if g := e.group("rg-1"); g.InfoHash != fast {
		t.Fatalf("16 h at this rate is fine: %+v", g)
	}
}

// The third dead download: the album fails, «Повторить» forgets them.
func TestQueueFailsAfterThreeDeadDownloads(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.step()
	for i := range maxDead {
		g := e.group("rg-0")
		if g.State != playlistimport.GroupDownloading || len(g.Dead) != i {
			t.Fatalf("try %d: %+v", i, g)
		}
		e.setDownload(g.InfoHash, torrent.StatusQueued, 0) // the metadata never came
		for e.group("rg-0").InfoHash == g.InfoHash {
			e.run(5 * time.Minute)
		}
	}
	g := e.group("rg-0")
	if g.State != playlistimport.GroupFailed || g.Error != msgDead || len(g.Dead) != maxDead {
		t.Fatalf("failed %+v", g)
	}
	if s, _ := e.track("Группа крови"); s != playlistimport.TrackFailed {
		t.Fatalf("song %s", s)
	}
	e.run(time.Hour)
	if g := e.group("rg-0"); g.State != playlistimport.GroupFailed {
		t.Fatalf("retried automatically %+v", g)
	}
	if err := e.st.RetryGroup(context.Background(), g.ID); err != nil {
		t.Fatal(err)
	}
	if g := e.group("rg-0"); g.State != playlistimport.GroupWaiting || len(g.Dead) != 0 {
		t.Fatalf("retry %+v", g)
	}
}

// A download given up and removed whose requeue did not happen (an error,
// a stop) is requeued on the next tick, not failed as «removed».
func TestQueueGivenUpButNotRequeued(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.step()
	g := e.group("rg-0")
	if err := (queueStore{e.st}).markGivenUp(context.Background(), g.ID, g.InfoHash, false); err != nil {
		t.Fatal(err)
	}
	e.acq.set(func(f *fakeAcq) { delete(f.states, g.InfoHash) })
	e.clk.add(10 * time.Second)
	e.step()
	if g2 := e.group("rg-0"); g2.State == playlistimport.GroupFailed || !slices.Equal(g2.Dead, []string{g.InfoHash}) {
		t.Fatalf("after the removal %+v", g2)
	}
	if s, _ := e.track("Группа крови"); s == playlistimport.TrackFailed {
		t.Fatal("song failed")
	}
}

// The automatic switch away from a dead swarm replaces the album's
// download with another release: the album waits for that one instead of
// failing as «removed».
func TestQueueWaitsForReplacedDownload(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.step()
	old := e.group("rg-0").InfoHash
	const repl = "rrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrrr"
	e.acq.set(func(f *fakeAcq) {
		delete(f.states, old)
		f.states[repl] = acquire.TrackDownload{Known: true, Present: true, Status: torrent.StatusDownloading, Name: "Кино - Группа крови"}
		f.running[repl] = runningAcq{extID: "rg-0"}
	})
	e.step()
	e.clk.add(10 * time.Second)
	e.step()
	if g := e.group("rg-0"); g.State != playlistimport.GroupWaitingDownload || g.InfoHash != repl {
		t.Fatalf("replaced %+v", g)
	}
	if s, _ := e.track("Группа крови"); s != playlistimport.TrackMatched {
		t.Fatalf("song %s", s)
	}
}

// The live worker ticks every second, and the clock is saved at most once
// a minute: the running time between the saves still counts (an hour
// without a byte gives the download up after an hour, not a day).
func TestQueueClockCountsUnsavedTicks(t *testing.T) {
	e := newQueueEnv(t, [2]string{"Группа крови", "Группа крови"})
	e.step()
	ih := e.group("rg-0").InfoHash
	e.setDownload(ih, torrent.StatusDownloading, 0)
	for end := e.clk.now().Add(65 * time.Minute); e.clk.now().Before(end); {
		e.clk.add(10 * time.Second)
		e.step()
	}
	if g := e.group("rg-0"); !slices.Equal(g.Dead, []string{ih}) || !slices.Equal(e.removed(), []string{ih + " with files"}) {
		t.Fatalf("an hour without a byte at 10 s ticks: %+v removed %v", g, e.removed())
	}
}
