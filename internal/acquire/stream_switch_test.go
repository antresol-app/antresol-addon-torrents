package acquire

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

const (
	ihSwitchA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	ihSwitchB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	ihSwitchC = "cccccccccccccccccccccccccccccccccccccccc"
)

// streamEpisode starts «Смотреть сейчас» of S01E03 from a release whose
// torrent is ih.
func streamEpisode(t *testing.T, e *env, ih string, episode int) AcquireResult {
	t.Helper()
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Теория большого взрыва S01", "https://t/"+ih, 1, 5)}}
	e.eng.nextIH = ih
	e.eng.nextFiles = []torrent.FileInfo{
		{Index: 0, Path: "TBBT.S01/TBBT.S01E03.mkv", Size: 800},
		{Index: 1, Path: "TBBT.S01/TBBT.S01E04.mkv", Size: 800},
	}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 1418, Kind: KindSeries, Season: 1, Episode: episode, Mode: ModeStream})
	if err != nil {
		t.Fatal(err)
	}
	if res.InfoHash != ih {
		t.Fatalf("stream %s, want %s", res.InfoHash, ih)
	}
	return res
}

func acquisitionCount(t *testing.T, e *env, ih string) int {
	t.Helper()
	var n int
	if err := e.svc.o.DB.QueryRow(`SELECT COUNT(*) FROM acquisitions WHERE infohash=?`, ih).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Picking another source for the same episode replaces the stream watched
// before: the torrent goes with its data and its acquisition, and the core
// is told (its watch inputs go too).
func TestStreamSwitchReplacesPrevious(t *testing.T) {
	e := newEnv(t)
	var told []string
	e.svc.SetOnRemoved(func(ih string) { told = append(told, ih) })
	streamEpisode(t, e, ihSwitchA, 3)
	streamEpisode(t, e, ihSwitchB, 3)
	if !e.eng.removed[ihSwitchA] || !e.eng.removedData[ihSwitchA] {
		t.Fatalf("previous stream kept: removed %v", e.eng.removed)
	}
	if e.eng.removed[ihSwitchB] {
		t.Fatal("the new stream was removed")
	}
	if acquisitionCount(t, e, ihSwitchA) != 0 || acquisitionCount(t, e, ihSwitchB) != 1 {
		t.Fatal("acquisitions not updated")
	}
	if len(told) != 1 || told[0] != ihSwitchA {
		t.Fatalf("told %v", told)
	}

	// another episode is another title: the stream of S01E03 stays
	streamEpisode(t, e, ihSwitchC, 4)
	if e.eng.removed[ihSwitchB] {
		t.Fatal("a stream of another episode was replaced")
	}
}

// shortGrace makes supersededGrace short for one test.
func shortGrace(t *testing.T) time.Duration {
	old := supersededGrace
	supersededGrace = 50 * time.Millisecond
	t.Cleanup(func() { supersededGrace = old })
	return supersededGrace
}

// waitRemoved waits up to a second for torrent ih to be removed.
func waitRemoved(e *env, ih string) bool {
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		e.eng.mu.Lock()
		gone := e.eng.removed[ih]
		e.eng.mu.Unlock()
		if gone {
			return true
		}
	}
	return false
}

func isRemoved(e *env, ih string) bool {
	e.eng.mu.Lock()
	defer e.eng.mu.Unlock()
	return e.eng.removed[ih]
}

// The player is still on the old source when another one is picked: the
// old stream is kept while it is read, and goes a grace after its reader
// closes.
func TestStreamSwitchWhileOldIsRead(t *testing.T) {
	grace := shortGrace(t)
	e := newEnv(t)
	streamEpisode(t, e, ihSwitchA, 3)
	release := e.disk.Use(ihSwitchA)
	streamEpisode(t, e, ihSwitchB, 3)
	if isRemoved(e, ihSwitchA) || acquisitionCount(t, e, ihSwitchA) != 1 {
		t.Fatal("a stream being read was removed")
	}
	time.Sleep(2 * grace)
	if isRemoved(e, ihSwitchA) {
		t.Fatal("removed while its reader is open")
	}
	release()
	if !waitRemoved(e, ihSwitchA) {
		t.Fatal("superseded stream kept after its reader closed")
	}
	if !e.eng.removedData[ihSwitchA] || acquisitionCount(t, e, ihSwitchA) != 0 || isRemoved(e, ihSwitchB) {
		t.Fatalf("removed %v", e.eng.removed)
	}
}

// Reopened within the grace (the same viewer's next request), a superseded
// stream is kept; it goes only a full grace after the last reader.
func TestStreamSwitchReopenWithinGraceKeeps(t *testing.T) {
	grace := shortGrace(t)
	e := newEnv(t)
	streamEpisode(t, e, ihSwitchA, 3)
	e.disk.Use(ihSwitchA)() // a request in flight at the switch...
	release := e.disk.Use(ihSwitchA)
	streamEpisode(t, e, ihSwitchB, 3)
	release() // ...ends
	time.Sleep(grace / 3)
	again := e.disk.Use(ihSwitchA) // reopened within the grace
	time.Sleep(2 * grace)
	if isRemoved(e, ihSwitchA) {
		t.Fatal("a superseded stream reopened within the grace was removed")
	}
	again()
	if !waitRemoved(e, ihSwitchA) {
		t.Fatal("kept after its last reader closed")
	}
}

// Picking the superseded source again, or saving it, takes the mark off.
func TestStreamSwitchSupersededSavedOrPickedAgainKept(t *testing.T) {
	grace := shortGrace(t)
	e := newEnv(t)
	streamEpisode(t, e, ihSwitchA, 3)
	relA := e.disk.Use(ihSwitchA)
	streamEpisode(t, e, ihSwitchB, 3)
	if _, err := e.svc.Promote(context.Background(), ihSwitchA, false); err != nil {
		t.Fatal(err)
	}
	relA()

	// C replaces B while B is read, then B is picked again before its
	// reader closes
	relB := e.disk.Use(ihSwitchB)
	streamEpisode(t, e, ihSwitchC, 3)
	streamEpisode(t, e, ihSwitchB, 3)
	relB()
	time.Sleep(3 * grace)
	if isRemoved(e, ihSwitchA) {
		t.Fatal("a superseded stream that was saved got removed")
	}
	if isRemoved(e, ihSwitchB) {
		t.Fatal("a superseded stream picked again got removed")
	}
	if !isRemoved(e, ihSwitchC) {
		t.Fatal("C, replaced by B picked again, kept")
	}
}

// A saved download of the same episode (a promoted stream) is never touched.
func TestStreamSwitchKeepsSavedDownload(t *testing.T) {
	e := newEnv(t)
	streamEpisode(t, e, ihSwitchA, 3)
	if _, err := e.svc.Promote(context.Background(), ihSwitchA, false); err != nil {
		t.Fatal(err)
	}
	streamEpisode(t, e, ihSwitchB, 3)
	if e.eng.removed[ihSwitchA] || acquisitionCount(t, e, ihSwitchA) != 1 {
		t.Fatal("a saved download was removed by a stream switch")
	}
}

// A stream left at 0 % goes once it has been idle past keepRecent, though
// the cache is far below its limit; one opened a moment ago stays.
func TestPruneDropsIdleEmptyStream(t *testing.T) {
	now := time.Now()
	eng := newFakeEngine()
	eng.put(torrent.Status{InfoHash: "empty", Mode: torrent.ModeStream, Size: 900 << 30, Done: 0, AddedAt: now.Add(-2 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "fresh", Mode: torrent.ModeStream, Size: 90 << 30, Done: 0, AddedAt: now.Add(-time.Minute)})
	eng.put(torrent.Status{InfoHash: "partial", Mode: torrent.ModeStream, Size: 4 << 30, Done: 1 << 20, AddedAt: now.Add(-3 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "read", Mode: torrent.ModeStream, Size: 4 << 30, Done: 0, AddedAt: now.Add(-3 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "dl", Mode: torrent.ModeDownload, Size: 4 << 30, Done: 0, AddedAt: now.Add(-9 * time.Hour)})
	d := &Disk{Root: "/", Engine: eng}
	release := d.Use("read")
	defer release()
	if err := d.PruneStreamCache(context.Background(), 1<<40); err != nil {
		t.Fatal(err)
	}
	if !eng.removed["empty"] || !eng.removedData["empty"] {
		t.Fatalf("idle 0 %% stream kept: %v", eng.removed)
	}
	if eng.removed["fresh"] || eng.removed["partial"] || eng.removed["read"] || eng.removed["dl"] {
		t.Fatalf("removed %v", eng.removed)
	}
}

// streamAs is streamEpisode asked by profile uid.
func streamAs(t *testing.T, e *env, ih string, episode int, uid string) {
	t.Helper()
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Теория большого взрыва S01", "https://t/"+ih, 1, 5)}}
	e.eng.nextIH = ih
	e.eng.nextFiles = []torrent.FileInfo{
		{Index: 0, Path: "TBBT.S01/TBBT.S01E03.mkv", Size: 800},
		{Index: 1, Path: "TBBT.S01/TBBT.S01E04.mkv", Size: 800},
	}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 1418, Kind: KindSeries, Season: 1, Episode: episode, Mode: ModeStream, UserID: 1, UserUID: uid})
	if err != nil {
		t.Fatal(err)
	}
	if res.InfoHash != ih {
		t.Fatalf("stream %s, want %s", res.InfoHash, ih)
	}
}

// Another viewer opening the same episode from another source does not
// take the first viewer's stream away, paused (no reader) or playing; the
// first viewer's own switch still replaces it.
func TestStreamSwitchScopedToViewer(t *testing.T) {
	grace := shortGrace(t)
	e := newEnv(t)
	streamAs(t, e, ihSwitchA, 3, "uid-one")
	streamAs(t, e, ihSwitchB, 3, "uid-two")
	if isRemoved(e, ihSwitchA) {
		t.Fatal("another viewer's paused stream was replaced")
	}
	release := e.disk.Use(ihSwitchB)
	streamAs(t, e, ihSwitchC, 3, "uid-one") // viewer one switches: A goes
	if !isRemoved(e, ihSwitchA) {
		t.Fatal("the viewer's own previous stream kept")
	}
	release()
	time.Sleep(3 * grace)
	if isRemoved(e, ihSwitchB) || isRemoved(e, ihSwitchC) {
		t.Fatalf("removed %v", e.eng.removed)
	}
}

// A stream both viewers watch goes only once neither references it.
func TestStreamSwitchSharedStreamKeptWhileReferenced(t *testing.T) {
	e := newEnv(t)
	streamAs(t, e, ihSwitchA, 3, "uid-one")
	streamAs(t, e, ihSwitchA, 3, "uid-two")
	streamAs(t, e, ihSwitchB, 3, "uid-one")
	if isRemoved(e, ihSwitchA) {
		t.Fatal("a stream another viewer watches was replaced")
	}
	streamAs(t, e, ihSwitchB, 3, "uid-two")
	if !isRemoved(e, ihSwitchA) {
		t.Fatal("a stream nobody watches any more was kept")
	}
}

// The identity of a stream is what it is watched for, not what it was
// first added for: a pack streamed for E03 then E04 is replaced by another
// source of E04, not of E03.
func TestStreamSwitchPackIdentityPerRequest(t *testing.T) {
	e := newEnv(t)
	streamEpisode(t, e, ihSwitchA, 3)
	streamEpisode(t, e, ihSwitchA, 4)
	streamEpisode(t, e, ihSwitchC, 3)
	if isRemoved(e, ihSwitchA) {
		t.Fatal("the E04 stream was replaced by a source of E03")
	}
	streamEpisode(t, e, ihSwitchB, 4)
	if !isRemoved(e, ihSwitchA) {
		t.Fatal("the E04 stream kept after another source of E04")
	}
}

// A delayed drop re-checks before removing: a viewer back on the stream
// keeps it; Close stops the pending drops; one timer per stream.
func TestSupersededDropRechecksAndStops(t *testing.T) {
	grace := shortGrace(t)
	eng := newFakeEngine()
	eng.put(torrent.Status{InfoHash: "a", Mode: torrent.ModeStream})
	eng.put(torrent.Status{InfoHash: "b", Mode: torrent.ModeStream})
	d := &Disk{Root: "/", Engine: eng}
	d.dropSuperseded = func(ih string, at time.Time) { _ = d.dropDue(context.Background(), ih, at) }
	for range 5 {
		d.Use("a")()
	}
	d.Supersede("a")
	d.mu.Lock()
	n := len(d.timers)
	d.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d timers", n)
	}
	d.Watching("a", "uid-two", StreamItem{Kind: KindMovie, TMDBID: 1}) // watched again
	time.Sleep(3 * grace)
	if isRemovedEng(eng, "a") {
		t.Fatal("a superseded stream a viewer watches again was removed")
	}
	// the check is in the removal's critical section: a due timer whose
	// stream was touched since is not acted on
	d.Supersede("b")
	d.mu.Lock()
	at := d.lastUsed["b"]
	d.mu.Unlock()
	d.Touch("b")
	if err := d.dropDue(context.Background(), "b", at); err == nil || isRemovedEng(eng, "b") {
		t.Fatal("a stream touched after its timer was set was dropped")
	}
	d.Supersede("b")
	d.Close()
	time.Sleep(3 * grace)
	if isRemovedEng(eng, "b") {
		t.Fatal("a delayed drop ran after Close")
	}
}

func isRemovedEng(eng *fakeEngine, ih string) bool {
	eng.mu.Lock()
	defer eng.mu.Unlock()
	return eng.removed[ih]
}

// The removal of a stream does not hold Disk's lock: other streams are
// read meanwhile; a reader of the stream being removed waits for it.
func TestDropStreamDoesNotBlockOthers(t *testing.T) {
	eng := newFakeEngine()
	eng.put(torrent.Status{InfoHash: "a", Mode: torrent.ModeStream})
	d := &Disk{Root: "/", Engine: eng}
	inRemove, finish := make(chan struct{}), make(chan struct{})
	eng.beforeRm = func(_ *fakeEngine, ih string) {
		close(inRemove)
		<-finish
	}
	errc := make(chan error, 1)
	go func() { errc <- d.DropStream(context.Background(), "a") }()
	<-inRemove
	used := make(chan struct{})
	go func() { d.Use("other")(); d.Touch("other"); close(used) }()
	select {
	case <-used:
	case <-time.After(2 * time.Second):
		t.Fatal("another stream's read waited for the removal")
	}
	waited := make(chan struct{})
	go func() { d.Use("a")(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("a reader of the stream being removed did not wait")
	case <-time.After(50 * time.Millisecond):
	}
	close(finish)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	<-waited
}

// cancelledStream asks «Смотреть сейчас» of S01E03 from ih as profile uid
// and cancels the request while the torrent's file list is awaited (the
// viewer pressed «Отмена»): Acquire answers the context's error.
func cancelledStream(t *testing.T, e *env, ih, uid string) {
	t.Helper()
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Теория большого взрыва S01", "https://t/"+ih, 1, 5)}}
	e.eng.nextIH = ih
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "TBBT.S01/TBBT.S01E03.mkv", Size: 800}}
	ctx, cancel := context.WithCancel(context.Background())
	e.eng.mu.Lock()
	e.eng.waitInfo = func(wctx context.Context) error {
		cancel()
		<-wctx.Done()
		return wctx.Err()
	}
	e.eng.mu.Unlock()
	defer func() {
		e.eng.mu.Lock()
		e.eng.waitInfo = nil
		e.eng.mu.Unlock()
	}()
	_, err := e.svc.Acquire(ctx, AcquireRequest{TMDBID: 1418, Kind: KindSeries, Season: 1, Episode: 3, Mode: ModeStream, UserID: 1, UserUID: uid})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stream: %v", err)
	}
}

// Cancelled before the player opened: the stream torrent nobody watches
// goes a grace later, its acquisition with it.
func TestStreamCancelledBeforePlayerDropsIt(t *testing.T) {
	shortGrace(t)
	e := newEnv(t)
	cancelledStream(t, e, ihSwitchA, "uid-one")
	if !waitRemoved(e, ihSwitchA) || !e.eng.removedData[ihSwitchA] {
		t.Fatalf("cancelled stream kept: removed %v", e.eng.removed)
	}
	if acquisitionCount(t, e, ihSwitchA) != 0 {
		t.Fatal("the cancelled stream's acquisition kept")
	}
}

// Another viewer watches the same torrent: a cancelled start of it keeps it.
func TestStreamCancelledKeepsAnotherViewersStream(t *testing.T) {
	grace := shortGrace(t)
	e := newEnv(t)
	streamAs(t, e, ihSwitchA, 3, "uid-two")
	cancelledStream(t, e, ihSwitchA, "uid-one")
	time.Sleep(3 * grace)
	if isRemoved(e, ihSwitchA) {
		t.Fatal("another viewer's stream was dropped by a cancelled start")
	}
}
