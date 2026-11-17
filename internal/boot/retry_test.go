package boot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/importer"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// Errors a retry cannot cure are final; a busy source, a core that does not
// answer yet or a locked database are not.
func TestBoot_FinalAndTransientErrors(t *testing.T) {
	imported := func(r *rig) {
		r.conf.ImportFrom = "/nowhere/vault.db"
		d, err := store.Open(filepath.Join(r.conf.DataDir, "torrents.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		if _, err := d.Exec(`INSERT INTO import_state (id, source, stamp, counts_json, phase, fingerprint, updated_at)
			VALUES (1, '/nowhere/vault.db', 'x', '{}', 1, 'a', 'x')`); err != nil {
			t.Fatal(err)
		}
	}
	importErr := func(err error) func(*rig) {
		return func(r *rig) {
			r.conf.ImportFrom = "/nowhere/vault.db"
			r.deps.Import = func(context.Context, importer.Options) (importer.Counts, error) { return nil, err }
		}
	}
	cases := []struct {
		name  string
		setup func(*rig)
		final bool
	}{
		{"source changed", func(r *rig) {
			imported(r)
			r.deps.Fingerprint = func(context.Context, string, string) (string, error) { return "b", nil }
		}, true},
		{"not empty", importErr(fmt.Errorf("%w (1 rows in torrents)", importer.ErrNotEmpty)), true},
		{"bad app_settings", importErr(fmt.Errorf("%w: noSeeding = 3: wrong type", importer.ErrBadSettings)), true},
		{"state write", func(r *rig) {
			if os.Geteuid() == 0 {
				t.Skip("root writes into a 0500 directory")
			}
			dbDir := t.TempDir()
			r.deps.OpenDB = func(string) (*sql.DB, error) { return store.Open(filepath.Join(dbDir, "torrents.db")) }
			if err := os.Chmod(r.conf.DataDir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(r.conf.DataDir, 0o700) })
		}, true},
		{"snapshot busy at import", importErr(errors.New("importer: snapshot of x: kept changing")), false},
		{"snapshot busy at the check", func(r *rig) {
			imported(r)
			r.deps.Fingerprint = func(context.Context, string, string) (string, error) {
				return "", errors.New("importer: snapshot of x: kept changing")
			}
		}, false},
		{"database busy", func(r *rig) {
			r.deps.OpenDB = func(string) (*sql.DB, error) { return nil, errors.New("database is locked") }
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, false)
			c.setup(r)
			_, err := r.boot()
			if err == nil {
				t.Fatal("Boot did not fail")
			}
			if IsFinal(err) != c.final {
				t.Fatalf("IsFinal(%v) = %v, want %v", err, IsFinal(err), c.final)
			}
			if r.built != 0 {
				t.Fatal("engine built")
			}
		})
	}
}

// Retry tries again after a transient error, with the pause it is given,
// and reports each failure; it stops at once on a final one.
func TestRetry(t *testing.T) {
	var reported []string
	n := 0
	err := Retry(context.Background(), func() error {
		n++
		if n < 3 {
			return fmt.Errorf("busy %d", n)
		}
		return nil
	}, func(err error) { reported = append(reported, err.Error()) }, func(int) time.Duration { return time.Millisecond })
	if err != nil || n != 3 || len(reported) != 2 || reported[1] != "busy 2" {
		t.Fatalf("err %v, tries %d, reported %v", err, n, reported)
	}

	n, reported = 0, nil
	err = Retry(context.Background(), func() error {
		n++
		return Final(errors.New("refused"))
	}, func(err error) { reported = append(reported, err.Error()) }, func(int) time.Duration { return time.Millisecond })
	if err == nil || err.Error() != "refused" || !IsFinal(err) || n != 1 || len(reported) != 1 {
		t.Fatalf("err %v, tries %d, reported %v", err, n, reported)
	}

	ctx, cancel := context.WithCancel(context.Background())
	n = 0
	err = Retry(ctx, func() error {
		n++
		cancel()
		return errors.New("busy")
	}, func(error) {}, func(int) time.Duration { return time.Hour })
	if !errors.Is(err, context.Canceled) || n != 1 {
		t.Fatalf("after cancel: err %v, tries %d", err, n)
	}
}

func TestBackoff(t *testing.T) {
	if Backoff(0) != 2*time.Second || Backoff(1) != 4*time.Second || Backoff(10) != time.Minute {
		t.Fatalf("Backoff: %v %v %v", Backoff(0), Backoff(1), Backoff(10))
	}
}
