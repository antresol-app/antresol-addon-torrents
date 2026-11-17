package torrent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

const (
	seriesName = "Сериал S01 1080p"
	replacePL  = 64 << 10 // piece length of the replace fixtures
)

// relFile is one file of a release: its path inside the release folder
// ("/"-separated) and its bytes. Two versions of a release share the slices.
type relFile struct {
	path string
	data []byte
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// episodes are ep<from>..ep<to>.mkv of size bytes each (+ i*extra, so
// sizes and offsets can be made unaligned to the piece length).
func episodes(t *testing.T, from, to, size, extra int) []relFile {
	var out []relFile
	for i := from; i <= to; i++ {
		out = append(out, relFile{fmt.Sprintf("ep%02d.mkv", i), randBytes(t, size+i*extra)})
	}
	return out
}

// placeFiles writes files into dir/seriesName.
func placeFiles(t *testing.T, dir string, files ...relFile) {
	t.Helper()
	for _, f := range files {
		p := filepath.Join(dir, seriesName, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// buildRelease makes the metafile of files as a seriesName folder (no
// network: the files are written to a temp dir and hashed there).
func buildRelease(t *testing.T, files ...relFile) (metafile []byte, ih string) {
	t.Helper()
	dir := t.TempDir()
	placeFiles(t, dir, files...)
	info := metainfo.Info{PieceLength: replacePL}
	if err := info.BuildFromFilePath(filepath.Join(dir, seriesName)); err != nil {
		t.Fatal(err)
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: ib}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mi.HashInfoBytes().HexString()
}

func verified(t *testing.T, e *Engine, ih string) (done, size int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done, size, err := e.WaitVerified(ctx, ih)
	if err != nil {
		t.Fatal(err)
	}
	return done, size
}

// filesOf maps a torrent's files by their path inside the release folder.
func filesOf(t *testing.T, e *Engine, ih string) map[string]FileInfo {
	t.Helper()
	h, ok := e.Get(ih)
	if !ok {
		t.Fatalf("no torrent %s", ih)
	}
	out := map[string]FileInfo{}
	for _, f := range h.Files() {
		out[strings.TrimPrefix(f.Path, seriesName+"/")] = f
	}
	return out
}

// sameOnDisk fails unless every file is in dir/seriesName with its bytes.
func sameOnDisk(t *testing.T, dir string, files ...relFile) {
	t.Helper()
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(dir, seriesName, filepath.FromSlash(f.path)))
		if err != nil {
			t.Fatalf("%s: %v", f.path, err)
		}
		if !bytes.Equal(b, f.data) {
			t.Fatalf("%s: other bytes on disk", f.path)
		}
	}
}

func recordOf(t *testing.T, e *Engine, ih string) record {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	en, ok := e.entries[ih]
	if !ok {
		t.Fatalf("no torrent %s", ih)
	}
	return en.rec
}

var testRelease = json.RawMessage(`{"title":"Сериал","quality":"1080p"}`)

// addVerified adds metafile in mode with the links a pinned release has and
// waits until its data is verified complete.
func addVerified(t *testing.T, e *Engine, metafile []byte, ih, mode, save string, files []int) {
	t.Helper()
	item := int64(42)
	if _, err := e.AddTorrentFile(context.Background(), metafile, AddOptions{
		Mode: mode, SaveDir: save, ItemID: &item, AddedBy: 7, AddedByUID: "uid-7", Release: testRelease,
		SourceURL: "https://tracker.example/topic/1", Files: files,
	}); err != nil {
		t.Fatal(err)
	}
	if done, size := verified(t, e, ih); done != size || size == 0 {
		t.Fatalf("old torrent verified %d of %d", done, size)
	}
}

func checkLinks(t *testing.T, r record) {
	t.Helper()
	if r.ItemID == nil || *r.ItemID != 42 || r.AddedBy == nil || *r.AddedBy != 7 || r.AddedByUID != "uid-7" ||
		r.SourceURL != "https://tracker.example/topic/1" || string(r.Release) != string(testRelease) {
		t.Fatalf("links not carried over: %+v", r)
	}
}

// v2 appends ep08: ep01–ep07 stay complete (no byte re-downloaded), ep08 is
// not selected unless keepSelected says so, the old torrent and its piece
// rows are gone, the links are carried over.
func TestReplaceKeepsDataWhenFilesAppended(t *testing.T) {
	v1files := episodes(t, 1, 7, 4*replacePL, 0) // piece-aligned: the last piece of v1 is whole
	v2files := append(append([]relFile{}, v1files...), episodes(t, 8, 8, 4*replacePL, 0)...)
	v1, ih1 := buildRelease(t, v1files...)
	v2, ih2 := buildRelease(t, v2files...)
	dataDir := t.TempDir()
	e := openEngine(t, dataDir, engOpts{})
	save := t.TempDir()
	placeFiles(t, save, v1files...)
	addVerified(t, e, v1, ih1, ModeDownload, save, nil)
	waitFor(t, e, ih1, 10*time.Second, func(s Status) bool { return s.CompletedAt != nil })
	var hooks hookCount
	e.OnComplete(hooks.add)

	h, err := e.Replace(context.Background(), ih1, v2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h.InfoHash() != ih2 {
		t.Fatalf("handle %s, want %s", h.InfoHash(), ih2)
	}
	if _, ok := e.Get(ih1); ok {
		t.Fatal("old torrent still in the engine")
	}
	verified(t, e, ih2)
	fs := filesOf(t, e, ih2)
	for _, f := range v1files {
		if fi := fs[f.path]; fi.Progress != 1 || !fi.Selected {
			t.Fatalf("%s: %+v", f.path, fi)
		}
	}
	if fi := fs["ep08.mkv"]; fi.Progress != 0 || fi.Selected {
		t.Fatalf("ep08: %+v", fi)
	}
	sameOnDisk(t, save, v1files...)
	time.Sleep(500 * time.Millisecond)
	if n := hooks.get(); n != 0 {
		t.Fatalf("OnComplete ran %d times for files organized already", n)
	}
	r := recordOf(t, e, ih2)
	checkLinks(t, r)
	if r.CompletedAt == nil {
		t.Fatal("completion not carried over")
	}
	if r.Mode != ModeDownload || r.SaveDir != save {
		t.Fatalf("record %+v", r)
	}
	if n, _ := pieceRows(t, e.db, ih1); n != 0 {
		t.Fatalf("%d piece rows of the old torrent left", n)
	}
	if _, err := os.Stat(e.metafilePath(ih1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old metafile: %v", err)
	}

	// keepSelected adds the new episode to the selection
	e2 := openEngine(t, t.TempDir(), engOpts{})
	save2 := t.TempDir()
	placeFiles(t, save2, v1files...)
	addVerified(t, e2, v1, ih1, ModeDownload, save2, []int{0, 1, 2}) // ep01–ep03 only
	if _, err := e2.Replace(context.Background(), ih1, v2, func(p string) bool { return p == seriesName+"/ep08.mkv" }); err != nil {
		t.Fatal(err)
	}
	verified(t, e2, ih2)
	for p, fi := range filesOf(t, e2, ih2) {
		want := p == "ep01.mkv" || p == "ep02.mkv" || p == "ep03.mkv" || p == "ep08.mkv"
		if fi.Selected != want {
			t.Fatalf("%s selected %v, want %v", p, fi.Selected, want)
		}
	}
}

// v2 inserts «RUS Sound/ep08.mka» before the videos: every offset shifts,
// yet every piece that lies in the old files verifies; only the pieces
// touching the new file are fetched, and then the old files are whole.
func TestReplaceKeepsDataWhenFilesShift(t *testing.T) {
	v1files := episodes(t, 1, 7, 3*replacePL, 1234) // unaligned sizes
	mka := relFile{"RUS Sound/ep08.mka", randBytes(t, 2*replacePL+777)}
	v2files := append([]relFile{mka}, v1files...)
	v1, ih1 := buildRelease(t, v1files...)
	v2, ih2 := buildRelease(t, v2files...)
	e := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	placeFiles(t, save, v1files...)
	addVerified(t, e, v1, ih1, ModeDownload, save, nil)
	waitFor(t, e, ih1, 10*time.Second, func(s Status) bool { return s.CompletedAt != nil })
	var hooks hookCount
	e.OnComplete(func(s Status) {
		if s.InfoHash == ih2 {
			hooks.add(s)
		}
	})

	h, err := e.Replace(context.Background(), ih1, v2, func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	verified(t, e, ih2)
	mi, _ := metainfo.Load(bytes.NewReader(v2))
	info, _ := mi.UnmarshalInfo()
	if info.UpvertedFiles()[0].BestPath()[0] != "RUS Sound" {
		t.Fatalf("the sound file is not first: %v", info.UpvertedFiles()[0].BestPath())
	}
	mkaLen := int64(len(mka.data))
	tt := recordTorrent(t, e, ih2)
	touching := 0
	for i := range tt.NumPieces() {
		begin := int64(i) * replacePL
		want := begin >= mkaLen // the piece lies wholly in the old files
		if !want {
			touching++
		}
		if got := tt.PieceState(i).Complete; got != want {
			t.Fatalf("piece %d (at %d) complete %v, want %v", i, begin, got, want)
		}
	}
	fs := filesOf(t, e, ih2)
	for i, f := range v1files {
		fi := fs[f.path]
		if i > 0 && fi.Progress != 1 {
			t.Fatalf("%s: %+v", f.path, fi)
		}
		// ep01 lacks only the bytes of the piece it shares with the new file
		if missing := float64(len(f.data)) * (1 - fi.Progress); missing > replacePL {
			t.Fatalf("%s: %.0f bytes missing", f.path, missing)
		}
	}
	sameOnDisk(t, save, v1files...)

	// a seeder of v2: only the pieces touching the new file come over
	seed := openEngine(t, t.TempDir(), engOpts{})
	src := t.TempDir()
	placeFiles(t, src, v2files...)
	addVerified(t, seed, v2, ih2, ModeSeed, src, nil)
	before := h.Status().Downloaded
	connect(t, e, seed, ih2)
	st := waitFor(t, e, ih2, 30*time.Second, func(s Status) bool { return s.Size > 0 && s.Done == s.Size })
	if got := st.Downloaded - before; got < mkaLen || got > int64(touching)*replacePL {
		t.Fatalf("downloaded %d bytes, want %d..%d", got, mkaLen, int64(touching)*replacePL)
	}
	for _, fi := range filesOf(t, e, ih2) {
		if fi.Progress != 1 {
			t.Fatalf("%s: %+v", fi.Path, fi)
		}
	}
	sameOnDisk(t, save, v2files...)
	deadline := time.Now().Add(5 * time.Second)
	for hooks.get() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := hooks.get(); n != 1 {
		t.Fatalf("OnComplete ran %d times once the new file was there, want 1", n)
	}
}

// recordTorrent is the live anacrolix torrent of ih.
func recordTorrent(t *testing.T, e *Engine, ih string) *atorrent.Torrent {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	en := e.entries[ih]
	if en == nil || en.t == nil {
		t.Fatalf("no live torrent %s", ih)
	}
	return en.t
}

// Stream mode: StreamDir/<old> becomes StreamDir/<new>, the data in it is
// kept, and a file the new release no longer lists is deleted.
func TestReplaceStreamMovesDir(t *testing.T) {
	eps := episodes(t, 1, 4, 3*replacePL, 0)
	v1files, v2files := eps[:3], eps[1:] // v2: ep01 dropped, ep04 added
	v1, ih1 := buildRelease(t, v1files...)
	v2, ih2 := buildRelease(t, v2files...)
	e := openEngine(t, t.TempDir(), engOpts{})
	placeFiles(t, e.streamDir(ih1), v1files...)
	addVerified(t, e, v1, ih1, ModeStream, "", nil)

	if _, err := e.Replace(context.Background(), ih1, v2, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.streamDir(ih1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old stream dir: %v", err)
	}
	verified(t, e, ih2)
	r := recordOf(t, e, ih2)
	checkLinks(t, r)
	if r.Mode != ModeStream || r.SaveDir != e.streamDir(ih2) {
		t.Fatalf("record %+v", r)
	}
	fs := filesOf(t, e, ih2)
	for _, p := range []string{"ep02.mkv", "ep03.mkv"} {
		if fs[p].Progress != 1 {
			t.Fatalf("%s: %+v", p, fs[p])
		}
	}
	sameOnDisk(t, e.streamDir(ih2), eps[1:3]...)
	if _, err := os.Stat(filepath.Join(e.streamDir(ih2), seriesName, "ep01.mkv")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dropped file left: %v", err)
	}
}

// A metafile that cannot be read changes nothing.
func TestReplaceRollsBackOnBadMetafile(t *testing.T) {
	files := episodes(t, 1, 2, 3*replacePL, 0)
	v1, ih1 := buildRelease(t, files...)
	e := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	placeFiles(t, save, files...)
	addVerified(t, e, v1, ih1, ModeDownload, save, nil)
	before, _ := status(e, ih1)
	if _, err := e.Replace(context.Background(), ih1, []byte("not a torrent"), nil); err == nil {
		t.Fatal("garbage accepted")
	}
	after, ok := status(e, ih1)
	if !ok {
		t.Fatal("old torrent gone")
	}
	if after.Status != before.Status || after.Done != before.Done || after.Size != before.Size || after.SaveDir != before.SaveDir || after.Mode != before.Mode {
		t.Fatalf("status %+v, was %+v", after, before)
	}
	sameOnDisk(t, save, files...)
}

// A failure after the old torrent was removed re-adds it as it was: mode,
// dir, selection, links, completion; the new torrent is gone; no byte lost.
func TestReplaceRollsBackAfterRemoval(t *testing.T) {
	for _, step := range []string{"removed", "added", "verified", "unlinked"} {
		for _, mode := range []string{ModeDownload, ModeStream} {
			t.Run(step+"/"+mode, func(t *testing.T) {
				eps := episodes(t, 1, 4, 3*replacePL, 0)
				v1files, v2files := eps[:3], eps
				v1, ih1 := buildRelease(t, v1files...)
				v2, ih2 := buildRelease(t, v2files...)
				e := openEngine(t, t.TempDir(), engOpts{})
				save := t.TempDir()
				dir := save
				if mode == ModeStream {
					save, dir = "", e.streamDir(ih1)
				}
				placeFiles(t, dir, v1files...)
				var sel []int
				if mode == ModeDownload {
					sel = []int{0, 2}
				}
				addVerified(t, e, v1, ih1, mode, save, sel)
				waitFor(t, e, ih1, 10*time.Second, func(s Status) bool { return s.CompletedAt != nil })
				was := recordOf(t, e, ih1)
				var hooks hookCount
				e.OnComplete(func(s Status) {
					if s.InfoHash == ih1 {
						hooks.add(s)
					}
				})

				injected := errors.New("injected")
				replaceFault = func(s string) error {
					if s == step {
						return injected
					}
					return nil
				}
				t.Cleanup(func() { replaceFault = nil })
				if _, err := e.Replace(context.Background(), ih1, v2, nil); !errors.Is(err, injected) {
					t.Fatalf("err %v", err)
				}
				replaceFault = nil
				if _, ok := e.Get(ih2); ok {
					t.Fatal("new torrent left in the engine")
				}
				if _, err := os.Stat(e.metafilePath(ih2)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("new metafile: %v", err)
				}
				if done, size := verified(t, e, ih1); done != size {
					t.Fatalf("rolled back torrent verified %d of %d", done, size)
				}
				r := recordOf(t, e, ih1)
				checkLinks(t, r)
				if r.Mode != was.Mode || r.SaveDir != was.SaveDir || fmt.Sprint(r.Files) != fmt.Sprint(was.Files) ||
					r.CompletedAt == nil || !r.CompletedAt.Equal(*was.CompletedAt) || !r.AddedAt.Equal(was.AddedAt) {
					t.Fatalf("record %+v, was %+v", r, was)
				}
				sameOnDisk(t, dir, v1files...)
				// complete again, yet no second OnComplete (organize) for it
				waitFor(t, e, ih1, 10*time.Second, func(s Status) bool { return s.Status == StatusSeeding || s.Status == StatusDownloading })
				time.Sleep(500 * time.Millisecond)
				if n := hooks.get(); n != 0 {
					t.Fatalf("OnComplete ran %d times after the rollback", n)
				}
				if mode == ModeStream {
					if _, err := os.Stat(e.streamDir(ih2)); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("new stream dir: %v", err)
					}
				}
			})
		}
	}
}

// The same release again changes nothing.
func TestReplaceSameInfohashNoop(t *testing.T) {
	files := episodes(t, 1, 2, 3*replacePL, 0)
	v1, ih1 := buildRelease(t, files...)
	e := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	placeFiles(t, save, files...)
	addVerified(t, e, v1, ih1, ModeDownload, save, nil)
	old, _ := e.Get(ih1)
	h, err := e.Replace(context.Background(), ih1, v1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if h != old {
		t.Fatal("another handle")
	}
	if n, _ := pieceRows(t, e.db, ih1); n == 0 {
		t.Fatal("piece rows dropped")
	}
}

// The old torrent is gone before the new one is added, so the namesake rule
// does not move the new one aside: it stays at SaveDir/<name>. A file the
// new release no longer lists is left where it is (download).
func TestReplaceNoNamesakeMove(t *testing.T) {
	eps := episodes(t, 1, 4, 3*replacePL, 0)
	v1files, v2files := eps[:3], eps[1:]
	v1, ih1 := buildRelease(t, v1files...)
	v2, ih2 := buildRelease(t, v2files...)
	e := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	placeFiles(t, save, v1files...)
	addVerified(t, e, v1, ih1, ModeDownload, save, nil)

	h, err := e.Replace(context.Background(), ih1, v2, nil)
	if err != nil {
		t.Fatal(err)
	}
	verified(t, e, ih2)
	if st := h.Status(); st.SaveDir != save || st.Error != "" {
		t.Fatalf("status %+v", st)
	}
	if _, err := os.Stat(namesakeDir(save, ih2)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("namesake dir: %v", err)
	}
	fs := filesOf(t, e, ih2)
	for _, p := range []string{"ep02.mkv", "ep03.mkv"} {
		if fs[p].Progress != 1 {
			t.Fatalf("%s: %+v", p, fs[p])
		}
	}
	sameOnDisk(t, save, v1files...) // ep01 left to organize
}

// A stream torrent watched lately but added long ago is not left prunable:
// the stream cache spares a torrent by AddedAt or by its last use, which is
// kept under the old infohash; the new one counts as just added.
func TestReplaceStreamIsFreshForPrune(t *testing.T) {
	eps := episodes(t, 1, 3, 3*replacePL, 0)
	v1, ih1 := buildRelease(t, eps[:2]...)
	v2, ih2 := buildRelease(t, eps...)
	e := openEngine(t, t.TempDir(), engOpts{})
	placeFiles(t, e.streamDir(ih1), eps[:2]...)
	addVerified(t, e, v1, ih1, ModeStream, "", nil)
	long := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	e.mu.Lock()
	e.entries[ih1].rec.AddedAt = long
	e.mu.Unlock()

	if _, err := e.Replace(context.Background(), ih1, v2, nil); err != nil {
		t.Fatal(err)
	}
	if at := recordOf(t, e, ih2).AddedAt; time.Since(at) > time.Minute {
		t.Fatalf("new stream torrent AddedAt %v, want now", at)
	}
	st, _ := status(e, ih2)
	if time.Since(st.AddedAt) > time.Minute {
		t.Fatalf("status AddedAt %v", st.AddedAt)
	}
}

// A re-upload that keeps a file's path but changes its bytes: the file in
// the torrent's folder is a hard link of the library's copy (organize links
// when it can). The new release's download must never write into that
// inode: the library keeps the old bytes, the torrent gets the new ones.
func TestReplaceNeverWritesIntoAHardLinkedFile(t *testing.T) {
	v1files := episodes(t, 1, 3, 3*replacePL, 0)
	fixed := relFile{v1files[1].path, randBytes(t, len(v1files[1].data))} // ep02 re-encoded, same size
	v2files := []relFile{v1files[0], fixed, v1files[2]}
	v1, ih1 := buildRelease(t, v1files...)
	v2, ih2 := buildRelease(t, v2files...)
	e := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	placeFiles(t, save, v1files...)
	addVerified(t, e, v1, ih1, ModeDownload, save, nil)
	waitFor(t, e, ih1, 10*time.Second, func(s Status) bool { return s.CompletedAt != nil })
	lib := filepath.Join(t.TempDir(), "Сериал S01E02.mkv")
	if err := os.Link(filepath.Join(save, seriesName, fixed.path), lib); err != nil {
		t.Fatal(err)
	}
	lib1 := filepath.Join(t.TempDir(), "Сериал S01E01.mkv") // unchanged: linked still
	if err := os.Link(filepath.Join(save, seriesName, v1files[0].path), lib1); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Replace(context.Background(), ih1, v2, nil); err != nil {
		t.Fatal(err)
	}
	if st, _ := e.Get(ih2); st.Status().Status == StatusPaused {
		t.Fatal("the new torrent is left paused")
	}
	seed := openEngine(t, t.TempDir(), engOpts{})
	src := t.TempDir()
	placeFiles(t, src, v2files...)
	addVerified(t, seed, v2, ih2, ModeSeed, src, nil)
	connect(t, e, seed, ih2)
	waitFor(t, e, ih2, 30*time.Second, func(s Status) bool { return s.Size > 0 && s.Done == s.Size })
	sameOnDisk(t, save, v2files...)
	if b, err := os.ReadFile(lib); err != nil || !bytes.Equal(b, v1files[1].data) {
		t.Fatalf("the library's file was written through the hard link (err %v)", err)
	}
	if b, err := os.ReadFile(lib1); err != nil || !bytes.Equal(b, v1files[0].data) {
		t.Fatalf("the unchanged library file: err %v", err)
	}
	if soleLink(filepath.Join(save, seriesName, v1files[0].path)) {
		t.Fatal("a fully verified file lost its hard link for nothing")
	}
	if !soleLink(filepath.Join(save, seriesName, fixed.path)) {
		t.Fatal("the changed file still shares the library's inode")
	}
}

// An old torrent waiting for space: the new one is held paused until its
// hard links are broken — a waiting_space torrent would be resumed by the
// disk guard (acquire.guardSpace) as soon as space frees, which can be
// during the hash check, and fetch into the library's inode. After the
// links are broken its hold is waiting_space again.
func TestReplaceWaitingSpaceNeverWritesIntoHardLink(t *testing.T) {
	v1files := episodes(t, 1, 2, 3*replacePL, 0)
	fixed := relFile{v1files[1].path, randBytes(t, len(v1files[1].data))}
	v2files := []relFile{v1files[0], fixed}
	v1, ih1 := buildRelease(t, v1files...)
	v2, ih2 := buildRelease(t, v2files...)
	e := openEngine(t, t.TempDir(), engOpts{})
	save := t.TempDir()
	placeFiles(t, save, v1files...)
	addVerified(t, e, v1, ih1, ModeDownload, save, nil)
	waitFor(t, e, ih1, 10*time.Second, func(s Status) bool { return s.CompletedAt != nil })
	if err := e.HoldForSpace(context.Background(), ih1); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(t.TempDir(), "Сериал S01E02.mkv")
	if err := os.Link(filepath.Join(save, seriesName, fixed.path), lib); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	replaceFault = func(step string) error {
		h, ok := e.Get(ih2)
		if !ok {
			return nil
		}
		st := h.Status().Status
		seen[step] = st
		// the disk guard: space freed, every waiting_space torrent resumes
		if st == StatusWaitingSpace && step != "unlinked" {
			return e.Resume(context.Background(), ih2)
		}
		return nil
	}
	t.Cleanup(func() { replaceFault = nil })
	if _, err := e.Replace(context.Background(), ih1, v2, nil); err != nil {
		t.Fatal(err)
	}
	replaceFault = nil
	for _, step := range []string{"added", "verified", "unlinked"} {
		if seen[step] != StatusPaused {
			t.Fatalf("at %s the new torrent was %q, want paused (%v)", step, seen[step], seen)
		}
	}
	h, _ := e.Get(ih2)
	if st := h.Status().Status; st != StatusWaitingSpace {
		t.Fatalf("hold after Replace %q, want waiting_space", st)
	}
	if !soleLink(filepath.Join(save, seriesName, fixed.path)) {
		t.Fatal("the changed file still shares the library's inode")
	}
	// space frees: it downloads, never into the library's copy
	if err := e.Resume(context.Background(), ih2); err != nil {
		t.Fatal(err)
	}
	seed := openEngine(t, t.TempDir(), engOpts{})
	src := t.TempDir()
	placeFiles(t, src, v2files...)
	addVerified(t, seed, v2, ih2, ModeSeed, src, nil)
	connect(t, e, seed, ih2)
	waitFor(t, e, ih2, 30*time.Second, func(s Status) bool { return s.Size > 0 && s.Done == s.Size })
	sameOnDisk(t, save, v2files...)
	if b, err := os.ReadFile(lib); err != nil || !bytes.Equal(b, v1files[1].data) {
		t.Fatalf("the library's file was written through the hard link (err %v)", err)
	}
}
