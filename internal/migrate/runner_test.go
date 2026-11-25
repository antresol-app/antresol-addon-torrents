package migrate

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	mediaimport "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/mediaimport/runkit"
	"github.com/antresol-app/antresol-addon-torrents/internal/holds"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// arrFixtures puts a Sonarr database (two monitored series, one not) and
// a Radarr one (a movie to get, one Radarr has) into the import folder.
func (e *svcEnv) arrFixtures() {
	e.place(sonarrDB(e.t, []sonarrRow{
		{ID: 1, Title: "Сёгун", Year: 2024, TvdbID: 1, TmdbID: 126308, Monitored: true},
		{ID: 2, Title: "Тьма", Year: 2017, TvdbID: 2, TmdbID: 70523, Monitored: true},
		{ID: 3, Title: "Забытый", Year: 2001, TvdbID: 3, TmdbID: 3, Monitored: false},
	}, []sonarrEpisode{{SeriesID: 1, Season: 1, Episode: 1, Monitored: true}}, false), e.s.Paths.Sonarr)
	e.place(radarrDB(e.t, []radarrRow{
		{Title: "Дюна", Year: 2021, TmdbID: 438631, Monitored: true},
		{Title: "Брат", Year: 1997, TmdbID: 20992, Monitored: true, MovieFileID: 5},
	}, false), e.s.Paths.Radarr)
}

type followCols struct {
	kind   string
	tmdb   int
	active bool
	source string
	user   sql.NullInt64
}

func (e *svcEnv) follows() []followCols {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT kind, tmdb_id, active, source, user_id FROM follows ORDER BY kind, tmdb_id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []followCols
	for rows.Next() {
		var f followCols
		if err := rows.Scan(&f.kind, &f.tmdb, &f.active, &f.source, &f.user); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

func linesOf(rep *Report, source, status string) []string {
	var out []string
	for _, l := range rep.Lines {
		if l.Source == source && l.Status == status {
			out = append(out, l.Name)
		}
	}
	return out
}

// The dry run lists what Sonarr and Radarr have; a real run needs it, and
// creates the follows inactive in the addon's database (nothing goes
// to the core's tracking until «Перейти на Antresol»); the report is in
// the addon's reports dir.
func TestRunner_DryRunThenRun(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.arrFixtures()
	srcs := []string{"sonarr", "radarr"}
	if _, err := e.s.Start(context.Background(), mediaimport.RunRequest{Sources: srcs, AdminID: e.admin}); !errors.Is(err, ErrNeedsDryRun) {
		t.Fatalf("no plan: %v", err)
	}
	plan := e.start(mediaimport.RunRequest{Sources: srcs, DryRun: true})
	if got := linesOf(plan, SourceSonarr, StatusPlanned); !slices.Equal(got, []string{"Сёгун (2024)", "Тьма (2017)"}) {
		t.Fatalf("planned series %v (%+v)", got, plan.Lines)
	}
	if got := linesOf(plan, SourceRadarr, StatusPlanned); !slices.Equal(got, []string{"Дюна (2021)"}) {
		t.Fatalf("planned movies %v", got)
	}
	if got := linesOf(plan, SourceRadarr, StatusSkipped); !slices.Equal(got, []string{"Брат (1997)"}) {
		t.Fatalf("skipped movies %v", got)
	}
	if len(e.follows()) != 0 {
		t.Fatal("the dry run wrote follows")
	}
	real := e.start(mediaimport.RunRequest{Sources: srcs, PlanID: plan.ID})
	if real.DryRun || real.FinishedAt.IsZero() || len(linesOf(real, SourceSonarr, StatusImported)) != 2 || len(linesOf(real, SourceRadarr, StatusImported)) != 1 {
		t.Fatalf("real %+v", real.Lines)
	}
	fs := e.follows()
	if len(fs) != 3 {
		t.Fatalf("follows %+v", fs)
	}
	for _, f := range fs {
		if f.active || (f.source != "sonarr" && f.source != "radarr") {
			t.Fatalf("follow %+v", f)
		}
	}
	if len(e.track.Titles) != 0 || len(e.track.Kicks) != 0 {
		t.Fatalf("the run reached the core's tracking: %+v %v", e.track.Titles, e.track.Kicks)
	}
	for _, id := range []string{plan.ID, real.ID} {
		if _, err := os.Stat(filepath.Join(e.reports, id+".json")); err != nil {
			t.Fatalf("report %s not under the addon's dir: %v", id, err)
		}
	}
}

// «Перейти на Antresol» switches the imported follows on, and each
// reaches the core's tracking (ensure + kick); one the core refuses waits
// for a retry.
func TestSwitch_EnsuresAndKicks(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.imported()
	ctx := context.Background()
	if n, err := e.s.Switch(ctx, 4, true); err != nil || n != 2 {
		t.Fatalf("switch %d %v", n, err)
	}
	var got []string
	for _, ti := range e.track.Titles {
		got = append(got, ti.Kind+":"+ti.Name)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"movie:Дюна", "series:Сёгун"}) || len(e.track.Kicks) != 2 {
		t.Fatalf("titles %v kicks %v", got, e.track.Kicks)
	}

	// the core down: the switched-on follows are queued, none lost
	e2 := newSvcEnv(t, nil)
	e2.imported()
	e2.track.Err = errors.New("core down")
	if n, err := e2.s.Switch(ctx, 4, true); err != nil || n != 2 {
		t.Fatalf("switch %d %v", n, err)
	}
	if q := e2.kicks.waiting(); len(q) != 2 {
		t.Fatalf("queued %+v", q)
	}
	e2.track.Err = nil
	if len(e2.track.Titles) != 0 {
		t.Fatalf("titles while the core refused: %+v", e2.track.Titles)
	}
}

// The follows a run creates carry the core's id of the admin who started
// it (X-Antresol-User), not the old database's ids, and its uid from
// the same header (StartAs).
func TestRunner_AddedByIsCoreAdmin(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.arrFixtures()
	const other int64 = 42
	plan := e.start(mediaimport.RunRequest{Sources: []string{"sonarr", "radarr"}, DryRun: true, AdminID: other})
	e.startAs(mediaimport.RunRequest{Sources: []string{"sonarr", "radarr"}, PlanID: plan.ID, AdminID: other}, "A")
	fs := e.follows()
	if len(fs) != 3 {
		t.Fatalf("follows %+v", fs)
	}
	for _, f := range fs {
		if !f.user.Valid || f.user.Int64 != other {
			t.Fatalf("follow %+v, want user %d", f, other)
		}
	}
	if uids := e.followUIDs(); !slices.Equal(uids, []string{"A", "A", "A"}) {
		t.Fatalf("uids %v", uids)
	}
}

// the admin's uid also without a TMDB key (no follow service); a run
// started without one (Start) names nobody: no follow by an id alone.
func TestRunner_AdminUIDWithoutFollowService(t *testing.T) {
	const other int64 = 42
	e := newSvcEnv(t, nil)
	e.arrFixtures()
	e.s.Follow = nil
	plan := e.start(mediaimport.RunRequest{Sources: []string{"sonarr", "radarr"}, DryRun: true, AdminID: other})
	e.startAs(mediaimport.RunRequest{Sources: []string{"sonarr", "radarr"}, PlanID: plan.ID, AdminID: other}, "A")
	if uids := e.followUIDs(); !slices.Equal(uids, []string{"A", "A", "A"}) {
		t.Fatalf("uids %v", uids)
	}

	e2 := newSvcEnv(t, nil)
	e2.arrFixtures()
	plan = e2.start(mediaimport.RunRequest{Sources: []string{"sonarr", "radarr"}, DryRun: true, AdminID: other})
	e2.start(mediaimport.RunRequest{Sources: []string{"sonarr", "radarr"}, PlanID: plan.ID, AdminID: other})
	for _, f := range e2.follows() {
		if f.user.Valid {
			t.Fatalf("a follow by an id alone: %+v", f)
		}
	}
	if uids := e2.followUIDs(); !slices.Equal(uids, []string{"", "", ""}) {
		t.Fatalf("uids %v", uids)
	}
	// the uid is the running report's only
	if got := e2.s.AdminUID(plan.ID); got != "" {
		t.Fatalf("uid of a finished run %q", got)
	}
}

// followUIDs are the follows' user_uid ("" = none), by kind and tmdb id.
func (e *svcEnv) followUIDs() []string {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT COALESCE(user_uid, '') FROM follows ORDER BY kind, tmdb_id`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		rows.Scan(&u)
		out = append(out, u)
	}
	return out
}

// TestSnapshotsOnly: a run reads Sonarr and Radarr only through
// snapshots: their databases and Sonarr's live -wal are byte for byte
// the same after a dry run and a real one.
func TestSnapshotsOnly(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.arrFixtures()
	w, err := sql.Open("sqlite", "file:"+e.s.Paths.Sonarr+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.SetMaxOpenConns(1)
	mustExec(t, w, `INSERT INTO Series (Id, TvdbId, Title, Images, Monitored, Year, TmdbId) VALUES (9, 0, 'Свежий', '[]', 1, 2026, 9)`)
	files := []string{e.s.Paths.Sonarr, e.s.Paths.Sonarr + "-wal", e.s.Paths.Radarr}
	before := make([]string, len(files))
	for i, p := range files {
		before[i] = fileHash(t, p)
	}
	plan := e.start(mediaimport.RunRequest{Sources: []string{"sonarr", "radarr"}, DryRun: true})
	real := e.start(mediaimport.RunRequest{Sources: []string{"sonarr", "radarr"}, PlanID: plan.ID})
	if l := lineByName(t, real, "Свежий (2026)"); l.Status != StatusImported {
		t.Fatalf("the -wal row: %+v", l)
	}
	for i, p := range files {
		if fileHash(t, p) != before[i] {
			t.Fatalf("%s changed", p)
		}
	}
	if ents, _ := os.ReadDir(e.tmp); len(ents) != 0 {
		t.Fatalf("snapshots left: %v", ents)
	}
}

// --- qBittorrent through the runner ---

// qbService is the addon's «Переезд» over env e (the real engine, the
// fake core's library and qBittorrent); holdsChanged counts the calls.
type qbService struct {
	*Service
	reports string
	mu      sync.Mutex
	changed int
}

func newQBService(t *testing.T, e *env) *qbService {
	t.Helper()
	root := filepath.Dir(e.state) // <root>/vault/migrate
	q := &qbService{reports: filepath.Join(root, "reports")}
	ctx, cancel := context.WithCancel(context.Background())
	r := NewRunner(&Runner{ReportDir: q.reports, TmpDir: filepath.Join(filepath.Dir(root), "tmp", "migrate"), BaseCtx: ctx})
	o := e.opts()
	o.BTBackupDir = ""
	q.Service = New(r, Service{DB: e.db, Engine: e.eng, Library: e.lib, Paths: SourcePaths{QBBackup: e.bt}, QB: o})
	q.HoldsChanged = func() {
		q.mu.Lock()
		q.changed++
		q.mu.Unlock()
	}
	t.Cleanup(func() { cancel(); r.Wait() })
	return q
}

func (q *qbService) calls() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.changed
}

func (q *qbService) run(t *testing.T, req mediaimport.RunRequest) *Report {
	t.Helper()
	req.Sources = []string{SourceQBittorrent}
	uid := "" // the admin's uid, as X-Antresol-User carries it
	if req.AdminID != 0 {
		uid = "uid-" + strconv.FormatInt(req.AdminID, 10)
	}
	id, err := q.StartAs(context.Background(), req, uid)
	if err != nil {
		t.Fatal(err)
	}
	q.Wait()
	rep, err := q.Report(id)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func (q *qbService) rollback(t *testing.T, from string) *Report {
	t.Helper()
	id, err := q.Rollback(context.Background(), from)
	if err != nil {
		t.Fatal(err)
	}
	q.Wait()
	rep, err := q.Report(id)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// With no-seeding on, an imported complete download is attached
// through the engine's own path, so it is parked as acquire's finished
// ones are: it never uploads. And migrate never touches the upload flag.
func TestQBImport_NoSeeding(t *testing.T) {
	e := newEnvWith(t, torrent.EngineConfig{NoSeeding: true})
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(200 << 10)}, nil)
	if l := onlyLine(t, e.run(e.opts())); l.Status != StatusImported {
		t.Fatalf("line %+v", l)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := status(t, e, ih)
		// Parked (the engine's own park path) is what proves no upload:
		// Uploaded == 0 holds anyway on a LocalOnly engine with no peers
		if st.Parked && st.Status == torrent.StatusSeeding && st.Done == st.Size && st.Uploaded == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not parked: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// the source holds no seeding switch of its own
	banned := regexp.MustCompile(`AllowDataUpload|DisallowDataUpload|SetNoSeeding|SaveNoSeeding|uploadOff|NoSeeding`)
	err := filepath.WalkDir(".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if m := banned.Find(b); m != nil {
			t.Errorf("%s names %s: the engine's attach decides uploading", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A finished qBittorrent download hard-linked into the library: the
// engine has it, its library file carries the lower-case infohash (one
// PUT /library/refs/{ref}), its move state is in data/migrate/qbt-state,
// and BT_backup is untouched.
func TestQBImport_BindsRefsAndCopiesState(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"X.S01.1080p/X.S01E01.mkv": randBytes(200 << 10), "X.S01.1080p/X.S01E02.mkv": randBytes(100 << 10)}, nil)
	p, _ := libraryEpisode(t, e, filepath.Join(e.save, "X.S01.1080p", "X.S01E01.mkv"))
	bt := treeHash(t, e.bt)
	q := newQBService(t, e)
	plan := q.run(t, mediaimport.RunRequest{DryRun: true})
	rep := q.run(t, mediaimport.RunRequest{PlanID: plan.ID, AdminID: coreAdmin})
	if l := onlyLine(t, rep); l.Status != StatusImported || l.Ref != ih {
		t.Fatalf("line %+v", l)
	}
	if st := status(t, e, ih); st.Mode != torrent.ModeSeed || st.AddedBy == nil || *st.AddedBy != coreAdmin || st.AddedByUID != "uid-7" {
		t.Fatalf("engine %+v", st)
	}
	if ih != strings.ToLower(ih) || !slices.Equal(e.lib.Bound(ih), []string{p}) {
		t.Fatalf("bound %v", e.lib.Bound(ih))
	}
	st, ok, err := loadState(e.state, ih)
	if err != nil || !ok || !st.Done || st.Report != rep.ID {
		t.Fatalf("move state %+v %v %v", st, ok, err)
	}
	if rel, _ := filepath.Rel(filepath.Dir(filepath.Dir(e.state)), statePath(e.state, ih)); rel != filepath.Join("migrate", "qbt-state", ih+".json") {
		t.Fatalf("state file at %s", rel)
	}
	if treeHash(t, e.bt) != bt {
		t.Fatal("BT_backup changed")
	}
}

// «Вернуть раздачи в qBittorrent» gives qBittorrent its torrent back:
// Antresol drops its copy (data kept), qBittorrent starts it again (it
// was running there), BT_backup and the data are as they were.
func TestRollback_RestoresBTBackup(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(200 << 10)}, nil)
	bt, data := treeHash(t, e.bt), treeHash(t, e.save)
	q := newQBService(t, e)
	plan := q.run(t, mediaimport.RunRequest{DryRun: true})
	from := q.run(t, mediaimport.RunRequest{PlanID: plan.ID})
	if s, _ := e.qb.stateOf(ih); s != "stoppedUP" {
		t.Fatalf("qBittorrent after the move: %q", s)
	}
	back := q.rollback(t, from.ID)
	if l := onlyLine(t, back); l.Status != StatusImported || back.RollbackOf != from.ID || back.FinishedAt.IsZero() {
		t.Fatalf("rollback %+v", back)
	}
	notInVault(t, e, ih)
	if s, _ := e.qb.stateOf(ih); s != "uploading" {
		t.Fatalf("qBittorrent after the rollback: %q", s)
	}
	if treeHash(t, e.bt) != bt || treeHash(t, e.save) != data {
		t.Fatal("BT_backup or the data changed")
	}
}

// With KeepPaused on (boot: no-seeding), a rollback gives every torrent
// back paused, even one that was running in qBittorrent: Antresol's copy
// is gone (no attached torrent left on the no-seeding engine either),
// qBittorrent is never started, and the line says why.
func TestRollback_PausedUnderNoSeeding(t *testing.T) {
	e := newEnvWith(t, torrent.EngineConfig{NoSeeding: true})
	running, _ := e.addTorrent(map[string][]byte{"A/ep1.mkv": randBytes(200 << 10)}, nil)
	paused, _ := e.addTorrent(map[string][]byte{"B/ep1.mkv": randBytes(100 << 10)}, map[string]any{"paused": 1})
	e.qb.setState(paused, "stoppedUP")
	bt := treeHash(t, e.bt)
	q := newQBService(t, e)
	keep := 0
	q.QB.KeepPaused = func(context.Context) bool { keep++; return true }
	plan := q.run(t, mediaimport.RunRequest{DryRun: true})
	from := q.run(t, mediaimport.RunRequest{PlanID: plan.ID})
	if c := from.Tally(); c[StatusImported] != 2 {
		t.Fatalf("import %+v", from.Lines)
	}
	n := len(e.qb.posts())
	keep = 0 // the plan and the import asked too (their notes)
	back := q.rollback(t, from.ID)
	if len(back.Lines) != 2 || keep != 2 { // asked per line
		t.Fatalf("rollback %+v (asked %d)", back.Lines, keep)
	}
	for _, l := range back.Lines {
		if l.Status != StatusImported || l.Reason != "Раздачи возвращены на паузе: включён запрет раздачи" {
			t.Fatalf("line %+v", l)
		}
	}
	for _, ih := range []string{running, paused} {
		notInVault(t, e, ih)
		if s, _ := e.qb.stateOf(ih); s != "stoppedUP" {
			t.Fatalf("%s in qBittorrent: %q", ih, s)
		}
		if _, ok, _ := loadState(e.state, ih); ok {
			t.Fatalf("%s: move state left", ih)
		}
	}
	if posts := e.qb.posts()[n:]; len(posts) != 0 {
		t.Fatalf("qBittorrent was sent %+v", posts)
	}
	if treeHash(t, e.bt) != bt {
		t.Fatal("BT_backup changed")
	}
	if list := e.eng.List(); len(list) != 0 {
		t.Fatalf("attached torrents left %+v", list)
	}
}

// HoldsChanged runs once per real qBittorrent run and once per
// rollback (not for a dry run); a holds pusher then holds the imported
// download's data, and after the rollback of the only one the set is [].
func TestQBImport_HoldsChanged(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(200 << 10)}, nil)
	q := newQBService(t, e)
	var sets [][]addonkit.Hold
	p := &holds.Pusher{Src: &holds.EngineSource{Engine: e.eng}, FileRoots: []string{filepath.Dir(e.save)},
		Put: func(_ context.Context, h []addonkit.Hold) error { sets = append(sets, h); return nil }}
	q.HoldsChanged = func() {
		q.mu.Lock()
		q.changed++
		q.mu.Unlock()
		p.Changed()
	}
	plan := q.run(t, mediaimport.RunRequest{DryRun: true})
	if q.calls() != 0 {
		t.Fatalf("a dry run: %d calls", q.calls())
	}
	from := q.run(t, mediaimport.RunRequest{PlanID: plan.ID})
	if q.calls() != 1 {
		t.Fatalf("after the run: %d calls", q.calls())
	}
	if err := p.PushNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(sets) != 1 || !holds1(sets[0], ih) {
		t.Fatalf("set after the import %+v (ih %s)", sets, ih)
	}
	t.Run("rollback", func(t *testing.T) {
		q.rollback(t, from.ID)
		if q.calls() != 2 {
			t.Fatalf("after the rollback: %d calls", q.calls())
		}
		if err := p.PushNow(context.Background()); err != nil {
			t.Fatal(err)
		}
		if last := sets[len(sets)-1]; len(last) != 0 {
			t.Fatalf("set after the rollback %+v", last)
		}
	})
}

// holds1: hs holds exactly the download ih, with files.
func holds1(hs []addonkit.Hold, ih string) bool {
	return len(hs) == 1 && hs[0].Key == ih && len(hs[0].Files) > 0
}

// An old build's report, copied into the reports dir at import, is
// in the history with its torrents counted, and its qBittorrent part can
// be given back.
func TestRunner_ListsImportedReports(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(200 << 10)}, nil)
	// the old build moved it: its copy seeds in the engine, its state
	// and report are what the import copied
	moved := e.run(e.opts())
	q := newQBService(t, e)
	old := &Report{ID: "20261020T100000Z-abcdef", Sources: []string{"jellyfin", SourceQBittorrent},
		StartedAt: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), FinishedAt: time.Date(2026, 10, 20, 10, 5, 0, 0, time.UTC),
		Lines: append([]Line{{Source: "jellyfin", Kind: "progress", Name: "Дюна", Status: StatusImported}}, moved.Lines...)}
	if err := SaveReport(q.reports, old); err != nil {
		t.Fatal(err)
	}
	list, err := q.Reports()
	if err != nil || len(list) != 1 || list[0].ID != old.ID || q.Torrents(old.ID) != 1 {
		t.Fatalf("history %+v %v", list, err)
	}
	back := q.rollback(t, old.ID)
	if l := onlyLine(t, back); l.Status != StatusImported || l.Ref != ih {
		t.Fatalf("rollback %+v", back.Lines)
	}
	notInVault(t, e, ih)
}

// KeepPaused is asked for every line, right before qBittorrent would be
// started: no-seeding turned on during a rollback keeps the remaining
// torrents paused.
func TestRollback_KeepPausedAskedPerLine(t *testing.T) {
	e := newEnv(t)
	a, _ := e.addTorrent(map[string][]byte{"A/ep1.mkv": randBytes(100 << 10)}, nil)
	b, _ := e.addTorrent(map[string][]byte{"B/ep1.mkv": randBytes(100 << 10)}, nil)
	q := newQBService(t, e)
	plan := q.run(t, mediaimport.RunRequest{DryRun: true})
	from := q.run(t, mediaimport.RunRequest{PlanID: plan.ID})
	asked := 0
	q.QB.KeepPaused = func(context.Context) bool { asked++; return asked > 1 } // turned on after the first line
	back := q.rollback(t, from.ID)
	if len(back.Lines) != 2 || asked != 2 {
		t.Fatalf("rollback %+v (asked %d)", back.Lines, asked)
	}
	started := 0
	for _, ih := range []string{a, b} {
		if s, _ := e.qb.stateOf(ih); s == "uploading" {
			started++
		}
	}
	if started != 1 || back.Lines[1].Reason != reasonKeptPaused {
		t.Fatalf("started %d, lines %+v", started, back.Lines)
	}
}
