// Package importer is the torrents addon's one-time import from the old
// build's vault.db. Phase 1 runs before the engine is built:
// from a snapshot of vault.db (never the live file) it copies the family's
// rows, the addon's settings and files into the addon's own database and
// data dir, and records import_state last, in the same transaction. Phase 2
// (phase2.go) binds the library refs once the core is reachable.
package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/sqlitesnap"
)

// Options of an import.
type Options struct {
	From, FromDataDir string  // ANTRESOL_TORRENTS_IMPORT_FROM, …_IMPORT_DATA_DIR (default: dir of From)
	DataDir           string  // the addon's data dir
	DB                *sql.DB // torrents.db
	Log               *slog.Logger
}

// Counts are the rows (per table), and "settings", "files", "refs",
// "missingSaveDirs", of an import.
type Counts map[string]int

// State is the import_state row. Stamp is the import's time; Fingerprint
// identifies the source rows it was taken from.
type State struct {
	Source, Stamp, Fingerprint string
	Counts                     Counts
	Phase                      int
	Error                      string
}

// ErrNotEmpty: torrents.db already has downloads, sources or acquisitions
// and no import_state; an import never merges into them.
var ErrNotEmpty = errors.New("importer: the addon database already has data")

// ErrBadSettings: the source's app_settings is not what the old build
// writes; the import refuses rather than fall back to defaults.
var ErrBadSettings = errors.New("importer: settings app_settings is not valid")

// LoadState reads import_state; ok false when there is none.
func LoadState(ctx context.Context, db *sql.DB) (State, bool, error) {
	var st State
	var counts string
	err := db.QueryRowContext(ctx, `SELECT source, stamp, counts_json, phase, fingerprint, error FROM import_state WHERE id = 1`).
		Scan(&st.Source, &st.Stamp, &counts, &st.Phase, &st.Fingerprint, &st.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, err
	}
	if err := json.Unmarshal([]byte(counts), &st.Counts); err != nil {
		return State{}, false, fmt.Errorf("importer: import_state counts: %w", err)
	}
	return st, true, nil
}

func (o Options) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.Default()
}

func (o Options) fromDataDir() string {
	if o.FromDataDir != "" {
		return o.FromDataDir
	}
	return filepath.Dir(o.From)
}

// Phase1 imports From into o.DB and o.DataDir: it refuses a
// database that already has data; takes a snapshot of From into
// DataDir/import; copies the fileDirs files first (each renamed into place, so
// a rerun overwrites); then, in one transaction, the family's rows over the
// columns both sides have, the settings, the refs to bind and, last,
// import_state. Any failure before the commit leaves no import_state, and
// the next start redoes everything. DataDir/import is deleted at the end.
func Phase1(ctx context.Context, o Options) (Counts, error) {
	if o.From == "" || o.DataDir == "" || o.DB == nil {
		return nil, errors.New("importer: From, DataDir and DB are required")
	}
	if err := refuseNonEmpty(ctx, o.DB); err != nil {
		return nil, err
	}
	work := filepath.Join(o.DataDir, "import")
	if err := os.RemoveAll(work); err != nil { // a crashed run's leftovers
		return nil, err
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	snap, err := sqlitesnap.Snapshot(o.From, work)
	if err != nil {
		return nil, fmt.Errorf("importer: snapshot of %s: %w", o.From, err)
	}
	src, err := sqlitesnap.OpenRO(snap)
	if err != nil {
		return nil, fmt.Errorf("importer: open snapshot: %w", err)
	}
	defer src.Close()

	counts := Counts{}
	n, err := copyFiles(o.fromDataDir(), filepath.Join(work, "files"), o.DataDir)
	if err != nil {
		return nil, err
	}
	counts["files"] = n

	fp, err := fingerprintOf(ctx, src)
	if err != nil {
		return nil, err
	}

	conn, err := o.DB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	for _, table := range familyTables {
		n, err := copyTable(ctx, src, tx, table)
		if err != nil {
			return nil, err
		}
		counts[table] = n
	}
	if counts["missingSaveDirs"], err = checkSaveDirs(ctx, tx, o.log()); err != nil {
		return nil, err
	}
	if counts["settings"], err = copySettings(ctx, src, tx); err != nil {
		return nil, err
	}
	if counts["refs"], err = copyRefs(ctx, src, tx); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(counts)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, `INSERT INTO import_state (id, source, stamp, counts_json, phase, fingerprint, updated_at)
		VALUES (1, ?, ?, ?, 1, ?, ?)`, o.From, now, string(raw), fp, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	o.log().Info("import phase 1 done", "from", o.From, "counts", counts)
	return counts, nil
}

// refuseNonEmpty: rows in torrents, sources or acquisitions. It runs
// on the pool outside the import's transaction: the addon is the only
// writer of torrents.db and the import runs before anything else starts.
func refuseNonEmpty(ctx context.Context, db *sql.DB) error {
	for _, t := range []string{"torrents", "sources", "acquisitions"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM `+t).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w (%d rows in %s)", ErrNotEmpty, n, t)
		}
	}
	return nil
}

// checkSaveDirs warns about every download whose save dir does not exist.
func checkSaveDirs(ctx context.Context, tx *sql.Tx, log *slog.Logger) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT infohash, save_dir FROM torrents`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	missing := 0
	for rows.Next() {
		var ih, dir string
		if err := rows.Scan(&ih, &dir); err != nil {
			return 0, err
		}
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			missing++
			log.Warn("import: the save dir of a download does not exist", "infohash", ih, "dir", dir)
		}
	}
	return missing, rows.Err()
}
