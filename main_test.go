package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// TestMain_BadEnvExitsFirst: a bad ANTRESOL_TORRENTS_NO_SEEDING stops the
// program with exit 1 before it makes a socket, its data dir or a database,
// and before it asks the core anything (there is none here).
func TestMain_BadEnvExitsFirst(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the program")
	}
	bin := filepath.Join(t.TempDir(), "addon")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	sockDir, err := os.MkdirTemp("/tmp", "ad")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	for _, v := range []string{"1", "ON", " on", "true"} {
		data := filepath.Join(t.TempDir(), "data")
		sock := filepath.Join(sockDir, "s")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cmd := exec.CommandContext(ctx, bin)
		cmd.Env = []string{
			"ANTRESOL_TORRENTS_NO_SEEDING=" + v,
			"ANTRESOL_ADDON_ID=torrents",
			"ANTRESOL_ADDON_DATA_DIR=" + data,
			"ANTRESOL_ADDON_SOCKET=" + sock,
			"ANTRESOL_CONTRACT=1",
			"ANTRESOL_CORE_URL=http://127.0.0.1:1",
			"ANTRESOL_ADDON_TOKEN=x",
		}
		out, err := cmd.CombinedOutput()
		cancel()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			t.Fatalf("%q: %v (want exit 1 within 5 s)\n%s", v, err, out)
		}
		if _, err := os.Stat(sock); !os.IsNotExist(err) {
			t.Fatalf("%q: a socket was made", v)
		}
		if _, err := os.Stat(data); !os.IsNotExist(err) {
			t.Fatalf("%q: the data dir was made", v)
		}
		if _, err := os.Stat(filepath.Join(data, "torrents.db")); !os.IsNotExist(err) {
			t.Fatalf("%q: torrents.db was made", v)
		}
	}
}

// TestMain_ReadyAndStop: against a core that answers only /info the
// program boots, builds its services, writes ready.state (the process's
// start time) and answers ready; SIGTERM stops it within the core's 10 s
// and the engine's database stays.
func TestMain_ReadyAndStop(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the program")
	}
	bin := filepath.Join(t.TempDir(), "addon")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	media := t.TempDir()
	var holdPuts, holdGets atomic.Int32
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/addon-core/v1/info" {
			json.NewEncoder(w).Encode(map[string]any{"id": "torrents", "contract": 1, "mediaRoot": media, "fileRoots": []string{media}, "missingRoots": []string{}})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/addon-core/v1/files/holds" {
			holdGets.Add(1)
			w.Write([]byte(`{"holds":[]}`))
			return
		}
		if r.Method == http.MethodPut && r.URL.Path == "/api/addon-core/v1/files/holds" {
			if holdGets.Load() == 0 {
				w.WriteHeader(http.StatusConflict)
				w.Write([]byte(`{"error":"pushed before reading the saved set"}`))
				return
			}
			var b struct {
				Holds []json.RawMessage `json:"holds"`
			}
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Holds == nil {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"error":"bad holds"}`))
				return
			}
			holdPuts.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"none"}`))
	}))
	defer core.Close()
	sockDir, err := os.MkdirTemp("/tmp", "ad")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	data := filepath.Join(t.TempDir(), "data")
	os.MkdirAll(data, 0o700)
	os.WriteFile(filepath.Join(data, "ready.state"), []byte("old\n"), 0o644) // an earlier run's
	sock := filepath.Join(sockDir, "s")
	cmd := exec.Command(bin)
	cmd.Env = []string{
		"ANTRESOL_ADDON_ID=torrents", "ANTRESOL_ADDON_DATA_DIR=" + data, "ANTRESOL_ADDON_SOCKET=" + sock,
		"ANTRESOL_CONTRACT=1", "ANTRESOL_CORE_URL=" + core.URL, "ANTRESOL_ADDON_TOKEN=x",
		"ANTRESOL_TORRENTS_LOCAL_ONLY=on", "ANTRESOL_TORRENTS_TORRENT_PORT=0",
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	began := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stopped := false
	defer func() {
		if !stopped {
			cmd.Process.Kill()
			<-exited
		}
	}()
	hc := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	health := func() map[string]any {
		resp, err := hc.Get("http://addon/health")
		if err != nil {
			return nil
		}
		defer resp.Body.Close()
		var h map[string]any
		json.NewDecoder(resp.Body).Decode(&h)
		return h
	}
	deadline := time.Now().Add(30 * time.Second)
	// before ready the admin routes are not served yet
	if resp, err := hc.Get("http://addon/api/admin/settings"); err == nil {
		resp.Body.Close()
		if h := health(); h != nil && h["ready"] != true && resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("admin routes before ready: %d", resp.StatusCode)
		}
	}
	for h := health(); h == nil || h["ready"] != true; h = health() {
		if b, err := os.ReadFile(filepath.Join(data, "ready.state")); err == nil && string(b) == "old\n" && h != nil {
			t.Fatalf("the earlier run's ready.state was kept")
		}
		if h != nil && h["error"] != nil {
			t.Fatalf("boot error: %v\n%s", h["error"], out.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready in 30 s: %v\n%s", h, out.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	b, err := os.ReadFile(filepath.Join(data, "ready.state"))
	if err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSuffix(string(b), "\n"))
	if err != nil || at.Before(began.Add(-2*time.Second)) || at.After(time.Now()) || !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("ready.state %q: %v", b, err)
	}
	if holdPuts.Load() == 0 {
		t.Fatal("ready without a holds push")
	}
	// the profiles' routes serve («Смотреть сейчас» of a torrent the
	// engine does not have: 404), and so do the socket's media reads;
	// the media are never under /api/
	adult := `{"id":2,"name":"b","isAdmin":false,"isKid":false,"lang":"ru"}`
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/watch/" + strings.Repeat("a", 40) + "/0"},
		{http.MethodGet, "/media/" + strings.Repeat("a", 40) + "/0"},
		{http.MethodGet, "/api/media/" + strings.Repeat("a", 40) + "/0"},
	} {
		req, _ := http.NewRequest(c.method, "http://addon"+c.path, nil)
		req.Header.Set(addonkit.UserHeader, adult)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s after ready: %d", c.method, c.path, resp.StatusCode)
		}
	}
	// the find routes share that mux: a cancel of an unknown kind is the
	// handler's own 400 (not a 404 of no route, nor the 503 of the start)
	req, _ := http.NewRequest(http.MethodPost, "http://addon/api/acquire/cancel", strings.NewReader(`{"kind":"book","tmdbId":1}`))
	req.Header.Set(addonkit.UserHeader, adult)
	if resp, err := hc.Do(req); err != nil {
		t.Fatal(err)
	} else if resp.Body.Close(); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("find route after ready: %d", resp.StatusCode)
	}
	// the admin routes are mounted (an admin's GET settings), and the
	// files.* hooks answer
	req, _ = http.NewRequest(http.MethodGet, "http://addon/api/admin/settings", nil)
	req.Header.Set(addonkit.UserHeader, `{"id":1,"name":"a","isAdmin":true,"isKid":false,"lang":"ru"}`)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if _, on := st["noSeeding"]; resp.StatusCode != http.StatusOK || !on || st["noSeedingForced"] != false {
		t.Fatalf("GET /api/admin/settings: %d %v", resp.StatusCode, st)
	}
	resp, err = hc.Post("http://addon/hooks/files.plan", "application/json", strings.NewReader(`{"moves":[],"keys":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("files.plan: %d", resp.StatusCode)
	}
	// the tracking hooks answer too (none 404 while ready): the scans of a
	// title with no pin and no name give no sources
	for hook, body := range map[string]string{
		"tracking.scan:pinned":  `{"title":{"id":7,"kind":"series","tmdbId":100},"episodes":[]}`,
		"tracking.scan:torrent": `{"title":{"id":7,"kind":"series","tmdbId":100},"episodes":[]}`,
		"tracking.titles":       `{}`,
		"notify.interest":       `{"kind":"series","tmdbId":100}`,
		"voiceover.origLang":    `{"fileId":5}`,
	} {
		resp, err := hc.Post("http://addon/hooks/"+hook, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", hook, resp.StatusCode, b)
		}
	}
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		stopped = true
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("still running 10 s after SIGTERM\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(data, "torrents.db")); err != nil {
		t.Fatal(err)
	}
}

// bootStatus: an error shows in /health as not ready; clear() hands /health
// back to the addon's own answer.
func TestBootStatus_FailAndClear(t *testing.T) {
	var s bootStatus
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addonkit.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "ready": true})
	})
	h := s.wrap(inner)
	get := func() map[string]any {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		var m map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	s.fail(errors.New("holds push: antresol core API: 400 outside"))
	if m := get(); m["ready"] != false || m["error"] != "holds push: antresol core API: 400 outside" {
		t.Fatalf("/health %v", m)
	}
	s.clear()
	if m := get(); m["ready"] != true || m["error"] != nil {
		t.Fatalf("/health after clear %v", m)
	}
}

// Before the start ends every late route answers 503 «Расширение
// запускается» in the asking profile's language, Russian without one.
func TestLateHandler_Starting(t *testing.T) {
	l := &lateHandler{}
	for _, c := range []struct{ user, want string }{
		{"", "Расширение запускается"},
		{`{"id":1,"lang":"en"}`, "The extension is starting"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/api/watch/x/0", nil)
		if c.user != "" {
			r.Header.Set(addonkit.UserHeader, c.user)
		}
		w := httptest.NewRecorder()
		l.ServeHTTP(w, r)
		var body map[string]string
		json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusServiceUnavailable || body["error"] != c.want || w.Header().Get("Retry-After") == "" {
			t.Fatalf("%q: %d %s", c.user, w.Code, w.Body)
		}
	}
	l.set(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	w := httptest.NewRecorder()
	l.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/media/x", nil))
	if w.Code != http.StatusTeapot {
		t.Fatalf("after set: %d", w.Code)
	}
}

// TestFollowAPI_503UntilReady: the follow, pinned and voice-over routes
// join the profiles' /api/ mux, set once the first holds push is done
// (503 «Расширение запускается» before it, 200 after), against a core
// that answers /info with missingRoots and GET /files/holds and has
// TMDB on. TestFollowAPI_NotUnderAdmin: an adult's GET /api/follow
// reaches the handler (the routes are not under /api/admin/, which has
// none of them).
func TestFollowAPI_503UntilReady(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the program")
	}
	bin := filepath.Join(t.TempDir(), "addon")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	media := t.TempDir()
	release := make(chan struct{})
	var released atomic.Bool
	core := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/api/addon-core/v1"
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == base+"/info":
			json.NewEncoder(w).Encode(map[string]any{"id": "torrents", "contract": 1, "mediaRoot": media,
				"fileRoots": []string{media}, "missingRoots": []string{}})
		case r.Method == http.MethodGet && r.URL.Path == base+"/files/holds":
			w.Write([]byte(`{"holds":[]}`))
		case r.Method == http.MethodPut && r.URL.Path == base+"/files/holds":
			// the first push waits until the test has seen the 503s
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodHead && r.URL.Path == base+"/metadata/tmdb":
			w.WriteHeader(http.StatusBadRequest) // TMDB on: the follows exist
		case r.Method == http.MethodGet && r.URL.Path == base+"/users":
			w.Write([]byte(`[{"id":1,"name":"a","isAdmin":true},{"id":2,"name":"b"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":"none"}`))
		}
	}))
	defer core.Close()
	defer func() {
		if !released.Load() {
			close(release)
		}
	}()
	sockDir, err := os.MkdirTemp("/tmp", "ad")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	data := filepath.Join(t.TempDir(), "data")
	sock := filepath.Join(sockDir, "s")
	cmd := exec.Command(bin)
	cmd.Env = []string{
		"ANTRESOL_ADDON_ID=torrents", "ANTRESOL_ADDON_DATA_DIR=" + data, "ANTRESOL_ADDON_SOCKET=" + sock,
		"ANTRESOL_CONTRACT=1", "ANTRESOL_CORE_URL=" + core.URL, "ANTRESOL_ADDON_TOKEN=x",
		"ANTRESOL_TORRENTS_LOCAL_ONLY=on", "ANTRESOL_TORRENTS_TORRENT_PORT=0",
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
	}()
	hc := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
	const adult = `{"id":2,"name":"b","isAdmin":false,"isKid":false,"lang":"ru"}`
	const admin = `{"id":1,"name":"a","isAdmin":true,"isKid":false,"lang":"ru"}`
	get := func(path, user string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, "http://addon"+path, nil)
		req.Header.Set(addonkit.UserHeader, user)
		resp, err := hc.Do(req)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	// before the first holds push: the socket answers, the routes are 503
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, body := get("/api/follow", adult)
		if code == http.StatusServiceUnavailable && strings.Contains(body, "Расширение запускается") {
			break
		}
		if code != 0 || time.Now().After(deadline) {
			t.Fatalf("GET /api/follow before ready: %d %s\n%s", code, body, out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, p := range []string{"/api/pinned/series/1", "/api/items/1/voiceovers"} {
		if code, body := get(p, adult); code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s before ready: %d %s", p, code, body)
		}
	}
	released.Store(true)
	close(release)
	for {
		code, body := get("/api/follow", adult)
		if code == http.StatusOK {
			if strings.TrimSpace(body) != `{"items":[]}` {
				t.Fatalf("GET /api/follow: %s", body)
			}
			break
		}
		if code != http.StatusServiceUnavailable || time.Now().After(deadline) {
			t.Fatalf("GET /api/follow after the push: %d %s\n%s", code, body, out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	// the pin of a title the core does not track: none (the handler's
	// answer); a kid is refused by the handler
	if code, body := get("/api/pinned/series/1", adult); code != http.StatusOK || strings.TrimSpace(body) != `{"pin":null}` {
		t.Fatalf("GET /api/pinned/series/1: %d %s", code, body)
	}
	if code, _ := get("/api/follow", `{"id":3,"name":"c","isKid":true}`); code != http.StatusForbidden {
		t.Fatalf("kid GET /api/follow: %d", code)
	}
	// nothing of it under the admins' mux
	if code, _ := get("/api/admin/follow", admin); code != http.StatusNotFound {
		t.Fatalf("GET /api/admin/follow: %d", code)
	}
}

// TestManifestDeclaresFlareSolverr: the manifest the program embeds
// declares the FlareSolverr helper service, and the watch is built on it.
func TestManifestDeclaresFlareSolverr(t *testing.T) {
	w, err := solverHelper(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if w == nil || w.Helper.Address() != "http://flaresolverr:8191" || w.Helper.Health != "/" {
		t.Fatalf("helper %+v", w)
	}
	disk, err := os.ReadFile("antresol-addon.json")
	if err != nil || string(disk) != string(manifestJSON) {
		t.Fatal("the embedded manifest is not antresol-addon.json")
	}
	if w, err := solverHelper([]byte(`{"id":"torrents"}`)); err != nil || w != nil {
		t.Fatalf("none declared: %+v %v", w, err)
	}
}
