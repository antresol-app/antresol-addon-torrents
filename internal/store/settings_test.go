package store

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/releaseprefs"
)

func settings(t *testing.T) Settings {
	t.Helper()
	return Settings{DB: open(t, filepath.Join(t.TempDir(), "torrents.db"))}
}

// TestSettings_Defaults: a new database has no rows; every getter answers
// its default (no-seeding off, upload unlimited, the caller's cache size).
func TestSettings_Defaults(t *testing.T) {
	ctx := context.Background()
	s := settings(t)
	if on, err := s.NoSeeding(ctx); err != nil || on {
		t.Fatalf("NoSeeding = %v, %v; want false", on, err)
	}
	if limited, kbs, err := s.Upload(ctx); err != nil || limited || kbs != 0 {
		t.Fatalf("Upload = %v, %d, %v; want false, 0", limited, kbs, err)
	}
	if gb, err := s.StreamCacheGB(ctx, 12.5); err != nil || gb != 12.5 {
		t.Fatalf("StreamCacheGB = %v, %v; want the default 12.5", gb, err)
	}
}

// TestSettings_RoundTrip: what is set is read back, and stored as the
// documented strings ("1"/"0", decimal numbers).
func TestSettings_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := settings(t)
	if err := s.SaveNoSeeding(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUpload(ctx, true, 512); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStreamCacheGB(ctx, 30); err != nil {
		t.Fatal(err)
	}
	if on, err := s.NoSeeding(ctx); err != nil || !on {
		t.Fatalf("NoSeeding = %v, %v", on, err)
	}
	if limited, kbs, err := s.Upload(ctx); err != nil || !limited || kbs != 512 {
		t.Fatalf("Upload = %v, %d, %v", limited, kbs, err)
	}
	if gb, err := s.StreamCacheGB(ctx, 1); err != nil || gb != 30 {
		t.Fatalf("StreamCacheGB = %v, %v", gb, err)
	}
	want := map[string]string{"no_seeding": "1", "upload_limited": "1", "upload_limit_kbs": "512", "stream_cache_gb": "30"}
	for k, v := range want {
		var got string
		if err := s.DB.QueryRow(`SELECT value FROM settings WHERE key=?`, k).Scan(&got); err != nil || got != v {
			t.Fatalf("settings[%s] = %q, %v; want %q", k, got, err, v)
		}
	}
	// and back off
	if err := s.SaveNoSeeding(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUpload(ctx, false, 0); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.NoSeeding(ctx); on {
		t.Fatal("NoSeeding still on")
	}
	if limited, kbs, _ := s.Upload(ctx); limited || kbs != 0 {
		t.Fatalf("Upload = %v, %d after turning it off", limited, kbs)
	}
}

// TestSettings_BadStoredValue: a value that does not parse is an error,
// never a silent default (no_seeding above all).
func TestSettings_BadStoredValue(t *testing.T) {
	ctx := context.Background()
	s := settings(t)
	if _, err := s.DB.Exec(`INSERT INTO settings (key, value) VALUES ('no_seeding', 'yes'), ('upload_limit_kbs', 'x'), ('stream_cache_gb', 'big')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NoSeeding(ctx); err == nil {
		t.Fatal("NoSeeding accepted 'yes'")
	}
	if _, _, err := s.Upload(ctx); err == nil {
		t.Fatal("Upload accepted 'x'")
	}
	if _, err := s.StreamCacheGB(ctx, 1); err == nil {
		t.Fatal("StreamCacheGB accepted 'big'")
	}
	for _, v := range []string{"NaN", "Inf", "-Inf", "+Inf"} {
		if _, err := s.DB.Exec(`UPDATE settings SET value=? WHERE key='stream_cache_gb'`, v); err != nil {
			t.Fatal(err)
		}
		if _, err := s.StreamCacheGB(ctx, 1); err == nil {
			t.Fatalf("StreamCacheGB accepted %q", v)
		}
	}
	for _, gb := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1} {
		if err := s.SetStreamCacheGB(ctx, gb); err == nil {
			t.Fatalf("SetStreamCacheGB accepted %v", gb)
		}
	}
}

// TestSettings_ReleasePrefsShareTable: the copied releaseprefs.Store works
// on the same settings table.
func TestSettings_ReleasePrefsShareTable(t *testing.T) {
	ctx := context.Background()
	s := settings(t)
	if err := (releaseprefs.Store{DB: s.DB}).Save(ctx, releaseprefs.Default()); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := s.DB.QueryRow(`SELECT value FROM settings WHERE key='release_prefs'`).Scan(&raw); err != nil || raw == "" {
		t.Fatalf("release_prefs row: %q, %v", raw, err)
	}
}

// TestStore_AddonStateTables: 0013 adds settings, import_state (one row at
// most) and import_refs (state defaults to pending, only the four states).
func TestStore_AddonStateTables(t *testing.T) {
	d := open(t, filepath.Join(t.TempDir(), "torrents.db"))
	if _, err := d.Exec(`INSERT INTO import_state (id, source, stamp, counts_json, phase, updated_at) VALUES (1, '/v.db', 's', '{}', 1, 'now')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO import_state (id, source, stamp, counts_json, phase, updated_at) VALUES (2, '/v.db', 's', '{}', 1, 'now')`); err == nil {
		t.Fatal("import_state took a second row")
	}
	var e, fp string
	if err := d.QueryRow(`SELECT error, fingerprint FROM import_state`).Scan(&e, &fp); err != nil || e != "" || fp != "" {
		t.Fatalf("import_state defaults error %q fingerprint %q, %v", e, fp, err)
	}
	if _, err := d.Exec(`INSERT INTO import_refs (path, ref) VALUES ('/m/a.mkv', 'abc')`); err != nil {
		t.Fatal(err)
	}
	var st string
	if err := d.QueryRow(`SELECT state FROM import_refs WHERE path='/m/a.mkv'`).Scan(&st); err != nil || st != "pending" {
		t.Fatalf("import_refs.state default %q, %v", st, err)
	}
	for _, ok := range []string{"bound", "missing", "taken"} {
		if _, err := d.Exec(`UPDATE import_refs SET state=? WHERE path='/m/a.mkv'`, ok); err != nil {
			t.Fatalf("state %s refused: %v", ok, err)
		}
	}
	if _, err := d.Exec(`UPDATE import_refs SET state='done' WHERE path='/m/a.mkv'`); err == nil {
		t.Fatal("import_refs took state 'done'")
	}
	if _, err := d.Exec(`INSERT INTO import_refs (path, ref) VALUES ('/m/a.mkv', 'def')`); err == nil {
		t.Fatal("import_refs took a path twice")
	}
}
