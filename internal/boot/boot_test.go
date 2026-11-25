package boot

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/importer"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
	_ "modernc.org/sqlite"
)

// rig: a data dir and Deps whose NewEngine records the config and the
// state file as it read when the engine was built.
type rig struct {
	t          *testing.T
	conf       Conf
	built      int
	cfg        torrent.EngineConfig
	stateAtNew string
	eng        *fakeEngine
	events     []string
	deps       Deps
}

func newRig(t *testing.T, forced bool) *rig {
	r := &rig{t: t}
	dir := t.TempDir()
	r.conf = Conf{DataDir: dir, DownloadDir: filepath.Join(dir, "dl"), StreamDir: filepath.Join(dir, "stream"),
		Port: 1234, ForcedNoSeeding: forced, LocalOnly: true}
	r.deps = Deps{
		OpenDB: store.Open,
		Import: func(ctx context.Context, o importer.Options) (importer.Counts, error) {
			r.events = append(r.events, "import")
			return importer.Phase1(ctx, o)
		},
		Fingerprint: func(ctx context.Context, from, dataDir string) (string, error) {
			r.events = append(r.events, "fingerprint")
			return importer.Fingerprint(ctx, from, dataDir)
		},
		NewEngine: func(cfg torrent.EngineConfig, db *sql.DB) (Engine, error) {
			r.events = append(r.events, "engine")
			r.built++
			r.cfg = cfg
			r.stateAtNew = readState(cfg.DataDir)
			r.eng = &fakeEngine{dataDir: cfg.DataDir}
			return r.eng, nil
		},
	}
	return r
}

func (r *rig) boot() (Booted, error) {
	b, err := Boot(context.Background(), r.conf, r.deps)
	if b.DB != nil {
		r.t.Cleanup(func() { b.DB.Close() })
	}
	return b, err
}

// setting writes no_seeding into the rig's torrents.db before the boot.
func (r *rig) setting(v string) {
	d, err := store.Open(filepath.Join(r.conf.DataDir, "torrents.db"))
	if err != nil {
		r.t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`INSERT INTO settings (key, value) VALUES ('no_seeding', ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, v); err != nil {
		r.t.Fatal(err)
	}
}

func TestBoot_ForcedBeforeEngine(t *testing.T) {
	r := newRig(t, true)
	r.setting("0")
	b, err := r.boot()
	if err != nil {
		t.Fatal(err)
	}
	if r.built != 1 || r.stateAtNew != "on forced\n" || !r.cfg.NoSeeding || !r.cfg.NoUpload {
		t.Fatalf("engine built %d times, state then %q, NoSeeding %v, NoUpload %v", r.built, r.stateAtNew, r.cfg.NoSeeding, r.cfg.NoUpload)
	}
	if b.Engine == nil || b.NoSeed == nil || b.DB == nil {
		t.Fatalf("Booted = %+v", b)
	}
	if r.cfg.DataDir != r.conf.DataDir || r.cfg.StreamDir != r.conf.StreamDir || r.cfg.DownloadDir != r.conf.DownloadDir ||
		r.cfg.ListenPort != 1234 || !r.cfg.NoUPnP || !r.cfg.LocalOnly {
		t.Fatalf("EngineConfig = %+v", r.cfg)
	}
}

func TestBoot_ForcedPersistsSetting(t *testing.T) {
	r := newRig(t, true)
	r.setting("0")
	b, err := r.boot()
	if err != nil {
		t.Fatal(err)
	}
	if on, err := (store.Settings{DB: b.DB}).NoSeeding(context.Background()); err != nil || !on {
		t.Fatalf("after a forced boot NoSeeding = %v, %v", on, err)
	}
	b.DB.Close()
	// the next start, without the override
	r.conf.ForcedNoSeeding = false
	if _, err := r.boot(); err != nil {
		t.Fatal(err)
	}
	if r.stateAtNew != "on\n" || !r.cfg.NoSeeding {
		t.Fatalf("second boot: state %q, NoSeeding %v", r.stateAtNew, r.cfg.NoSeeding)
	}
}

func TestBoot_SettingOn(t *testing.T) {
	r := newRig(t, false)
	r.setting("1")
	if _, err := r.boot(); err != nil {
		t.Fatal(err)
	}
	// a setting can be turned off again: only a forced value shuts the client
	if r.stateAtNew != "on\n" || !r.cfg.NoSeeding || r.cfg.NoUpload {
		t.Fatalf("state %q, NoSeeding %v, NoUpload %v", r.stateAtNew, r.cfg.NoSeeding, r.cfg.NoUpload)
	}
}

func TestBoot_Off(t *testing.T) {
	r := newRig(t, false)
	if _, err := r.boot(); err != nil {
		t.Fatal(err)
	}
	if r.stateAtNew != "off\n" || r.cfg.NoSeeding {
		t.Fatalf("state %q, NoSeeding %v", r.stateAtNew, r.cfg.NoSeeding)
	}
}

// An unreadable setting: no engine, an error, and (fail closed) state on.
func TestBoot_SettingsErrorNoEngine(t *testing.T) {
	r := newRig(t, false)
	r.setting("x")
	if _, err := r.boot(); err == nil {
		t.Fatal("Boot hid the settings error")
	}
	if r.built != 0 {
		t.Fatal("engine built on an unreadable setting")
	}
	if s := readState(r.conf.DataDir); s != "on\n" && s != "<none>" {
		t.Fatalf("state %q after a settings error", s)
	}
}

// source: an old-build vault.db from the importer's fixture.
func source(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	from := filepath.Join(dir, "vault.db")
	body, err := os.ReadFile("../importer/testdata/vault-main.sql")
	if err != nil {
		t.Fatal(err)
	}
	v, err := sql.Open("sqlite", "file:"+from+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if _, err := v.Exec(string(body)); err != nil {
		t.Fatal(err)
	}
	return from
}

func TestBoot_ImportedSettingReachesEngine(t *testing.T) {
	r := newRig(t, false)
	r.conf.ImportFrom = source(t) // its app_settings has noSeeding true
	if _, err := r.boot(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.events, ",") != "import,engine" {
		t.Fatalf("events %v; want import before engine", r.events)
	}
	if r.stateAtNew != "on\n" || !r.cfg.NoSeeding {
		t.Fatalf("state %q, NoSeeding %v", r.stateAtNew, r.cfg.NoSeeding)
	}
	if r.cfg.UploadLimit != 512*1024 {
		t.Fatalf("UploadLimit = %d, want the imported 512 KB/s", r.cfg.UploadLimit)
	}
}

func TestBoot_ImportFailureNoEngine(t *testing.T) {
	r := newRig(t, false)
	r.conf.ImportFrom = filepath.Join(t.TempDir(), "absent.db")
	r.deps.Import = func(context.Context, importer.Options) (importer.Counts, error) {
		return nil, errors.New("half-way")
	}
	if _, err := r.boot(); err == nil || !strings.Contains(err.Error(), "half-way") {
		t.Fatalf("Boot = %v", err)
	}
	if r.built != 0 {
		t.Fatal("engine built after a failed import")
	}
	if s := readState(r.conf.DataDir); s != "<none>" {
		t.Fatalf("state %q after a failed import", s)
	}
}

func TestBoot_SourceChangedAfterImportRefused(t *testing.T) {
	r := newRig(t, true)
	r.conf.ImportFrom = "/nowhere/vault.db"
	d, err := store.Open(filepath.Join(r.conf.DataDir, "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO import_state (id, source, stamp, counts_json, phase, fingerprint, updated_at)
		VALUES (1, '/nowhere/vault.db', 'x', '{}', 1, 'a', 'x')`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	r.deps.Import = func(context.Context, importer.Options) (importer.Counts, error) {
		t.Fatal("imported again")
		return nil, nil
	}
	r.deps.Fingerprint = func(context.Context, string, string) (string, error) { return "b", nil }
	_, err = r.boot()
	if err == nil || err.Error() != "Источник изменился после импорта: очистите данные расширения и импортируйте заново" {
		t.Fatalf("Boot = %v", err)
	}
	if r.built != 0 {
		t.Fatal("engine built on a changed source")
	}
	// the same fingerprint boots
	r.deps.Fingerprint = func(context.Context, string, string) (string, error) { return "a", nil }
	if _, err := r.boot(); err != nil {
		t.Fatal(err)
	}
	if r.built != 1 || r.stateAtNew != "on forced\n" {
		t.Fatalf("built %d, state %q", r.built, r.stateAtNew)
	}
	// a fingerprint that fails: no engine either
	r.deps.Fingerprint = func(context.Context, string, string) (string, error) { return "", errors.New("unreadable") }
	if _, err := r.boot(); err == nil {
		t.Fatal("Boot hid the fingerprint error")
	}
	if r.built != 1 {
		t.Fatal("engine built on an unreadable source")
	}
}

func TestBoot_StateWriteFailureNoEngine(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a 0500 directory: the state write cannot be made to fail this way")
	}
	r := newRig(t, true)
	dbDir := t.TempDir()
	r.deps.OpenDB = func(string) (*sql.DB, error) { return store.Open(filepath.Join(dbDir, "torrents.db")) }
	if err := os.Chmod(r.conf.DataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(r.conf.DataDir, 0o700) })
	if _, err := r.boot(); err == nil {
		t.Fatal("Boot hid the state write error")
	}
	if r.built != 0 {
		t.Fatal("engine built without a state file")
	}
}

func TestBoot_OpenDBFailureNoEngine(t *testing.T) {
	r := newRig(t, true)
	r.deps.OpenDB = func(string) (*sql.DB, error) { return nil, errors.New("no db") }
	if _, err := r.boot(); err == nil {
		t.Fatal("Boot hid the open error")
	}
	if r.built != 0 {
		t.Fatal("engine built without a database")
	}
}

// A crash can leave a snapshot of vault.db (password hashes, sessions) in
// the data dir: the import's work dir or a fingerprint's temp dir. Boot
// removes both before anything else, whether it imports or not.
func TestBoot_RemovesLeftoverSnapshots(t *testing.T) {
	r := newRig(t, false)
	left := []string{filepath.Join(r.conf.DataDir, "import", "vault.db-1"), filepath.Join(r.conf.DataDir, "fingerprint-123", "vault.db")}
	for _, p := range left {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("snapshot"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(r.conf.DataDir, "fingerprint.txt") // not a temp dir of ours
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.boot(); err != nil {
		t.Fatal(err)
	}
	for _, p := range left {
		if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
			t.Fatalf("%s left behind (%v)", filepath.Dir(p), err)
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("removed a file that is not a snapshot: %v", err)
	}
}

// TestBoot_MapsUserUIDsAfterImport: the import's id-keyed follows,
// pins, torrents and acquisitions get their profile's uid in the same boot, after the import and
// before the engine (nothing serves yet); every row carries one.
func TestBoot_MapsUserUIDsAfterImport(t *testing.T) {
	r := newRig(t, false)
	r.conf.ImportFrom = source(t)
	r.deps.MapUsers = func(ctx context.Context, db *sql.DB) error {
		r.events = append(r.events, "map")
		return follow.MapUserUIDs(ctx, db, func(context.Context) ([]addonkit.User, error) {
			return []addonkit.User{{ID: 1, UID: "u1"}, {ID: 2, UID: "u2"}}, nil
		})
	}
	b, err := r.boot()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(r.events, ",") != "import,map,engine" {
		t.Fatalf("events %v", r.events)
	}
	for _, c := range [][2]string{{"follows", "user_uid"}, {"pinned_releases", "pinned_by_uid"},
		{"torrents", "added_by_uid"}, {"acquisitions", "added_by_uid"}} {
		var n, bare int
		if err := b.DB.QueryRow(`SELECT COUNT(*), COUNT(*) - COUNT(`+c[1]+`) FROM `+c[0]).Scan(&n, &bare); err != nil {
			t.Fatal(err)
		}
		if n == 0 || bare != 0 {
			t.Fatalf("%s: %d rows, %d without a uid", c[0], n, bare)
		}
	}
}

// TestBoot_MapUsersFailureNoEngine: the core's profiles not read, the
// boot fails (retried: not Final) before the engine.
func TestBoot_MapUsersFailureNoEngine(t *testing.T) {
	r := newRig(t, false)
	r.deps.MapUsers = func(context.Context, *sql.DB) error { return errors.New("core down") }
	if _, err := r.boot(); err == nil || IsFinal(err) || r.built != 0 {
		t.Fatalf("err %v (final %v), engines %d", err, IsFinal(err), r.built)
	}
}

// TestBoot_MakesDownloadDir: the boot makes the downloads folder
// before the engine (mode 0755, the engine's), so the core's disk check does
// not block «Перейти на Antresol» on a fresh server; while the media disk is
// not mounted (a storage folder missing) it makes nothing there.
func TestBoot_MakesDownloadDir(t *testing.T) {
	r := newRig(t, false)
	r.conf.DownloadDir = filepath.Join(r.conf.DataDir, "media", "dl")
	if _, err := r.boot(); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(r.conf.DownloadDir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o755 {
		t.Fatalf("downloads folder: %v %v", st, err)
	}

	r = newRig(t, false)
	media := filepath.Join(r.conf.DataDir, "media")
	r.conf.DownloadDir = filepath.Join(media, "dl")
	r.conf.StorageDirs = []string{media}
	if _, err := r.boot(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(media); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("made the downloads folder on a disk that is not mounted: %v", err)
	}
	if r.built != 1 {
		t.Fatalf("engine built %d times", r.built)
	}
}
