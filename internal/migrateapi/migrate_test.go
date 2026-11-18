package migrateapi_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/antresol-app/antresol-addon-torrents/internal/admin"
	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	mediaimport "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/mediaimport/runkit"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/migrate"
	"github.com/antresol-app/antresol-addon-torrents/internal/migrateapi"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings" // the English catalogs
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// mgIndexer is Prowlarr: one tracker to add; block holds it (a long run).
type mgIndexer struct{ block chan struct{} }

func (f *mgIndexer) PreviewProwlarrIn(ctx context.Context, _, _ string) ([]indexer.Imported, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return []indexer.Imported{{Name: "RuTor", Reason: indexer.ReasonWillAdd}}, nil
}

func (f *mgIndexer) ImportProwlarrIn(ctx context.Context, p, t string) ([]indexer.Imported, error) {
	return f.PreviewProwlarrIn(ctx, p, t)
}

// mgEngine has no torrents.
type mgEngine struct{}

func (mgEngine) AddTorrentFile(context.Context, []byte, torrent.AddOptions) (torrent.Handle, error) {
	return nil, errors.New("fake")
}
func (mgEngine) Get(string) (torrent.Handle, bool)                        { return nil, false }
func (mgEngine) Resume(context.Context, string) error                     { return nil }
func (mgEngine) Pause(context.Context, string) error                      { return nil }
func (mgEngine) RemoveIfMode(context.Context, string, string, bool) error { return torrent.ErrNotFound }
func (mgEngine) Offline() string                                          { return "" }
func (mgEngine) WaitVerified(context.Context, string) (int64, int64, error) {
	return 0, 0, errors.New("fake")
}

// mgTMDB: series 42 has one aired season of two episodes; movie 7 is out.
type mgTMDB struct{}

func (mgTMDB) TV(_ context.Context, id int) (tmdb.Details, error) {
	return tmdb.Details{ID: id, Title: "Сёгун", Seasons: []tmdb.SeasonCount{{Number: 1, Episodes: 2}}}, nil
}
func (mgTMDB) Season(_ context.Context, _, n int) (tmdb.SeasonDetails, error) {
	return tmdb.SeasonDetails{Number: n, AirDate: "2024-01-01", Episodes: []tmdb.Episode{{Number: 1, AirDate: "2024-01-01"}, {Number: 2, AirDate: "2024-01-08"}}}, nil
}
func (mgTMDB) Movie(_ context.Context, id int) (tmdb.Details, error) {
	return tmdb.Details{ID: id, Title: "Дюна", Year: 2024, ReleaseDate: "2024-02-27"}, nil
}
func (mgTMDB) ImageURL(_, p string) string { return p }

type mgAcq struct{}

func (mgAcq) Acquire(context.Context, acquire.AcquireRequest) (acquire.AcquireResult, error) {
	return acquire.AcquireResult{InfoHash: "ih"}, nil
}
func (mgAcq) Pending(context.Context) ([]acquire.PendingAcquisition, error) { return nil, nil }

// events records what the run published.
type events struct {
	mu   sync.Mutex
	got  []string
	data []mediaimport.Progress
}

func (e *events) Event(_ context.Context, name string, data any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, name)
	if p, ok := data.(mediaimport.Progress); ok {
		e.data = append(e.data, p)
	}
	return nil
}

type env struct {
	t       *testing.T
	svc     *migrate.Service
	h       http.Handler
	idx     *mgIndexer
	ev      *events
	reports string
	mu      sync.Mutex
	ready   bool
}

const adminID = 7

func newEnv(t *testing.T) *env { return newEnvWith(t, nil) }

// newEnvWith builds «Переезд» over the addon's own database; lib (nil: an
// empty fake) is the core's library the qBittorrent import binds in.
func newEnvWith(t *testing.T, lib migrate.LibraryFiles) *env {
	t.Helper()
	d, err := store.Open(filepath.Join(t.TempDir(), "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	data := t.TempDir()
	e := &env{t: t, idx: &mgIndexer{}, ev: &events{}, reports: filepath.Join(data, "migrate", "reports"), ready: true}
	ctx, cancel := context.WithCancel(context.Background())
	r := migrate.NewRunner(&migrate.Runner{ReportDir: e.reports, TmpDir: filepath.Join(data, "tmp", "migrate"), BaseCtx: ctx,
		Progress: migrate.PublishTo(e.ev, nil),
		LibraryReady: func(context.Context) (bool, string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.ready, "Медиатека ещё сканируется — подождите"
		}})
	if lib == nil {
		lib = &coretest.Library{}
	}
	src, left := migrate.DefaultPaths(t.TempDir(), "")
	fl := &follow.Service{DB: d, Acquire: mgAcq{}, TMDB: mgTMDB{}, Library: &coretest.Library{}, Pause: -1,
		Now: func() time.Time { return time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC) }}
	e.svc = migrate.New(r, migrate.Service{DB: d, Engine: mgEngine{}, Indexer: e.idx, Follow: fl, Library: lib,
		Paths: src, Leftover: left, QB: migrate.QBOptions{StateDir: filepath.Join(data, "migrate", "qbt-state"),
			KeepPaused: func(context.Context) bool { return false }}})
	e.h = admin.Routes(admin.Deps{Migrate: migrateapi.Routes(migrateapi.Deps{Migrate: e.svc})})
	t.Cleanup(func() { cancel(); e.svc.Wait() })
	return e
}

var adminUser = addonkit.User{ID: adminID, UID: "uid-admin", Name: "Александр", IsAdmin: true, Lang: "ru"}

func (e *env) do(method, path, body string, u *addonkit.User) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if u != nil {
		b, _ := json.Marshal(u)
		r.Header.Set(addonkit.UserHeader, string(b))
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) admin(method, path, body string) *httptest.ResponseRecorder {
	return e.do(method, path, body, &adminUser)
}

func errText(w *httptest.ResponseRecorder) string {
	var v struct {
		Error string `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &v)
	return v.Error
}

func reportID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var r struct {
		ReportID string `json:"reportId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.ReportID == "" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	return r.ReportID
}

// The status has the core's shape (sources, leftovers, users, profiles,
// running, the switch state, libraryReady): the addon's sources map no
// profiles, so users and profiles are empty lists.
func TestMigrateAPI_Status(t *testing.T) {
	e := newEnv(t)
	w := e.admin("GET", "/api/admin/migrate", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var st struct {
		Sources []mediaimport.SourceStatus `json:"sources"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	if len(st.Sources) != 4 || st.Sources[0].ID != "prowlarr" || st.Sources[3].ID != "qbittorrent" {
		t.Fatalf("sources %s", w.Body)
	}
	for _, k := range []string{`"running":null`, `"state":{"switchedAt":null}`, `"pendingFollows":0`, `"leftovers":[]`, `"users":[]`, `"profiles":[]`,
		`"libraryReady":{"ok":true,"reason":""}`} {
		if !strings.Contains(w.Body.String(), k) {
			t.Errorf("missing %s in %s", k, w.Body)
		}
	}
}

func TestMigrateAPI_RunNeedsDryRun(t *testing.T) {
	e := newEnv(t)
	w := e.admin("POST", "/api/admin/migrate/run", `{"sources":["prowlarr"],"dryRun":false}`)
	if w.Code != http.StatusConflict || errText(w) != "Сначала проверьте, что будет перенесено" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	w = e.admin("POST", "/api/admin/migrate/run", `{"sources":["prowlarr"],"dryRun":true,"userMap":{}}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	plan := reportID(t, w)
	e.svc.Wait()
	w = e.admin("POST", "/api/admin/migrate/run", `{"sources":["prowlarr"],"dryRun":false,"planId":"`+plan+`"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("real: %d %s", w.Code, w.Body)
	}
	e.svc.Wait()
	w = e.admin("GET", "/api/admin/migrate/reports/"+plan, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"RuTor"`) || !strings.Contains(w.Body.String(), `"counts":{"planned":1}`) {
		t.Fatalf("report %d %s", w.Code, w.Body)
	}
	w = e.admin("GET", "/api/admin/migrate/reports", "")
	if w.Code != 200 || strings.Count(w.Body.String(), `"id"`) != 2 || strings.Contains(w.Body.String(), "RuTor") || !strings.Contains(w.Body.String(), `"torrents":0`) {
		t.Fatalf("reports %d %s", w.Code, w.Body)
	}
	for body, msg := range map[string]string{
		`{"sources":["navidrome"],"dryRun":true}`: "Неизвестный источник",
		`{"sources":[],"dryRun":true}`:            "Выберите, что перенести",
		`{"sources":`:                             "Неверный запрос",
	} {
		if w := e.admin("POST", "/api/admin/migrate/run", body); w.Code != 400 || errText(w) != msg {
			t.Errorf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	// the admin's language
	en := adminUser
	en.Lang = "en"
	if w := e.do("POST", "/api/admin/migrate/run", `{"sources":["prowlarr"]}`, &en); errText(w) != "First check what will be moved" {
		t.Fatalf("en: %d %s", w.Code, w.Body)
	}
}

// a real run takes the admin's uid from X-Antresol-User; a header
// without one (an older core) refuses it, before anything starts: its
// follows and torrents would be nobody's. A dry run writes nothing and
// needs none.
func TestMigrateAPI_RunNeedsAdminUID(t *testing.T) {
	e := newEnv(t)
	noUID := adminUser
	noUID.UID = ""
	w := e.do("POST", "/api/admin/migrate/run", `{"sources":["prowlarr"],"dryRun":true}`, &noUID)
	if w.Code != http.StatusAccepted {
		t.Fatalf("dry run: %d %s", w.Code, w.Body)
	}
	plan := reportID(t, w)
	e.svc.Wait()
	real := `{"sources":["prowlarr"],"dryRun":false,"planId":"` + plan + `"}`
	if w := e.do("POST", "/api/admin/migrate/run", real, &noUID); w.Code != http.StatusForbidden ||
		errText(w) != "Сервер не назвал профиль администратора — перенос не начат" {
		t.Fatalf("real run without a uid: %d %s", w.Code, w.Body)
	}
	noUID.Lang = "en"
	if w := e.do("POST", "/api/admin/migrate/run", real, &noUID); errText(w) != "The server did not name the admin's profile, so the move did not start" {
		t.Fatalf("en: %d %s", w.Code, w.Body)
	}
	if w := e.admin("POST", "/api/admin/migrate/run", real); w.Code != http.StatusAccepted {
		t.Fatalf("with the uid: %d %s", w.Code, w.Body)
	}
	e.svc.Wait()
}

func TestMigrateAPI_RefusedUntilScanned(t *testing.T) {
	e := newEnv(t)
	e.mu.Lock()
	e.ready = false
	e.mu.Unlock()
	w := e.admin("POST", "/api/admin/migrate/run", `{"sources":["prowlarr"],"dryRun":true}`)
	if w.Code != http.StatusConflict || errText(w) != "Медиатека ещё сканируется — подождите" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := e.admin("GET", "/api/admin/migrate/state", ""); !strings.Contains(w.Body.String(), `"libraryReady":{"ok":false,"reason":"Медиатека ещё сканируется — подождите"}`) {
		t.Fatalf("state %s", w.Body)
	}
}

func TestMigrateAPI_Busy(t *testing.T) {
	e := newEnv(t)
	e.idx.block = make(chan struct{})
	w := e.admin("POST", "/api/admin/migrate/run", `{"sources":["prowlarr"],"dryRun":true}`)
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	id := reportID(t, w)
	if w := e.admin("POST", "/api/admin/migrate/run", `{"sources":["prowlarr"],"dryRun":true}`); w.Code != http.StatusConflict || errText(w) != "Перенос уже идёт" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// the page polls the state; it reads no old database
	before := e.svc.Scans()
	for range 3 {
		if w := e.admin("GET", "/api/admin/migrate/state", ""); !strings.Contains(w.Body.String(), `"running":"`+id+`"`) || strings.Contains(w.Body.String(), `"sources"`) {
			t.Fatalf("state while running %s", w.Body)
		}
	}
	if e.svc.Scans() != before {
		t.Fatal("the state poll read the old databases")
	}
	close(e.idx.block)
	e.svc.Wait()
	if w := e.admin("GET", "/api/admin/migrate/state", ""); !strings.Contains(w.Body.String(), `"running":null`) {
		t.Fatalf("after %s", w.Body)
	}
}

// TestMigrateAPI_AdminOnly: through admin.Routes, an adult's or a kid's
// header is 403 and none is 403 too (the proxy sends admins only); no run
// starts.
func TestMigrateAPI_AdminOnly(t *testing.T) {
	e := newEnv(t)
	adult := &addonkit.User{ID: 2, Name: "Мама"}
	kid := &addonkit.User{ID: 3, Name: "Сын", IsKid: true}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/admin/migrate", ""},
		{"GET", "/api/admin/migrate/state", ""},
		{"POST", "/api/admin/migrate/run", `{"sources":["prowlarr"],"dryRun":true}`},
		{"GET", "/api/admin/migrate/reports", ""},
		{"GET", "/api/admin/migrate/reports/20261025T193000Z-abcdef", ""},
		{"POST", "/api/admin/migrate/reports/20261025T193000Z-abcdef/rollback", ""},
		{"GET", "/api/admin/migrate/switch", ""},
		{"POST", "/api/admin/migrate/switch", `{"confirm":0,"arrStopped":true}`},
	} {
		for _, u := range []*addonkit.User{adult, kid, nil} {
			if w := e.do(c.method, c.path, c.body, u); w.Code != http.StatusForbidden {
				t.Errorf("%v %s %s: %d", u, c.method, c.path, w.Code)
			}
		}
	}
	if e.svc.Running() != "" {
		t.Fatal("a refused request started a run")
	}
}

// TestMigrateAPI_ProgressEvent: a run publishes admin.migrate.progress
// after each source, the last one finished.
func TestMigrateAPI_ProgressEvent(t *testing.T) {
	e := newEnv(t)
	id := reportID(t, e.admin("POST", "/api/admin/migrate/run", `{"sources":["prowlarr","sonarr"],"dryRun":true}`))
	e.svc.Wait()
	e.ev.mu.Lock()
	defer e.ev.mu.Unlock()
	if len(e.ev.got) != 2 || e.ev.got[0] != "admin.migrate.progress" || e.ev.got[1] != "admin.migrate.progress" {
		t.Fatalf("events %v", e.ev.got)
	}
	p := e.ev.data
	if p[0].ReportID != id || p[0].Source != "prowlarr" || p[0].Done != 1 || p[0].Total != 2 || p[0].Finished ||
		p[1].Source != "sonarr" || p[1].Done != 2 || !p[1].Finished {
		t.Fatalf("progress %+v", p)
	}
}

func TestMigrateAPI_Switch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, in := range []follow.ImportFollow{
		{Kind: follow.KindSeries, TmdbID: 42, Title: "Сёгун", Source: follow.SourceSonarr,
			Import: &follow.ImportState{Baseline: time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC), Want: [][2]int{{1, 2}}}},
		{Kind: follow.KindMovie, TmdbID: 7, Title: "Дюна", Source: follow.SourceRadarr},
	} {
		if _, err := follow.ImportInactive(ctx, e.svc.DB, addonkit.User{ID: adminID, UID: "uid-admin"}, in); err != nil {
			t.Fatal(err)
		}
	}
	w := e.admin("GET", "/api/admin/migrate/switch", "")
	if w.Code != 200 || w.Body.String() != `{"follows":2,"episodes":1,"movies":1,"errors":0,"blocked":""}`+"\n" {
		t.Fatalf("preview %d %q", w.Code, w.Body)
	}
	if w := e.admin("POST", "/api/admin/migrate/switch", `{"confirm":2,"arrStopped":false}`); w.Code != 400 || errText(w) != "Сначала остановите Sonarr и Radarr" {
		t.Fatalf("arr: %d %s", w.Code, w.Body)
	}
	if w := e.admin("POST", "/api/admin/migrate/switch", `{"confirm":5,"arrStopped":true}`); w.Code != 409 || errText(w) != "Число загрузок изменилось — проверьте ещё раз" {
		t.Fatalf("count: %d %s", w.Code, w.Body)
	}
	if w := e.admin("POST", "/api/admin/migrate/switch", `{"confirm":2,"arrStopped":true}`); w.Code != 200 || w.Body.String() != `{"activated":2}`+"\n" {
		t.Fatalf("switch: %d %s", w.Code, w.Body)
	}
	if w := e.admin("GET", "/api/admin/migrate/state", ""); strings.Contains(w.Body.String(), `"switchedAt":null`) || !strings.Contains(w.Body.String(), `"pendingFollows":0`) {
		t.Fatalf("state %s", w.Body)
	}
	e.svc.Follow = nil
	if w := e.admin("GET", "/api/admin/migrate/switch", ""); w.Code != 409 || errText(w) != "Для слежения нужен ключ TMDB" {
		t.Fatalf("no tmdb: %d %s", w.Code, w.Body)
	}
}

// qbStopped is a qBittorrent Web API that has nothing running.
func qbStopped(t *testing.T) string {
	qb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/app/version" {
			w.Write([]byte("v5.3.0"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(qb.Close)
	return qb.URL
}

func TestMigrateAPI_Rollback(t *testing.T) {
	e := newEnv(t)
	e.svc.QB.WebURL = qbStopped(t)
	now := time.Now()
	ih := strings.Repeat("ab", 20)
	real := &migrate.Report{ID: migrate.NewReportID(now), Sources: []string{"qbittorrent"}, StartedAt: now, FinishedAt: now,
		Lines: []migrate.Line{{Source: "qbittorrent", Kind: "torrent", Name: "Дюна", Status: migrate.StatusImported, Ref: ih, Prev: migrate.PrevStopped}}}
	dry := &migrate.Report{ID: migrate.NewReportID(now.Add(time.Second)), DryRun: true, StartedAt: now, FinishedAt: now,
		Lines: []migrate.Line{{Source: "qbittorrent", Status: migrate.StatusPlanned, Ref: ih}}}
	for _, r := range []*migrate.Report{real, dry} {
		if err := migrate.SaveReport(e.reports, r); err != nil {
			t.Fatal(err)
		}
	}
	if w := e.admin("GET", "/api/admin/migrate/reports", ""); !strings.Contains(w.Body.String(), `"torrents":1`) {
		t.Fatalf("list %s", w.Body)
	}
	w := e.admin("POST", "/api/admin/migrate/reports/"+real.ID+"/rollback", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	back := reportID(t, w)
	e.svc.Wait()
	w = e.admin("GET", "/api/admin/migrate/reports/"+back, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Возвращена в qBittorrent") || !strings.Contains(w.Body.String(), `"rollbackOf":"`+real.ID+`"`) {
		t.Fatalf("rollback report %d %s", w.Code, w.Body)
	}
	if w := e.admin("POST", "/api/admin/migrate/reports/"+real.ID+"/rollback", ""); w.Code != http.StatusConflict || errText(w) != "Раздачи из этого переноса уже возвращены" {
		t.Fatalf("twice: %d %s", w.Code, w.Body)
	}
	if w := e.admin("POST", "/api/admin/migrate/reports/"+dry.ID+"/rollback", ""); w.Code != http.StatusConflict || errText(w) != "В этом отчёте нет перенесённых раздач" {
		t.Fatalf("dry: %d %s", w.Code, w.Body)
	}
	if w := e.admin("POST", "/api/admin/migrate/reports/20200101T000000Z-000000/rollback", ""); w.Code != http.StatusNotFound {
		t.Fatalf("missing: %d %s", w.Code, w.Body)
	}
}

func TestMigrateAPI_ReportIDValidated(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/api/admin/migrate/reports/..%2f..", "/api/admin/migrate/reports/..%2F..%2Fvault", "/api/admin/migrate/reports/x",
		"/api/admin/migrate/reports/20261025T193000Z-abcdef"} {
		if w := e.admin("GET", p, ""); w.Code != http.StatusNotFound || errText(w) != "Отчёт не найден" {
			t.Errorf("%s: %d %s", p, w.Code, w.Body)
		}
	}
}

// slowCore is the core with a media-server run of its own in progress:
// its /library/import takes until release is closed.
type slowCore struct {
	srv     *httptest.Server
	release chan struct{}
	inside  chan struct{}
}

func newSlowCore(t *testing.T) *slowCore {
	c := &slowCore{release: make(chan struct{}), inside: make(chan struct{}, 1)}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if strings.HasSuffix(r.URL.Path, "/library/import") {
			c.inside <- struct{}{}
			<-c.release
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"files":[],"missing":[],"taken":[]}`)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// TestMigrateAPI_ConcurrentWithCoreRun: the core's own «Переезд» run is
// importing into the library (a slow /library/import); the addon's run
// is not held by it (its lock is its own) and finishes, and the two keep
// their reports apart.
func TestMigrateAPI_ConcurrentWithCoreRun(t *testing.T) {
	core := newSlowCore(t)
	lib := coreadapt.Library{C: addonkit.NewClient(addonkit.Env{CoreURL: core.srv.URL, Token: "tok"})}
	e := newEnvWith(t, lib)
	coreReports := t.TempDir() // the core's DataDir/migrate
	coreRep := &migrate.Report{ID: migrate.NewReportID(time.Now()), Sources: []string{"jellyfin"}, StartedAt: time.Now(), Lines: []migrate.Line{}}
	if err := migrate.SaveReport(coreReports, coreRep); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		lib.Import(context.Background(), []string{"/m/movies/x.mkv"}, "", 0)
	}()
	<-core.inside
	w := e.admin("POST", "/api/admin/migrate/run", `{"sources":["prowlarr"],"dryRun":true}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	id := reportID(t, w)
	finished := make(chan struct{})
	go func() { e.svc.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the addon's run waited on the core's")
	}
	w = e.admin("GET", "/api/admin/migrate/reports", "")
	if !strings.Contains(w.Body.String(), id) || strings.Contains(w.Body.String(), coreRep.ID) {
		t.Fatalf("addon history %s", w.Body)
	}
	if ents, _ := os.ReadDir(coreReports); len(ents) != 1 {
		t.Fatalf("the core's reports %v", ents)
	}
	close(core.release)
	<-done
}

// A run refused as not ready never answers an empty text, even when the
// library turns ready between the refusal and the reason.
func TestMigrateAPI_NotReadyNeverEmpty(t *testing.T) {
	e := newEnv(t)
	asked := 0
	e.svc.LibraryReady = func(context.Context) (bool, string) { asked++; return asked > 1, "" }
	w := e.admin("POST", "/api/admin/migrate/run", `{"sources":["prowlarr"],"dryRun":true}`)
	if w.Code != http.StatusConflict || errText(w) != "Медиатека ещё сканируется — подождите" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// fakeRadarr is Radarr's API with one monitored movie; it wants key.
func fakeRadarr(t *testing.T, key string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v3/system/status":
			io.WriteString(w, `{"appName":"Radarr","version":"5.0"}`)
		case "/api/v3/movie":
			io.WriteString(w, `[{"id":1,"title":"Дюна","year":2021,"tmdbId":438631,"monitored":true,"hasFile":false},
				{"id":2,"title":"Брат","year":1997,"tmdbId":2,"monitored":false,"hasFile":true}]`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// «Проверить» on a card whose database is not mounted: the address and
// the key are checked and kept; the status shows the connection, never
// the key; «Забыть» drops it.
func TestMigrateAPI_Connect(t *testing.T) {
	e := newEnv(t)
	const key = "radarr-secret-key"
	srv := fakeRadarr(t, key)
	path := "/api/admin/migrate/sources/radarr/connect"

	w := e.admin("POST", path, `{"url":"`+srv.URL+`","apiKey":"wrong"}`)
	if w.Code != http.StatusBadRequest || errText(w) != "Radarr не принял API-ключ — скопируйте его в Radarr: «Settings → General → API Key»" {
		t.Fatalf("wrong key: %d %s", w.Code, w.Body)
	}
	en := adminUser
	en.Lang = "en"
	w = e.do("POST", path, `{"url":"`+srv.URL+`","apiKey":"wrong"}`, &en)
	if w.Code != http.StatusBadRequest || errText(w) != "Radarr didn’t accept the API key — copy it from Radarr: “Settings → General → API Key”" {
		t.Fatalf("wrong key (en): %d %s", w.Code, w.Body)
	}

	w = e.admin("POST", path, `{"url":"`+srv.URL+`","apiKey":"`+key+`"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"detail":"2 фильма, 1 отслеживается"`) || strings.Contains(w.Body.String(), key) {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}

	w = e.admin("GET", "/api/admin/migrate", "")
	if strings.Contains(w.Body.String(), key) {
		t.Fatalf("the key is in the status: %s", w.Body)
	}
	var st struct {
		Sources []struct {
			mediaimport.SourceStatus
			Connect *migrate.RemoteView `json:"connect"`
		} `json:"sources"`
	}
	json.Unmarshal(w.Body.Bytes(), &st)
	for _, s := range st.Sources {
		if s.Connect == nil || s.Connect.Kind == "" {
			t.Errorf("%s has no connection form: %+v", s.ID, s)
		}
		if s.ID == "radarr" && (!s.Found || s.Detail != "2 фильма, 1 отслеживается" || !s.Connect.Connected || s.Connect.URL != srv.URL || !s.Connect.HasSecret) {
			t.Errorf("radarr %+v %+v", s.SourceStatus, s.Connect)
		}
		if s.ID == "qbittorrent" && s.Connect.Kind != "login" {
			t.Errorf("qbittorrent %+v", s.Connect)
		}
	}

	if w := e.admin("POST", "/api/admin/migrate/sources/jellyfin/connect", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown: %d %s", w.Code, w.Body)
	}
	if w := e.do("POST", path, `{"url":"`+srv.URL+`","apiKey":"`+key+`"}`, &addonkit.User{ID: 9, UID: "u9", Name: "Гость"}); w.Code != http.StatusForbidden {
		t.Fatalf("not an admin: %d %s", w.Code, w.Body)
	}

	if w := e.admin("DELETE", path, ""); w.Code != http.StatusOK {
		t.Fatalf("forget: %d %s", w.Code, w.Body)
	}
	w = e.admin("GET", "/api/admin/migrate", "")
	if !strings.Contains(w.Body.String(), `"connected":false`) || strings.Contains(w.Body.String(), srv.URL) {
		t.Fatalf("after forget: %s", w.Body)
	}
}
