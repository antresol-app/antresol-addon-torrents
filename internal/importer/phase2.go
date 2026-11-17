package importer

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// DefaultBatch is how many refs one BindRefs call carries: well under the
// core's 32 MiB body limit for that route.
const DefaultBatch = 2000

// RefBinder binds library files to refs (addonkit.Client.BindRefs).
type RefBinder interface {
	BindRefs(ctx context.Context, pairs []addonkit.RefPath) (addonkit.Bound, error)
}

// Phase2 binds the refs phase 1 left pending, batch at a time (batch ≤ 0:
// DefaultBatch). Each answer's missing and taken paths are marked so, the
// rest of its batch bound; with no pending row left, import_state.phase
// becomes 2 and done is true. A failed call returns the error with its
// batch still pending (earlier batches stay bound) and records it in
// import_state.error; a rerun sends only pending rows. Without an
// import_state there is nothing to bind: done, no call. The caller runs it
// after the engine has started, on every start and every 60 s after an
// error, until done.
func Phase2(ctx context.Context, db *sql.DB, b RefBinder, batch int) (done bool, err error) {
	if batch <= 0 {
		batch = DefaultBatch
	}
	st, ok, err := LoadState(ctx, db)
	if err != nil {
		return false, err
	}
	if !ok || st.Phase >= 2 {
		return true, nil
	}
	for {
		pairs, err := pending(ctx, db, batch)
		if err != nil {
			return false, err
		}
		if len(pairs) == 0 {
			break
		}
		ans, err := b.BindRefs(ctx, pairs)
		if err != nil {
			setError(context.WithoutCancel(ctx), db, err)
			return false, fmt.Errorf("importer: bind %d refs: %w", len(pairs), err)
		}
		if err := mark(ctx, db, pairs, ans); err != nil {
			return false, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.ExecContext(ctx, `UPDATE import_state SET phase = 2, error = '', updated_at = ? WHERE id = 1`, now); err != nil {
		return false, err
	}
	return true, nil
}

func pending(ctx context.Context, db *sql.DB, limit int) ([]addonkit.RefPath, error) {
	rows, err := db.QueryContext(ctx, `SELECT path, ref FROM import_refs WHERE state = 'pending' ORDER BY path LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []addonkit.RefPath
	for rows.Next() {
		var p addonkit.RefPath
		if err := rows.Scan(&p.Path, &p.Ref); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// mark sets the batch's states in one transaction: missing, taken, else bound.
func mark(ctx context.Context, db *sql.DB, pairs []addonkit.RefPath, ans addonkit.Bound) error {
	state := make(map[string]string, len(pairs))
	for _, p := range pairs {
		state[p.Path] = "bound"
	}
	for _, p := range ans.Missing {
		if _, ok := state[p]; ok {
			state[p] = "missing"
		}
	}
	for _, p := range ans.Taken {
		if _, ok := state[p]; ok {
			state[p] = "taken"
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for p, s := range state {
		if _, err := tx.ExecContext(ctx, `UPDATE import_refs SET state = ? WHERE path = ? AND state = 'pending'`, s, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// setError records cause in import_state.error; a failed write is logged
// (the call's own error still goes back to the caller).
func setError(ctx context.Context, db *sql.DB, cause error) {
	if _, err := db.ExecContext(ctx, `UPDATE import_state SET error = ?, updated_at = ? WHERE id = 1`, cause.Error(), time.Now().UTC().Format(time.RFC3339)); err != nil {
		slog.Error("importer: record the phase 2 error", "cause", cause, "err", err)
	}
}
