package migrate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

const fakeAPIKey = "ffffffffffffffffffffffffffffffff"

// fakeArr is Sonarr, Radarr or Prowlarr's API over the same rows the
// database fixtures hold: /api/<v>/system/status names the app, every
// route wants X-Api-Key, GETs only.
type fakeArr struct {
	*httptest.Server
	app    string
	routes map[string]any // path (no /api/<v>/) → JSON answer
	mu     sync.Mutex
	paths  []string
}

func newFakeArr(t *testing.T, app string, routes map[string]any) *fakeArr {
	t.Helper()
	f := &fakeArr{app: app, routes: routes}
	ver := "v3"
	if app == "Prowlarr" {
		ver = "v1"
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
		if r.Header.Get("X-Api-Key") != fakeAPIKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/"+ver+"/")
		if p == "system/status" {
			json.NewEncoder(w).Encode(map[string]string{"appName": f.app, "version": "4.0.0"})
			return
		}
		if p == "episode" {
			p += "?seriesId=" + r.URL.Query().Get("seriesId")
		}
		v, ok := f.routes[p]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if raw, ok := v.(string); ok {
			w.Write([]byte(raw))
			return
		}
		json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeArr) remote() *Remote { return &Remote{URL: f.URL + "/", APIKey: fakeAPIKey} }

// writes: the requests that were not GETs (none must be).
func (f *fakeArr) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, p := range f.paths {
		if !strings.HasPrefix(p, "GET ") {
			out = append(out, p)
		}
	}
	return out
}

// apiClients mirrors addDownloadClient: an enabled qBittorrent that
// removes completed downloads (or not), a disabled one that would.
func apiClients(removeCompleted bool) []map[string]any {
	return []map[string]any{
		{"name": "qBittorrent", "implementation": "QBittorrent", "enable": true, "removeCompletedDownloads": removeCompleted},
		{"name": "Old", "implementation": "Transmission", "enable": false, "removeCompletedDownloads": true},
	}
}

// sonarrAPI is Sonarr's API over the rows sonarrDB puts in its database.
func sonarrAPI(t *testing.T, rows []sonarrRow, eps []sonarrEpisode, removeCompleted bool) *fakeArr {
	series := []map[string]any{}
	routes := map[string]any{"downloadclient": apiClients(removeCompleted)}
	for _, r := range rows {
		var images []map[string]any
		json.Unmarshal([]byte(r.Images), &images) // "not json" has none
		if images == nil {
			images = []map[string]any{}
		}
		series = append(series, map[string]any{"id": r.ID, "title": r.Title, "year": r.Year, "tvdbId": r.TvdbID, "tmdbId": r.TmdbID,
			"monitored": r.Monitored, "images": images, "path": "/srv/media/media/series/" + r.Title})
		list := []map[string]any{}
		for _, e := range eps {
			if e.SeriesID == r.ID {
				list = append(list, map[string]any{"seriesId": e.SeriesID, "seasonNumber": e.Season, "episodeNumber": e.Episode,
					"monitored": e.Monitored, "hasFile": e.FileID > 0, "episodeFileId": e.FileID})
			}
		}
		routes["episode?seriesId="+strconv.Itoa(r.ID)] = list
	}
	routes["series"] = series
	return newFakeArr(t, "Sonarr", routes)
}

// radarrAPI is Radarr's API over the rows radarrDB puts in its database.
func radarrAPI(t *testing.T, rows []radarrRow, removeCompleted bool) *fakeArr {
	movies := []map[string]any{}
	for i, r := range rows {
		movies = append(movies, map[string]any{"id": i + 1, "title": r.Title, "year": r.Year, "tmdbId": r.TmdbID, "monitored": r.Monitored,
			"hasFile": r.MovieFileID > 0, "movieFileId": r.MovieFileID,
			"images": []map[string]any{{"coverType": "poster", "url": "/MediaCover/1/poster.jpg", "remoteUrl": "https://image.tmdb.org/t/p/original/" + r.Title + ".jpg"}}})
	}
	return newFakeArr(t, "Radarr", map[string]any{"movie": movies, "downloadclient": apiClients(removeCompleted)})
}

// sameImport runs one importer over the database and over the API (each
// into a fresh Antresol) and requires the same lines and follows.
func sameImport(t *testing.T, run func(e *arrEnv, o ArrOptions) *Report, viaDB, viaAPI func(o *ArrOptions)) (*Report, []followRow) {
	t.Helper()
	a, b := newArrEnv(t), newArrEnv(t)
	oa, ob := a.opts(), b.opts()
	viaDB(&oa)
	viaAPI(&ob)
	ra, rb := run(a, oa), run(b, ob)
	if !reflect.DeepEqual(rb.Lines, ra.Lines) {
		t.Fatalf("over the API\n%+v\nwant (database)\n%+v", rb.Lines, ra.Lines)
	}
	fa, fb := a.follows(), b.follows()
	if !reflect.DeepEqual(fb, fa) {
		t.Fatalf("follows over the API\n%+v\nwant (database)\n%+v", fb, fa)
	}
	return rb, fb
}

func TestArrAPISonarrMatchesDatabase(t *testing.T) {
	var eps []sonarrEpisode
	for i := 1; i <= 10; i++ {
		ep := sonarrEpisode{SeriesID: 1, Season: 1, Episode: i, FileID: i}
		switch i {
		case 3, 7:
			ep.Monitored, ep.FileID = true, 0
		case 5:
			ep.Monitored = true
		case 9:
			ep.FileID = 0
		}
		eps = append(eps, ep)
	}
	eps = append(eps, sonarrEpisode{SeriesID: 1, Season: 0, Episode: 1, Monitored: true}, sonarrEpisode{SeriesID: 2, Season: 2, Episode: 4, Monitored: true})
	rows := []sonarrRow{
		{ID: 1, Title: "Сёгун", Year: 2024, TmdbID: 126308, Monitored: true,
			Images: `[{"coverType":"banner","url":"https://b"},{"coverType":"poster","url":"https://local/poster.jpg","remoteUrl":"https://artworks.thetvdb.com/a.jpg"}]`},
		{ID: 2, Title: "Breaking Bad", Year: 2008, TvdbID: 81189, Monitored: true}, // TVDB only: TMDB finds it
		{ID: 3, Title: "Tiny World", Year: 2020, TvdbID: 384341, TmdbID: 99999, Monitored: false},
		{ID: 4, Title: "Unknown", Year: 2001, TvdbID: 1, Monitored: true}, // TMDB does not know it
		{ID: 5, Title: "C", Year: 2003, TmdbID: 3, Monitored: true, Images: `[{"coverType":"poster","url":"/MediaCover/3/poster.jpg"}]`},
	}
	for _, removeCompleted := range []bool{false, true} {
		tm := &fakeFinder{ids: map[string]int{"81189": 1396}}
		api := sonarrAPI(t, rows, eps, removeCompleted)
		rep, fs := sameImport(t, func(e *arrEnv, o ArrOptions) *Report { return e.sonarr(o, tm) },
			func(o *ArrOptions) { o.SonarrDB = sonarrDB(t, rows, eps, removeCompleted) },
			func(o *ArrOptions) {
				o.SonarrDB, o.SonarrAPI = filepath.Join(t.TempDir(), "not-mounted.db"), api.remote()
			})
		if len(fs) != 3 || rep.Tally()[StatusFailed] != 1 {
			t.Fatalf("follows %+v lines %+v", fs, rep.Lines)
		}
		if w := rep.Tally()[StatusWarning]; (w == 1) != removeCompleted {
			t.Fatalf("removeCompleted=%v: lines %+v", removeCompleted, rep.Lines)
		}
		if got := api.writes(); len(got) != 0 {
			t.Fatalf("wrote to Sonarr: %v", got)
		}
	}
}

func TestArrAPIRadarrMatchesDatabase(t *testing.T) {
	rows := []radarrRow{
		{Title: "Interstellar", Year: 2014, TmdbID: 157336, Monitored: true, MovieFileID: 4},
		{Title: "Dune", Year: 2021, TmdbID: 438631, Monitored: true},
		{Title: "Unwanted", Year: 2020, TmdbID: 5, Monitored: false},
		{Title: "NoID", Year: 2020, Monitored: true},
	}
	for _, removeCompleted := range []bool{false, true} {
		api := radarrAPI(t, rows, removeCompleted)
		rep, fs := sameImport(t, func(e *arrEnv, o ArrOptions) *Report { return e.radarr(o, nil) },
			func(o *ArrOptions) { o.RadarrDB = radarrDB(t, rows, removeCompleted) },
			func(o *ArrOptions) { o.RadarrAPI = api.remote() })
		if len(fs) != 1 || fs[0].TmdbID != 438631 || fs[0].Poster != "https://image.tmdb.org/t/p/original/Dune.jpg" {
			t.Fatalf("follows %+v lines %+v", fs, rep.Lines)
		}
	}
}

// A dry run over the API writes nothing, as over the database.
func TestArrAPIDryRun(t *testing.T) {
	rows := []radarrRow{{Title: "Dune", Year: 2021, TmdbID: 438631, Monitored: true}}
	api := radarrAPI(t, rows, false)
	e := newArrEnv(t)
	o := e.opts()
	o.RadarrAPI, o.DryRun = api.remote(), true
	rep := e.radarr(o, nil)
	if l := onlyLine(t, rep); l.Status != StatusPlanned || l.Ref != "movie:438631" {
		t.Fatalf("line %+v", l)
	}
	if fs := e.follows(); len(fs) != 0 {
		t.Fatalf("follows %+v", fs)
	}
}

func TestArrAPIWrongKey(t *testing.T) {
	api := sonarrAPI(t, []sonarrRow{{ID: 1, Title: "A", Year: 2001, TmdbID: 1, Monitored: true}}, nil, false)
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrAPI = &Remote{URL: api.URL, APIKey: "wrong"}
	rep := e.sonarr(o, nil)
	var failed []Line
	for _, l := range rep.Lines {
		if l.Status == StatusFailed {
			failed = append(failed, l)
		}
	}
	if len(failed) != 1 || failed[0].Name != "Sonarr" || !strings.Contains(failed[0].Reason, "Sonarr не принял API-ключ") {
		t.Fatalf("lines %+v", rep.Lines)
	}
	if fs := e.follows(); len(fs) != 0 {
		t.Fatalf("follows %+v", fs)
	}
	// the key goes in the header, never in the address or a line
	for _, p := range api.paths {
		if strings.Contains(p, "wrong") {
			t.Fatalf("the key is in %q", p)
		}
	}
	for _, l := range rep.Lines {
		if strings.Contains(l.Reason, "wrong") {
			t.Fatalf("the key is in %+v", l)
		}
	}
}

// An answer without removeCompletedDownloads (an older version): the
// warning that it could not be checked, as for a database without the
// column.
func TestArrAPIRemoveCompletedUnknown(t *testing.T) {
	api := newFakeArr(t, "Radarr", map[string]any{"movie": `[]`, "downloadclient": `[{"name":"qBittorrent","enable":true}]`})
	e := newArrEnv(t)
	o := e.opts()
	o.RadarrAPI = api.remote()
	rep := e.radarr(o, nil)
	if l := onlyLine(t, rep); l.Status != StatusWarning || !strings.Contains(l.Reason, "не удалось проверить, удаляет ли он завершённые загрузки") {
		t.Fatalf("line %+v", l)
	}
}

func TestArrAPICheck(t *testing.T) {
	ctx := context.Background()
	sonarr := sonarrAPI(t, nil, nil, false)
	radarr := radarrAPI(t, nil, false)
	cases := []struct {
		app  string
		r    Remote
		want string // "" = fine
	}{
		{"Sonarr", Remote{URL: sonarr.URL, APIKey: fakeAPIKey}, ""},
		{"Sonarr", Remote{URL: sonarr.URL + "/", APIKey: "bad"}, "Sonarr не принял API-ключ — скопируйте его в Sonarr: «Settings → General → API Key»"},
		{"Sonarr", Remote{URL: radarr.URL, APIKey: fakeAPIKey}, "отвечает Radarr, а не Sonarr"},
		{"Sonarr", Remote{URL: "", APIKey: fakeAPIKey}, "Укажите адрес Sonarr"},
		{"Sonarr", Remote{URL: "sonarr:8989", APIKey: fakeAPIKey}, "Адрес Sonarr должен начинаться с http:// или https://"},
		{"Sonarr", Remote{URL: sonarr.URL}, "Укажите API-ключ Sonarr"},
		{"Sonarr", Remote{URL: "http://127.0.0.1:1", APIKey: fakeAPIKey}, "Sonarr не отвечает по адресу http://127.0.0.1:1"},
		{"Prowlarr", Remote{URL: sonarr.URL, APIKey: fakeAPIKey}, "По адресу " + sonarr.URL + " нет API Prowlarr — проверьте адрес"},
	}
	for _, c := range cases {
		err := newArrClient(c.app, c.r, nil).check(ctx)
		if c.want == "" {
			if err != nil {
				t.Errorf("%s %+v: %v", c.app, c.r, err)
			}
			continue
		}
		re, ok := err.(*RemoteError)
		if !ok || !strings.Contains(re.Msg, c.want) {
			t.Errorf("%s %s: %v, want %q", c.app, c.r.URL, err, c.want)
		}
	}
}

// Prowlarr over its API, with the real Manager: the indexers become
// sources as from the database; one whose password Prowlarr hid gets a
// warning to type it in.
func TestArrAPIProwlarr(t *testing.T) {
	const indexers = `[{"id":1,"name":"RuTor","implementation":"Cardigann","enable":true,"priority":25,"tags":[1],"fields":[{"name":"definitionFile","value":"rutor"}]},
	  {"id":2,"name":"Kinozal","implementation":"Cardigann","enable":true,"priority":25,"tags":[],"fields":[
	    {"name":"definitionFile","value":"kinozal"},{"name":"username","value":"kz"},{"name":"password","value":"********"}]}]`
	api := newFakeArr(t, "Prowlarr", map[string]any{"indexer": indexers, "tag": `[{"id":1,"label":"flaresolverr"}]`})
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := indexer.NewManager(d, indexer.NewCatalog(filepath.Join(dir, "definitions")), indexer.Options{})
	for _, dry := range []bool{true, false} {
		rep := &Report{}
		o := ArrOptions{ProwlarrDB: filepath.Join(dir, "no", "prowlarr.db"), ProwlarrAPI: api.remote(), DryRun: dry}
		if err := ImportProwlarr(context.Background(), o, m, rep); err != nil {
			t.Fatal(err)
		}
		want := StatusImported
		if dry {
			want = StatusPlanned
		}
		if l := lineByName(t, rep, "RuTor"); l.Status != want {
			t.Fatalf("dry=%v: lines %+v", dry, rep.Lines)
		}
		var warns []string
		for _, l := range rep.Lines {
			if l.Status == StatusWarning {
				warns = append(warns, l.Reason)
			}
		}
		if len(warns) != 2 || !strings.Contains(strings.Join(warns, "\n"), "Kinozal: Prowlarr не отдаёт пароли по API (password)") ||
			!strings.Contains(strings.Join(warns, "\n"), "RuTor: нужен FlareSolverr") {
			t.Fatalf("dry=%v: warnings %q", dry, warns)
		}
	}
	srcs, err := m.Sources(context.Background())
	if err != nil || len(srcs) != 2 {
		t.Fatalf("sources %+v %v", srcs, err)
	}

	// a wrong key: one failed line, nothing added
	rep := &Report{}
	o := ArrOptions{ProwlarrAPI: &Remote{URL: api.URL, APIKey: "bad"}}
	if err := ImportProwlarr(context.Background(), o, m, rep); err != nil {
		t.Fatal(err)
	}
	if l := onlyLine(t, rep); l.Status != StatusFailed || !strings.Contains(l.Reason, "Prowlarr не принял API-ключ") {
		t.Fatalf("line %+v", l)
	}
}

var _ ProwlarrListImporter = (*indexer.Manager)(nil)
