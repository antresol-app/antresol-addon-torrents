package importer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/releaseprefs"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/sqlitesnap"
)

// familyTables are the vault.db tables the addon takes over, in an order
// that satisfies their references.
var familyTables = []string{"torrents", "torrent_pieces", "sources", "acquisitions", "follows",
	"release_attempts", "release_switch_failures", "pinned_releases"}

// fingerprintTables are the tables whose rows identify the source:
// nothing in the core writes them since the move (but fingerprintSkip), unlike
// vault.db as a whole.
var fingerprintTables = []string{"torrents", "acquisitions", "sources", "follows",
	"release_attempts", "release_switch_failures", "pinned_releases"}

// fingerprintSkip are the columns of fingerprintTables the core does write
// since the move, through a foreign key: deleting a profile nulls them (ON DELETE
// SET NULL on users). They are left out of the fingerprint, or a routine
// profile delete would read as «Источник изменился». Should the core ever
// delete tracked_titles, pinned_releases.title_id needs the same treatment.
var fingerprintSkip = map[string]map[string]bool{
	"follows":         {"user_id": true},
	"pinned_releases": {"pinned_by": true},
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// columns lists a table's columns in table order (lower-cased names).
func columns(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, strings.ToLower(n))
	}
	return out, rows.Err()
}

// copyTable copies table from src into tx row by row over the columns both
// sides have (a column only one side has is skipped), and asserts that
// the destination then holds as many rows as the source.
func copyTable(ctx context.Context, src *sql.DB, tx *sql.Tx, table string) (int, error) {
	ok, err := sqlitesnap.HasTable(ctx, src, table)
	if err != nil || !ok {
		return 0, err
	}
	srcCols, err := columns(ctx, src, table)
	if err != nil {
		return 0, err
	}
	dstCols, err := columns(ctx, tx, table)
	if err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, c := range dstCols {
		have[c] = true
	}
	var cols []string
	for _, c := range srcCols {
		if have[c] {
			cols = append(cols, quote(c))
		}
	}
	if len(cols) == 0 {
		return 0, fmt.Errorf("importer: %s has no column in common", table)
	}
	list := strings.Join(cols, ", ")
	ins, err := tx.PrepareContext(ctx, `INSERT INTO `+quote(table)+` (`+list+`) VALUES (`+strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")+`)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	rows, err := src.QueryContext(ctx, `SELECT `+list+` FROM `+quote(table))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	n := 0
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return 0, err
		}
		if _, err := ins.ExecContext(ctx, vals...); err != nil {
			return 0, fmt.Errorf("importer: %s row %d: %w", table, n+1, err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	var want, got int
	if err := src.QueryRowContext(ctx, `SELECT count(*) FROM `+quote(table)).Scan(&want); err != nil {
		return 0, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM `+quote(table)).Scan(&got); err != nil {
		return 0, err
	}
	if want != got || got != n {
		return 0, fmt.Errorf("importer: %s: %d rows in the source, %d copied, %d in torrents.db", table, want, n, got)
	}
	return n, nil
}

// copied as they are, under their own keys
var verbatimSettings = []string{releaseprefs.SettingsKey, "migration", "import_token_key"}

// copySettings writes the addon's settings rows: the app_settings JSON of
// the old build becomes upload_limited, upload_limit_kbs,
// stream_cache_gb and no_seeding; a row that does not parse, or a value of
// the wrong type, fails the import (never a default no-seeding off).
func copySettings(ctx context.Context, src *sql.DB, tx *sql.Tx) (int, error) {
	ok, err := sqlitesnap.HasTable(ctx, src, "settings")
	if err != nil || !ok {
		return 0, err
	}
	get := func(key string) (string, bool, error) {
		var v string
		err := src.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return v, err == nil, err
	}
	var out [][2]string
	if raw, ok, err := get("app_settings"); err != nil {
		return 0, err
	} else if ok {
		kv, err := appSettings(raw)
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrBadSettings, err)
		}
		out = append(out, kv...)
	}
	for _, k := range verbatimSettings {
		v, ok, err := get(k)
		if err != nil {
			return 0, err
		}
		if ok {
			out = append(out, [2]string{k, v})
		}
	}
	for _, kv := range out {
		// A row may exist already: a forced start without IMPORT_FROM writes
		// no_seeding=1. The import overwrites, except that no_seeding
		// never goes from on to off.
		q := `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`
		if kv[0] == "no_seeding" {
			q = `INSERT INTO settings (key, value) VALUES (?, ?)
				ON CONFLICT(key) DO UPDATE SET value = CASE WHEN settings.value = '1' THEN '1' ELSE excluded.value END`
		}
		if _, err := tx.ExecContext(ctx, q, kv[0], kv[1]); err != nil {
			return 0, err
		}
	}
	return len(out), nil
}

// appSettings maps the old build's app_settings JSON to settings rows.
func appSettings(raw string) ([][2]string, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("importer: settings app_settings does not parse: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("importer: settings app_settings does not parse: text after the object")
	}
	// The keys are matched exactly: the old build only ever writes them
	// in this case (its appSettingsFull marshals these field names).
	bad := func(k string, v any) error {
		return fmt.Errorf("importer: settings app_settings %s = %v: wrong type", k, v)
	}
	var out [][2]string
	flag := func(k, key string) error {
		v, ok := m[k]
		if !ok {
			return nil
		}
		b, ok := v.(bool)
		if !ok {
			return bad(k, v)
		}
		out = append(out, [2]string{key, map[bool]string{true: "1", false: "0"}[b]})
		return nil
	}
	if err := flag("noSeeding", "no_seeding"); err != nil {
		return nil, err
	}
	if err := flag("uploadLimited", "upload_limited"); err != nil {
		return nil, err
	}
	if v, ok := m["uploadLimitKBs"]; ok {
		num, isNum := v.(json.Number)
		n, err := num.Int64()
		if !isNum || err != nil || n < 0 {
			return nil, bad("uploadLimitKBs", v)
		}
		out = append(out, [2]string{"upload_limit_kbs", strconv.FormatInt(n, 10)})
	}
	if v, ok := m["streamCacheGB"]; ok {
		num, isNum := v.(json.Number)
		f, err := num.Float64()
		if !isNum || err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
			return nil, bad("streamCacheGB", v)
		}
		out = append(out, [2]string{"stream_cache_gb", strconv.FormatFloat(f, 'f', -1, 64)})
	}
	return out, nil
}

// copyRefs writes the refs phase 2 binds: every library file with an
// infohash, keyed by path, the ref the lower-case infohash.
func copyRefs(ctx context.Context, src *sql.DB, tx *sql.Tx) (int, error) {
	ok, err := sqlitesnap.HasTable(ctx, src, "files")
	if err != nil || !ok {
		return 0, err
	}
	cols, err := sqlitesnap.Columns(ctx, src, "files")
	if err != nil {
		return 0, err
	}
	if !cols["torrent_infohash"] {
		return 0, nil
	}
	rows, err := src.QueryContext(ctx, `SELECT path, lower(torrent_infohash) FROM files WHERE torrent_infohash IS NOT NULL AND torrent_infohash != ''`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var p, ref string
		if err := rows.Scan(&p, &ref); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO import_refs (path, ref) VALUES (?, ?)`, p, ref); err != nil {
			return 0, err
		}
		n++
	}
	return n, rows.Err()
}

// RemoveLeftovers deletes the snapshots of vault.db a crashed run can leave
// in dataDir (Phase1's import dir, Fingerprint's fingerprint-* dirs): full
// copies of the core's database, password hashes and sessions included.
// Boot calls it first thing, before anything snapshots again.
func RemoveLeftovers(dataDir string) error {
	dirs, err := filepath.Glob(filepath.Join(dataDir, "fingerprint-*"))
	if err != nil {
		return err
	}
	var errs []error
	for _, d := range append(dirs, filepath.Join(dataDir, "import")) {
		if fi, err := os.Lstat(d); err != nil || !fi.IsDir() {
			continue
		}
		errs = append(errs, os.RemoveAll(d))
	}
	return errors.Join(errs...)
}

// Fingerprint takes a snapshot of from into a temp dir under dataDir and
// answers the fingerprint of its family rows; the snapshot goes.
func Fingerprint(ctx context.Context, from, dataDir string) (string, error) {
	tmp, err := os.MkdirTemp(dataDir, "fingerprint-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	snap, err := sqlitesnap.Snapshot(from, tmp)
	if err != nil {
		return "", fmt.Errorf("importer: snapshot of %s: %w", from, err)
	}
	src, err := sqlitesnap.OpenRO(snap)
	if err != nil {
		return "", err
	}
	defer src.Close()
	return fingerprintOf(ctx, src)
}

// fingerprintOf: sha256 over every row of fingerprintTables, ordered by
// their columns (all but fingerprintSkip) and typed, plus the row count of
// torrent_pieces.
func fingerprintOf(ctx context.Context, src *sql.DB) (string, error) {
	h := sha256.New()
	for _, table := range fingerprintTables {
		fmt.Fprintf(h, "table %s\n", table)
		ok, err := sqlitesnap.HasTable(ctx, src, table)
		if err != nil {
			return "", err
		}
		if !ok {
			fmt.Fprintf(h, "absent\n")
			continue
		}
		all, err := columns(ctx, src, table)
		if err != nil {
			return "", err
		}
		var cols []string
		for _, c := range all {
			if !fingerprintSkip[table][c] {
				cols = append(cols, c)
			}
		}
		quoted := make([]string, len(cols))
		order := make([]string, len(cols))
		for i, c := range cols {
			quoted[i] = quote(c)
			order[i] = strconv.Itoa(i + 1)
		}
		fmt.Fprintf(h, "columns %s\n", strings.Join(cols, ","))
		rows, err := src.QueryContext(ctx, `SELECT `+strings.Join(quoted, ", ")+` FROM `+quote(table)+` ORDER BY `+strings.Join(order, ", "))
		if err != nil {
			return "", err
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return "", err
			}
			var b bytes.Buffer
			for _, v := range vals {
				switch x := v.(type) {
				case nil:
					b.WriteString("N;")
				case int64:
					fmt.Fprintf(&b, "I%d;", x)
				case float64:
					fmt.Fprintf(&b, "F%s;", strconv.FormatFloat(x, 'g', -1, 64))
				case string:
					fmt.Fprintf(&b, "S%d:%s;", len(x), x)
				case []byte:
					fmt.Fprintf(&b, "B%d:%s;", len(x), x)
				default:
					fmt.Fprintf(&b, "?%v;", x)
				}
			}
			b.WriteByte('\n')
			h.Write(b.Bytes())
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return "", err
		}
		if err := rows.Close(); err != nil {
			return "", err
		}
	}
	var pieces int
	if ok, err := sqlitesnap.HasTable(ctx, src, "torrent_pieces"); err != nil {
		return "", err
	} else if ok {
		if err := src.QueryRowContext(ctx, `SELECT count(*) FROM torrent_pieces`).Scan(&pieces); err != nil {
			return "", err
		}
	}
	fmt.Fprintf(h, "torrent_pieces %d\n", pieces)
	return hex.EncodeToString(h.Sum(nil)), nil
}
