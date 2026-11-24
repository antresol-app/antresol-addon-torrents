package pinned

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

// keepFailedFor is how long a source that fails the scan of a title keeps
// its earlier rows: a week since it last answered it. The scan runs daily,
// so a week is a week of rounds in which the source could not be asked, an
// outage (a tracker down, a login refused, FlareSolverr gone) and not a
// hiccup; a link a tracker has not confirmed for that long is more likely
// dead than alive, and a source that stays broken (or is left paused) must
// not keep an episode «available» for ever. The core stamps every row a
// scan hands it with the scan's own time, so the week is counted from
// torrent_scan_answers, not from the rows' last_seen.
const keepFailedFor = 7 * 24 * time.Hour

// settle is what a scan of a title does with the sources that failed in it
// (res.Failed) while others answered: the core replaces the title's
// torrent rows with whatever the scan returns, so a failed source's rows
// would go, and come back as new when it answers again (its episodes new
// to «Вышло новое», their first_seen lost). settle returns which earlier
// rows (prev) to hand back with the scan's own: those of a failed source
// that answered the scan of this title within keepFailedFor. A source that
// answered is replaced by what it found, one that was not asked (switched
// off, removed) is dropped as before. It also records, in the addon's own
// store (it survives a restart), when each source last answered, and
// forgets those that took no part.
func (c *torrentScanner) settle(ctx context.Context, titleID int64, res indexer.SearchResponse, prev []tracking.Source, now time.Time) (func(tracking.Source) bool, error) {
	answers, err := c.pins.scanAnswers(ctx, titleID)
	if err != nil {
		return nil, err
	}
	trackerOf := map[string]int64{} // a row's source id -> the failed tracker it is of
	var failed []int64
	for _, e := range res.Failed {
		failed = append(failed, e.Tracker())
		for _, id := range e.Ids() {
			trackerOf[strconv.FormatInt(id, 10)] = e.Tracker()
		}
	}
	// a tracker with no answer on record (rows of a scan from before the
	// record was kept) last answered when its rows were last seen
	lastSeen := map[int64]time.Time{}
	for _, x := range prev {
		if k, ok := trackerOf[x.Meta.SourceID]; ok && x.LastSeen.After(lastSeen[k]) {
			lastSeen[k] = x.LastSeen
		}
	}
	live := map[int64]bool{}
	seeds := map[int64]time.Time{}
	for _, k := range failed {
		at, ok := answers[k]
		if !ok {
			if at = lastSeen[k]; !at.IsZero() {
				seeds[k] = at
			}
		}
		live[k] = !at.IsZero() && now.Sub(at) < keepFailedFor
	}
	if err := c.pins.saveScanAnswers(ctx, titleID, res.AnsweredIDs, seeds, failed, now); err != nil {
		return nil, err
	}
	return func(x tracking.Source) bool {
		k, ok := trackerOf[x.Meta.SourceID]
		return ok && live[k]
	}, nil
}

// scanAnswers is when each source (by its tracker's id) last answered the
// scan of the title.
func (s *Service) scanAnswers(ctx context.Context, titleID int64) (map[int64]time.Time, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT source_id, answered_at FROM torrent_scan_answers WHERE title_id=?`, titleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var id int64
		var at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			out[id] = t
		}
	}
	return out, rows.Err()
}

// saveScanAnswers records that the answered sources answered the scan of
// the title at now, and the seeds (the sources that failed with no answer
// on record: when their rows were last seen) unless one is kept already;
// the record of a source in neither list nor in failed is dropped.
func (s *Service) saveScanAnswers(ctx context.Context, titleID int64, answered []int64, seeds map[int64]time.Time, failed []int64, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now.UTC().Format(time.RFC3339)
	ids := map[int64]bool{}
	for _, id := range answered {
		ids[id] = true
		if _, err := tx.ExecContext(ctx, `INSERT INTO torrent_scan_answers(title_id, source_id, answered_at) VALUES(?,?,?)
			ON CONFLICT(title_id, source_id) DO UPDATE SET answered_at=excluded.answered_at`, titleID, id, ts); err != nil {
			return err
		}
	}
	for id, at := range seeds {
		if _, err := tx.ExecContext(ctx, `INSERT INTO torrent_scan_answers(title_id, source_id, answered_at) VALUES(?,?,?)
			ON CONFLICT(title_id, source_id) DO NOTHING`, titleID, id, at.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	for _, id := range failed {
		ids[id] = true
	}
	q, args := `DELETE FROM torrent_scan_answers WHERE title_id=?`, []any{titleID}
	if len(ids) > 0 {
		q += ` AND source_id NOT IN (?` + strings.Repeat(",?", len(ids)-1) + `)`
		for id := range ids {
			args = append(args, id)
		}
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return err
	}
	return tx.Commit()
}
