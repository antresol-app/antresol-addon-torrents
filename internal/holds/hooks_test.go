package holds

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// hookEnv is a real engine (local only) seeding the folder R/dl/A, and the
// hooks served as the core calls them.
type hookEnv struct {
	eng     *torrent.Engine
	r, ih   string
	mux     *addonkit.HookMux
	h       *Hooks
	mu      sync.Mutex
	removed []string
	changed int
}

func newHookEnv(t *testing.T, pending time.Duration) *hookEnv {
	t.Helper()
	return newHookEnvWith(t, pending, false)
}

// newHookEnvWith is newHookEnv with the engine's «Не раздавать» at start
// (noSeed: the config boot hands the engine).
func newHookEnvWith(t *testing.T, pending time.Duration, noSeed bool) *hookEnv {
	t.Helper()
	r := t.TempDir()
	data := t.TempDir()
	d, err := store.Open(filepath.Join(data, "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	eng, err := torrent.New(torrent.EngineConfig{DataDir: data, StreamDir: filepath.Join(data, "stream"), LocalOnly: true, NoSeeding: noSeed}, d)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close(); d.Close() })
	dir := filepath.Join(r, "dl", "A")
	mustMkdir(t, dir)
	mustMkdir(t, filepath.Join(r, "movies"))
	buf := make([]byte, 300<<10)
	rand.Read(buf)
	if err := os.WriteFile(filepath.Join(dir, "a.mkv"), buf, 0o644); err != nil {
		t.Fatal(err)
	}
	info := metainfo.Info{PieceLength: 256 << 10}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: ib}
	var mf bytes.Buffer
	if err := mi.Write(&mf); err != nil {
		t.Fatal(err)
	}
	ih := mi.HashInfoBytes().HexString()
	if _, err := eng.AddTorrentFile(context.Background(), mf.Bytes(), torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: filepath.Join(r, "dl")}); err != nil {
		t.Fatal(err)
	}
	e := &hookEnv{eng: eng, r: r, ih: ih, mux: &addonkit.HookMux{}}
	e.waitStatus(t, func(s torrent.Status) bool { return s.Status == torrent.StatusSeeding && s.Parked == noSeed })
	e.h = &Hooks{Engine: eng, Pending: pending,
		Remove: func(_ context.Context, ih string) error {
			e.mu.Lock()
			e.removed = append(e.removed, ih)
			e.mu.Unlock()
			return nil
		},
		Changed: func() { e.mu.Lock(); e.changed++; e.mu.Unlock() },
	}
	e.h.Register(e.mux)
	return e
}

func (e *hookEnv) status(t *testing.T) torrent.Status {
	t.Helper()
	for _, s := range e.eng.List() {
		if s.InfoHash == e.ih {
			return s
		}
	}
	t.Fatal("torrent gone")
	return torrent.Status{}
}

func (e *hookEnv) waitStatus(t *testing.T, ok func(torrent.Status) bool) torrent.Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s := e.status(t)
		if ok(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// call posts hook name with body and returns the status and answer.
func (e *hookEnv) call(t *testing.T, name string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	e.mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/hooks/"+name, bytes.NewReader(b)))
	return w.Code, strings.TrimSpace(w.Body.String())
}

type move struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// A move of the download's own folder keeps its name: it follows; a move
// of the folder above it into the trash stops it; keys not asked are not
// answered.
func TestHooks_PlanFollow(t *testing.T) {
	e := newHookEnv(t, time.Minute)
	from, to := filepath.Join(e.r, "dl", "A"), filepath.Join(e.r, "movies", "A")
	code, body := e.call(t, "files.plan", map[string]any{"moves": []move{{from, to}}, "keys": []string{e.ih, "zz"}})
	if code != 200 || body != `{"follow":["`+e.ih+`"],"stop":[]}` {
		t.Fatalf("%d %s", code, body)
	}
	code, body = e.call(t, "files.plan", map[string]any{"moves": []move{{filepath.Join(e.r, "dl"), ""}}, "keys": []string{e.ih}})
	if code != 200 || body != `{"follow":[],"stop":["`+e.ih+`"]}` {
		t.Fatalf("trash: %d %s", code, body)
	}
	code, body = e.call(t, "files.plan", map[string]any{"moves": []move{{from, to}}, "keys": []string{"zz"}})
	if code != 200 || body != `{"follow":[],"stop":[]}` {
		t.Fatalf("other key: %d %s", code, body)
	}
}

// detach, the data moved, attach renamed: the download now lives at
// R/movies/A, and the holds are re-pushed.
func TestHooks_DetachAttach(t *testing.T) {
	e := newHookEnv(t, time.Minute)
	from, to := filepath.Join(e.r, "dl", "A"), filepath.Join(e.r, "movies", "A")
	mb := map[string]any{"from": from, "to": to, "keys": []string{e.ih}}
	if code, body := e.call(t, "files.detach", mb); code != 204 {
		t.Fatalf("detach %d %s", code, body)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	mb["renamed"] = true
	if code, body := e.call(t, "files.attach", mb); code != 204 {
		t.Fatalf("attach %d %s", code, body)
	}
	s := e.waitStatus(t, func(s torrent.Status) bool { return s.Status == torrent.StatusSeeding })
	if s.SaveDir != filepath.Join(e.r, "movies") {
		t.Fatalf("save dir %q", s.SaveDir)
	}
	// an attach with nothing pending is fine
	if code, body := e.call(t, "files.attach", mb); code != 204 {
		t.Fatalf("second attach %d %s", code, body)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.changed == 0 {
		t.Fatal("holds not re-pushed after the move")
	}
}

// detach, the data moved, no attach: after Pending the relocation is
// finished by what is on disk (from gone, to there: renamed).
func TestHooks_AttachLost(t *testing.T) {
	e := newHookEnv(t, 50*time.Millisecond)
	from, to := filepath.Join(e.r, "dl", "A"), filepath.Join(e.r, "movies", "A")
	if code, body := e.call(t, "files.detach", map[string]any{"from": from, "to": to, "keys": []string{e.ih}}); code != 204 {
		t.Fatalf("detach %d %s", code, body)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	s := e.waitStatus(t, func(s torrent.Status) bool { return s.SaveDir == filepath.Join(e.r, "movies") })
	if s.Status != torrent.StatusSeeding {
		e.waitStatus(t, func(s torrent.Status) bool { return s.Status == torrent.StatusSeeding })
	}
	// nothing moved on disk: restored in place
	e2 := newHookEnv(t, 50*time.Millisecond)
	from2 := filepath.Join(e2.r, "dl", "A")
	if code, _ := e2.call(t, "files.detach", map[string]any{"from": from2, "to": filepath.Join(e2.r, "movies", "A"), "keys": []string{e2.ih}}); code != 204 {
		t.Fatal(code)
	}
	time.Sleep(200 * time.Millisecond)
	if s := e2.waitStatus(t, func(s torrent.Status) bool { return s.Status == torrent.StatusSeeding }); s.SaveDir != filepath.Join(e2.r, "dl") {
		t.Fatalf("not moved, save dir %q", s.SaveDir)
	}
}

// stop removes the download, its data kept.
func TestHooks_Stop(t *testing.T) {
	e := newHookEnv(t, time.Minute)
	if code, body := e.call(t, "files.stop", map[string]any{"keys": []string{e.ih}}); code != 204 {
		t.Fatalf("%d %s", code, body)
	}
	e.mu.Lock()
	removed := append([]string(nil), e.removed...)
	e.mu.Unlock()
	if !reflect.DeepEqual(removed, []string{e.ih}) {
		t.Fatalf("removed %v", removed)
	}
	if _, err := os.Stat(filepath.Join(e.r, "dl", "A", "a.mkv")); err != nil {
		t.Fatal(err)
	}
}

// pause holds the download; resume releases only what pause held: a
// download the admin paused before stays paused.
func TestHooks_PauseResume(t *testing.T) {
	e := newHookEnv(t, time.Minute)
	keys := map[string]any{"keys": []string{e.ih, "zz"}}
	if code, body := e.call(t, "files.pause", keys); code != 204 {
		t.Fatalf("pause %d %s", code, body)
	}
	e.waitStatus(t, func(s torrent.Status) bool { return s.Status == torrent.StatusPaused })
	if code, body := e.call(t, "files.resume", keys); code != 204 {
		t.Fatalf("resume %d %s", code, body)
	}
	e.waitStatus(t, func(s torrent.Status) bool { return s.Status == torrent.StatusSeeding })
	// paused by the admin: a move's pause/resume leaves it paused
	if err := e.eng.Pause(context.Background(), e.ih); err != nil {
		t.Fatal(err)
	}
	e.call(t, "files.pause", keys)
	if code, _ := e.call(t, "files.resume", keys); code != 204 {
		t.Fatal(code)
	}
	time.Sleep(100 * time.Millisecond)
	if s := e.status(t); s.Status != torrent.StatusPaused {
		t.Fatalf("admin's pause lost: %s", s.Status)
	}
	// waiting for disk space: back to waiting after the move
	if err := e.eng.HoldForSpace(context.Background(), e.ih); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(t, func(s torrent.Status) bool { return s.Status == torrent.StatusWaitingSpace })
	e.call(t, "files.pause", keys)
	e.waitStatus(t, func(s torrent.Status) bool { return s.Status == torrent.StatusPaused })
	if code, _ := e.call(t, "files.resume", keys); code != 204 {
		t.Fatal(code)
	}
	e.waitStatus(t, func(s torrent.Status) bool { return s.Status == torrent.StatusWaitingSpace })
}

// A detach while the data is already being moved is refused with today's
// text.
func TestHooks_DetachBusyRefused(t *testing.T) {
	e := newHookEnv(t, time.Minute)
	from, to := filepath.Join(e.r, "dl", "A"), filepath.Join(e.r, "movies", "A")
	mb := map[string]any{"from": from, "to": to, "keys": []string{e.ih}}
	if code, _ := e.call(t, "files.detach", mb); code != 204 {
		t.Fatal(code)
	}
	code, body := e.call(t, "files.detach", map[string]any{"from": filepath.Join(e.r, "dl"), "to": filepath.Join(e.r, "x"), "keys": []string{e.ih}})
	if code != 409 || !strings.Contains(body, "Не удалось остановить раздачу этих файлов") {
		t.Fatalf("%d %s", code, body)
	}
	mb["renamed"] = false
	e.call(t, "files.attach", mb)
}

// «Не раздавать» on: a move's pause, resume, detach and attach never bring
// the finished download back to the swarm: it stays parked (dropped from
// the client, nothing uploaded) all along, and is parked at its new place.
func TestHooks_NoSeedingStaysParked(t *testing.T) {
	e := newHookEnvWith(t, time.Minute, true)
	from, to := filepath.Join(e.r, "dl", "A"), filepath.Join(e.r, "movies", "A")
	keys := map[string]any{"keys": []string{e.ih}}
	// never in the swarm: parked, or paused by the move (still out of it)
	watch := func(what string, d time.Duration) {
		t.Helper()
		for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
			if s := e.status(t); s.Status == torrent.StatusSeeding && !s.Parked || s.UpRate != 0 || s.Uploaded != 0 {
				t.Fatalf("%s: back in the swarm: %+v", what, s)
			}
		}
	}
	if code, body := e.call(t, "files.pause", keys); code != 204 {
		t.Fatalf("pause %d %s", code, body)
	}
	watch("paused", 200*time.Millisecond)
	mb := map[string]any{"from": from, "to": to, "keys": []string{e.ih}}
	if code, body := e.call(t, "files.detach", mb); code != 204 {
		t.Fatalf("detach %d %s", code, body)
	}
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	mb["renamed"] = true
	if code, body := e.call(t, "files.attach", mb); code != 204 {
		t.Fatalf("attach %d %s", code, body)
	}
	watch("attached", 300*time.Millisecond)
	if code, body := e.call(t, "files.resume", keys); code != 204 {
		t.Fatalf("resume %d %s", code, body)
	}
	watch("resumed", 500*time.Millisecond)
	s := e.waitStatus(t, func(s torrent.Status) bool { return s.Status == torrent.StatusSeeding && s.Parked })
	if s.SaveDir != filepath.Join(e.r, "movies") {
		t.Fatalf("save dir %q", s.SaveDir)
	}
}
