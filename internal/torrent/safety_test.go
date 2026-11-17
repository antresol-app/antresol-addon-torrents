package torrent

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

func pieceRows(t *testing.T, d *sql.DB, ih string) (all, complete int) {
	t.Helper()
	if err := d.QueryRow(`SELECT COUNT(*), COALESCE(SUM(complete),0) FROM torrent_pieces WHERE infohash=?`, ih).Scan(&all, &complete); err != nil {
		t.Fatal(err)
	}
	return
}

// started with the media disk missing, the engine attaches nothing,
// keeps piece state, refuses new torrents, never creates folders on the
// root disk, and starts everything once the disk is back.
func TestStorageOfflineHoldsTorrents(t *testing.T) {
	media := filepath.Join(t.TempDir(), "media-disk")
	src := filepath.Join(media, "torrents")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := newFixture(t, src)
	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cfg := EngineConfig{
		DataDir: dataDir, StreamDir: filepath.Join(media, "tmp", "vault-stream"), LocalOnly: true,
		StallTimeout: 2 * time.Second, tick: 50 * time.Millisecond,
		StorageCheck: DirsPresent(media, src), StorageRecheck: 100 * time.Millisecond,
	}
	e, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if err := e.Pause(context.Background(), fx.ih); err != nil {
		t.Fatal(err)
	}
	e.Close()
	_, before := pieceRows(t, d, fx.ih)
	if before == 0 {
		t.Fatal("no complete pieces recorded")
	}

	// The USB disk is not mounted: the mount point is an empty folder.
	unplugged := media + ".unplugged"
	if err := os.Rename(media, unplugged); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(media, 0o755); err != nil {
		t.Fatal(err)
	}
	e2, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if e2.Offline() == "" {
		t.Fatal("engine did not notice the missing disk")
	}
	time.Sleep(400 * time.Millisecond) // several monitor ticks and rechecks
	s, ok := status(e2, fx.ih)
	if !ok || s.Status != StatusError || !strings.Contains(s.Error, "Медиадиск недоступен") {
		t.Fatalf("status %+v", s)
	}
	if _, after := pieceRows(t, d, fx.ih); after != before {
		t.Fatalf("piece state changed while offline: %d → %d", before, after)
	}
	if entries, _ := os.ReadDir(media); len(entries) != 0 {
		t.Fatalf("engine wrote onto the root disk: %v", entries)
	}
	other := newFixture(t, t.TempDir())
	if _, err := e2.AddTorrentFile(context.Background(), other.metafile, AddOptions{Mode: ModeDownload, SaveDir: src}); !errors.Is(err, ErrStorageOffline) {
		t.Fatalf("add while offline: %v", err)
	}
	if err := e2.Resume(context.Background(), fx.ih); !errors.Is(err, ErrStorageOffline) {
		t.Fatalf("resume while offline: %v", err)
	}
	var st string
	if err := d.QueryRow(`SELECT status FROM torrents WHERE infohash=?`, fx.ih).Scan(&st); err != nil || st != StatusPaused {
		t.Fatalf("stored status %q %v: the hold must survive", st, err)
	}

	// The disk comes back: the torrent starts from its stored state.
	if err := os.Remove(media); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(unplugged, media); err != nil {
		t.Fatal(err)
	}
	s = waitFor(t, e2, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusPaused && s.Done == testSize })
	if s.Error != "" || e2.Offline() != "" {
		t.Fatalf("after the disk is back: %+v (offline %q)", s, e2.Offline())
	}
}

// a download whose files are all gone at start is held with an error
// instead of being downloaded again; its piece state is kept.
func TestMissingDataIsHeldNotRedownloaded(t *testing.T) {
	src := t.TempDir()
	fx := newFixture(t, src)
	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true, StallTimeout: 2 * time.Second,
		tick: 50 * time.Millisecond, StorageRecheck: 100 * time.Millisecond}
	e, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	e.Close()
	_, before := pieceRows(t, d, fx.ih)
	moved := t.TempDir()
	if err := os.Rename(filepath.Join(src, fx.name), filepath.Join(moved, fx.name)); err != nil {
		t.Fatal(err)
	}

	e2, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	s := waitFor(t, e2, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusError })
	if !strings.Contains(s.Error, "Файлы раздачи не найдены") {
		t.Fatalf("error %q", s.Error)
	}
	time.Sleep(300 * time.Millisecond)
	if _, after := pieceRows(t, d, fx.ih); after != before {
		t.Fatalf("piece state forgotten: %d → %d", before, after)
	}
	if _, err := os.Stat(filepath.Join(src, fx.name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file re-created at the old path: %v", err)
	}
	// The files come back: the torrent seeds again by itself.
	if err := os.Rename(filepath.Join(moved, fx.name), filepath.Join(src, fx.name)); err != nil {
		t.Fatal(err)
	}
	s = waitFor(t, e2, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if s.Error != "" || s.Downloaded != 0 {
		t.Fatalf("after the files are back: %+v", s)
	}
}

// a move of a folder that holds torrent data re-points the torrent:
// nothing is downloaded or re-hashed, it keeps seeding from the new place.
func TestRelocateFollowsMove(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "torrents", "movies")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	fx := newFixture(t, src)
	e := openEngine(t, t.TempDir(), engOpts{})
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	from := filepath.Join(root, "torrents")
	to := filepath.Join(root, "archive", "torrents")
	os.MkdirAll(filepath.Dir(to), 0o755)

	follow, stop := e.PlanRelocate(from, to)
	if len(follow) != 1 || follow[0] != fx.ih || len(stop) != 0 {
		t.Fatalf("plan: follow %v stop %v", follow, stop)
	}
	// Trash: data goes away, the torrent must be stopped.
	if f, s := e.PlanRelocate(from, ""); len(f) != 0 || len(s) != 1 {
		t.Fatalf("trash plan: follow %v stop %v", f, s)
	}
	// Renaming the torrent's own file changes its name: stop.
	file := filepath.Join(src, fx.name)
	if f, s := e.PlanRelocate(file, filepath.Join(src, "renamed.mkv")); len(f) != 0 || len(s) != 1 {
		t.Fatalf("rename plan: follow %v stop %v", f, s)
	}

	if err := e.Relocate(context.Background(), from, to, func() error { return os.Rename(from, to) }); err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, e, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusSeeding && s.Done == testSize })
	if want := filepath.Join(to, "movies"); s.SaveDir != want || s.Downloaded != 0 {
		t.Fatalf("after move: save %q want %q, downloaded %d", s.SaveDir, want, s.Downloaded)
	}
	if _, err := os.Stat(filepath.Join(src, fx.name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old path re-created: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(to, "movies", fx.name))
	if err != nil || !bytes.Equal(got, fx.data) {
		t.Fatalf("data at the new place: %v", err)
	}
	var save string
	if err := e.db.QueryRow(`SELECT save_dir FROM torrents WHERE infohash=?`, fx.ih).Scan(&save); err != nil || save != filepath.Join(to, "movies") {
		t.Fatalf("stored save_dir %q %v", save, err)
	}

	// A failed rename re-attaches the torrent unchanged.
	boom := errors.New("boom")
	if err := e.Relocate(context.Background(), to, filepath.Join(root, "x"), func() error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
	s = waitFor(t, e, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if s.SaveDir != filepath.Join(to, "movies") {
		t.Fatalf("save dir changed by a failed move: %q", s.SaveDir)
	}
}

// the monitor's periodic save writes only the counters, so a promote
// saved meanwhile is never overwritten by its stale copy.
func TestCounterSaveKeepsMode(t *testing.T) {
	e := openEngine(t, t.TempDir(), engOpts{})
	fx := newFixture(t, t.TempDir())
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeStream, AddedBy: 7, AddedByUID: "uid-7"}); err != nil {
		t.Fatal(err)
	}
	if err := e.SetModeTo(context.Background(), fx.ih, ModeDownload, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := e.st.updateCounters(context.Background(), []counters{{ih: fx.ih, up: 123, down: 456}}); err != nil {
		t.Fatal(err)
	}
	var mode string
	var target sql.NullString
	var up, down int64
	if err := e.db.QueryRow(`SELECT mode, target_dir, uploaded, downloaded FROM torrents WHERE infohash=?`, fx.ih).Scan(&mode, &target, &up, &down); err != nil {
		t.Fatal(err)
	}
	if mode != ModeDownload || !target.Valid || up != 123 || down != 456 {
		t.Fatalf("mode %q target %v counters %d/%d", mode, target, up, down)
	}
	// who added it is part of the status.
	s, _ := status(e, fx.ih)
	if s.AddedBy == nil || *s.AddedBy != 7 || s.AddedByUID != "uid-7" {
		t.Fatalf("addedBy %v", s.AddedBy)
	}
}

// Deferred minor: with two blocked duplicates of one owner, the
// senior one takes the files over when the owner goes; the other stays
// blocked by it (never two torrents writing one path).
func TestUnblockDuplicatesSeniorFirst(t *testing.T) {
	src := t.TempDir()
	a := newFixture(t, src)
	b := sameDataFrom(t, a, "b") // the same files under other infohashes
	c := sameDataFrom(t, a, "c")
	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true, StallTimeout: 2 * time.Second, tick: 100 * time.Millisecond}
	e, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddTorrentFile(context.Background(), a.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, e, a.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	for _, fx := range []fixture{b, c} {
		if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir()}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // distinct added_at: restore order
	}
	e.Close()
	if _, err := d.Exec(`UPDATE torrents SET save_dir=? WHERE infohash IN (?,?)`, src, b.ih, c.ih); err != nil {
		t.Fatal(err)
	}
	e2, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { e2.Close(); d.Close() }()
	waitFor(t, e2, b.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusError })
	waitFor(t, e2, c.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusError })
	// Both blocked by the owner (which one c names depends on which
	// metadata callback ran first).
	e2.mu.Lock()
	e2.entries[c.ih].dupOf = a.ih
	e2.mu.Unlock()
	if err := e2.Remove(context.Background(), a.ih, false); err != nil {
		t.Fatal(err)
	}
	e2.mu.Lock()
	bDup, cDup := e2.entries[b.ih].dupOf, e2.entries[c.ih].dupOf
	e2.mu.Unlock()
	if bDup != "" || cDup != b.ih {
		t.Fatalf("after the owner left: b blocked by %q, c blocked by %q (want b free, c blocked by b)", bDup, cDup)
	}
}

// C-N1 + C-N3: a data-missing torrent says so (the UI offers «Продолжить»),
// and a pause on it survives a restart that stored it while detached.
func TestMissingDataKeepsHoldAcrossRestart(t *testing.T) {
	src := t.TempDir()
	fx := newFixture(t, src)
	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	cfg := EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), LocalOnly: true, StallTimeout: 2 * time.Second,
		tick: 50 * time.Millisecond, StorageRecheck: 100 * time.Millisecond}
	e, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeSeed, SaveDir: src}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, e, fx.ih, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding })
	if err := e.Pause(context.Background(), fx.ih); err != nil {
		t.Fatal(err)
	}
	e.Close()
	moved := t.TempDir()
	if err := os.Rename(filepath.Join(src, fx.name), filepath.Join(moved, fx.name)); err != nil {
		t.Fatal(err)
	}

	e2, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	s := waitFor(t, e2, fx.ih, 5*time.Second, func(s Status) bool { return s.Status == StatusError })
	if !s.DataMissing {
		t.Fatalf("DataMissing not reported: %+v", s)
	}
	e2.Close() // stores the torrent while it is detached

	if err := os.Rename(filepath.Join(moved, fx.name), filepath.Join(src, fx.name)); err != nil {
		t.Fatal(err)
	}
	e3, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	defer e3.Close()
	s = waitFor(t, e3, fx.ih, 10*time.Second, func(s Status) bool { return s.Status != StatusError && s.Status != StatusQueued })
	if s.Status != StatusPaused || s.DataMissing {
		t.Fatalf("hold lost over a restart: %+v", s)
	}
}
