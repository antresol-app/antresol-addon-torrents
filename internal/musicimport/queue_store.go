//go:build ignore

package musicimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
)

// queueStore is the download queue's SQL over the import tables: the
// download-only moves of a group, on top of the core store.
type queueStore struct{ *playlistimport.Store }

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// activeGroups counts the albums being searched or downloaded for a
// running job. A cancelled or failed job's downloads go on (they are
// library content) but no longer hold the import's slots.
func (s queueStore) activeGroups(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_groups g JOIN import_jobs j ON j.id = g.job_id
		WHERE g.state IN ('searching', 'downloading') AND j.status = 'downloading'`).Scan(&n)
	return n, err
}

// queueCandidates returns the job's albums the queue may start, oldest
// first: waiting, waiting for space, and failed ones due for their
// automatic retry (failed once, at or before retryBefore).
func (s queueStore) queueCandidates(ctx context.Context, jobID int64, retryBefore time.Time) ([]playlistimport.Group, error) {
	return s.GroupsWhere(ctx, `g.job_id = ? AND (g.state IN ('waiting', 'waiting_space', 'waiting_download')
		OR g.state = 'failed' AND g.attempts <= ? AND g.updated_at <= ?)`, jobID, playlistimport.AutoRetries, stamp(retryBefore))
}

// moveGroup moves an album from one state to another (compare-and-set:
// false when it was not in from, e.g. a cancel dropped it).
func (s queueStore) moveGroup(ctx context.Context, id int64, from, to string, at time.Time) (bool, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE import_groups SET state = ?, updated_at = ? WHERE id = ? AND state = ?`, to, stamp(at), id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// waitFor records the download an album waits for (another job's, or
// «На сервер»; named name, done bytes so far): the album is
// waiting_download, and existing finds the album that download brings.
// The progress clock (wait_at) restarts with another download or more
// bytes.
func (s queueStore) waitFor(ctx context.Context, g playlistimport.Group, ih, name string, done int64, at time.Time) error {
	since := g.WaitAt
	if g.State != playlistimport.GroupWaitingDownload || g.InfoHash != ih || done > g.WaitDone || since.IsZero() {
		since = at
	}
	_, err := s.DB.ExecContext(ctx, `UPDATE import_groups SET state = ?, infohash = ?, wait_title = ?, wait_done = ?, wait_at = ?,
		updated_at = CASE WHEN state = ? THEN updated_at ELSE ? END
		WHERE id = ? AND state IN ('waiting', 'waiting_space', 'waiting_download', 'failed')`,
		playlistimport.GroupWaitingDownload, ih, name, max(done, g.WaitDone), stamp(since), playlistimport.GroupWaitingDownload, stamp(at), g.ID)
	return err
}

// giveUpWait stops an album waiting for the download ih (paused, failed,
// removed or stalled): it waits for its own search, which leaves ih out.
func (s queueStore) giveUpWait(ctx context.Context, id int64, ih string, at time.Time) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE import_groups SET state = ?, skip_infohash = ?, wait_title = '', wait_done = 0, wait_at = NULL,
		updated_at = ? WHERE id = ? AND state = ?`, playlistimport.GroupWaiting, ih, stamp(at), id, playlistimport.GroupWaitingDownload)
	return err
}

// startGroup records a started download: the album downloads ih (bytes),
// its songs downloading are on the way, missing ones are not in the
// release.
func (s queueStore) startGroup(ctx context.Context, groupID int64, ih string, bytes int64, downloading, missing []int64, at time.Time) (bool, error) {
	moved := false
	err := s.InTx(ctx, func(tx *sql.Tx) error {
		// the progress clock (wait_done, wait_at) starts with the download
		res, err := tx.ExecContext(ctx, `UPDATE import_groups SET state = ?, infohash = ?, bytes = ?, error = '', updated_at = ?,
			wait_title = '', wait_done = 0, wait_at = ?, run_s = 0, quiet_s = 0, run_base = 0 WHERE id = ? AND state = ?`,
			playlistimport.GroupDownloading, ih, bytes, stamp(at), stamp(at), groupID, playlistimport.GroupSearching)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil // cancelled meanwhile: the download stays on the torrent screen
		}
		moved = true
		if err := playlistimport.SetTracks(ctx, tx, playlistimport.TrackDownloading, "", downloading, nil); err != nil {
			return err
		}
		return playlistimport.SetTracks(ctx, tx, playlistimport.TrackNotFound, "", missing, nil)
	})
	return moved, err
}

// saveClock stores the clock of the album's download ih.
func (s queueStore) saveClock(ctx context.Context, id int64, ih string, c playlistimport.RunClock) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE import_groups SET run_s = ?, quiet_s = ?, run_base = ?, wait_done = ?
		WHERE id = ? AND state = ? AND infohash = ?`,
		int64(c.Run/time.Second), int64(c.Quiet/time.Second), c.Base, c.Done, id, playlistimport.GroupDownloading, ih)
	return err
}

// markGivenUp lists the album's download ih as given up — dead
// (dead_json: never searched again) or slow (slow_json: only when nothing
// else holds the songs) — before it is removed, so that a removal whose
// requeue fails is not taken for an admin's removal on the next tick.
func (s queueStore) markGivenUp(ctx context.Context, id int64, ih string, slow bool) error {
	col := "dead_json"
	if slow {
		col = "slow_json"
	}
	return s.InTx(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT `+col+` FROM import_groups WHERE id = ? AND state = ? AND infohash = ?`,
			id, playlistimport.GroupDownloading, ih).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var list []string
		_ = json.Unmarshal([]byte(raw), &list)
		if slices.Contains(list, ih) {
			return nil
		}
		b, err := json.Marshal(append(list, ih))
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE import_groups SET `+col+` = ? WHERE id = ?`, string(b), id)
		return err
	})
}

// requeue puts a downloading album back in the queue: its download ih
// went (given up, or replaced by another download of the album). Its
// songs on the way and those the release lacked are matched again. When
// ih is the maxDead-th dead download it fails instead with msg (only
// «Повторить» is left, which forgets them).
func (s queueStore) requeue(ctx context.Context, id int64, ih, msg string, maxDead int, at time.Time) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		var raw string
		err := tx.QueryRowContext(ctx, `SELECT dead_json FROM import_groups WHERE id = ? AND state = ? AND infohash = ?`,
			id, playlistimport.GroupDownloading, ih).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // it ended or went meanwhile
		}
		if err != nil {
			return err
		}
		var dead []string
		_ = json.Unmarshal([]byte(raw), &dead)
		if _, err := tx.ExecContext(ctx, `UPDATE import_groups SET state = ?, infohash = '', bytes = 0, wait_title = '',
			wait_done = 0, wait_at = NULL, run_s = 0, quiet_s = 0, run_base = NULL, updated_at = ? WHERE id = ?`,
			playlistimport.GroupWaiting, stamp(at), id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE import_tracks SET state = ?, reason = '' WHERE group_id = ? AND state IN (?, ?)`,
			playlistimport.TrackMatched, id, playlistimport.TrackDownloading, playlistimport.TrackNotFound); err != nil {
			return err
		}
		if !slices.Contains(dead, ih) || len(dead) < maxDead {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE import_tracks SET state = ? WHERE group_id = ? AND state = ?`,
			playlistimport.TrackFailed, id, playlistimport.TrackMatched); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE import_groups SET state = ?, error = ?, attempts = MAX(attempts, ?) WHERE id = ?`,
			playlistimport.GroupFailed, msg, playlistimport.AutoRetries+1, id)
		return err
	})
}

// noFormat settles an album whose releases are all of another format: its
// songs ids are not found until «Скачать в другом формате».
func (s queueStore) noFormat(ctx context.Context, groupID int64, ids []int64, msg string, at time.Time) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		if err := playlistimport.SetTracks(ctx, tx, playlistimport.TrackNotFound, "", ids, nil); err != nil {
			return err
		}
		return playlistimport.Settle(ctx, tx, groupID, playlistimport.GroupNoFormat, msg, at)
	})
}

// failGroup ends an album with an error: its songs ids and any that
// joined it fail with it. retry: a failed search, retried automatically
// once (attempts + 1); else (a download that went wrong) only «Повторить»
// is left.
func (s queueStore) failGroup(ctx context.Context, groupID int64, ids []int64, msg string, retry bool, at time.Time) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		if err := playlistimport.SetTracks(ctx, tx, playlistimport.TrackFailed, "", ids, nil); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE import_tracks SET state = ? WHERE group_id = ? AND state = ?`,
			playlistimport.TrackFailed, groupID, playlistimport.TrackMatched); err != nil {
			return err
		}
		attempts := `attempts + 1`
		if !retry {
			attempts = `MAX(attempts, ` + strconv.Itoa(playlistimport.AutoRetries+1) + `)`
		}
		_, err := tx.ExecContext(ctx, `UPDATE import_groups SET state = ?, error = ?, attempts = `+attempts+`, updated_at = ? WHERE id = ?`,
			playlistimport.GroupFailed, msg, stamp(at), groupID)
		return err
	})
}

// finishGroup ends an organized download: the songs done are linked to
// their library tracks (track id → item), the songs failed were not found
// among the placed files (reason «Ошибка»).
func (s queueStore) finishGroup(ctx context.Context, groupID int64, done map[int64]int64, failed []int64, album int64, at time.Time) error {
	return s.InTx(ctx, func(tx *sql.Tx) error {
		ids := make([]int64, 0, len(done))
		for id := range done {
			ids = append(ids, id)
		}
		if err := playlistimport.SetTracks(ctx, tx, playlistimport.TrackDone, "", ids, done); err != nil {
			return err
		}
		if err := playlistimport.SetTracks(ctx, tx, playlistimport.TrackFailed, msgError, failed, nil); err != nil {
			return err
		}
		if album != 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE import_groups SET album_item_id = ? WHERE id = ?`, album, groupID); err != nil {
				return err
			}
		}
		return playlistimport.Settle(ctx, tx, groupID, playlistimport.GroupDone, "", at)
	})
}

// stillDownloading: the job has not been cancelled meanwhile.
func (s queueStore) stillDownloading(ctx context.Context, jobID int64) bool {
	j, err := s.Job(ctx, jobID)
	return err == nil && j.Status == playlistimport.StatusDownloading
}
