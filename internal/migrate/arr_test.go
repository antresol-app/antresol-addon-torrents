package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

var arrNow = time.Date(2026, 10, 25, 19, 30, 0, 0, time.UTC)

// coreAdmin is the core's id of the admin running the migration (as
// X-Antresol-User gives it): a bare id, the addon's follows keep it with
// no foreign key and no profile behind it here.
const coreAdmin int64 = 7

type arrEnv struct {
	t     *testing.T
	db    *sql.DB
	admin int64
	tmp   string
}

func newArrEnv(t *testing.T) *arrEnv {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	// the admin is a core profile (coretest's id): the addon's follows
	// keep the id with no foreign key
	return &arrEnv{t: t, db: d, admin: coreAdmin, tmp: filepath.Join(dir, "tmp")}
}

func (e *arrEnv) opts() ArrOptions {
	return ArrOptions{UserID: e.admin, UserUID: "uid-admin", Tmp: e.tmp, Now: func() time.Time { return arrNow }}
}

type followRow struct {
	Kind, Title, Poster, Source, State string
	TmdbID                             int
	Active                             bool
	UserID                             sql.NullInt64
}

func (e *arrEnv) follows() []followRow {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT kind, tmdb_id, title, poster, source, state_json, active, user_id FROM follows ORDER BY id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []followRow
	for rows.Next() {
		var f followRow
		if err := rows.Scan(&f.Kind, &f.TmdbID, &f.Title, &f.Poster, &f.Source, &f.State, &f.Active, &f.UserID); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

type fakeFinder struct {
	mu    sync.Mutex
	calls [][3]string
	ids   map[string]int // extID → tmdb id
}

func (f *fakeFinder) Find(_ context.Context, kind, source, extID string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [3]string{kind, source, extID})
	if id, ok := f.ids[extID]; ok {
		return id, nil
	}
	return 0, tmdb.ErrNotFound
}

func lineByName(t *testing.T, rep *Report, name string) Line {
	t.Helper()
	for _, l := range rep.Lines {
		if l.Name == name {
			return l
		}
	}
	t.Fatalf("no line %q in %+v", name, rep.Lines)
	return Line{}
}

func (e *arrEnv) sonarr(o ArrOptions, tm TMDBFinder) *Report {
	e.t.Helper()
	rep := &Report{}
	if err := ImportSonarr(context.Background(), o, e.db, tm, rep); err != nil {
		e.t.Fatal(err)
	}
	return rep
}

func (e *arrEnv) radarr(o ArrOptions, tm TMDBFinder) *Report {
	e.t.Helper()
	rep := &Report{}
	if err := ImportRadarr(context.Background(), o, e.db, tm, rep); err != nil {
		e.t.Fatal(err)
	}
	return rep
}

func threeSeries(t *testing.T, removeCompleted bool) string {
	return sonarrDB(t, []sonarrRow{
		{ID: 1, Title: "Planet Earth II", Year: 2016, TvdbID: 318408, TmdbID: 68595, Monitored: true},
		{ID: 2, Title: "Africa", Year: 2013, TvdbID: 265752, TmdbID: 46597, Monitored: true},
		{ID: 3, Title: "Tiny World", Year: 2020, TvdbID: 384341, TmdbID: 99999, Monitored: false},
	}, []sonarrEpisode{{SeriesID: 1, Season: 1, Episode: 1, Monitored: true}}, removeCompleted)
}

func TestArrSonarrFollowsInactive(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrDB = threeSeries(t, false)
	rep := e.sonarr(o, nil)
	if left, _ := os.ReadDir(e.tmp); len(left) != 0 {
		t.Fatalf("snapshot left in Tmp: %v", left)
	}
	fs := e.follows()
	if len(fs) != 2 {
		t.Fatalf("follows = %+v", fs)
	}
	for _, f := range fs {
		if f.Kind != "series" || f.Active || f.Source != "sonarr" || f.UserID.Int64 != e.admin {
			t.Fatalf("follow = %+v", f)
		}
		if f.TmdbID == 99999 {
			t.Fatal("unmonitored series imported")
		}
	}
	l := lineByName(t, rep, "Planet Earth II (2016)")
	if l.Status != StatusImported || l.Kind != "follow" || l.Source != SourceSonarr || l.Ref != "series:68595" ||
		!strings.Contains(l.Reason, "Включится кнопкой «Перейти на Antresol»") || !strings.Contains(l.Reason, "отслеживается 1 серия") {
		t.Fatalf("line = %+v", l)
	}
	for _, l := range rep.Lines {
		if strings.Contains(l.Name, "Tiny World") {
			t.Fatalf("unmonitored series reported: %+v", l)
		}
	}
}

func TestArrSonarrStoresWanted(t *testing.T) {
	e := newArrEnv(t)
	var eps []sonarrEpisode
	for i := 1; i <= 10; i++ {
		ep := sonarrEpisode{SeriesID: 1, Season: 1, Episode: i, FileID: i} // has a file
		switch i {
		case 3, 7:
			ep.Monitored, ep.FileID = true, 0 // wanted
		case 5:
			ep.Monitored = true // monitored, has a file
		case 9:
			ep.FileID = 0 // missing, not monitored
		}
		eps = append(eps, ep)
	}
	eps = append(eps, sonarrEpisode{SeriesID: 1, Season: 0, Episode: 1, Monitored: true}) // special
	o := e.opts()
	o.SonarrDB = sonarrDB(t, []sonarrRow{{ID: 1, Title: "Сёгун", Year: 2024, TmdbID: 126308, Monitored: true}}, eps, false)
	rep := e.sonarr(o, nil)
	fs := e.follows()
	if len(fs) != 1 {
		t.Fatalf("follows = %+v", fs)
	}
	var st struct {
		Import struct {
			Baseline time.Time `json:"importBaseline"`
			Want     [][2]int  `json:"want"`
		} `json:"import"`
	}
	if err := json.Unmarshal([]byte(fs[0].State), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Import.Baseline.Equal(arrNow) || !reflect.DeepEqual(st.Import.Want, [][2]int{{1, 3}, {1, 7}}) {
		t.Fatalf("state_json = %s", fs[0].State)
	}
	if l := rep.Lines[0]; !strings.Contains(l.Reason, "отслеживается 2 серии") {
		t.Fatalf("reason = %q", l.Reason)
	}
}

func TestArrTvdbFallback(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrDB = sonarrDB(t, []sonarrRow{{ID: 1, Title: "Breaking Bad", Year: 2008, TvdbID: 81189, Monitored: true}}, nil, false)
	tm := &fakeFinder{ids: map[string]int{"81189": 1396}}
	rep := e.sonarr(o, tm)
	if want := [][3]string{{"tv", "tvdb_id", "81189"}}; !reflect.DeepEqual(tm.calls, want) {
		t.Fatalf("Find calls %v", tm.calls)
	}
	fs := e.follows()
	if len(fs) != 1 || fs[0].TmdbID != 1396 {
		t.Fatalf("follows = %+v", fs)
	}
	if l := rep.Lines[0]; l.Ref != "series:1396" || l.Status != StatusImported {
		t.Fatalf("line = %+v", l)
	}
}

func TestArrNoTMDB(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrDB = sonarrDB(t, []sonarrRow{{ID: 1, Title: "Breaking Bad", Year: 2008, TvdbID: 81189, Monitored: true}}, nil, false)
	for _, tm := range []TMDBFinder{nil, &fakeFinder{}} {
		rep := e.sonarr(o, tm)
		l := rep.Lines[0]
		if l.Status != StatusFailed || !strings.Contains(l.Reason, "TMDB") {
			t.Fatalf("line = %+v", l)
		}
	}
	if fs := e.follows(); len(fs) != 0 {
		t.Fatalf("follows = %+v", fs)
	}
}

func TestArrPosterFromImages(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrDB = sonarrDB(t, []sonarrRow{
		{ID: 1, Title: "A", Year: 2001, TmdbID: 1, Monitored: true,
			Images: `[{"coverType":"banner","url":"https://b","remoteUrl":"https://b-remote"},{"coverType":"poster","url":"https://local/poster.jpg","remoteUrl":"https://artworks.thetvdb.com/a.jpg"}]`},
		{ID: 2, Title: "B", Year: 2002, TmdbID: 2, Monitored: true, Images: `[{"coverType":"poster","url":"https://artworks.thetvdb.com/b.jpg"}]`},
		{ID: 3, Title: "C", Year: 2003, TmdbID: 3, Monitored: true, Images: `[{"coverType":"poster","url":"/MediaCover/3/poster.jpg"}]`},
		{ID: 4, Title: "D", Year: 2004, TmdbID: 4, Monitored: true, Images: `not json`},
	}, nil, false)
	e.sonarr(o, nil)
	got := map[int]string{}
	for _, f := range e.follows() {
		got[f.TmdbID] = f.Poster
	}
	want := map[int]string{1: "https://artworks.thetvdb.com/a.jpg", 2: "https://artworks.thetvdb.com/b.jpg", 3: "", 4: ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("posters %v, want %v", got, want)
	}
}

func TestArrRadarrMissingOnly(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.RadarrDB = radarrDB(t, []radarrRow{
		{Title: "Interstellar", Year: 2014, TmdbID: 157336, Monitored: true, MovieFileID: 4},
		{Title: "Dune", Year: 2021, TmdbID: 438631, Monitored: true},
		{Title: "Unwanted", Year: 2020, TmdbID: 5, Monitored: false},
	}, false)
	rep := e.radarr(o, nil)
	if l := lineByName(t, rep, "Interstellar (2014)"); l.Status != StatusSkipped || l.Reason != "Уже в медиатеке" {
		t.Fatalf("with file = %+v", l)
	}
	l := lineByName(t, rep, "Dune (2021)")
	if l.Status != StatusImported || l.Source != SourceRadarr || l.Ref != "movie:438631" || !strings.Contains(l.Reason, "Перейти на Antresol") {
		t.Fatalf("missing = %+v", l)
	}
	fs := e.follows()
	if len(fs) != 1 || fs[0].Kind != "movie" || fs[0].TmdbID != 438631 || fs[0].Active || fs[0].Source != "radarr" ||
		fs[0].Title != "Dune" || fs[0].Poster != "https://image.tmdb.org/t/p/original/Dune.jpg" || fs[0].UserID.Int64 != e.admin {
		t.Fatalf("follows = %+v", fs)
	}
	if strings.Contains(fs[0].State, "import") {
		t.Fatalf("movie follow has import state: %s", fs[0].State)
	}
	if len(rep.Lines) != 2 {
		t.Fatalf("lines = %+v", rep.Lines)
	}
}

func TestArrRemoveCompletedWarning(t *testing.T) {
	for _, on := range []bool{true, false} {
		e := newArrEnv(t)
		o := e.opts()
		o.SonarrDB = threeSeries(t, on)
		o.RadarrDB = radarrDB(t, nil, on)
		for name, rep := range map[string]*Report{"Sonarr": e.sonarr(o, nil), "Radarr": e.radarr(o, nil)} {
			var warns []Line
			for _, l := range rep.Lines {
				if l.Status == StatusWarning {
					warns = append(warns, l)
				}
			}
			if !on {
				if len(warns) != 0 {
					t.Fatalf("%s: warnings %+v", name, warns)
				}
				continue
			}
			if len(warns) != 1 || !strings.HasPrefix(warns[0].Reason, name+" удаляет завершённые загрузки (qBittorrent)") ||
				!strings.Contains(warns[0].Reason, "torrents/") {
				t.Fatalf("%s: warnings %+v", name, warns)
			}
		}
	}
}

// A Sonarr without a DownloadClients table has no clients: no warning.
func TestArrRemoveCompletedNoTable(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrDB = threeSeries(t, true)
	d, err := sql.Open("sqlite", "file:"+o.SonarrDB)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, d, `DROP TABLE DownloadClients`)
	d.Close()
	rep := e.sonarr(o, nil)
	if c := rep.Tally(); c[StatusWarning] != 0 || c[StatusFailed] != 0 || c[StatusImported] != 2 {
		t.Fatalf("counts %v: %+v", c, rep.Lines)
	}
}

// A clients table that cannot be checked (no RemoveCompletedDownloads
// column, a read error) is a warning: the advice never vanishes silently.
func TestArrRemoveCompletedUnknown(t *testing.T) {
	for name, table := range map[string]string{
		"no column":  `CREATE TABLE "DownloadClients" ("Id" INTEGER PRIMARY KEY, "Enable" INTEGER NOT NULL, "Name" TEXT)`,
		"read error": `CREATE TABLE "DownloadClients" ("Id" INTEGER PRIMARY KEY, "Enable" INTEGER NOT NULL, "RemoveCompletedDownloads" INTEGER, "Name" TEXT)`,
	} {
		e := newArrEnv(t)
		o := e.opts()
		o.SonarrDB = threeSeries(t, false)
		d, err := sql.Open("sqlite", "file:"+o.SonarrDB)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, d, `DROP TABLE DownloadClients`)
		mustExec(t, d, table)
		mustExec(t, d, `INSERT INTO DownloadClients (Id, Enable) VALUES (1, 1)`)
		if name == "read error" { // Name overflows when read
			mustExec(t, d, `UPDATE DownloadClients SET RemoveCompletedDownloads=1`)
			mustExec(t, d, `ALTER TABLE DownloadClients RENAME COLUMN Name TO NameRaw`)
			mustExec(t, d, `ALTER TABLE DownloadClients ADD COLUMN Name TEXT GENERATED ALWAYS AS (abs(-9223372036854775807 - 1)) VIRTUAL`)
		}
		d.Close()
		rep := e.sonarr(o, nil)
		var warns []Line
		for _, l := range rep.Lines {
			if l.Status == StatusWarning {
				warns = append(warns, l)
			}
		}
		if len(warns) != 1 || warns[0].Reason != "Sonarr: не удалось проверить, удаляет ли он завершённые загрузки — проверьте в настройках клиента загрузки до переноса раздач" {
			t.Fatalf("%s: warnings %+v", name, warns)
		}
		if c := rep.Tally(); c[StatusImported] != 2 {
			t.Fatalf("%s: counts %v", name, c)
		}
	}
}

// Columns older migrations left nullable do not break the import.
func TestArrNullColumns(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrDB = sonarrDB(t, []sonarrRow{{ID: 1, Title: "A", Year: 2001, TmdbID: 1, Monitored: true}}, []sonarrEpisode{
		{SeriesID: 1, Season: 1, Episode: 1, Monitored: true}, {SeriesID: 1, Season: 1, Episode: 2, Monitored: true}}, false)
	o.RadarrDB = radarrDB(t, []radarrRow{{Title: "Dune", Year: 2021, TmdbID: 438631, Monitored: true}}, false)
	for _, q := range []struct{ db, sql string }{
		{o.SonarrDB, `UPDATE Episodes SET EpisodeFileId=NULL`},
		{o.SonarrDB, `INSERT INTO Series (Id, TvdbId, Title, Monitored, Year, TmdbId) VALUES (2, 0, 'B', 1, NULL, NULL)`},
		{o.RadarrDB, `UPDATE Movies SET MovieFileId=NULL`},
		{o.RadarrDB, `UPDATE MovieMetadata SET Year=NULL`},
	} {
		d, err := sql.Open("sqlite", "file:"+q.db)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, d, q.sql)
		d.Close()
	}
	rep := e.sonarr(o, nil)
	if l := lineByName(t, rep, "A (2001)"); l.Status != StatusImported || !strings.Contains(l.Reason, "отслеживается 2 серии") {
		t.Fatalf("A = %+v", l)
	}
	if l := lineByName(t, rep, "B"); l.Status != StatusFailed || l.Reason != reasonNoTMDB {
		t.Fatalf("B = %+v", l)
	}
	rep = e.radarr(o, nil)
	if l := lineByName(t, rep, "Dune"); l.Status != StatusImported {
		t.Fatalf("Dune = %+v", l)
	}
}

// A movie Radarr holds without a TMDB id says so (no TMDB call is made).
func TestArrRadarrNoTmdbID(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.RadarrDB = radarrDB(t, []radarrRow{{Title: "Odd", Year: 2001, Monitored: true}}, false)
	rep := e.radarr(o, nil)
	if l := rep.Lines[0]; l.Status != StatusFailed || l.Reason != "В Radarr у фильма нет TMDB id" {
		t.Fatalf("line = %+v", l)
	}
}

// One series whose episodes cannot be read fails alone.
func TestArrEpisodesReadFailsOneSeries(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrDB = threeSeries(t, false)
	d, err := sql.Open("sqlite", "file:"+o.SonarrDB)
	if err != nil {
		t.Fatal(err)
	}
	// series 1's episode number overflows when read
	mustExec(t, d, `ALTER TABLE Episodes RENAME COLUMN EpisodeNumber TO EpisodeNumberRaw`)
	mustExec(t, d, `ALTER TABLE Episodes ADD COLUMN EpisodeNumber INTEGER GENERATED ALWAYS AS (CASE WHEN SeriesId=1 THEN abs(-9223372036854775807 - 1) ELSE EpisodeNumberRaw END) VIRTUAL`)
	d.Close()
	rep := e.sonarr(o, nil)
	if l := lineByName(t, rep, "Planet Earth II (2016)"); l.Status != StatusFailed || !strings.Contains(l.Reason, "Sonarr") {
		t.Fatalf("broken series = %+v", l)
	}
	if l := lineByName(t, rep, "Africa (2013)"); l.Status != StatusImported {
		t.Fatalf("next series = %+v", l)
	}
}

func TestArrIdempotent(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrDB = threeSeries(t, false)
	o.RadarrDB = radarrDB(t, []radarrRow{{Title: "Dune", Year: 2021, TmdbID: 438631, Monitored: true}}, false)
	e.sonarr(o, nil)
	e.radarr(o, nil)
	n := len(e.follows())
	for _, rep := range []*Report{e.sonarr(o, nil), e.radarr(o, nil)} {
		for _, l := range rep.Lines {
			if l.Status != StatusSkipped || l.Reason != "Уже отслеживается" {
				t.Fatalf("second run line %+v", l)
			}
		}
	}
	if len(e.follows()) != n || n != 3 {
		t.Fatalf("follows %d → %d", n, len(e.follows()))
	}
}

func TestArrDryRunNoWrites(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.DryRun = true
	o.SonarrDB = threeSeries(t, true)
	o.RadarrDB = radarrDB(t, []radarrRow{{Title: "Dune", Year: 2021, TmdbID: 438631, Monitored: true}}, false)
	before := [2]string{treeHash(t, filepath.Dir(o.SonarrDB)), treeHash(t, filepath.Dir(o.RadarrDB))}
	reps := []*Report{e.sonarr(o, nil), e.radarr(o, nil)}
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM follows`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("follows = %d, %v", n, err)
	}
	planned := 0
	for _, rep := range reps {
		for _, l := range rep.Lines {
			switch l.Status {
			case StatusPlanned:
				planned++
			case StatusWarning: // remove-completed advice is given in the dry run too
			default:
				t.Fatalf("dry-run line %+v", l)
			}
		}
	}
	if planned != 3 {
		t.Fatalf("planned = %d", planned)
	}
	if after := [2]string{treeHash(t, filepath.Dir(o.SonarrDB)), treeHash(t, filepath.Dir(o.RadarrDB))}; after != before {
		t.Fatal("source tree changed")
	}
	// a title already followed is «Уже отслеживается» in the dry run too
	o.DryRun = false
	e.sonarr(o, nil)
	o.DryRun = true
	for _, l := range e.sonarr(o, nil).Lines {
		if l.Status != StatusWarning && l.Reason != "Уже отслеживается" {
			t.Fatalf("dry run after import: %+v", l)
		}
	}
}

// Sonarr is running: the newest rows are only in the -wal.
func TestArrReadsLiveWAL(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrDB = sonarrDB(t, []sonarrRow{{ID: 1, Title: "Old", Year: 2000, TmdbID: 1, Monitored: true}}, nil, false)
	w, err := sql.Open("sqlite", "file:"+o.SonarrDB+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	mustExec(t, w, `INSERT INTO Series (Id, TvdbId, Title, Images, Monitored, Year, TmdbId) VALUES (2, 0, 'Fresh', '[]', 1, 2026, 2)`)
	if st, err := os.Stat(o.SonarrDB + "-wal"); err != nil || st.Size() == 0 {
		t.Fatalf("the row is not in the -wal: %v", err)
	}
	before := treeHash(t, filepath.Dir(o.SonarrDB))
	rep := e.sonarr(o, nil)
	if l := lineByName(t, rep, "Fresh (2026)"); l.Status != StatusImported {
		t.Fatalf("line = %+v", l)
	}
	if treeHash(t, filepath.Dir(o.SonarrDB)) != before {
		t.Fatal("source tree changed")
	}
}

func TestArrMissingDB(t *testing.T) {
	e := newArrEnv(t)
	o := e.opts()
	o.SonarrDB = filepath.Join(t.TempDir(), "nope", "sonarr.db")
	rep := e.sonarr(o, nil)
	if len(rep.Lines) != 1 || rep.Lines[0].Status != StatusFailed || !strings.Contains(rep.Lines[0].Reason, "Sonarr") {
		t.Fatalf("lines = %+v", rep.Lines)
	}
}

// --- Prowlarr ---

type fakeProwlarr struct {
	preview, imported int
	tmp               string
	list              []indexer.Imported
	err               error
}

func (f *fakeProwlarr) PreviewProwlarrIn(_ context.Context, _, tmp string) ([]indexer.Imported, error) {
	f.preview++
	f.tmp = tmp
	return f.list, f.err
}

func (f *fakeProwlarr) ImportProwlarrIn(_ context.Context, _, tmp string) ([]indexer.Imported, error) {
	f.imported++
	f.tmp = tmp
	out := make([]indexer.Imported, len(f.list))
	for i, im := range f.list {
		if im.Reason == indexer.ReasonWillAdd {
			im.Reason, im.Source = "", &indexer.Source{ID: int64(i + 1), Name: im.Name}
		}
		out[i] = im
	}
	return out, f.err
}

func prowlarrList() []indexer.Imported {
	return []indexer.Imported{
		{Name: "RuTracker.org", DefinitionID: "rutracker", Reason: indexer.ReasonWillAdd},
		{Name: "Kinozal", DefinitionID: "kinozal", Reason: indexer.ReasonWillAdd, NeedsFlareSolverr: true},
		{Name: "RuTor", DefinitionID: "rutor", Reason: indexer.ReasonAlready},
		{Name: "Some Usenet", Reason: "Тип «Newznab» не поддерживается: Antresol подключает только торрент-трекеры"},
		{Name: "Lost", DefinitionID: "lost", Reason: "Описание трекера «lost» не найдено"},
	}
}

func TestProwlarrDryRunPreviews(t *testing.T) {
	f := &fakeProwlarr{list: prowlarrList()}
	o := ArrOptions{ProwlarrDB: "/prowlarr/prowlarr.db", DryRun: true, FlareSolverrSet: true}
	rep := &Report{}
	if err := ImportProwlarr(context.Background(), o, f, rep); err != nil {
		t.Fatal(err)
	}
	if f.preview != 1 || f.imported != 0 {
		t.Fatalf("preview %d import %d", f.preview, f.imported)
	}
	want := map[string]string{"RuTracker.org": StatusPlanned, "Kinozal": StatusPlanned, "RuTor": StatusSkipped,
		"Some Usenet": StatusWarning, "Lost": StatusFailed}
	for name, st := range want {
		if l := lineByName(t, rep, name); l.Status != st || l.Source != SourceProwlarr || l.Kind != "source" || l.Reason == "" {
			t.Errorf("%s = %+v, want %s", name, l, st)
		}
	}
}

func TestProwlarrImportLines(t *testing.T) {
	f := &fakeProwlarr{list: prowlarrList()}
	o := ArrOptions{ProwlarrDB: "/prowlarr/prowlarr.db", FlareSolverrSet: true}
	rep := &Report{}
	if err := ImportProwlarr(context.Background(), o, f, rep); err != nil {
		t.Fatal(err)
	}
	if f.preview != 0 || f.imported != 1 {
		t.Fatalf("preview %d import %d", f.preview, f.imported)
	}
	if l := lineByName(t, rep, "RuTracker.org"); l.Status != StatusImported || l.Ref != "source:1" {
		t.Fatalf("line = %+v", l)
	}
	if c := rep.Tally(); c[StatusImported] != 2 || c[StatusWarning] != 1 {
		t.Fatalf("counts %v", c)
	}
}

func TestProwlarrFlareSolverrWarning(t *testing.T) {
	for _, set := range []bool{false, true} {
		f := &fakeProwlarr{list: prowlarrList()}
		rep := &Report{}
		if err := ImportProwlarr(context.Background(), ArrOptions{ProwlarrDB: "/p.db", DryRun: true, FlareSolverrSet: set}, f, rep); err != nil {
			t.Fatal(err)
		}
		var warns []string
		for _, l := range rep.Lines {
			if l.Status == StatusWarning && strings.Contains(l.Reason, "FlareSolverr") {
				warns = append(warns, l.Reason)
			}
		}
		want := []string(nil)
		if !set {
			want = []string{"Kinozal: нужен FlareSolverr — укажите его адрес в «Управление → Настройки загрузок»"}
		}
		if !reflect.DeepEqual(warns, want) {
			t.Fatalf("FlareSolverrSet=%v: warnings %q", set, warns)
		}
	}
}

func TestProwlarrUnreadable(t *testing.T) {
	f := &fakeProwlarr{err: &indexer.ImportError{Msg: "Файл базы Prowlarr не найден: /p.db"}}
	rep := &Report{}
	if err := ImportProwlarr(context.Background(), ArrOptions{ProwlarrDB: "/p.db"}, f, rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Lines) != 1 || rep.Lines[0].Status != StatusFailed || rep.Lines[0].Reason != "Файл базы Prowlarr не найден: /p.db" {
		t.Fatalf("lines = %+v", rep.Lines)
	}
	f = &fakeProwlarr{err: errors.New("disk I/O")}
	if err := ImportProwlarr(context.Background(), ArrOptions{ProwlarrDB: "/p.db"}, f, &Report{}); err == nil {
		t.Fatal("internal error swallowed")
	}
}

// The snapshot goes into the run's folder, and nothing is left there.
func TestProwlarrSnapshotInRunTmp(t *testing.T) {
	f := &fakeProwlarr{list: prowlarrList()}
	tmp := filepath.Join(t.TempDir(), "run-1")
	if err := ImportProwlarr(context.Background(), ArrOptions{ProwlarrDB: "/p.db", DryRun: true, Tmp: tmp}, f, &Report{}); err != nil {
		t.Fatal(err)
	}
	if f.tmp != tmp {
		t.Fatalf("tmp = %q, want %q", f.tmp, tmp)
	}
}

// The real Manager: the snapshot (passwords, cookies) is taken under Tmp
// and removed; the system temp folder is not used.
func TestProwlarrRealManagerUsesRunTmp(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	pdb := fixtureDB(t, "prowlarr.db", []string{
		`CREATE TABLE "Indexers" ("Id" INTEGER PRIMARY KEY, "Name" TEXT NOT NULL, "Implementation" TEXT NOT NULL, "Settings" TEXT,
			"Enable" INTEGER, "Priority" INTEGER NOT NULL DEFAULT 25, "Tags" TEXT)`,
		`INSERT INTO Indexers (Name, Implementation, Settings, Enable, Tags) VALUES ('RuTor', 'Cardigann', '{"definitionFile":"rutor"}', 1, '[]')`,
	}, func(*sql.DB) {})
	sys := t.TempDir()
	t.Setenv("TMPDIR", sys)
	tmp := filepath.Join(dir, "tmp", "migrate", "run-1")
	m := indexer.NewManager(d, indexer.NewCatalog(filepath.Join(dir, "definitions")), indexer.Options{})
	rep := &Report{}
	if err := ImportProwlarr(context.Background(), ArrOptions{ProwlarrDB: pdb, DryRun: true, Tmp: tmp}, m, rep); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{sys, tmp} {
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Fatalf("left in %s: %v", dir, left)
		}
	}
	if len(rep.Lines) != 1 || rep.Lines[0].Status != StatusPlanned {
		t.Fatalf("lines = %+v", rep.Lines)
	}
}

// The real indexer.Manager satisfies ProwlarrImporter.
var _ ProwlarrImporter = (*indexer.Manager)(nil)
