package follow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/antresol-app/antresol/addonkit"
)

// uidMapKey is the settings row of MapUserUIDs: "done:<import stamp>"
// ("done:" without an import) once the rows it found were mapped.
const uidMapKey = "user_uid_map"

// errProfileList: the core's profile list is empty or has a profile
// without its uid; the mapping waits for a good one (Boot retries).
var errProfileList = errors.New("follow: the profile list has no profiles or a profile without its uid")

// MapUserUIDs gives the rows keyed by a profile id alone (follows.user_id,
// pinned_releases.pinned_by, torrents.added_by, acquisitions.added_by:
// the old build's, and the import's) the
// uid of the profile that has the id now (profiles: GET /users, asked
// only when there is something to map: a new database needs no core
// answer); an id no profile has is a deleted
// profile's, as ForgetGone makes it. It runs once per database and once
// more after each import (the settings row uidMapKey names the import it
// mapped), in one transaction with that row; Boot calls it after the
// import and before anything serves. A later start changes nothing.
func MapUserUIDs(ctx context.Context, db *sql.DB, profiles func(context.Context) ([]addonkit.User, error)) error {
	mark, err := uidMapMark(ctx, db)
	if err != nil {
		return err
	}
	var done string
	err = db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, uidMapKey).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done == mark {
		return nil
	}
	var todo int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM follows WHERE user_uid IS NULL AND user_id IS NOT NULL)
		+ (SELECT COUNT(*) FROM pinned_releases WHERE pinned_by_uid IS NULL AND pinned_by IS NOT NULL)
		+ (SELECT COUNT(*) FROM torrents WHERE added_by_uid IS NULL AND added_by IS NOT NULL)
		+ (SELECT COUNT(*) FROM acquisitions WHERE added_by_uid IS NULL AND added_by IS NOT NULL)`).Scan(&todo); err != nil {
		return err
	}
	if todo == 0 {
		// nothing to map (a new database): the core is not asked
		_, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, uidMapKey, mark)
		return err
	}
	us, err := profiles(ctx)
	if err != nil {
		return fmt.Errorf("follow: profiles for the uid mapping: %w", err)
	}
	byID := make(map[string]string, len(us))
	for _, u := range us {
		if u.UID == "" || u.ID <= 0 {
			return errProfileList
		}
		byID[strconv.FormatInt(u.ID, 10)] = u.UID
	}
	if len(byID) == 0 {
		return errProfileList
	}
	b, err := json.Marshal(byID)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// the ids the list has get its uid; the rest are deleted profiles'
	for _, q := range []string{
		`UPDATE follows SET user_uid = (SELECT value FROM json_each(?1) WHERE key = CAST(follows.user_id AS TEXT))
			WHERE user_uid IS NULL AND user_id IS NOT NULL`,
		`UPDATE pinned_releases SET pinned_by_uid = (SELECT value FROM json_each(?1) WHERE key = CAST(pinned_releases.pinned_by AS TEXT))
			WHERE pinned_by_uid IS NULL AND pinned_by IS NOT NULL`,
		`UPDATE torrents SET added_by_uid = (SELECT value FROM json_each(?1) WHERE key = CAST(torrents.added_by AS TEXT))
			WHERE added_by_uid IS NULL AND added_by IS NOT NULL`,
		`UPDATE acquisitions SET added_by_uid = (SELECT value FROM json_each(?1) WHERE key = CAST(acquisitions.added_by AS TEXT))
			WHERE added_by_uid IS NULL AND added_by IS NOT NULL`,
	} {
		if _, err := tx.ExecContext(ctx, q, string(b)); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`UPDATE follows SET user_id = NULL, user_gone = 1 WHERE user_uid IS NULL AND user_id IS NOT NULL`,
		`UPDATE pinned_releases SET pinned_by = NULL WHERE pinned_by_uid IS NULL AND pinned_by IS NOT NULL`,
		`UPDATE torrents SET added_by = NULL WHERE added_by_uid IS NULL AND added_by IS NOT NULL`,
		`UPDATE acquisitions SET added_by = NULL WHERE added_by_uid IS NULL AND added_by IS NOT NULL`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, uidMapKey, mark); err != nil {
		return err
	}
	return tx.Commit()
}

// uidMapMark is the uidMapKey value of the database as it is: the import's
// stamp, if any.
func uidMapMark(ctx context.Context, db *sql.DB) (string, error) {
	var stamp string
	err := db.QueryRowContext(ctx, `SELECT stamp FROM import_state WHERE id = 1`).Scan(&stamp)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	return "done:" + stamp, nil
}
