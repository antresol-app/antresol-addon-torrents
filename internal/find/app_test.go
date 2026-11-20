package find

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol/addonkit"
)

// appAcq is the find fake with the app protocol's extras: it records the
// release requests and hands a started stream's file back.
type appAcq struct {
	*fakeAcq
	relReqs []acquire.ReleaseRequest
	started acquire.AcquireResult
}

func (a *appAcq) Releases(ctx context.Context, req acquire.ReleaseRequest, progress func(indexer.Progress)) (indexer.SearchResponse, error) {
	a.mu.Lock()
	a.relReqs = append(a.relReqs, req)
	a.mu.Unlock()
	return a.fakeAcq.Releases(ctx, req, progress)
}

func (a *appAcq) Acquire(ctx context.Context, req acquire.AcquireRequest) (acquire.AcquireResult, error) {
	res, err := a.fakeAcq.Acquire(ctx, req)
	if err == nil && a.started.InfoHash != "" {
		res = a.started
	}
	return res, err
}

func (a *appAcq) Discover(ctx context.Context, q string) ([]acquire.Candidate, error) {
	id := int64(77)
	a.mu.Lock()
	a.tmdbLang = tmdb.ContextLanguage(ctx)
	a.mu.Unlock()
	return []acquire.Candidate{
		{TMDBID: 603, Kind: "movie", Title: "Матрица", Year: 1999, Poster: "https://img.example/w342/m.jpg", Overview: "Хакер", InLibrary: true, LibraryItemID: &id},
		{TMDBID: 5, Kind: "series", Title: "Без года"},
		{Kind: "music", ExtID: "x", Title: "Альбом"},
	}, nil
}

// appEnv is the app routes over the fakes.
type appEnv struct {
	acq        *appAcq
	api        http.Handler
	sources    bool
	hasTMDB    bool
	sourcesErr error
	pinned     struct {
		found bool
		err   error
		calls [][3]int
	}
	watched    []string
	watchErr   error
	approved   int
	added      int
	seasonsErr error
}

func newAppEnv(t *testing.T) *appEnv {
	t.Helper()
	e := &appEnv{acq: &appAcq{fakeAcq: &fakeAcq{}}, sources: true, hasTMDB: true}
	d := AppDeps{
		Deps: Deps{Acquire: e.acq,
			Languages: func(_ context.Context, id int64) []string {
				if id == 5 {
					return []string{"ru", "en"}
				}
				return nil
			},
			Approve: func(context.Context, addonkit.ApproveRequest) (int, error) { e.approved++; return 1, nil },
			Added:   func() { e.added++ },
		},
		HasSources:  func(context.Context) (bool, error) { return e.sources, e.sourcesErr },
		HasMetadata: func() bool { return e.hasTMDB },
		Seasons: func(_ context.Context, id int) ([]tmdb.SeasonCount, error) {
			if e.seasonsErr != nil {
				return nil, e.seasonsErr
			}
			if id == 99 {
				return nil, nil
			}
			return []tmdb.SeasonCount{{Number: 1, Episodes: 3}, {Number: 2, Episodes: 1}}, nil
		},
		PlayPinned: func(_ context.Context, _ addonkit.User, kind string, id int64, season, episode int) (string, bool, error) {
			e.pinned.calls = append(e.pinned.calls, [3]int{int(id), season, episode})
			if e.pinned.err != nil {
				return "", e.pinned.found, e.pinned.err
			}
			return "pin-input", e.pinned.found, nil
		},
		Watch: func(_ context.Context, _ addonkit.User, ih string, idx int) (string, error) {
			e.watched = append(e.watched, ih+"/"+string(rune('0'+idx)))
			if e.watchErr != nil {
				return "", e.watchErr
			}
			return "in-1", nil
		},
		WatchError: func(w http.ResponseWriter, r *http.Request, _ string, err error) {
			writeErr(w, r, http.StatusBadGateway, err.Error())
		},
	}
	mux := http.NewServeMux()
	RegisterApp(mux, d)
	e.api = mux
	return e
}

func (e *appEnv) call(method, path, user, body string) *httptest.ResponseRecorder {
	return do(e.api, method, path, user, body)
}

// post sends a JSON body as the asking adult.
func (e *appEnv) post(path, body string) *httptest.ResponseRecorder {
	return e.call("POST", path, adultU, body)
}

func decodeMap(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %s", w.Body)
	}
	return m
}

type stepAnswer struct {
	Title string
	Rows  []struct {
		Ref, Label, Detail string
		Badges             []string
		Current, Final     bool
	}
	Note, Empty string
}

func (e *appEnv) step(t *testing.T, body string) stepAnswer {
	t.Helper()
	w := e.post("/api/app/step", body)
	var s stepAnswer
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &s) != nil {
		t.Fatalf("step %s: %d %s", body, w.Code, w.Body)
	}
	return s
}

func refBody(way, kind string, id int, ref string, extra string) string {
	b, _ := json.Marshal(ref)
	return `{"way":"` + way + `","kind":"` + kind + `","tmdbId":` + itoa(id) + `,"ref":` + string(b) + extra + `}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// --- the description and who may ask ---

func TestApp_Info(t *testing.T) {
	e := newAppEnv(t)
	w := e.call("GET", "/api/app", adultU, "")
	const want = `{"v":1,"ways":[` +
		`{"id":"watch","label":"Смотреть сейчас","icon":"play","role":"watch","kinds":["movie","series"],"scope":"outside","episodes":true},` +
		`{"id":"keep","label":"Скачать","icon":"download","role":"keep","kinds":["movie","series"],"scope":"outside","episodes":false}],` +
		`"search":{"label":"Найти и добавить"}}`
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("ru: %d %s", w.Code, w.Body)
	}
	en := e.call("GET", "/api/app", `{"id":5,"name":"Маша","lang":"en"}`, "")
	var info struct {
		Ways   []struct{ Label string }
		Search struct{ Label string }
	}
	if json.Unmarshal(en.Body.Bytes(), &info) != nil || len(info.Ways) != 2 || info.Ways[0].Label != "Watch Now" ||
		info.Ways[1].Label != "Download" || info.Search.Label != "Find and Add" {
		t.Fatalf("en: %s", en.Body)
	}
}

// No enabled source, or no TMDB: the addon has nothing for the app (404), so
// the app shows no button; state and cancel still serve a download running.
func TestApp_NotOffered(t *testing.T) {
	routes := []struct{ method, path, body string }{
		{"GET", "/api/app", ""},
		{"GET", "/api/app/search?q=x", ""},
		{"POST", "/api/app/step", `{"way":"watch","kind":"movie","tmdbId":1}`},
		{"POST", "/api/app/run", `{"way":"watch","kind":"movie","tmdbId":1,"quick":true}`},
	}
	for name, off := range map[string]func(*appEnv){
		"no sources": func(e *appEnv) { e.sources = false },
		"no TMDB":    func(e *appEnv) { e.hasTMDB = false },
	} {
		e := newAppEnv(t)
		off(e)
		for _, c := range routes {
			if w := e.call(c.method, c.path, adultU, c.body); w.Code != 404 {
				t.Errorf("%s: %s %s: %d %s", name, c.method, c.path, w.Code, w.Body)
			}
		}
		if len(e.acq.acquired) != 0 || len(e.watched) != 0 {
			t.Errorf("%s: something started", name)
		}
		if w := e.call("GET", "/api/app/state?kind=movie&tmdbId=42", adultU, ""); w.Code != 200 {
			t.Errorf("%s: state %d", name, w.Code)
		}
		if w := e.post("/api/app/cancel", `{"way":"keep","kind":"movie","tmdbId":42}`); w.Code != 200 {
			t.Errorf("%s: cancel %d", name, w.Code)
		}
	}
	// a source list that cannot be read is an error, not «nothing to offer»
	e := newAppEnv(t)
	e.sourcesErr = errors.New("db")
	if w := e.call("GET", "/api/app", adultU, ""); w.Code != 500 {
		t.Fatalf("source list error: %d %s", w.Code, w.Body)
	}
}

func TestApp_KidRefused(t *testing.T) {
	e := newAppEnv(t)
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/app", ""},
		{"GET", "/api/app/search?q=x", ""},
		{"POST", "/api/app/step", `{"way":"watch","kind":"movie","tmdbId":1}`},
		{"POST", "/api/app/run", `{"way":"watch","kind":"movie","tmdbId":1,"quick":true}`},
		{"GET", "/api/app/state?kind=movie&tmdbId=42", ""},
		{"POST", "/api/app/cancel", `{"way":"keep","kind":"movie","tmdbId":42}`},
	} {
		for _, u := range []string{kidU, ""} {
			if w := e.call(c.method, c.path, u, c.body); w.Code != 403 {
				t.Errorf("%s %s as %q: %d %s", c.method, c.path, u, w.Code, w.Body)
			}
		}
	}
	if len(e.acq.acquired)+len(e.acq.cancelled)+len(e.watched) != 0 || len(e.pinned.calls) != 0 {
		t.Fatal("a kid reached the services")
	}
}

// --- the search ---

func TestApp_Search(t *testing.T) {
	e := newAppEnv(t)
	w := e.call("GET", "/api/app/search?q=matrix", `{"id":5,"lang":"en"}`, "")
	const want = `{"titles":[` +
		`{"kind":"movie","tmdbId":603,"title":"Матрица","year":1999,"poster":"https://img.example/w342/m.jpg","overview":"Хакер","inLibrary":true,"libraryItemId":77},` +
		`{"kind":"series","tmdbId":5,"title":"Без года","inLibrary":false}]}`
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != want {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	// TMDB is asked in the profile's language
	if e.acq.tmdbLang != "en-US" {
		t.Fatalf("language %q", e.acq.tmdbLang)
	}
}

// --- the steps ---

func TestApp_Step_WatchSeriesChoosesSeasonAndEpisode(t *testing.T) {
	e := newAppEnv(t)
	seasons := e.step(t, `{"way":"watch","kind":"series","tmdbId":10}`)
	if seasons.Title != "Сезон" || len(seasons.Rows) != 2 || seasons.Rows[0].Label != "Сезон 1" || seasons.Rows[0].Detail != "3 серии" ||
		seasons.Rows[1].Detail != "1 серия" || seasons.Rows[0].Final || seasons.Rows[0].Current || seasons.Empty == "" {
		t.Fatalf("seasons: %+v", seasons)
	}
	eps := e.step(t, refBody("watch", "series", 10, seasons.Rows[0].Ref, ""))
	if eps.Title != "Серия" || len(eps.Rows) != 3 || eps.Rows[2].Label != "Серия 3" || eps.Rows[2].Final {
		t.Fatalf("episodes: %+v", eps)
	}
	if len(e.acq.relReqs) != 0 {
		t.Fatal("releases asked before the episode was chosen")
	}
	e.acq.relResp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relL("Show S01E03 1080p", "http://t/s1e3", "ru")}}
	rels := e.step(t, refBody("watch", "series", 10, eps.Rows[2].Ref, ""))
	if rels.Title != "Раздачи" || len(rels.Rows) != 1 || !rels.Rows[0].Final {
		t.Fatalf("releases: %+v", rels)
	}
	got := e.acq.relReqs
	if len(got) != 1 || got[0].Kind != "series" || got[0].TMDBID != 10 || got[0].Season != 1 || got[0].Episode != 3 ||
		strings.Join(got[0].Langs, ",") != "ru,en" {
		t.Fatalf("release request: %+v", got)
	}
}

// A call that names the season and episode (a series' episode menu) goes
// straight to the releases; a season alone asks the episode.
func TestApp_Step_WatchGivenEpisode(t *testing.T) {
	e := newAppEnv(t)
	e.acq.relResp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relL("x", "http://t/x.torrent")}}
	s := e.step(t, `{"way":"watch","kind":"series","tmdbId":10,"season":2,"episode":4}`)
	if s.Title != "Раздачи" || len(e.acq.relReqs) != 1 || e.acq.relReqs[0].Season != 2 || e.acq.relReqs[0].Episode != 4 {
		t.Fatalf("%+v %+v", s, e.acq.relReqs)
	}
	s = e.step(t, `{"way":"watch","kind":"series","tmdbId":10,"season":2}`)
	if s.Title != "Серия" || len(s.Rows) != 1 || s.Rows[0].Label != "Серия 1" {
		t.Fatalf("season only: %+v", s)
	}
	// a film has neither, whatever the call says
	e.step(t, `{"way":"watch","kind":"movie","tmdbId":3,"season":2,"episode":4}`)
	if r := e.acq.relReqs[len(e.acq.relReqs)-1]; r.Kind != "movie" || r.Season != 0 || r.Episode != 0 {
		t.Fatalf("film request: %+v", r)
	}
}

// A release is a row of what it is: quality, codec, size; who has it; the
// audio, the subtitles of «Мои языки».
func TestApp_Step_ReleaseRows(t *testing.T) {
	e := newAppEnv(t)
	good := indexer.ScoredRelease{SourceID: 4, SourceName: "Трекер", Info: release.Info{Resolution: 1080, HDR: true, Codec: "x265",
		Source: "WEB-DL", Audio: []string{"Dub"}, Subs: []string{"ru", "ja"}, Studios: []string{"LostFilm", "Кубик в Кубе", "Третья"}}, Seeders: 24}
	good.Result.Title = "Фильм 2020 WEB-DL 1080p"
	good.Result.Size = 13198428365 // 12,3 GiB
	good.Result.Magnet = "magnet:?xt=urn:btih:aaaa"
	pack := indexer.ScoredRelease{SourceID: 4, SourceName: "Трекер", Info: release.Info{Resolution: 2160, IsSeasonPack: true, Subs: []string{"sub"}}}
	pack.Result.Title = "Фильм 4K пакет"
	pack.Result.Link = "http://t/p.torrent"
	bare := indexer.ScoredRelease{SourceID: 5, SourceName: "Другой"}
	bare.Result.Title = "Совсем без пометок, очень длинное название раздачи, которое надо обрезать, потому что оно не помещается в строку"
	bare.Result.Link = "http://t/b.torrent"
	empty := indexer.ScoredRelease{}
	empty.Result.Title = "no link"
	e.acq.relResp = indexer.SearchResponse{
		Releases: []indexer.ScoredRelease{good, pack, bare, empty},
		Failed:   []indexer.SourceError{{SourceID: 9, Name: "Мёртвый", Message: "x"}, {SourceID: 8, Name: "Тоже"}},
	}
	s := e.step(t, `{"way":"watch","kind":"movie","tmdbId":3}`)
	if len(s.Rows) != 3 {
		t.Fatalf("rows: %+v", s.Rows)
	}
	r := s.Rows[0]
	if r.Label != "1080p · HDR · x265 · 12,3 ГБ" || r.Detail != "LostFilm, Кубик в Кубе · Трекер · Раздают: 24" || !r.Final || r.Current ||
		strings.Join(r.Badges, "|") != "WEB-DL|Dub|Субтитры: RU" {
		t.Fatalf("row 0: %+v", r)
	}
	if s.Rows[1].Label != "4K" || strings.Join(s.Rows[1].Badges, "|") != "весь сезон|Субтитры" {
		t.Fatalf("row 1: %+v", s.Rows[1])
	}
	if !strings.HasSuffix(s.Rows[2].Label, "…") || len([]rune(s.Rows[2].Label)) != 80 || s.Rows[2].Detail != "Другой · Раздают: 0" {
		t.Fatalf("row 2: %+v", s.Rows[2])
	}
	if !strings.Contains(s.Note, "Лучшие — сверху") || !strings.Contains(s.Note, "Не ответили: Мёртвый, Тоже. Остальные источники проверены.") {
		t.Fatalf("note: %q", s.Note)
	}
	// each row's ref leads to a run that starts exactly that release
	w := e.post("/api/app/run", refBody("watch", "movie", 3, s.Rows[1].Ref, ""))
	if w.Code != 200 || len(e.acq.acquired) != 1 || e.acq.acquired[0].ReleaseLink != "http://t/p.torrent" || e.acq.acquired[0].SourceID != 4 {
		t.Fatalf("run of row 1: %d %s %+v", w.Code, w.Body, e.acq.acquired)
	}
}

func TestApp_Step_ReleaseRowsEnglish(t *testing.T) {
	e := newAppEnv(t)
	good := indexer.ScoredRelease{SourceName: "Tracker", Info: release.Info{Resolution: 720, Subs: []string{"en"}}, Seeders: 3}
	good.Result.Size = 1536 << 20
	good.Result.Link = "http://t/a"
	e.acq.relResp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{good}}
	w := e.call("POST", "/api/app/step", `{"id":5,"name":"M","lang":"en"}`, `{"way":"watch","kind":"movie","tmdbId":3}`)
	var s stepAnswer
	if json.Unmarshal(w.Body.Bytes(), &s) != nil || len(s.Rows) != 1 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if s.Title != "Releases" || s.Rows[0].Label != "720p · 1.5 GB" || s.Rows[0].Detail != "Tracker · Sharing: 3" ||
		strings.Join(s.Rows[0].Badges, "|") != "Subtitles: EN" || s.Empty != "No suitable releases found." ||
		!strings.HasPrefix(s.Note, "Best first.") {
		t.Fatalf("%+v", s)
	}
}

func TestApp_Step_NoReleases(t *testing.T) {
	e := newAppEnv(t)
	s := e.step(t, `{"way":"watch","kind":"movie","tmdbId":3}`)
	if len(s.Rows) != 0 || s.Empty != "Подходящих раздач не нашлось." {
		t.Fatalf("%+v", s)
	}
	// the rows are an empty list, never null
	if w := e.post("/api/app/step", `{"way":"watch","kind":"movie","tmdbId":3}`); !strings.Contains(w.Body.String(), `"rows":[]`) {
		t.Fatalf("%s", w.Body)
	}
}

func TestApp_Step_KeepMovie(t *testing.T) {
	e := newAppEnv(t)
	q := e.step(t, `{"way":"keep","kind":"movie","tmdbId":3}`)
	var labels []string
	for _, r := range q.Rows {
		labels = append(labels, r.Label)
		if r.Final || r.Current {
			t.Fatalf("a quality row is a step: %+v", r)
		}
	}
	if q.Title != "Качество" || strings.Join(labels, "|") != "Любое|720p|1080p|4K" || q.Note == "" {
		t.Fatalf("qualities: %+v", q)
	}
	if len(e.acq.relReqs) != 0 {
		t.Fatal("releases asked before the quality")
	}
	e.acq.relResp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relL("r1", "magnet:?xt=urn:btih:bb")}}
	rels := e.step(t, refBody("keep", "movie", 3, q.Rows[2].Ref, "")) // 1080p
	if len(rels.Rows) != 2 || rels.Rows[0].Label != "Лучшая раздача" || rels.Rows[0].Detail != "1080p" || !rels.Rows[0].Final || !rels.Rows[1].Final {
		t.Fatalf("releases: %+v", rels)
	}
	// «Лучшая раздача» downloads the server's pick in that quality: no link
	w := e.post("/api/app/run", refBody("keep", "movie", 3, rels.Rows[0].Ref, ""))
	if w.Code != 200 || len(e.acq.acquired) != 1 {
		t.Fatalf("run: %d %s", w.Code, w.Body)
	}
	if a := e.acq.acquired[0]; a.Mode != "download" || a.ReleaseLink != "" || a.Quality != "1080p" || a.Resolution != "1080p" || a.Kind != "movie" || a.Season != 0 {
		t.Fatalf("best pick: %+v", a)
	}
	// any quality
	any := e.step(t, refBody("keep", "movie", 3, q.Rows[0].Ref, ""))
	if any.Rows[0].Detail != "Любое качество" {
		t.Fatalf("any: %+v", any.Rows[0])
	}
}

func TestApp_Step_KeepSeriesAsksSeasonThenQuality(t *testing.T) {
	e := newAppEnv(t)
	seasons := e.step(t, `{"way":"keep","kind":"series","tmdbId":10}`)
	if len(seasons.Rows) != 2 || seasons.Title != "Сезон" {
		t.Fatalf("seasons: %+v", seasons)
	}
	q := e.step(t, refBody("keep", "series", 10, seasons.Rows[1].Ref, ""))
	if q.Title != "Качество" || len(q.Rows) != 4 {
		t.Fatalf("qualities: %+v", q)
	}
	e.acq.relResp = indexer.SearchResponse{}
	rels := e.step(t, refBody("keep", "series", 10, q.Rows[1].Ref, "")) // 720p of season 2
	if got := e.acq.relReqs; len(got) != 1 || got[0].Season != 2 || got[0].Episode != 0 || got[0].Kind != "series" {
		t.Fatalf("request: %+v", got)
	}
	if len(rels.Rows) != 1 || rels.Rows[0].Label != "Лучшая раздача" || rels.Empty == "" {
		t.Fatalf("releases: %+v", rels)
	}
	// the season of the call skips the season step
	if s := e.step(t, `{"way":"keep","kind":"series","tmdbId":10,"season":1}`); s.Title != "Качество" {
		t.Fatalf("given season: %+v", s)
	}
	// an episode is never downloaded alone
	if s := e.step(t, `{"way":"keep","kind":"series","tmdbId":10,"season":1,"episode":2}`); s.Title != "Качество" {
		t.Fatalf("given episode: %+v", s)
	}
}

func TestApp_Step_Errors(t *testing.T) {
	e := newAppEnv(t)
	for name, c := range map[string]struct {
		body string
		code int
	}{
		"way gone":        {`{"way":"old","kind":"movie","tmdbId":1}`, 404},
		"no way":          {`{"kind":"movie","tmdbId":1}`, 404},
		"bad kind":        {`{"way":"watch","kind":"music","tmdbId":1}`, 400},
		"no id":           {`{"way":"watch","kind":"movie"}`, 400},
		"negative season": {`{"way":"watch","kind":"series","tmdbId":1,"season":-1}`, 400},
		"not JSON":        {`nope`, 400},
		"bad ref":         {`{"way":"watch","kind":"movie","tmdbId":1,"ref":"!!"}`, 400},
		"foreign ref":     {`{"way":"watch","kind":"movie","tmdbId":1,"ref":"e30"}`, 400}, // base64 of {}
		"quality ref":     {`{"way":"watch","kind":"movie","tmdbId":1,"ref":"` + (appRef{K: refQuality, Q: "480p"}).encode() + `"}`, 400},
		"release as step": {`{"way":"watch","kind":"movie","tmdbId":1,"ref":"` + (appRef{K: refRelease, L: "x"}).encode() + `"}`, 400},
		"huge ref":        {`{"way":"watch","kind":"movie","tmdbId":1,"ref":"` + strings.Repeat("A", maxRef+1) + `"}`, 400},
	} {
		w := e.post("/api/app/step", c.body)
		if w.Code != c.code || decodeMap(t, w)["error"] == "" {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if len(e.acq.relReqs) != 0 {
		t.Fatal("an invalid step searched")
	}
	// TMDB failing is the service's error, with its text
	e.seasonsErr = userErr()
	if w := e.post("/api/app/step", `{"way":"watch","kind":"series","tmdbId":10}`); w.Code != 503 || !strings.Contains(w.Body.String(), "TMDB") {
		t.Fatalf("seasons error: %d %s", w.Code, w.Body)
	}
	e.seasonsErr = nil
	e.acq.relErr = userErr()
	if w := e.post("/api/app/step", `{"way":"watch","kind":"movie","tmdbId":3}`); w.Code != 503 {
		t.Fatalf("releases error: %d %s", w.Code, w.Body)
	}
	// a series without TMDB seasons lists none: the app shows `empty`
	e.acq.relErr = nil
	s := e.step(t, `{"way":"watch","kind":"series","tmdbId":99}`)
	if len(s.Rows) != 0 || s.Empty != "Сезонов не нашлось" {
		t.Fatalf("no seasons: %+v", s)
	}
}

// userErr is an error of the acquire service with a message for people
// (TMDB not set up: 503), as the services give them.
func userErr() error {
	_, err := (&acquire.Service{}).Discover(context.Background(), "x")
	return err
}

// relL is a release the app can start (it has a link).
func relL(title, link string, subs ...string) indexer.ScoredRelease {
	r := rel(title, subs...)
	r.Result.Link = link
	return r
}

// --- the run ---

func TestApp_Run_WatchRelease(t *testing.T) {
	e := newAppEnv(t)
	e.acq.started = acquire.AcquireResult{InfoHash: "ABCDEF", FileIndex: 3}
	ref := appRef{K: refRelease, S: 1, E: 3, L: "magnet:?xt=urn:btih:cc", Src: 4}.encode()
	w := e.post("/api/app/run", refBody("watch", "series", 10, ref, ""))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"input":"in-1"}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(e.acq.acquired) != 1 {
		t.Fatalf("acquired: %+v", e.acq.acquired)
	}
	a := e.acq.acquired[0]
	if a.Mode != "stream" || a.Kind != "series" || a.TMDBID != 10 || a.Season != 1 || a.Episode != 3 || a.ReleaseLink != "magnet:?xt=urn:btih:cc" ||
		a.SourceID != 4 || a.UserID != 5 || !a.CheckLink || strings.Join(a.Langs, ",") != "ru,en" {
		t.Fatalf("request: %+v", a)
	}
	if strings.Join(e.watched, ",") != "abcdef/3" {
		t.Fatalf("watched %v", e.watched)
	}
	if len(e.pinned.calls) != 0 {
		t.Fatal("the pinned release was tried although a release was chosen")
	}
	if e.added != 1 {
		t.Fatalf("holds told %d times", e.added)
	}
	// a film's season and episode are dropped, whatever the ref says
	film := appRef{K: refRelease, S: 2, E: 4, L: "http://t/x"}.encode()
	if w := e.post("/api/app/run", refBody("watch", "movie", 3, film, `,"season":2,"episode":4`)); w.Code != 200 {
		t.Fatalf("film: %d %s", w.Code, w.Body)
	}
	if a := e.acq.acquired[1]; a.Season != 0 || a.Episode != 0 || a.Kind != "movie" {
		t.Fatalf("film request: %+v", a)
	}
	// an admin's link is not checked
	if w := e.call("POST", "/api/app/run", adminU, refBody("watch", "movie", 3, film, "")); w.Code != 200 {
		t.Fatalf("admin: %d %s", w.Code, w.Body)
	}
	if last := e.acq.acquired[len(e.acq.acquired)-1]; last.CheckLink || last.UserID != 1 {
		t.Fatalf("admin request: %+v", last)
	}
}

func TestApp_Run_WatchQuick(t *testing.T) {
	e := newAppEnv(t)
	e.acq.started = acquire.AcquireResult{InfoHash: "ab", FileIndex: 0}
	// no pin: the server's pick (no link)
	w := e.post("/api/app/run", `{"way":"watch","kind":"movie","tmdbId":3,"quick":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"input":"in-1"`) {
		t.Fatalf("pick: %d %s", w.Code, w.Body)
	}
	if got := e.pinned.calls; len(got) != 1 || got[0] != [3]int{3, 0, 0} {
		t.Fatalf("pin tried: %v", got)
	}
	if a := e.acq.acquired[0]; a.Mode != "stream" || a.ReleaseLink != "" {
		t.Fatalf("server pick: %+v", a)
	}
	// a pin: its stream, nothing picked
	e.pinned.found = true
	e.acq.acquired = nil
	w = e.post("/api/app/run", `{"way":"watch","kind":"series","tmdbId":10,"season":2,"episode":5,"quick":true}`)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"input":"pin-input"}` || len(e.acq.acquired) != 0 {
		t.Fatalf("pin: %d %s %+v", w.Code, w.Body, e.acq.acquired)
	}
	if got := e.pinned.calls[1]; got != [3]int{10, 2, 5} {
		t.Fatalf("pin of the episode: %v", got)
	}
	// a series without its episode needs a pick; nothing starts
	for _, body := range []string{`{"way":"watch","kind":"series","tmdbId":10,"quick":true}`, `{"way":"watch","kind":"series","tmdbId":10,"season":2,"quick":true}`} {
		before := len(e.pinned.calls)
		if w := e.post("/api/app/run", body); w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"pick":true}` || len(e.pinned.calls) != before {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	// neither a ref nor quick is no request
	if w := e.post("/api/app/run", `{"way":"watch","kind":"movie","tmdbId":3}`); w.Code != 400 {
		t.Fatalf("nothing asked: %d", w.Code)
	}
	// a step's row is not a start
	if w := e.post("/api/app/run", refBody("watch", "series", 10, appRef{K: refSeason, S: 1}.encode(), "")); w.Code != 400 {
		t.Fatalf("season row: %d", w.Code)
	}
}

func TestApp_Run_WatchErrors(t *testing.T) {
	e := newAppEnv(t)
	e.acq.started = acquire.AcquireResult{InfoHash: "ab"}
	ref := appRef{K: refRelease, L: "http://t/x"}.encode()
	// the service's own message, in the profile's language, with its status
	e.acq.acquireErr = userErr()
	if w := e.post("/api/app/run", refBody("watch", "movie", 3, ref, "")); w.Code < 400 || decodeMap(t, w)["error"] == "" {
		t.Fatalf("acquire error: %d %s", w.Code, w.Body)
	}
	e.acq.acquireErr = nil
	// the core did not take the input: the watch's writer answers
	e.watchErr = errors.New("core said no")
	if w := e.post("/api/app/run", refBody("watch", "movie", 3, ref, "")); w.Code != 502 || decodeMap(t, w)["error"] != "core said no" {
		t.Fatalf("watch error: %d %s", w.Code, w.Body)
	}
	// so does a failing pinned start
	e.watchErr = nil
	e.pinned.found, e.pinned.err = true, errors.New("pin broke")
	if w := e.post("/api/app/run", `{"way":"watch","kind":"movie","tmdbId":3,"quick":true}`); w.Code != 502 || decodeMap(t, w)["error"] != "pin broke" {
		t.Fatalf("pin error: %d %s", w.Code, w.Body)
	}
}

func TestApp_Run_Keep(t *testing.T) {
	e := newAppEnv(t)
	ref := appRef{K: refRelease, S: 2, Q: "720p", L: "http://t/x.torrent", Src: 6}.encode()
	w := e.post("/api/app/run", refBody("keep", "series", 10, ref, ""))
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"done":"Загрузка сезона 2 началась"}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	a := e.acq.acquired[0]
	if a.Mode != "download" || a.Kind != "series" || a.Season != 2 || a.Episode != 0 || a.Quality != "720p" || a.Resolution != "720p" ||
		a.ReleaseLink != "http://t/x.torrent" || a.SourceID != 6 || !a.CheckLink {
		t.Fatalf("request: %+v", a)
	}
	if len(e.watched) != 0 {
		t.Fatal("a download opened a player")
	}
	// a film, in English
	e.acq.started = acquire.AcquireResult{InfoHash: "x", Status: "waiting_space"}
	en := e.call("POST", "/api/app/run", `{"id":5,"lang":"en"}`, refBody("keep", "movie", 3, appRef{K: refBest}.encode(), ""))
	if en.Code != 200 || !strings.Contains(en.Body.String(), "Waiting for disk space") {
		t.Fatalf("waiting: %d %s", en.Code, en.Body)
	}
	e.acq.started = acquire.AcquireResult{}
	if w := e.post("/api/app/run", refBody("keep", "movie", 3, appRef{K: refBest}.encode(), "")); strings.TrimSpace(w.Body.String()) != `{"done":"Загрузка началась"}` {
		t.Fatalf("movie: %d %s", w.Code, w.Body)
	}
	// an admin's download approves the kids' requests of the title
	if w := e.call("POST", "/api/app/run", adminU, refBody("keep", "movie", 3, appRef{K: refBest}.encode(), "")); w.Code != 200 || e.approved != 1 {
		t.Fatalf("admin: %d approved %d", w.Code, e.approved)
	}
}

func TestApp_Run_KeepQuick(t *testing.T) {
	e := newAppEnv(t)
	w := e.post("/api/app/run", `{"way":"keep","kind":"movie","tmdbId":3,"quick":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"done"`) || e.acq.acquired[0].Quality != "" || e.acq.acquired[0].ReleaseLink != "" {
		t.Fatalf("%d %s %+v", w.Code, w.Body, e.acq.acquired)
	}
	// a series asks its season first
	if w := e.post("/api/app/run", `{"way":"keep","kind":"series","tmdbId":10,"quick":true}`); strings.TrimSpace(w.Body.String()) != `{"pick":true}` || len(e.acq.acquired) != 1 {
		t.Fatalf("series: %d %s", w.Code, w.Body)
	}
	if w := e.post("/api/app/run", `{"way":"keep","kind":"series","tmdbId":10,"season":3,"quick":true}`); w.Code != 200 || e.acq.acquired[1].Season != 3 {
		t.Fatalf("series season: %d %s", w.Code, w.Body)
	}
	e.acq.acquireErr = userErr()
	if w := e.post("/api/app/run", `{"way":"keep","kind":"movie","tmdbId":3,"quick":true}`); w.Code < 400 {
		t.Fatalf("error: %d %s", w.Code, w.Body)
	}
}

// --- the state ---

func TestApp_State(t *testing.T) {
	e := newAppEnv(t)
	get := func(path string) (int, map[string]any) {
		w := e.call("GET", path, adultU, "")
		return w.Code, decodeMap(t, w)
	}
	// 42: active, half done
	code, m := get("/api/app/state?kind=movie&tmdbId=42")
	keep := m["ways"].(map[string]any)["keep"].(map[string]any)
	if code != 200 || keep["active"] != true || keep["fraction"] != 0.5 || keep["text"] != "Скачано 50%" || keep["cancel"] != true {
		t.Fatalf("active: %d %v", code, m)
	}
	// nothing downloading: inactive, no ring
	_, m = get("/api/app/state?kind=series&tmdbId=1&season=2")
	keep = m["ways"].(map[string]any)["keep"].(map[string]any)
	if keep["active"] != false || keep["fraction"] != nil || keep["cancel"] != nil || len(keep) != 1 {
		t.Fatalf("idle: %v", m)
	}
	// only the download way has a state
	if ways := m["ways"].(map[string]any); len(ways) != 1 {
		t.Fatalf("ways: %v", ways)
	}
	for _, bad := range []string{"/api/app/state", "/api/app/state?kind=music&tmdbId=1", "/api/app/state?kind=movie&tmdbId=x", "/api/app/state?kind=movie&tmdbId=1&season=-1"} {
		if code, _ := get(bad); code != 400 {
			t.Errorf("%s: %d", bad, code)
		}
	}
	en := e.call("GET", "/api/app/state?kind=movie&tmdbId=42", `{"id":5,"lang":"en"}`, "")
	if !strings.Contains(en.Body.String(), `"text":"Downloaded 50%"`) {
		t.Fatalf("en: %s", en.Body)
	}
}

func TestApp_StateKinds(t *testing.T) {
	// paused counts as running (it can be resumed or stopped); done is a ring that is full
	for status, want := range map[string]string{
		"paused": `{"active":true,"fraction":0.25,"text":"Приостановлено","cancel":true}`,
		"done":   `{"active":false,"fraction":1,"text":"Скачано"}`,
	} {
		done := int64(1)
		if status == "done" {
			done = 4
		}
		fake := &progressAcq{appAcq: &appAcq{fakeAcq: &fakeAcq{}}, p: acquire.TitleProgress{Status: status, Done: done, Size: 4}}
		mux := http.NewServeMux()
		RegisterApp(mux, AppDeps{Deps: Deps{Acquire: fake}})
		w := do(mux, "GET", "/api/app/state?kind=movie&tmdbId=1", adultU, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"keep":`+want) {
			t.Errorf("%s: %d %s", status, w.Code, w.Body)
		}
	}
}

// progressAcq answers one fixed progress for every title.
type progressAcq struct {
	*appAcq
	p acquire.TitleProgress
}

func (a *progressAcq) TitleProgress(context.Context, string, int, int) (acquire.TitleProgress, bool) {
	return a.p, true
}

// --- the cancel ---

func TestApp_Cancel(t *testing.T) {
	e := newAppEnv(t)
	w := e.post("/api/app/cancel", `{"way":"keep","kind":"series","tmdbId":10,"season":2}`)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"done":"Загрузка отменена"}` || len(e.acq.cancelled) != 1 {
		t.Fatalf("%d %s %v", w.Code, w.Body, e.acq.cancelled)
	}
	// the watch way has nothing to cancel
	if w := e.post("/api/app/cancel", `{"way":"watch","kind":"movie","tmdbId":3}`); w.Code != 404 || len(e.acq.cancelled) != 1 {
		t.Fatalf("watch: %d %s", w.Code, w.Body)
	}
	if w := e.post("/api/app/cancel", `{"way":"keep","kind":"music","tmdbId":3}`); w.Code != 400 {
		t.Fatalf("kind: %d", w.Code)
	}
}

func TestApp_CancelNothing(t *testing.T) {
	c := &countAcq{appAcq: &appAcq{fakeAcq: &fakeAcq{}}}
	mux := http.NewServeMux()
	RegisterApp(mux, AppDeps{Deps: Deps{Acquire: c}})
	w := do(mux, "POST", "/api/app/cancel", adultU, `{"way":"keep","kind":"movie","tmdbId":3}`)
	if w.Code != 200 || strings.TrimSpace(w.Body.String()) != `{"done":"Нечего отменять"}` {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

// countAcq cancels nothing.
type countAcq struct{ *appAcq }

func (a *countAcq) CancelTitle(context.Context, string, int, int, string) (int, error) { return 0, nil }
