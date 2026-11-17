package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

const ih = "0123456789abcdef0123456789abcdef01234567"

// fakeAcq is one torrent: files with their payloads on disk under dir.
type fakeAcq struct {
	files []torrent.FileInfo
	data  map[int][]byte
	title [4]int // tmdb, season, episode, ok
	opens int
	mu    sync.Mutex
}

type nopCloser struct{ *bytes.Reader }

func (nopCloser) Close() error { return nil }

func (a *fakeAcq) OpenStream(_ context.Context, h string, idx int) (torrent.FileInfo, func(context.Context) (io.ReadSeekCloser, error), func(), error) {
	if h != ih {
		return torrent.FileInfo{}, nil, nil, acquire.ErrNotFound
	}
	if idx < 0 || idx >= len(a.files) || !a.files[idx].Selected {
		return torrent.FileInfo{}, nil, nil, notFound{}
	}
	a.mu.Lock()
	a.opens++
	a.mu.Unlock()
	b := a.data[idx]
	return a.files[idx], func(context.Context) (io.ReadSeekCloser, error) { return nopCloser{bytes.NewReader(b)}, nil }, func() {}, nil
}

// notFound is acquire's «Файл не найден» (a user error of ErrNotFound).
type notFound struct{}

func (notFound) Error() string   { return "Файл не найден" }
func (notFound) Is(t error) bool { return t == acquire.ErrNotFound }

func (a *fakeAcq) StreamFiles(string) []torrent.FileInfo { return a.files }

func (a *fakeAcq) TorrentStatus(h string, file int) (acquire.StreamStatus, error) {
	if h != ih {
		return acquire.StreamStatus{}, acquire.ErrNotFound
	}
	st := acquire.StreamStatus{Status: "downloading", Mode: "stream", Size: 100, Done: 10}
	if file >= 0 {
		st.BufferHave, st.BufferWant = 5, 8
	}
	return st, nil
}

func (a *fakeAcq) WatchTitle(_ context.Context, _, file string) (string, int, int, int, bool) {
	if a.title[3] == 0 {
		return "", 0, 0, 0, false
	}
	return "series", a.title[0], a.title[1], a.title[2], true
}

// release is a series release of a video, a voice-over and subtitles (the
// relay order: audio, then subtitles), its files written under dir.
func release(t *testing.T) *fakeAcq {
	t.Helper()
	dir := t.TempDir()
	a := &fakeAcq{data: map[int][]byte{}, title: [4]int{42, 1, 1, 1}}
	for i, p := range []string{"Show/Show.S01E01.mkv", "Show/Rus Sound/Studio/Show.S01E01.mka", "Show/Subs/Show.S01E01.srt", "Show/Show.S01E02.mkv"} {
		b := bytes.Repeat([]byte{byte('a' + i)}, 100+i)
		for j := range b {
			b[j] = byte(j + i)
		}
		full := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, b, 0o644)
		a.files = append(a.files, torrent.FileInfo{Index: i, Path: p, FullPath: full, Size: int64(len(b)), Selected: true})
		a.data[i] = b
	}
	return a
}

// inputCore is a fake core recording /play/inputs calls.
type inputCore struct {
	mu      sync.Mutex
	posted  []addonkit.InputRequest
	deleted []string
	n       int
}

func (c *inputCore) client(t *testing.T) *addonkit.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/addon-core/v1/play/inputs":
			var in addonkit.InputRequest
			json.NewDecoder(r.Body).Decode(&in)
			c.posted = append(c.posted, in)
			c.n++
			addonkit.WriteJSON(w, 201, addonkit.InputCreated{ID: "in" + string(rune('0'+c.n))})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/addon-core/v1/play/inputs/"):
			c.deleted = append(c.deleted, strings.TrimPrefix(r.URL.Path, "/api/addon-core/v1/play/inputs/"))
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return addonkit.NewClient(addonkit.Env{CoreURL: srv.URL + "/", Token: "tok"})
}

func (c *inputCore) snapshot() ([]addonkit.InputRequest, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]addonkit.InputRequest(nil), c.posted...), append([]string(nil), c.deleted...)
}

type env struct {
	acq  *fakeAcq
	core *inputCore
	lib  *coretest.Library
	in   *Inputs
	api  http.Handler
	med  http.Handler
}

func newEnv(t *testing.T) *env {
	e := &env{acq: release(t), core: &inputCore{}, lib: &coretest.Library{}}
	e.in = &Inputs{Core: e.core.client(t)}
	d := Deps{Acquire: e.acq, Library: e.lib, Inputs: e.in}
	mux := http.NewServeMux()
	Register(mux, d)
	e.api, e.med = mux, Media(d)
	return e
}

func (e *env) do(h http.Handler, method, target, user string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if user != "" {
		r.Header.Set(addonkit.UserHeader, user)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const masha = `{"id":5,"name":"Маша","lang":"ru"}`

func (e *env) watch(t *testing.T, user string) string {
	t.Helper()
	w := e.do(e.api, "POST", "/api/watch/"+ih+"/0", user)
	var out struct {
		InputID string `json:"inputId"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.InputID == "" {
		t.Fatalf("watch: %d %s", w.Code, w.Body)
	}
	return out.InputID
}

func TestWatch_RegistersInput(t *testing.T) {
	e := newEnv(t)
	if id := e.watch(t, masha); id != "in1" {
		t.Fatalf("id %s", id)
	}
	posted, _ := e.core.snapshot()
	if len(posted) != 1 {
		t.Fatalf("%d registrations", len(posted))
	}
	in := posted[0]
	if in.UserID != 5 || in.Path != "/media/"+ih+"/0" || !in.Slow || in.Name != "Show/Show.S01E01.mkv" || in.Size != 100 || in.ItemID != 0 {
		t.Fatalf("%+v", in)
	}
	if in.Title == nil || *in.Title != (addonkit.InputTitle{Kind: "series", TMDBID: 42, Season: 1, Episode: 1}) {
		t.Fatalf("title %+v", in.Title)
	}
	var meta map[string]any
	json.Unmarshal(in.Meta, &meta)
	if meta["ih"] != ih || meta["idx"] != float64(0) || meta["mode"] != "stream" {
		t.Fatalf("meta %s", in.Meta)
	}
	s := in.Sidecars
	if len(s.Audio) != 1 || s.Audio[0] != (addonkit.AudioSidecar{Path: "/media/" + ih + "/0/s/1", Name: "Show/Rus Sound/Studio/Show.S01E01.mka", Size: 101}) {
		t.Fatalf("audio %+v", s.Audio)
	}
	if len(s.Subtitles) != 1 || s.Subtitles[0].Path != "/media/"+ih+"/0/s/2" || s.Subtitles[0].Name != "Show/Subs/Show.S01E01.srt" {
		t.Fatalf("subtitles %+v", s.Subtitles)
	}
}

// a new watch of the same file by the same profile registers afresh
// and drops the profile's earlier input; another profile's stays.
func TestWatch_ReplacesPreviousInput(t *testing.T) {
	e := newEnv(t)
	first := e.watch(t, masha)
	petya := e.watch(t, `{"id":6,"name":"Петя"}`)
	second := e.watch(t, masha)
	// the replaced one is dropped in the background (the answer never waits on it)
	deadline := time.Now().Add(5 * time.Second)
	posted, deleted := e.core.snapshot()
	for len(deleted) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		posted, deleted = e.core.snapshot()
	}
	if first == second || len(posted) != 3 || len(deleted) != 1 || deleted[0] != first {
		t.Fatalf("first %s second %s petya %s: %d registrations, deleted %v", first, second, petya, len(posted), deleted)
	}
}

// A file the library has already (at its own path, or as a hardlink of
// a library file of the download's ref) gives its item.
func TestWatch_ItemIDWhenInLibrary(t *testing.T) {
	e := newEnv(t)
	e.lib.Files = []library.File{{ID: 1, ItemID: 77, Path: e.acq.files[0].FullPath}}
	e.watch(t, masha)
	// a hardlink in the library, bound to the download's ref
	linked := filepath.Join(t.TempDir(), "Сериал S01E01.mkv")
	if err := os.Link(e.acq.files[0].FullPath, linked); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other.mkv")
	os.WriteFile(other, []byte("x"), 0o644)
	e.lib.Files = []library.File{{ID: 2, ItemID: 88, Path: other}, {ID: 3, ItemID: 99, Path: linked}}
	e.lib.Refs = map[string][]string{ih: {other, linked}}
	e.watch(t, masha)
	posted, _ := e.core.snapshot()
	if posted[0].ItemID != 77 || posted[1].ItemID != 99 {
		t.Fatalf("items %d %d", posted[0].ItemID, posted[1].ItemID)
	}
}

func TestWatch_BadFile404(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/watch/" + ih + "/9", "/api/watch/" + ih + "/-1", "/api/watch/xyz/0", "/api/watch/" + strings.Repeat("f", 40) + "/0"} {
		if w := e.do(e.api, "POST", p, masha); w.Code != 404 {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
	if w := e.do(e.api, "POST", "/api/watch/"+ih+"/9", `{"id":5,"lang":"en"}`); !strings.Contains(w.Body.String(), "File not found") {
		t.Errorf("not in the profile's language: %s", w.Body)
	}
	if posted, _ := e.core.snapshot(); len(posted) != 0 {
		t.Fatalf("registered %d", len(posted))
	}
}

func TestWatchRoutes_KidRefused(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{{"POST", "/api/watch/" + ih + "/0"}, {"GET", "/api/torrents/" + ih + "/status"}} {
		for _, u := range []string{`{"id":7,"isKid":true}`, ""} {
			if w := e.do(e.api, c.method, c.path, u); w.Code != 403 {
				t.Errorf("%s %s as %q: %d", c.method, c.path, u, w.Code)
			}
		}
	}
	if posted, _ := e.core.snapshot(); len(posted) != 0 {
		t.Fatalf("a kid registered %d", len(posted))
	}
}

func TestStatus_Adult(t *testing.T) {
	e := newEnv(t)
	w := e.do(e.api, "GET", "/api/torrents/"+ih+"/status?file=0", masha)
	var st acquire.StreamStatus
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil || st.Size != 100 || st.BufferHave != 5 || st.BufferWant != 8 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"bufferHave"`) {
		t.Fatalf("shape %s", w.Body)
	}
	if w := e.do(e.api, "GET", "/api/torrents/"+strings.Repeat("e", 40)+"/status", masha); w.Code != 404 {
		t.Fatalf("unknown: %d", w.Code)
	}
}

func TestMedia_Range(t *testing.T) {
	e := newEnv(t)
	w := e.do(e.med, "GET", "/media/"+ih+"/0", "", "Range", "bytes=10-19")
	if w.Code != 206 || !bytes.Equal(w.Body.Bytes(), e.acq.data[0][10:20]) || w.Header().Get("Content-Length") != "10" ||
		w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("Content-Type") != "video/x-matroska" {
		t.Fatalf("%d %v %q", w.Code, w.Header(), w.Body.Bytes())
	}
	if w := e.do(e.med, "HEAD", "/media/"+ih+"/0", ""); w.Code != 200 || w.Header().Get("Content-Length") != "100" {
		t.Fatalf("head %d %v", w.Code, w.Header())
	}
	// a sidecar of the video: its own bytes; a file that is none: 404
	if w := e.do(e.med, "GET", "/media/"+ih+"/0/s/1", ""); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), e.acq.data[1]) {
		t.Fatalf("sidecar %d", w.Code)
	}
	for _, n := range []string{"3", "0", "9", "x"} {
		if w := e.do(e.med, "GET", "/media/"+ih+"/0/s/"+n, ""); w.Code != 404 {
			t.Errorf("s/%s: %d", n, w.Code)
		}
	}
}

// The media routes are the socket's only: the proxy reaches /api/… alone.
func TestMedia_NotUnderAPI(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/media/" + ih + "/0", "/api/watch/" + ih + "/0/s/1"} {
		if w := e.do(e.api, "GET", p, masha); w.Code != 404 && w.Code != 405 {
			t.Errorf("%s: %d", p, w.Code)
		}
	}
	if w := e.do(e.med, "POST", "/media/"+ih+"/0", ""); w.Code != 405 {
		t.Errorf("POST media: %d", w.Code)
	}
}

// A removed download's inputs are dropped at the core, in the background.
func TestRemove_DropsInputs(t *testing.T) {
	e := newEnv(t)
	a := e.watch(t, masha)
	b := e.watch(t, `{"id":6}`)
	e.in.Drop(strings.ToUpper(ih))
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, deleted := e.core.snapshot()
		if len(deleted) == 2 {
			if !(deleted[0] == a && deleted[1] == b || deleted[0] == b && deleted[1] == a) {
				t.Fatalf("deleted %v", deleted)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deleted %v", deleted)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// forgotten: a new watch drops nothing more
	e.watch(t, masha)
	time.Sleep(30 * time.Millisecond)
	if _, deleted := e.core.snapshot(); len(deleted) != 2 {
		t.Fatalf("deleted %v", deleted)
	}
}

// A file left out of a partial download is never offered (a read would
// fetch it), and the video is not its own sidecar.
func TestWatch_SidecarsSelectedOnly(t *testing.T) {
	e := newEnv(t)
	e.acq.files[2].Selected = false
	e.watch(t, masha)
	posted, _ := e.core.snapshot()
	if s := posted[0].Sidecars; len(s.Subtitles) != 0 || len(s.Audio) != 1 || strings.HasSuffix(s.Audio[0].Path, "/s/0") {
		t.Fatalf("%+v", s)
	}
	if w := e.do(e.med, "GET", "/media/"+ih+"/0/s/2", ""); w.Code != 404 {
		t.Fatalf("an unselected sidecar served: %d", w.Code)
	}
}

// fakeCore cancels the request's context inside RegisterInput (a client
// gone right after) and records whether DropInput still had a live one.
type fakeCore struct {
	cancel context.CancelFunc
	// during runs inside RegisterInput (a remove racing the call)
	during func()
	// hold, when set, keeps DropInput until it is closed (a slow core)
	hold chan struct{}

	mu      sync.Mutex
	n       int
	dropped []string
	dropErr []error
}

func (c *fakeCore) RegisterInput(ctx context.Context, in addonkit.InputRequest) (addonkit.InputCreated, error) {
	c.mu.Lock()
	c.n++
	n := c.n
	c.mu.Unlock()
	if c.cancel != nil {
		c.cancel()
	}
	if c.during != nil {
		c.during()
	}
	return addonkit.InputCreated{ID: "f" + string(rune('0'+n))}, nil
}

func (c *fakeCore) DropInput(ctx context.Context, id string) error {
	if c.hold != nil {
		<-c.hold
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropped = append(c.dropped, id)
	c.dropErr = append(c.dropErr, ctx.Err())
	return ctx.Err()
}

// waitDropped waits for n drops at the core and returns them.
func (c *fakeCore) waitDropped(t *testing.T, n int) ([]string, []error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		d, e := append([]string(nil), c.dropped...), append([]error(nil), c.dropErr...)
		c.mu.Unlock()
		if len(d) >= n {
			time.Sleep(20 * time.Millisecond) // nothing more follows
			c.mu.Lock()
			d, e = append([]string(nil), c.dropped...), append([]error(nil), c.dropErr...)
			c.mu.Unlock()
			return d, e
		}
		if time.Now().After(deadline) {
			t.Fatalf("dropped %v, want %d", d, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// The replaced input is dropped even when the request has ended; entries
// past the core's input life are forgotten.
func TestInputs_DetachedDropAndPrune(t *testing.T) {
	c := &fakeCore{}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	in := &Inputs{Core: c, now: func() time.Time { return now }}
	k := key{5, ih, 0}
	if _, err := in.Register(context.Background(), k, addonkit.InputRequest{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	in.Register(ctx, k, addonkit.InputRequest{})
	if d, errs := c.waitDropped(t, 1); len(d) != 1 || d[0] != "f1" || errs[0] != nil {
		t.Fatalf("dropped %v with %v", d, errs)
	}
	c.cancel = nil
	in.Register(context.Background(), key{6, ih, 1}, addonkit.InputRequest{})
	if in.size() != 2 {
		t.Fatalf("size %d", in.size())
	}
	now = now.Add(inputLife + time.Minute)
	in.Register(context.Background(), key{7, ih, 2}, addonkit.InputRequest{})
	if in.size() != 1 {
		t.Fatalf("old entries kept: %d", in.size())
	}
}

func TestStatus_BadInfohash(t *testing.T) {
	e := newEnv(t)
	for _, h := range []string{"xyz", strings.Repeat("g", 40), strings.Repeat("a", 41)} {
		if w := e.do(e.api, "GET", "/api/torrents/"+h+"/status", masha); w.Code != 404 || !strings.Contains(w.Body.String(), "Раздача не найдена") {
			t.Errorf("%s: %d %s", h, w.Code, w.Body)
		}
	}
}

// A slow core never holds «Смотреть сейчас»: the new input is answered
// before the replaced one is dropped.
func TestInputs_ReplaceAnswersBeforeDrop(t *testing.T) {
	c := &fakeCore{}
	in := &Inputs{Core: c}
	k := key{5, ih, 0}
	if _, err := in.Register(context.Background(), k, addonkit.InputRequest{}); err != nil {
		t.Fatal(err)
	}
	c.hold = make(chan struct{})
	done := make(chan string, 1)
	go func() {
		id, _ := in.Register(context.Background(), k, addonkit.InputRequest{})
		done <- id
	}()
	select {
	case id := <-done:
		if id != "f2" {
			t.Fatalf("id %q", id)
		}
	case <-time.After(2 * time.Second):
		close(c.hold)
		t.Fatal("the answer waited on dropping the replaced input")
	}
	close(c.hold)
	if d, _ := c.waitDropped(t, 1); len(d) != 1 || d[0] != "f1" {
		t.Fatalf("dropped %v", d)
	}
}

// A download removed while its input is being registered: the new input
// is dropped at the core too, and the watch fails (nothing stays behind).
func TestInputs_RemoveDuringRegister(t *testing.T) {
	c := &fakeCore{}
	in := &Inputs{Core: c}
	c.during = func() { in.Drop(ih) }
	if _, err := in.Register(context.Background(), key{5, ih, 0}, addonkit.InputRequest{}); err == nil {
		t.Fatal("registered an input of a removed download")
	}
	if in.size() != 0 {
		t.Fatalf("kept %d", in.size())
	}
	if d, _ := c.waitDropped(t, 1); len(d) != 1 || d[0] != "f1" {
		t.Fatalf("dropped %v", d)
	}
	// another download's register is not affected
	c.during = nil
	if _, err := in.Register(context.Background(), key{5, strings.Repeat("c", 40), 0}, addonkit.InputRequest{}); err != nil {
		t.Fatal(err)
	}
}
