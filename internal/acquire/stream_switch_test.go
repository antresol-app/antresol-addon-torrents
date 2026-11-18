package acquire

import (
	"context"
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
