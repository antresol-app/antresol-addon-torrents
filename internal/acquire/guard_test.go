package acquire

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

func setDisk(e *env, pct int64) {
	*e.disk = *diskAt(pct)
	e.disk.Engine = e.eng
}

// at ≥ 95 % running downloads wait for space (seeding and stream
// torrents untouched, forced ones too); under 93 % they resume by
// themselves, oldest first.
func TestDiskGuardHoldsAndResumes(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	done := now.Add(-time.Hour)
	e.eng.put(torrent.Status{InfoHash: "old", Mode: ModeDownload, Status: torrent.StatusDownloading, Size: 100, Done: 10, AddedAt: now.Add(-3 * time.Hour)})
	e.eng.put(torrent.Status{InfoHash: "new", Mode: ModeDownload, Status: torrent.StatusDownloading, Size: 100, Done: 10, AddedAt: now.Add(-2 * time.Hour)})
	e.eng.put(torrent.Status{InfoHash: "forced", Mode: ModeDownload, Status: torrent.StatusDownloading, AddedAt: now})
	e.eng.put(torrent.Status{InfoHash: "seed", Mode: ModeDownload, Status: torrent.StatusSeeding, CompletedAt: &done})
	e.eng.put(torrent.Status{InfoHash: "stream", Mode: ModeStream, Status: torrent.StatusDownloading})
	e.eng.put(torrent.Status{InfoHash: "paused", Mode: ModeDownload, Status: torrent.StatusPaused})
	e.svc.setForced("forced")

	setDisk(e, 96)
	e.svc.checkDisk()
	if got := e.eng.held; len(got) != 2 || got[0] != "old" || got[1] != "new" {
		t.Fatalf("held %v, want [old new]", got)
	}

	setDisk(e, 94) // between the marks: nothing changes
	e.svc.checkDisk()
	if len(e.eng.resumed) != 0 {
		t.Fatalf("resumed at 94%%: %v", e.eng.resumed)
	}

	// 90 % used of 1000 bytes: room for 50 bytes below 95 %: "old" (90
	// left) starts anyway as the first; "new" does not fit any more.
	setDisk(e, 90)
	e.svc.checkDisk()
	if got := e.eng.resumed; len(got) != 1 || got[0] != "old" {
		t.Fatalf("resumed %v, want [old]", got)
	}
	setDisk(e, 50)
	e.svc.checkDisk()
	if got := e.eng.resumed; len(got) != 2 || got[1] != "new" {
		t.Fatalf("resumed %v, want [old new]", got)
	}
	if st, _ := e.eng.Get("paused"); st.Status().Status != torrent.StatusPaused {
		t.Fatal("a paused torrent was resumed")
	}
}

// «Загрузить всё равно» on a waiting download: the guard leaves it alone.
func TestResumeFromWaitingIsForced(t *testing.T) {
	e := newEnv(t)
	e.eng.put(torrent.Status{InfoHash: "w", Mode: ModeDownload, Status: torrent.StatusWaitingSpace})
	setDisk(e, 97)
	if err := e.svc.Resume(context.Background(), "w"); err != nil {
		t.Fatal(err)
	}
	e.svc.checkDisk()
	if len(e.eng.held) != 0 {
		t.Fatalf("forced download held again: %v", e.eng.held)
	}
}

// promoting a complete stream torrent on a full disk is not put
// on waiting_space (nothing left to download; playback keeps going).
func TestPromoteCompleteOnFullDiskNotHeld(t *testing.T) {
	e := newEnv(t)
	now := time.Now()
	e.eng.put(torrent.Status{InfoHash: "c", Mode: ModeStream, Status: torrent.StatusSeeding, CompletedAt: &now})
	setDisk(e, 97)
	st, err := e.svc.Promote(context.Background(), "c", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.eng.held) != 0 || st == torrent.StatusWaitingSpace {
		t.Fatalf("status %s held %v", st, e.eng.held)
	}
}

// organize with no file list yet (data being moved) is not an
// attempt and writes no error.
func TestOrganizeNotReadyIsNoAttempt(t *testing.T) {
	e := newEnv(t)
	e.idx.resp.Releases = nil
	now := time.Now()
	e.eng.put(torrent.Status{InfoHash: "m", Mode: ModeDownload, Status: torrent.StatusSeeding, CompletedAt: &now})
	if err := e.svc.saveAcquisition(context.Background(), acquisition{InfoHash: "m", Kind: KindMovie, TMDBID: 1, Title: "Дюна"}, 0, "", false); err != nil {
		t.Fatal(err)
	}
	e.svc.organize(context.Background(), "m", false, false)
	a, err := e.svc.acquisition(context.Background(), "m")
	if err != nil {
		t.Fatal(err)
	}
	if a.Attempts != 0 || a.Error != "" {
		t.Fatalf("attempts %d error %q", a.Attempts, a.Error)
	}
}

// while Remove drops a torrent no organize starts for it.
func TestNoOrganizeWhileRemoving(t *testing.T) {
	e := newEnv(t)
	e.svc.mu.Lock()
	e.svc.removing["r"] = true
	e.svc.mu.Unlock()
	e.svc.startOrganize("r", true)
	e.svc.mu.Lock()
	busy := e.svc.organizing["r"]
	e.svc.mu.Unlock()
	if busy {
		t.Fatal("organize started for a torrent being removed")
	}
}

// the torrent list names who added each download.
func TestTorrentsNameWhoAdded(t *testing.T) {
	e := newEnv(t)
	e.svc.o.Users = fakeUsers{5: "Мама"}
	by := int64(5)
	e.eng.put(torrent.Status{InfoHash: "x", Mode: ModeDownload, AddedBy: &by, AddedByUID: "uid-5"})
	e.eng.put(torrent.Status{InfoHash: "y", Mode: ModeDownload})
	names := map[string]string{}
	for _, v := range e.svc.Torrents() {
		names[v.InfoHash] = v.AddedByName
	}
	if names["x"] != "Мама" || names["y"] != "" {
		t.Fatalf("names %v", names)
	}
}

// The media disk is missing: a download fails with a clear message.
func TestAcquireStorageOffline(t *testing.T) {
	err := notFound(torrent.ErrStorageOffline)
	msg, ok := UserMessage(err)
	if !ok || !errors.Is(err, ErrUnavailable) || msg == "" {
		t.Fatalf("%v %q", err, msg)
	}
}
