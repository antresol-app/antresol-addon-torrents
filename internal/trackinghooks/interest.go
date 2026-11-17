package trackinghooks

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol/addonkit"
)

// interest answers notify.interest: the author of the title's active
// follow, told of its new episodes besides those who marked it, while
// that profile is still there, by uid (a deleted profile's
// follow tells nobody, nor a new profile given its id; nothing deletes
// the follow). The ids are the profiles' ids now, sorted, each once.
// Without the profile list (the core did not answer) the hook fails: the
// core then adds nobody from this addon this time.
func (d Deps) interest(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var q addonkit.InterestRequest
	if err := json.Unmarshal(in, &q); err != nil {
		return nil, err
	}
	out := addonkit.InterestAnswer{UserIDs: []int64{}}
	rows, err := d.DB.QueryContext(ctx, `SELECT user_uid FROM follows
		WHERE kind = ? AND tmdb_id = ? AND active = 1 AND user_uid IS NOT NULL AND user_uid != ''`, q.Kind, q.TMDBID)
	if err != nil {
		return nil, err
	}
	var uids []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			rows.Close()
			return nil, err
		}
		uids = append(uids, uid)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(uids) == 0 || d.Users == nil {
		return out, nil // no follow; or no profile list to tell them by (none is told)
	}
	profiles, err := d.Users.Profiles(ctx)
	if err != nil {
		return nil, err
	}
	// a deleted profile's follow is its own for good: a new profile that
	// gets the id later inherits no letters
	if err := follow.ForgetGone(ctx, d.DB, profiles); err != nil {
		return nil, err
	}
	for _, p := range profiles {
		if p.UID != "" && slices.Contains(uids, p.UID) {
			out.UserIDs = append(out.UserIDs, p.ID)
		}
	}
	slices.Sort(out.UserIDs)
	out.UserIDs = slices.Compact(out.UserIDs)
	return out, nil
}
