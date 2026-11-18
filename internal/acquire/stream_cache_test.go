package acquire

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

func diskAt(percent int64) *Disk {
	d := &Disk{Root: "/media"}
	d.statfs = func(string) (int64, int64, error) { return 1000, 1000 - percent*10, nil }
	return d
}

func TestDiskUsageAndLevel(t *testing.T) {
	for _, c := range []struct {
		pct  int64
		want string
	}{{84, LevelOK}, {85, LevelWarn}, {90, LevelWarn}, {95, LevelFull}, {96, LevelFull}} {
		d := diskAt(c.pct)
		if got := d.Level(); got != c.want {
			t.Errorf("%d%%: %s, want %s", c.pct, got, c.want)
		}
		used, total, err := d.Usage()
		if err != nil || total != 1000 || used != c.pct*10 {
			t.Errorf("usage %d/%d %v", used, total, err)
		}
	}
}

func TestPruneOnlyStream(t *testing.T) {
	now := time.Now()
	eng := newFakeEngine()
	eng.put(torrent.Status{InfoHash: "old", Mode: torrent.ModeStream, Done: 600, AddedAt: now.Add(-3 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "mid", Mode: torrent.ModeStream, Done: 600, AddedAt: now.Add(-2 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "new", Mode: torrent.ModeStream, Done: 600, AddedAt: now.Add(-1 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "keep", Mode: torrent.ModeDownload, Done: 5000, AddedAt: now.Add(-9 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "seed", Mode: torrent.ModeSeed, Done: 5000, AddedAt: now.Add(-9 * time.Hour)})
	d := &Disk{Root: "/", Engine: eng}
	d.Touch("old") // watched most recently: survives
	release := d.Use("mid")
	defer release()

	if err := d.PruneStreamCache(context.Background(), 1000); err != nil {
		t.Fatal(err)
	}
	// 1800 bytes of stream cache > 1000: the least recently used one that is
	// not being watched goes ("new"); "mid" is playing and "old" was just
	// watched (kept although the cache stays above its limit).
	if !eng.removed["new"] || !eng.removedData["new"] {
		t.Fatalf("new not removed with data: %v", eng.removed)
	}
	if eng.removed["mid"] {
		t.Fatal("a torrent being watched was pruned")
	}
	if eng.removed["keep"] || eng.removed["seed"] {
		t.Fatal("download/seed torrent pruned")
	}
	if eng.removed["old"] {
		t.Fatal("a title watched a moment ago was pruned")
	}
}

// a title bigger than the whole cache is kept while it is watched,
// even between requests (no reader open), and the most recently watched
// title is never pruned.
func TestPruneKeepsTitleBeingWatched(t *testing.T) {
	now := time.Now()
	eng := newFakeEngine()
	eng.put(torrent.Status{InfoHash: "bdremux", Mode: torrent.ModeStream, Done: 30 << 30, AddedAt: now.Add(-2 * time.Hour)})
	d := &Disk{Root: "/", Engine: eng}
	release := d.Use("bdremux")
	release() // the player paused: no request in flight
	d.now = func() time.Time { return now.Add(10 * time.Minute) }
	if err := d.PruneStreamCache(context.Background(), 20<<30); err != nil {
		t.Fatal(err)
	}
	if eng.removed["bdremux"] {
		t.Fatal("the title being watched was pruned between requests")
	}
	// Hours later it is still the newest one in the cache: kept.
	d.now = func() time.Time { return now.Add(5 * time.Hour) }
	if err := d.PruneStreamCache(context.Background(), 20<<30); err != nil {
		t.Fatal(err)
	}
	if eng.removed["bdremux"] {
		t.Fatal("the most recently watched title was pruned")
	}
	// Another title is watched later: the old one may go now.
	eng.put(torrent.Status{InfoHash: "next", Mode: torrent.ModeStream, Done: 1 << 30, AddedAt: now.Add(4 * time.Hour)})
	if err := d.PruneStreamCache(context.Background(), 20<<30); err != nil {
		t.Fatal(err)
	}
	if !eng.removed["bdremux"] || eng.removed["next"] {
		t.Fatalf("removed %v", eng.removed)
	}
}

func TestPruneRechecksBeforeRemove(t *testing.T) {
	now := time.Now()
	eng := newFakeEngine()
	eng.put(torrent.Status{InfoHash: "promoted", Mode: torrent.ModeStream, Done: 600, AddedAt: now.Add(-3 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "watched", Mode: torrent.ModeStream, Done: 600, AddedAt: now.Add(-2 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "idle", Mode: torrent.ModeStream, Done: 600, AddedAt: now.Add(-1 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "newest", Mode: torrent.ModeStream, Done: 1, AddedAt: now.Add(-40 * time.Minute)})
	d := &Disk{Root: "/", Engine: eng}
	var release func()
	// After the snapshot: one torrent is promoted, another starts playing.
	d.afterSnapshot = func() {
		eng.setMode("promoted", torrent.ModeDownload)
		release = d.Use("watched")
	}
	if err := d.PruneStreamCache(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	release()
	if eng.removed["promoted"] || eng.removed["watched"] {
		t.Fatalf("removed %v", eng.removed)
	}
	if !eng.removed["idle"] {
		t.Fatal("idle stream torrent kept")
	}
}

// A pruned stream-cache entry is a removed download: its watch inputs are
// dropped at the core too (SetOnRemoved), and only the pruned one's.
func TestPruneTellsOnRemoved(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	const ihA, ihB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	e.eng.put(torrent.Status{InfoHash: ihA, Mode: torrent.ModeStream, Done: 600, AddedAt: now.Add(-3 * time.Hour)})
	e.eng.put(torrent.Status{InfoHash: ihB, Mode: torrent.ModeStream, Done: 600, AddedAt: now.Add(-1 * time.Hour)})
	var got []string
	e.svc.SetOnRemoved(func(ih string) { got = append(got, ih) })
	if err := e.svc.o.Disk.PruneStreamCache(context.Background(), 700); err != nil {
		t.Fatal(err)
	}
	if !e.eng.removed[ihA] || e.eng.removed[ihB] || len(got) != 1 || got[0] != ihA {
		t.Fatalf("removed %v, told %v", e.eng.removed, got)
	}
}

// While the media disk is offline the stream torrents report no data:
// nothing is pruned (their files would stay on the disk without a
// record). A torrent in error, detached (no metadata) or with its data
// missing is never taken for one that never received data.
func TestPruneKeepsStreamsOfOfflineDisk(t *testing.T) {
	now := time.Now()
	eng := newFakeEngine()
	offline := torrent.Status{InfoHash: "off", Mode: torrent.ModeStream, Status: torrent.StatusError, Error: "Медиадиск недоступен", AddedAt: now.Add(-2 * time.Hour)}
	eng.put(offline)
	eng.put(torrent.Status{InfoHash: "empty", Mode: torrent.ModeStream, Size: 1 << 30, AddedAt: now.Add(-2 * time.Hour)})
	eng.offline = "Медиадиск недоступен"
	d := &Disk{Root: "/", Engine: eng}
	if err := d.PruneStreamCache(context.Background(), 1<<40); err != nil {
		t.Fatal(err)
	}
	if eng.removed["off"] || eng.removed["empty"] {
		t.Fatalf("pruned while the media disk is offline: %v", eng.removed)
	}

	eng.offline = ""
	eng.put(torrent.Status{InfoHash: "nometa", Mode: torrent.ModeStream, AddedAt: now.Add(-2 * time.Hour)})
	eng.put(torrent.Status{InfoHash: "lost", Mode: torrent.ModeStream, Size: 1 << 30, DataMissing: true, AddedAt: now.Add(-2 * time.Hour)})
	if err := d.PruneStreamCache(context.Background(), 1<<40); err != nil {
		t.Fatal(err)
	}
	if eng.removed["off"] || eng.removed["nometa"] || eng.removed["lost"] {
		t.Fatalf("an errored, detached or data-missing stream taken for empty: %v", eng.removed)
	}
	if !eng.removed["empty"] || !eng.removedData["empty"] {
		t.Fatal("a stream that never received data kept on a reachable disk")
	}

	// an unreadable disk prunes nothing either
	eng.put(torrent.Status{InfoHash: "empty2", Mode: torrent.ModeStream, Size: 1 << 30, AddedAt: now.Add(-2 * time.Hour)})
	d.statfs = func(string) (int64, int64, error) { return 0, 0, os.ErrNotExist }
	if err := d.PruneStreamCache(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if eng.removed["empty2"] {
		t.Fatal("pruned on an unreadable media disk")
	}
}
