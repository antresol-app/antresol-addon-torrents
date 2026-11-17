package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// makeTorrent writes files (paths relative to dir, all under one top-level
// name: a folder for a multi-file torrent, or one file) and returns the
// metafile and v1 infohash built from them.
func makeTorrent(t *testing.T, dir string, files map[string][]byte) (meta []byte, ih string) {
	t.Helper()
	root := ""
	for p, b := range files {
		top := strings.SplitN(p, "/", 2)[0]
		if root != "" && root != top {
			t.Fatalf("makeTorrent: %q is not under %q", p, root)
		}
		root = top
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 32 << 10}
	if err := info.BuildFromFilePath(filepath.Join(dir, root)); err != nil {
		t.Fatal(err)
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: ib, Announce: "http://tracker.invalid/announce"}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mi.HashInfoBytes().HexString()
}

// writeFastresume writes BT_backup/<ih>.fastresume as qBittorrent does.
func writeFastresume(t *testing.T, dir, ih string, fr map[string]any) {
	t.Helper()
	b, err := bencode.Marshal(fr)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ih+".fastresume"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// allPieces is a fastresume "pieces" value with every piece present.
func allPieces(t *testing.T, meta []byte) string {
	t.Helper()
	mi, err := metainfo.Load(bytes.NewReader(meta))
	if err != nil {
		t.Fatal(err)
	}
	inf, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Repeat("\x01", inf.NumPieces())
}

// treeHash is a SHA-256 over the names, sizes and contents of every file
// under dir.
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		h.Write([]byte(rel + "\x00"))
		if d.Type().IsRegular() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			h.Write(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fileHash(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// qbCall is one request the fake qBittorrent received.
type qbCall struct {
	Method, Path, Hashes string
	Cookie               string           // SID cookie, "" when none
	Engine               []torrent.Status // the Vault engine at that moment (POST only)
}

// fakeQBServer is a qBittorrent Web API stand-in. Torrent states are set by
// the test (state[hash]; missing → not in qBittorrent).
type fakeQBServer struct {
	*httptest.Server
	mu         sync.Mutex
	calls      []qbCall
	state      map[string]string
	stopCode   int  // reply to /torrents/stop (0 = 200)
	pauseCode  int  // reply to /torrents/pause
	startCode  int  // reply to /torrents/start and /resume
	ignoreStop bool // accept stop, but keep reporting the old state
	vanish     bool // a stopped torrent disappears from qBittorrent
	sid        string
	logins     int
	user, pass string
	engine     func() []torrent.Status
}

const fakeSID = "sid-123"

func fakeQB(t *testing.T) *fakeQBServer {
	f := &fakeQBServer{state: map[string]string{}, sid: fakeSID}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeQBServer) serve(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	c := qbCall{Method: r.Method, Path: r.URL.Path, Hashes: r.Form.Get("hashes")}
	if ck, err := r.Cookie("SID"); err == nil {
		c.Cookie = ck.Value
	}
	if r.Method == http.MethodPost && f.engine != nil {
		c.Engine = f.engine()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if f.user != "" && r.URL.Path != "/api/v2/auth/login" && c.Cookie != f.sid {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	reply := func(code int) bool {
		if code != 0 && code != 200 {
			w.WriteHeader(code)
			return false
		}
		return true
	}
	switch {
	case r.URL.Path == "/api/v2/auth/login" && r.Method == http.MethodPost:
		if r.Form.Get("username") == f.user && r.Form.Get("password") == f.pass {
			f.logins++
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: f.sid, Path: "/"})
			w.Write([]byte("Ok."))
		} else {
			w.Write([]byte("Fails."))
		}
	case r.URL.Path == "/api/v2/app/version" && r.Method == http.MethodGet:
		w.Write([]byte("v5.3.0"))
	case r.URL.Path == "/api/v2/torrents/stop" && r.Method == http.MethodPost:
		if reply(f.stopCode) && !f.ignoreStop {
			f.state[c.Hashes] = "stoppedUP"
			if f.vanish {
				delete(f.state, c.Hashes)
			}
		}
	case r.URL.Path == "/api/v2/torrents/pause" && r.Method == http.MethodPost:
		if reply(f.pauseCode) && !f.ignoreStop {
			f.state[c.Hashes] = "pausedUP"
		}
	case (r.URL.Path == "/api/v2/torrents/start" || r.URL.Path == "/api/v2/torrents/resume") && r.Method == http.MethodPost:
		if reply(f.startCode) {
			f.state[c.Hashes] = "uploading"
		}
	case r.URL.Path == "/api/v2/torrents/info" && r.Method == http.MethodGet:
		out := []map[string]string{}
		if s, ok := f.state[r.Form.Get("hashes")]; ok {
			out = append(out, map[string]string{"hash": r.Form.Get("hashes"), "state": s})
		}
		json.NewEncoder(w).Encode(out)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeQBServer) all() []qbCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// posts returns the POSTs other than the login.
func (f *fakeQBServer) posts() []qbCall {
	var out []qbCall
	for _, c := range f.all() {
		if c.Method == http.MethodPost && c.Path != "/api/v2/auth/login" {
			out = append(out, c)
		}
	}
	return out
}

// expire ends the current session: the old SID gets 403 from now on.
func (f *fakeQBServer) expire() {
	f.mu.Lock()
	f.sid += "x"
	f.mu.Unlock()
}

func (f *fakeQBServer) stateOf(ih string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.state[ih]
	return s, ok
}

func (f *fakeQBServer) setState(ih, s string) {
	f.mu.Lock()
	f.state[ih] = s
	f.mu.Unlock()
}

func inEngine(list []torrent.Status, ih string) (torrent.Status, bool) {
	for _, s := range list {
		if s.InfoHash == ih {
			return s, true
		}
	}
	return torrent.Status{}, false
}

// env is the addon's engine, the fake core's library, a qBittorrent
// BT_backup and data folder, and a fake qBittorrent API.
type env struct {
	t      *testing.T
	db     *sql.DB
	eng    *torrent.Engine
	lib    *coretest.Library
	qb     *fakeQBServer
	bt     string // BT_backup
	save   string // qBittorrent save path
	link   string // QBOptions.LinkDir
	state  string // QBOptions.StateDir
	series string // library root
	movies string // library root
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, torrent.EngineConfig{})
}

// newEnvWith is newEnv with the engine's config cfg (its folders and
// LocalOnly are set here).
func newEnvWith(t *testing.T, cfg torrent.EngineConfig) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{
		t:      t,
		bt:     filepath.Join(root, "qbt", "BT_backup"),
		save:   filepath.Join(root, "torrents"),
		link:   filepath.Join(root, "torrents", ".qbt-import"),
		state:  filepath.Join(root, "vault", "migrate", "qbt-state"),
		series: filepath.Join(root, "media", "series"),
		movies: filepath.Join(root, "media", "movies"),
	}
	for _, d := range []string{e.bt, e.save, e.series, e.movies} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dataDir := filepath.Join(root, "vault")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(filepath.Join(dataDir, "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	e.db = d
	cfg.DataDir, cfg.StreamDir, cfg.LocalOnly = dataDir, filepath.Join(dataDir, "stream"), true
	e.eng, err = torrent.New(cfg, d)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	e.lib = &coretest.Library{}
	e.qb = fakeQB(t)
	e.qb.engine = e.eng.List
	t.Cleanup(func() { e.eng.Close(); d.Close() })
	return e
}

// addTorrent creates the torrent's data in the save path, its .torrent in
// BT_backup and a complete, running fastresume (fr overrides keys; a nil
// value deletes one). qBittorrent reports it as uploading.
func (e *env) addTorrent(files map[string][]byte, fr map[string]any) (ih string, meta []byte) {
	e.t.Helper()
	meta, ih = makeTorrent(e.t, e.save, files)
	if err := os.WriteFile(filepath.Join(e.bt, ih+".torrent"), meta, 0o644); err != nil {
		e.t.Fatal(err)
	}
	res := map[string]any{
		"save_path":    e.save,
		"qBt-savePath": e.save,
		"qBt-name":     "",
		"qBt-category": "tv-sonarr",
		"pieces":       allPieces(e.t, meta),
		"paused":       0,
		"auto_managed": 0,
	}
	for k, v := range fr {
		if v == nil {
			delete(res, k)
		} else {
			res[k] = v
		}
	}
	writeFastresume(e.t, e.bt, ih, res)
	e.qb.setState(ih, "uploading")
	return ih, meta
}

func (e *env) opts() QBOptions {
	return QBOptions{
		BTBackupDir:   e.bt,
		WebURL:        e.qb.URL,
		LinkDir:       e.link,
		StateDir:      e.state,
		LibraryRoots:  []string{e.movies, e.series},
		AddedBy:       1,
		AddedByUID:    "uid-1",
		VerifyTimeout: 30 * time.Second,
		StopTimeout:   3 * time.Second,
		KeepPaused:    func(context.Context) bool { return false }, // sharing allowed: running ones restart in qB
	}
}

func (e *env) run(o QBOptions) *Report {
	e.t.Helper()
	rep := &Report{ID: NewReportID(time.Now()), Sources: []string{SourceQBittorrent}, DryRun: o.DryRun, StartedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := ImportQBittorrent(ctx, o, e.eng, e.lib, rep); err != nil {
		e.t.Fatal(err)
	}
	return rep
}

// line returns the only line of rep.
func onlyLine(t *testing.T, rep *Report) Line {
	t.Helper()
	if len(rep.Lines) != 1 {
		t.Fatalf("lines %+v", rep.Lines)
	}
	return rep.Lines[0]
}
