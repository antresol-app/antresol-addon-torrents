package find

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/admin"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// fakeAcq records the requests and answers from its fields.
type fakeAcq struct {
	mu         sync.Mutex
	tmdbLang   string
	mediaKind  string
	acquired   []acquire.AcquireRequest
	acquireErr error
	cancelled  []string
	promoted   []string
	batches    int
	// releases: progress steps sent before the final answer; gate holds
	// the final answer until closed
	steps   []indexer.Progress
	gate    chan struct{}
	relResp indexer.SearchResponse
	relErr  error
	files   acquire.ReleaseFiles
	list    []acquire.TorrentView
}

func (f *fakeAcq) Discover(ctx context.Context, q string) ([]acquire.Candidate, error) {
	f.mu.Lock()
	f.tmdbLang = tmdb.ContextLanguage(ctx)
	f.mu.Unlock()
	return []acquire.Candidate{{Kind: "movie", TMDBID: 603, Title: "Матрица"}}, nil
}

func (f *fakeAcq) DiscoverMedia(_ context.Context, kind, q string) ([]acquire.Candidate, error) {
	f.mu.Lock()
	f.mediaKind = kind
	f.mu.Unlock()
	return nil, nil
}

func (f *fakeAcq) Releases(ctx context.Context, req acquire.ReleaseRequest, progress func(indexer.Progress)) (indexer.SearchResponse, error) {
	if progress != nil {
		for _, p := range f.steps {
			progress(p)
		}
	}
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return indexer.SearchResponse{}, ctx.Err()
		}
	}
	return f.relResp, f.relErr
}

func (f *fakeAcq) Acquire(_ context.Context, req acquire.AcquireRequest) (acquire.AcquireResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquired = append(f.acquired, req)
	if f.acquireErr != nil {
		return acquire.AcquireResult{}, f.acquireErr
	}
	return acquire.AcquireResult{InfoHash: "abc"}, nil
}

func (f *fakeAcq) TitleProgress(_ context.Context, kind string, tmdbID, season int) (acquire.TitleProgress, bool) {
	if tmdbID == 42 {
		return acquire.TitleProgress{Status: "active", Done: 1, Size: 2}, true
	}
	if tmdbID == 43 {
		return acquire.TitleProgress{Status: "active", Done: 1, Size: 2, Stalled: torrent.StalledNoPeers,
			TrackerError: "passkey 123 of Петя", TrackerErrorKind: torrent.TrackerRejected}, true
	}
	return acquire.TitleProgress{}, false
}

func (f *fakeAcq) TitlesProgress(ctx context.Context, keys []acquire.TitleKey) map[acquire.TitleKey]acquire.TitleProgress {
	f.mu.Lock()
	f.batches++
	f.mu.Unlock()
	out := map[acquire.TitleKey]acquire.TitleProgress{}
	for _, k := range keys {
		if p, ok := f.TitleProgress(ctx, k.Kind, k.TMDBID, k.Season); ok {
			out[k] = p
		}
	}
	return out
}

func (f *fakeAcq) CancelTitle(_ context.Context, kind string, tmdbID, season int, keep string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, kind+"|"+keep)
	return 2, nil
}

func (f *fakeAcq) Promote(_ context.Context, ih string, force bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.promoted = append(f.promoted, ih+"|"+map[bool]string{true: "force", false: ""}[force])
	return "downloading", nil
}

func (f *fakeAcq) ReleaseFiles(context.Context, acquire.ReleaseFilesRequest) (acquire.ReleaseFiles, error) {
	return f.files, nil
}

func (f *fakeAcq) Torrents() []acquire.TorrentView { return f.list }

const (
	adultU = `{"id":5,"name":"Маша","lang":"ru"}`
	adminU = `{"id":1,"name":"Папа","isAdmin":true,"lang":"ru"}`
	kidU   = `{"id":7,"name":"Ваня","isKid":true,"isAdmin":true}`
)

type env struct {
	acq      *fakeAcq
	api      http.Handler
	admin    http.Handler
	approved []addonkit.ApproveRequest
	approveE error
	// approveErr, approveDeadline: the last approve's context when it ran
	approveErr      error
	approveDeadline bool
	added           int
	mu              sync.Mutex
}

func newEnv(t *testing.T) *env {
	e := &env{acq: &fakeAcq{}}
	d := Deps{Acquire: e.acq,
		Languages: func(_ context.Context, id int64) []string {
			if id == 5 {
				return []string{"ru", "en"}
			}
			return nil
		},
		Approve: func(ctx context.Context, a addonkit.ApproveRequest) (int, error) {
			e.mu.Lock()
			defer e.mu.Unlock()
			_, e.approveDeadline = ctx.Deadline()
			e.approveErr = ctx.Err()
			e.approved = append(e.approved, a)
			return 1, e.approveE
		},
		Added: func() { e.mu.Lock(); e.added++; e.mu.Unlock() },
	}
	mux := http.NewServeMux()
	Register(mux, d)
	files, stats := Admin(d)
	e.api, e.admin = mux, admin.Routes(admin.Deps{AcquireFiles: files, Stats: stats})
	return e
}

func do(h http.Handler, method, target, user, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if user != "" {
		r.Header.Set(addonkit.UserHeader, user)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

var routes = []struct{ method, path, body string }{
	{"GET", "/api/discover?q=x", ""},
	{"GET", "/api/releases?tmdbId=1&kind=movie", ""},
	{"POST", "/api/acquire", `{"tmdbId":1,"kind":"movie","mode":"download"}`},
	{"GET", "/api/acquire/progress?kind=movie&tmdbId=42", ""},
	{"POST", "/api/acquire/cancel", `{"kind":"movie","tmdbId":1}`},
	{"POST", "/api/torrents/abc/promote", ""},
}

func TestFind_KidRefused(t *testing.T) {
	e := newEnv(t)
	for _, c := range routes {
		for _, u := range []string{kidU, ""} {
			if w := do(e.api, c.method, c.path, u, c.body); w.Code != 403 || !strings.Contains(w.Body.String(), "Недоступно в детском профиле") {
				t.Errorf("%s %s as %q: %d %s", c.method, c.path, u, w.Code, w.Body)
			}
		}
	}
	for _, p := range []string{"/api/admin/stats"} {
		if w := do(e.admin, "GET", p, kidU, ""); w.Code != 403 {
			t.Errorf("%s as a kid: %d", p, w.Code)
		}
	}
	if len(e.acq.acquired)+len(e.acq.cancelled)+len(e.acq.promoted) != 0 {
		t.Fatal("a kid reached the service")
	}
}

// Without kind: TMDB in the profile's language; with one: its catalog.
func TestDiscover_Video(t *testing.T) {
	e := newEnv(t)
	w := do(e.api, "GET", "/api/discover?q=matrix", `{"id":5,"lang":"en"}`, "")
	var cs []acquire.Candidate
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &cs) != nil || len(cs) != 1 || cs[0].TMDBID != 603 || e.acq.tmdbLang != "en-US" {
		t.Fatalf("%d %s lang %q", w.Code, w.Body, e.acq.tmdbLang)
	}
	if w := do(e.api, "GET", "/api/discover?q=x&kind=music", adultU, ""); w.Code != 200 || strings.TrimSpace(w.Body.String()) != "[]" || e.acq.mediaKind != "music" {
		t.Fatalf("music: %d %s %q", w.Code, w.Body, e.acq.mediaKind)
	}
}

func rel(title string, subs ...string) indexer.ScoredRelease {
	r := indexer.ScoredRelease{Info: release.Info{Subs: subs}}
	r.Result.Title = title
	return r
}

// stream=1: a line per answering source while others are pending, flushed
// at once, then the final line; the subtitles cut to «Мои языки».
func TestReleases_Stream(t *testing.T) {
	e := newEnv(t)
	e.acq.steps = []indexer.Progress{
		{Pending: []indexer.PendingSource{{SourceID: 1, Name: "A"}, {SourceID: 2, Name: "B"}}},
		{SearchResponse: indexer.SearchResponse{Releases: []indexer.ScoredRelease{rel("fast", "ru", "ja", "sub")}}, Pending: []indexer.PendingSource{{SourceID: 2, Name: "B"}}},
	}
	e.acq.gate = make(chan struct{})
	var open sync.Once
	release := func() { open.Do(func() { close(e.acq.gate) }) }
	defer release() // a failed read never leaves the handler waiting
	e.acq.relResp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{rel("fast", "ru"), rel("slow", "en", "de")}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set(addonkit.UserHeader, adultU)
		e.api.ServeHTTP(w, r)
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/releases?tmdbId=1&kind=movie&stream=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-ndjson" {
		t.Fatalf("content type %q", ct)
	}
	rd := bufio.NewReader(resp.Body)
	var lines []map[string]any
	var raws []string
	readLine := func() map[string]any {
		t.Helper()
		done := make(chan string, 1)
		go func() { s, _ := rd.ReadString('\n'); done <- s }()
		select {
		case s := <-done:
			raws = append(raws, s)
			var m map[string]any
			if err := json.Unmarshal([]byte(s), &m); err != nil {
				t.Fatalf("line %q: %v", s, err)
			}
			return m
		case <-time.After(5 * time.Second):
			release()
			t.Fatal("no line: not flushed")
			return nil
		}
	}
	// both progress lines arrive while the slow source has not answered
	lines = append(lines, readLine(), readLine())
	release()
	lines = append(lines, readLine())
	if lines[0]["done"] != false || len(lines[0]["pending"].([]any)) != 2 || lines[1]["done"] != false || lines[2]["done"] != true {
		t.Fatalf("lines %v", lines)
	}
	if !strings.Contains(raws[1], `"subs":["ru","sub"]`) {
		t.Fatalf("subs not cut to «Мои языки»: %s", raws[1])
	}
	if p := lines[2]["pending"].([]any); len(p) != 0 || len(lines[2]["releases"].([]any)) != 2 {
		t.Fatalf("final %v", lines[2])
	}
}

// An error before the first line is a JSON error with its status; after
// it, a last line with the error.
func TestReleases_Errors(t *testing.T) {
	e := newEnv(t)
	e.acq.relErr = fmtUserErr()
	if w := do(e.api, "GET", "/api/releases?tmdbId=1&kind=movie&stream=1", adultU, ""); w.Code != 400 || !strings.Contains(w.Header().Get("Content-Type"), "application/json") || !strings.Contains(w.Body.String(), "Неизвестный фильм или сериал") {
		t.Fatalf("before a line: %d %v %s", w.Code, w.Header(), w.Body)
	}
	e.acq.steps = []indexer.Progress{{Pending: []indexer.PendingSource{{SourceID: 1, Name: "A"}}}}
	e.acq.relErr = errors.New("boom")
	w := do(e.api, "GET", "/api/releases?tmdbId=1&kind=movie&stream=1", adultU, "")
	last := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	if w.Code != 200 || len(last) != 2 || !strings.Contains(last[1], `"error":"Что-то пошло не так"`) || !strings.Contains(last[1], `"done":true`) {
		t.Fatalf("after a line: %d %s", w.Code, w.Body)
	}
	// without stream: one JSON answer, cut subtitles
	e.acq.steps, e.acq.relErr = nil, nil
	e.acq.relResp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{rel("a", "ja", "en")}}
	if w := do(e.api, "GET", "/api/releases?tmdbId=1&kind=movie", adultU, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"subs":["en"]`) {
		t.Fatalf("plain: %d %s", w.Code, w.Body)
	}
}

// fmtUserErr is a user error of the service (ErrInvalid: an unknown title).
func fmtUserErr() error {
	_, err := (&acquire.Service{}).TitleRating(context.Background(), "nope", 0)
	if err == nil {
		return acquire.ErrNoRelease
	}
	return err
}

func TestMineSubs(t *testing.T) {
	got := mineSubs([]string{"ru", "uk", "en", "ja", "sub"}, []string{"ru", "en"})
	if want := []string{"ru", "en", "sub"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if got := mineSubs([]string{"ja"}, nil); !reflect.DeepEqual(got, []string{"ja"}) {
		t.Errorf("no settings: %v", got)
	}
}

// An adult adds: the owner is the profile; no file lists, no full-disk
// download, the link checked; the holds go out.
func TestAcquire_AdultAddsAndOwns(t *testing.T) {
	e := newEnv(t)
	w := do(e.api, "POST", "/api/acquire", adultU, `{"tmdbId":603,"kind":"movie","mode":"download","files":[1],"force":true,"releaseLink":"https://t/x"}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	r := e.acq.acquired[0]
	if r.UserID != 5 || r.Files != nil || r.Force || !r.CheckLink || r.ReleaseLink != "https://t/x" {
		t.Fatalf("%+v", r)
	}
	if e.added != 1 || len(e.approved) != 0 {
		t.Fatalf("added %d approved %v", e.added, e.approved)
	}
	if w := do(e.api, "POST", "/api/acquire", adultU, `{"tmdbId":1,"kind":"movie","quality":"480p"}`); w.Code != 400 {
		t.Fatalf("bad quality: %d", w.Code)
	}
}

// An admin's download approves the kids' requests of it, the album's
// title and artist included; not a stream.
func TestAcquire_AdminApprovesRequests(t *testing.T) {
	e := newEnv(t)
	if w := do(e.api, "POST", "/api/acquire", adminU, `{"kind":"music","extId":"mb-1","title":"Группа крови","artist":"Кино","mode":"download","force":true}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !e.acq.acquired[0].Force || e.acq.acquired[0].CheckLink {
		t.Fatalf("admin %+v", e.acq.acquired[0])
	}
	want := addonkit.ApproveRequest{AdminID: 1, Kind: "music", ExtID: "mb-1", Title: "Группа крови", Artist: "Кино"}
	if len(e.approved) != 1 || e.approved[0] != want {
		t.Fatalf("approved %+v", e.approved)
	}
	do(e.api, "POST", "/api/acquire", adminU, `{"tmdbId":1,"kind":"movie","mode":"stream"}`)
	if len(e.approved) != 1 {
		t.Fatalf("a stream approved %+v", e.approved)
	}
}

func TestAcquire_ApproveFailureIgnored(t *testing.T) {
	e := newEnv(t)
	e.approveE = errors.New("core down")
	if w := do(e.api, "POST", "/api/acquire", adminU, `{"tmdbId":603,"kind":"movie","mode":"download"}`); w.Code != 200 || !strings.Contains(w.Body.String(), `"infoHash":"abc"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(e.approved) != 1 {
		t.Fatal("not asked")
	}
}

// The admin's tab gone right after the download: the requests are approved
// all the same, within a deadline of their own.
func TestAcquire_ApproveOutlivesTheRequest(t *testing.T) {
	e := newEnv(t)
	r := httptest.NewRequest("POST", "/api/acquire", strings.NewReader(`{"tmdbId":603,"kind":"movie","mode":"download"}`))
	r.Header.Set(addonkit.UserHeader, adminU)
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	w := httptest.NewRecorder()
	e.api.ServeHTTP(w, r.WithContext(ctx))
	if len(e.approved) != 1 {
		t.Fatalf("%d %s: not asked", w.Code, w.Body)
	}
	if e.approveErr != nil {
		t.Fatalf("approve ran on the closed request's context: %v", e.approveErr)
	}
	if !e.approveDeadline {
		t.Fatal("approve has no deadline")
	}
}

// «Выбрать другую раздачу»: the old unfinished download goes, the new kept.
func TestAcquire_ReplaceCancelsOld(t *testing.T) {
	e := newEnv(t)
	do(e.api, "POST", "/api/acquire", adultU, `{"tmdbId":42,"kind":"series","season":1,"mode":"download","replace":true}`)
	if len(e.acq.cancelled) != 1 || e.acq.cancelled[0] != "series|abc" {
		t.Fatalf("%v", e.acq.cancelled)
	}
}

func TestReleaseFiles_AdminOnly(t *testing.T) {
	e := newEnv(t)
	e.acq.files = acquire.ReleaseFiles{}
	if w := do(e.admin, "POST", "/api/admin/acquire/files", adultU, `{}`); w.Code != 403 {
		t.Fatalf("adult: %d", w.Code)
	}
	if w := do(e.admin, "POST", "/api/admin/acquire/files", adminU, `{}`); w.Code != 200 {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
}

func TestProgressAndCancel(t *testing.T) {
	e := newEnv(t)
	if w := do(e.api, "GET", "/api/acquire/progress?kind=series&tmdbId=42&season=1", adultU, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"active"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := do(e.api, "GET", "/api/acquire/progress?kind=movie&tmdbId=7", adultU, ""); w.Code != 204 {
		t.Fatalf("nothing: %d", w.Code)
	}
	for _, q := range []string{"kind=music&tmdbId=1", "kind=movie&tmdbId=0", "kind=movie&tmdbId=1&season=-1", "kind=movie&tmdbId=x"} {
		if w := do(e.api, "GET", "/api/acquire/progress?"+q, adultU, ""); w.Code != 400 {
			t.Errorf("%s: %d", q, w.Code)
		}
	}
	if w := do(e.api, "POST", "/api/acquire/cancel", adultU, `{"kind":"series","tmdbId":42,"season":1}`); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"cancelled":2}` {
		t.Fatalf("cancel %d %s", w.Code, w.Body)
	}
	if w := do(e.api, "POST", "/api/acquire/cancel", adultU, `{"kind":"book","tmdbId":1}`); w.Code != 400 {
		t.Fatalf("cancel a book: %d", w.Code)
	}
}

func TestPromote(t *testing.T) {
	e := newEnv(t)
	do(e.api, "POST", "/api/torrents/aaa/promote", adultU, `{"force":true}`)
	do(e.api, "POST", "/api/torrents/bbb/promote", adminU, `{"force":true}`)
	if w := do(e.api, "POST", "/api/torrents/ccc/promote", adultU, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"downloading"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if !reflect.DeepEqual(e.acq.promoted, []string{"aaa|", "bbb|force", "ccc|"}) || e.added != 3 {
		t.Fatalf("%v added %d", e.acq.promoted, e.added)
	}
	if w := do(e.api, "POST", "/api/torrents/ddd/promote", kidU, ""); w.Code != 403 {
		t.Fatalf("kid: %d", w.Code)
	}
}

// The seeding figures: admins only; a parked torrent is not seeding.
func TestStats_AdminOnly(t *testing.T) {
	e := newEnv(t)
	e.acq.list = []acquire.TorrentView{
		{Status: torrent.Status{Status: torrent.StatusSeeding, Uploaded: 300, Downloaded: 100}},
		{Status: torrent.Status{Status: torrent.StatusSeeding, Parked: true, Uploaded: 0, Downloaded: 100}},
		{Status: torrent.Status{Status: torrent.StatusDownloading, Downloaded: 200}},
	}
	if w := do(e.admin, "GET", "/api/admin/stats", adultU, ""); w.Code != 403 {
		t.Fatalf("adult: %d", w.Code)
	}
	w := do(e.admin, "GET", "/api/admin/stats", adminU, "")
	var st SeedingStats
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &st) != nil || st != (SeedingStats{Count: 1, UploadedTotal: 300, Ratio: 0.75}) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// The tracker's own words reach admins only; every adult gets the hint
// line in its language instead.
func TestProgress_TrackerTextAdminsOnly(t *testing.T) {
	e := newEnv(t)
	w := do(e.api, "GET", "/api/acquire/progress?kind=movie&tmdbId=43", `{"id":5,"lang":"en"}`, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "Петя") || !strings.Contains(w.Body.String(), `"hint":"No peers for this release yet — waiting…"`) {
		t.Fatalf("adult: %d %s", w.Code, w.Body)
	}
	w = do(e.api, "GET", "/api/acquire/progress?kind=movie&tmdbId=43", adminU, "")
	if !strings.Contains(w.Body.String(), "Петя") || !strings.Contains(w.Body.String(), `"hint":"Нет участников раздачи — ждём…"`) {
		t.Fatalf("admin: %s", w.Body)
	}
}

func TestReleaseFiles_KidRefused(t *testing.T) {
	e := newEnv(t)
	if w := do(e.admin, "POST", "/api/admin/acquire/files", kidU, `{}`); w.Code != 403 {
		t.Fatalf("kid: %d %s", w.Code, w.Body)
	}
}

// The search's cards ask for their rings in one request: by "kind:tmdbId"
// or "kind:tmdbId:season", only the titles with something, the hint in
// the profile's language and the source's own words for admins alone.
func TestTitlesProgress_Batch(t *testing.T) {
	e := newEnv(t)
	w := do(e.api, "GET", "/api/acquire/progress/batch?t=movie:42&t=movie:43&t=series:7:2&t=movie:1", adultU, "")
	var got map[string]map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got) != 2 || e.acq.batches != 1 {
		t.Fatalf("%d %s (%d batches)", w.Code, w.Body, e.acq.batches)
	}
	if got["movie:42"]["status"] != "active" || got["movie:43"]["hint"] == nil || got["movie:43"]["trackerError"] != nil {
		t.Fatalf("%s", w.Body)
	}
	if w := do(e.api, "GET", "/api/acquire/progress/batch?t=movie:43", adminU, ""); !strings.Contains(w.Body.String(), "passkey") {
		t.Fatalf("admin %s", w.Body)
	}
	for _, q := range []string{"t=book:1", "t=movie:x", "t=movie:1:-1", "", strings.Repeat("t=movie:1&", 101)} {
		if w := do(e.api, "GET", "/api/acquire/progress/batch?"+q, adultU, ""); w.Code != 400 {
			t.Errorf("%q: %d", q, w.Code)
		}
	}
	if w := do(e.api, "GET", "/api/acquire/progress/batch?t=movie:42", kidU, ""); w.Code != 403 {
		t.Fatalf("kid %d", w.Code)
	}
}
