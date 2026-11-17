package pinned

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// renameEnv: a download of the pinned release (E01, E02 and E01's track)
// on disk under <saveDir>/<release>; the tracker's topic is then
// re-uploaded with E01–E02 renamed, the track kept and E03 new, and the
// pin refreshed (the engine replaced it). imported: what Imported says.
type renameEnv struct {
	*env
	saveDir, root string
	oldVideos     []string // full paths of the renamed-away files
	track, newE1  string
	newIH         string
	imported      map[string]time.Time
}

func write(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newRenameEnv(t *testing.T, extraOld ...torrent.FileInfo) *renameEnv {
	t.Helper()
	ctx := context.Background()
	r := &renameEnv{env: newEnv(t), saveDir: filepath.Join(t.TempDir(), "torrents"), imported: map[string]time.Time{}}
	r.root = filepath.Join(r.saveDir, releaseName)
	link := "https://tracker.test/dl/1"
	oldPaths := []string{"Show.S01E01.1080p.mkv", "Show.S01E02.1080p.mkv", "Sound/Show.S01E01.mka"}
	mf := metafile(t, releaseName, oldPaths...)
	r.idx.downloads[link] = cardigann.DownloadResult{Torrent: mf}
	p, err := r.svc.PinRelease(ctx, r.title.ID, "3", link, "", releaseName, by(r.users[0]))
	if err != nil {
		t.Fatal(err)
	}
	var files []torrent.FileInfo
	for i, rel := range oldPaths {
		full := filepath.Join(r.root, filepath.FromSlash(rel))
		write(t, full)
		files = append(files, torrent.FileInfo{Index: i, Path: releaseName + "/" + rel, FullPath: full, Size: 1000, Selected: true})
	}
	r.oldVideos = []string{files[0].FullPath, files[1].FullPath}
	r.track = files[2].FullPath
	files = append(files, extraOld...)
	r.eng.has[p.InfoHash] = true
	r.eng.modes = map[string]string{p.InfoHash: torrent.ModeDownload}
	r.eng.data = map[string]*fakeData{p.InfoHash: {saveDir: r.saveDir, files: files, completed: true, status: torrent.StatusSeeding}}
	r.svc.Imported = func(_ context.Context, ih string) (time.Time, bool, error) {
		at, ok := r.imported[ih]
		return at, ok, nil
	}
	newPaths := []string{"Show.S01E01.WEB-DL.mkv", "Show.S01E02.WEB-DL.mkv", "Sound/Show.S01E01.mka", "Show.S01E03.WEB-DL.mkv"}
	nmf := metafile(t, releaseName, newPaths...)
	r.idx.downloads[link] = cardigann.DownloadResult{Torrent: nmf}
	r.newIH = ihOf(t, nmf)
	r.now = r.now.Add(7 * time.Hour)
	if changed, err := r.svc.Refresh(ctx, p); err != nil || !changed {
		t.Fatalf("refresh %v %v", changed, err)
	}
	r.newE1 = filepath.Join(r.root, "Show.S01E01.WEB-DL.mkv")
	write(t, r.newE1) // the new download's own file
	return r
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func (r *renameEnv) sweep(t *testing.T) {
	t.Helper()
	if err := r.svc.SweepStale(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func (r *renameEnv) pending(t *testing.T) string {
	t.Helper()
	var raw string
	if err := r.svc.DB.QueryRow(`SELECT stale_json FROM pinned_releases WHERE title_id=?`, r.title.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestRenamedReleaseOldFilesDeletedAfterImport: the files a renamed
// re-upload no longer lists are deleted only once the new download is
// complete and imported after the replace; a file the library names waits
// for the library to let go; files the new release lists stay.
func TestRenamedReleaseOldFilesDeletedAfterImport(t *testing.T) {
	r := newRenameEnv(t)
	if raw := r.pending(t); !strings.Contains(raw, "Show.S01E01.1080p.mkv") || strings.Contains(raw, ".mka") {
		t.Fatalf("pending %s", raw)
	}
	nd := r.eng.data[r.newIH]
	// downloading: kept
	r.sweep(t)
	// complete, not imported: kept
	nd.completed, nd.status = true, torrent.StatusSeeding
	r.sweep(t)
	// imported before the replace (the old torrent's import): kept
	r.imported[r.newIH] = r.now.Add(-time.Hour)
	r.sweep(t)
	for _, p := range r.oldVideos {
		if !exists(p) {
			t.Fatalf("%s deleted too soon", p)
		}
	}
	// the library still names E02's old file
	r.lib.Items = []library.Item{{ID: 1, Kind: "episode", Title: "E2"}}
	r.lib.Files = []library.File{{ID: 1, ItemID: 1, Path: r.oldVideos[1]}}
	// imported after the replace: deleted
	r.imported[r.newIH] = r.now.Add(time.Minute)
	r.sweep(t)
	if exists(r.oldVideos[0]) || !exists(r.oldVideos[1]) || !exists(r.track) || !exists(r.newE1) {
		t.Fatalf("after import: E01 old %v, E02 old %v, track %v, new %v", exists(r.oldVideos[0]), exists(r.oldVideos[1]), exists(r.track), exists(r.newE1))
	}
	if raw := r.pending(t); !strings.Contains(raw, "Show.S01E02.1080p.mkv") || strings.Contains(raw, "S01E01.1080p") {
		t.Fatalf("pending %s", raw)
	}
	// the library let go of it (rescanned onto the new file): deleted too
	r.lib.Files = nil
	r.sweep(t)
	if exists(r.oldVideos[1]) || r.pending(t) != "" || !exists(r.root) {
		t.Fatalf("E02 old %v, pending %q, root %v", exists(r.oldVideos[1]), r.pending(t), exists(r.root))
	}
}

// TestRenamedReleaseNothingDeletedOnFailure: a failed new download keeps
// the old files; one removed from the engine (or a pin removed) forgets
// them, deleting nothing.
func TestRenamedReleaseNothingDeletedOnFailure(t *testing.T) {
	r := newRenameEnv(t)
	nd := r.eng.data[r.newIH]
	nd.completed, nd.status = true, torrent.StatusError
	r.imported[r.newIH] = r.now.Add(time.Minute)
	r.sweep(t)
	if !exists(r.oldVideos[0]) || r.pending(t) == "" {
		t.Fatal("deleted after a failure")
	}
	delete(r.eng.has, r.newIH)
	r.sweep(t)
	if !exists(r.oldVideos[0]) || !exists(r.oldVideos[1]) || r.pending(t) != "" {
		t.Fatalf("removed download: old %v, pending %q", exists(r.oldVideos[0]), r.pending(t))
	}
	// back in the engine later: nothing is pending any more
	r.eng.has[r.newIH] = true
	nd.status = torrent.StatusSeeding
	r.sweep(t)
	if !exists(r.oldVideos[0]) {
		t.Fatal("deleted after the record was dropped")
	}

	// unpinned: the record goes with the pin
	u := newRenameEnv(t)
	u.eng.data[u.newIH].completed = true
	u.imported[u.newIH] = u.now.Add(time.Minute)
	if err := u.svc.Unpin(context.Background(), u.title.ID); err != nil {
		t.Fatal(err)
	}
	u.sweep(t)
	if !exists(u.oldVideos[0]) {
		t.Fatal("deleted after unpin")
	}
}

// TestRenamedReleaseNeverDeletesOutsideRoot: a file of the old torrent
// whose path leaves its own folder, a link inside it and a recorded path
// outside it are never deleted (nor what a link points to).
func TestRenamedReleaseNeverDeletesOutsideRoot(t *testing.T) {
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "keep.mkv")
	write(t, outside)
	target := filepath.Join(outsideDir, "target.mkv")
	write(t, target)
	// a file listed under the torrent's folder but stored elsewhere
	odd := torrent.FileInfo{Index: 3, Path: releaseName + "/odd.mkv", FullPath: outside, Size: 1}
	// a folder of another name beside it (a second root): not this torrent's own
	other := torrent.FileInfo{Index: 4, Path: "Other/x.mkv", Size: 1}
	r := newRenameEnv(t, odd, other)
	other.FullPath = filepath.Join(r.saveDir, "Other", "x.mkv")
	link := filepath.Join(r.root, "link.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// a hand-made record naming paths outside the root and a link
	if _, err := r.svc.DB.Exec(`UPDATE pinned_releases SET stale_json=? WHERE title_id=?`,
		`[{"root":"`+r.root+`","paths":["`+outside+`","`+r.root+`/../../escape.mkv","`+link+`"],"since":"2000-01-01T00:00:00Z"}]`, r.title.ID); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(filepath.Dir(r.saveDir), "escape.mkv"))
	r.eng.data[r.newIH].completed = true
	r.imported[r.newIH] = r.now.Add(time.Minute)
	r.sweep(t)
	if !exists(outside) || !exists(target) || !exists(link) || !exists(filepath.Join(filepath.Dir(r.saveDir), "escape.mkv")) {
		t.Fatalf("outside %v target %v link %v escape %v", exists(outside), exists(target), exists(link), exists(filepath.Join(filepath.Dir(r.saveDir), "escape.mkv")))
	}
	// what staleOf records leaves both out
	st := staleOf(modeHandle{ih: "x", mode: torrent.ModeDownload, d: &fakeData{saveDir: r.saveDir,
		files: []torrent.FileInfo{odd, other, {Index: 0, Path: releaseName + "/a.mkv", FullPath: filepath.Join(r.root, "a.mkv")}}}}, got{})
	if st == nil || len(st.Paths) != 1 || st.Paths[0] != filepath.Join(r.root, "a.mkv") || st.Root != r.root {
		t.Fatalf("stale %+v", st)
	}
}

// TestStaleListedAgainNotDeleted: old files a later re-upload lists again
// are the current release's: the record forgets them (also when that
// replace leaves nothing behind), and a sweep never deletes a file the
// current torrent lists.
func TestStaleListedAgainNotDeleted(t *testing.T) {
	ctx := context.Background()
	r := newRenameEnv(t)
	p, _, _ := r.svc.ByTitle(ctx, r.title.ID)
	// the old names are back, beside the new ones
	mf := metafile(t, releaseName, "Show.S01E01.1080p.mkv", "Show.S01E02.1080p.mkv", "Show.S01E01.WEB-DL.mkv", "Show.S01E02.WEB-DL.mkv",
		"Sound/Show.S01E01.mka", "Show.S01E03.WEB-DL.mkv")
	r.idx.downloads["https://tracker.test/dl/1"] = cardigann.DownloadResult{Torrent: mf}
	r.now = r.now.Add(7 * time.Hour)
	if changed, err := r.svc.Refresh(ctx, p); err != nil || !changed {
		t.Fatalf("refresh %v %v", changed, err)
	}
	if raw := r.pending(t); strings.Contains(raw, "1080p.mkv") {
		t.Fatalf("listed again, still pending: %s", raw)
	}
	// a record naming a file of the current torrent (written before the fix)
	ih3 := ihOf(t, mf)
	if _, err := r.svc.DB.Exec(`UPDATE pinned_releases SET stale_json=? WHERE title_id=?`,
		`[{"root":"`+r.root+`","paths":["`+r.oldVideos[0]+`"],"since":"2000-01-01T00:00:00Z"}]`, r.title.ID); err != nil {
		t.Fatal(err)
	}
	r.eng.data[ih3].completed = true
	r.imported[ih3] = r.now.Add(time.Minute)
	r.sweep(t)
	if !exists(r.oldVideos[0]) || r.pending(t) != "" {
		t.Fatalf("a file of the current release: exists %v, pending %q", exists(r.oldVideos[0]), r.pending(t))
	}
}

// TestPendingDeletionSurvivesEngineNotLoaded: a new download the engine
// does not answer for but still stores (not loaded yet) keeps the record;
// only one really gone drops it, deleting nothing.
func TestPendingDeletionSurvivesEngineNotLoaded(t *testing.T) {
	r := newRenameEnv(t)
	delete(r.eng.has, r.newIH)
	if _, err := r.svc.DB.Exec(`INSERT INTO torrents (infohash, name, mode, save_dir) VALUES (?,?,'download',?)`, r.newIH, releaseName, r.saveDir); err != nil {
		t.Fatal(err)
	}
	r.sweep(t)
	if r.pending(t) == "" || !exists(r.oldVideos[0]) {
		t.Fatal("record dropped while the torrent is stored")
	}
	if _, err := r.svc.DB.Exec(`DELETE FROM torrents`); err != nil {
		t.Fatal(err)
	}
	r.sweep(t)
	if r.pending(t) != "" || !exists(r.oldVideos[0]) {
		t.Fatalf("gone: pending %q, old %v", r.pending(t), exists(r.oldVideos[0]))
	}
}
