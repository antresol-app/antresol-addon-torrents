package follow

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol/addonkit"
)

func (e *env) importFollow(in ImportFollow) bool {
	e.t.Helper()
	created, err := ImportInactive(context.Background(), e.svc.DB, addonkit.User{}, in)
	if err != nil {
		e.t.Fatal(err)
	}
	return created
}

func (e *env) stateJSON(id int64) (string, bool) {
	e.t.Helper()
	var raw string
	var checked *string
	if err := e.svc.DB.QueryRow(`SELECT state_json, last_checked FROM follows WHERE id=?`, id).Scan(&raw, &checked); err != nil {
		e.t.Fatal(err)
	}
	return raw, checked != nil
}

func TestImportInactiveStoresState(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC)
	if !e.importFollow(ImportFollow{Kind: KindSeries, TmdbID: 42, Title: "Сёгун", Poster: "https://p", Source: "sonarr",
		Import: &ImportState{Baseline: base, Want: [][2]int{{1, 3}, {1, 7}}}}) {
		t.Fatal("not created")
	}
	f, err := e.svc.ByTMDB(context.Background(), KindSeries, 42)
	if err != nil {
		t.Fatal(err)
	}
	if f.Active || f.Source != "sonarr" || f.Title != "Сёгун" || f.Poster != "https://p" {
		t.Fatalf("follow = %+v", f)
	}
	raw, _ := e.stateJSON(f.ID)
	var st struct {
		Import struct {
			Baseline time.Time `json:"importBaseline"`
			Want     [][2]int  `json:"want"`
		} `json:"import"`
	}
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Import.Baseline.Equal(base) || !reflect.DeepEqual(st.Import.Want, [][2]int{{1, 3}, {1, 7}}) {
		t.Fatalf("state_json = %s", raw)
	}
}

func TestImportInactiveConflict(t *testing.T) {
	e := newEnv(t)
	f := e.add(KindSeries, 42) // vault, active
	if e.importFollow(ImportFollow{Kind: KindSeries, TmdbID: 42, Title: "X", Source: "sonarr", Import: &ImportState{Baseline: today}}) {
		t.Fatal("created over an existing follow")
	}
	got := e.get(f.ID)
	if !got.Active || got.Source != "vault" || got.Title != "Сёгун" {
		t.Fatalf("follow changed: %+v", got)
	}
}

func TestActivateImported(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.importFollow(ImportFollow{Kind: KindSeries, TmdbID: 1, Title: "A", Source: "sonarr", Import: &ImportState{Baseline: today}})
	e.importFollow(ImportFollow{Kind: KindMovie, TmdbID: 2, Title: "B", Source: "radarr"})
	e.importFollow(ImportFollow{Kind: KindSeries, TmdbID: 4, Title: "D", Source: "sonarr", Import: &ImportState{Baseline: today}})
	d, _ := e.svc.ByTMDB(ctx, KindSeries, 4)
	if err := e.svc.SetActive(ctx, d.ID, true); err != nil { // already active
		t.Fatal(err)
	}
	v := e.add(KindMovie, 3) // vault
	if err := e.svc.SetActive(ctx, v.ID, false); err != nil {
		t.Fatal(err)
	}
	e.svc.takeKicked() // Add kicked the vault follow

	n, err := e.svc.ActivateImported(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("activated %d, want 2", n)
	}
	if kicked := e.svc.takeKicked(); len(kicked) != n {
		t.Fatalf("kicked %v, want %d", kicked, n)
	}
	list, _ := e.svc.List(ctx)
	for _, f := range list {
		want := f.Source != "vault"
		if f.Active != want {
			t.Errorf("%s %d (%s): active=%v", f.Kind, f.TMDBID, f.Source, f.Active)
		}
	}
	if n, _ := e.svc.ActivateImported(ctx); n != 0 {
		t.Fatalf("second activation = %d", n)
	}
}

// Sonarr monitored 2 of 10 aired episodes: only those 2 are downloaded,
// not a season pack.
func TestImportedFollowRespectsSonarrMonitoring(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.importFollow(ImportFollow{Kind: KindSeries, TmdbID: 42, Title: "Сёгун", Source: "sonarr",
		Import: &ImportState{Baseline: today, Want: [][2]int{{1, 3}, {1, 7}}}})
	if n, err := e.svc.ActivateImported(ctx); err != nil || n != 1 {
		t.Fatalf("activate = %d, %v", n, err)
	}
	f, _ := e.svc.ByTMDB(ctx, KindSeries, 42)
	res, err := e.svc.Check(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := targets(e.acq.requests()), [][2]int{{1, 3}, {1, 7}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("acquired %v, want %v", got, want)
	}
	if res.Missing != 2 {
		t.Fatalf("missing = %d, want 2", res.Missing)
	}
	raw, _ := e.stateJSON(f.ID)
	if !json.Valid([]byte(raw)) || !strings.Contains(raw, `"importBaseline"`) {
		t.Fatalf("import state lost after a check: %s", raw)
	}
}

// Episodes aired on or after the baseline are followed as usual.
func TestImportedFollowNewEpisodesQueued(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.importFollow(ImportFollow{Kind: KindSeries, TmdbID: 42, Title: "Сёгун", Source: "sonarr",
		Import: &ImportState{Baseline: time.Date(2024, 1, 9, 15, 0, 0, 0, time.UTC)}})
	e.svc.ActivateImported(ctx)
	f, _ := e.svc.ByTMDB(ctx, KindSeries, 42)
	if _, err := e.svc.Check(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	// season(1, 10, 0): E09 aired 2024-01-09 (the baseline day), E10 after
	if got, want := targets(e.acq.requests()), [][2]int{{1, 9}, {1, 10}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("acquired %v, want %v", got, want)
	}
}

func TestPreviewQueuesNothing(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.episodes(42, 1, 1, 2, 3, 4, 5, 6, 7)
	f := e.add(KindSeries, 42)
	before, checked := e.stateJSON(f.ID)
	if checked {
		t.Fatal("checked before preview")
	}
	prev, err := e.svc.Preview(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(e.acq.requests()); n != 0 {
		t.Fatalf("preview acquired %d", n)
	}
	if after, checked := e.stateJSON(f.ID); after != before || checked {
		t.Fatalf("preview wrote state: %s → %s (checked %v)", before, after, checked)
	}
	res, err := e.svc.Check(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prev, res) {
		t.Fatalf("preview %+v, check %+v", prev, res)
	}
	if want := []string{"S01E08", "S01E09", "S01E10"}; !reflect.DeepEqual(res.Queued, want) {
		t.Fatalf("queued %v", res.Queued)
	}
}

// Preview works on an imported follow that is still inactive (what
// «Перейти на Vault» would download) and follows its Sonarr choice.
func TestPreviewInactiveImported(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.importFollow(ImportFollow{Kind: KindSeries, TmdbID: 42, Title: "Сёгун", Source: "sonarr",
		Import: &ImportState{Baseline: today, Want: [][2]int{{1, 2}}}})
	e.importFollow(ImportFollow{Kind: KindMovie, TmdbID: 7, Title: "Дюна", Source: "radarr"})
	s, _ := e.svc.ByTMDB(ctx, KindSeries, 42)
	res, err := e.svc.Preview(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"S01E02"}; !reflect.DeepEqual(res.Queued, want) {
		t.Fatalf("series preview %v", res.Queued)
	}
	m, _ := e.svc.ByTMDB(ctx, KindMovie, 7)
	res, err = e.svc.Preview(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"фильм"}; !reflect.DeepEqual(res.Queued, want) {
		t.Fatalf("movie preview %v", res.Queued)
	}
	if len(e.acq.requests()) != 0 {
		t.Fatal("preview acquired")
	}
}

// Old episodes without a date in a season over a year old count as aired;
// for an imported follow they are pre-baseline too.
func TestImportedFollowUndatedOldEpisodes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	old := tmdb.SeasonDetails{Number: 1, AirDate: "2020-01-01"}
	for i := 1; i <= 4; i++ {
		old.Episodes = append(old.Episodes, tmdb.Episode{Number: i})
	}
	e.tm.tv.Seasons = []tmdb.SeasonCount{{Number: 1, Episodes: 4}}
	e.tm.seasons = map[int]tmdb.SeasonDetails{1: old}
	e.importFollow(ImportFollow{Kind: KindSeries, TmdbID: 42, Title: "Сёгун", Source: "sonarr",
		Import: &ImportState{Baseline: today, Want: [][2]int{{1, 4}}}})
	s, _ := e.svc.ByTMDB(ctx, KindSeries, 42)
	res, err := e.svc.Preview(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"S01E04"}; !reflect.DeepEqual(res.Queued, want) {
		t.Fatalf("preview %v", res.Queued)
	}
}

// A year after the import, gaps counts undated episodes of a season that
// began shortly before the import as aired; Sonarr's choice still holds.
func TestImportedFollowUndatedYearLater(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	base := today
	young := tmdb.SeasonDetails{Number: 1, AirDate: base.AddDate(0, -2, 0).Format("2006-01-02")}
	for i := 1; i <= 4; i++ {
		young.Episodes = append(young.Episodes, tmdb.Episode{Number: i})
	}
	e.tm.tv.Seasons = []tmdb.SeasonCount{{Number: 1, Episodes: 4}}
	e.tm.seasons = map[int]tmdb.SeasonDetails{1: young}
	e.now = base.AddDate(0, 0, 400)
	e.importFollow(ImportFollow{Kind: KindSeries, TmdbID: 42, Title: "Сёгун", Source: "sonarr",
		Import: &ImportState{Baseline: base, Want: [][2]int{{1, 2}}}})
	s, _ := e.svc.ByTMDB(ctx, KindSeries, 42)
	res, err := e.svc.Preview(ctx, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"S01E02"}; !reflect.DeepEqual(res.Queued, want) {
		t.Fatalf("preview %v", res.Queued)
	}
}

// Preview tells the switch the whole volume: targets over the cap and why
// nothing would be queued.
func TestPreviewReportsLaterAndBlocked(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.tm.tv.Seasons = []tmdb.SeasonCount{{Number: 1, Episodes: 13}}
	e.tm.seasons = map[int]tmdb.SeasonDetails{1: season(1, 12, 1)} // not fully aired: one target per episode
	f := e.add(KindSeries, 42)
	res, err := e.svc.Preview(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Queued) != 10 || res.Later != 2 || res.Missing != 12 || res.Blocked != "" {
		t.Fatalf("preview %+v", res)
	}
	e.acq.disk = acquire.DiskUsage{Level: acquire.LevelFull}
	res, err = e.svc.Preview(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Queued) != 0 || res.Blocked == "" {
		t.Fatalf("preview on a full disk %+v", res)
	}
}

// After the switch a user may turn an imported follow off; calling
// ActivateImported again does not turn it back on.
func TestActivateImportedOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.importFollow(ImportFollow{Kind: KindMovie, TmdbID: 2, Title: "B", Source: "radarr"})
	if n, _ := e.svc.ActivateImported(ctx); n != 1 {
		t.Fatalf("activated %d", n)
	}
	f, _ := e.svc.ByTMDB(ctx, KindMovie, 2)
	if _, err := e.svc.Check(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.SetActive(ctx, f.ID, false); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.svc.ActivateImported(ctx); n != 0 {
		t.Fatalf("re-activated %d", n)
	}
}

// The follows «Перейти на Antresol» switches on are told (OnFollowed:
// the core tracks the title and refreshes it at once); importing them
// tells nothing (they stay inactive until the switch).
func TestActivateImportedTellsFollowed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	var told []string
	e.svc.OnFollowed = func(_ context.Context, f Follow) {
		told = append(told, fmt.Sprintf("%s:%d:%v", f.Kind, f.TMDBID, f.Active))
	}
	e.importFollow(ImportFollow{Kind: KindSeries, TmdbID: 1, Title: "A", Source: "sonarr", Import: &ImportState{Baseline: today}})
	e.importFollow(ImportFollow{Kind: KindMovie, TmdbID: 2, Title: "B", Source: "radarr"})
	if len(told) != 0 {
		t.Fatalf("an import told %v", told)
	}
	if n, err := e.svc.ActivateImported(ctx); err != nil || n != 2 {
		t.Fatalf("activate %d %v", n, err)
	}
	slices.Sort(told)
	if fmt.Sprint(told) != "[movie:2:true series:1:true]" {
		t.Fatalf("told %v", told)
	}
	if n, _ := e.svc.ActivateImported(ctx); n != 0 || len(told) != 2 {
		t.Fatalf("second activation %d, told %v", n, told)
	}
}
