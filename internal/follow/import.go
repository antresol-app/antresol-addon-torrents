package follow

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// Sources of imported follows (follows.source); Vault's own are "vault".
const (
	SourceSonarr = "sonarr"
	SourceRadarr = "radarr"
)

// ImportState is what a follow imported from Sonarr remembers: episodes
// that aired before Baseline (the import) and are not in Want count as
// had — Sonarr was not told to get them — while later ones are followed
// as usual. Want: [season, episode] Sonarr monitored and lacked.
type ImportState struct {
	Baseline time.Time `json:"importBaseline"`
	Want     [][2]int  `json:"want"`
}

// ImportFollow is a follow taken over from Sonarr or Radarr.
type ImportFollow struct {
	Kind   string
	TmdbID int
	Title  string
	Poster string
	Source string // sonarr | radarr
	Import *ImportState
}

// ImportInactive adds an inactive follow (switched on by ActivateImported,
// «Перейти на Vault»); a title already followed is left as it is
// (created false). It needs no Service: without a TMDB key there is none.
// by is the admin running the migration (its X-Antresol-User id and uid,
// migrate.Runner.StartAs): without its uid the follow is nobody's.
func ImportInactive(ctx context.Context, db *sql.DB, by addonkit.User, in ImportFollow) (created bool, err error) {
	if (in.Kind != KindSeries && in.Kind != KindMovie) || in.TmdbID <= 0 || (in.Source != SourceSonarr && in.Source != SourceRadarr) {
		return false, ErrInvalid
	}
	raw, err := json.Marshal(state{Import: in.Import})
	if err != nil {
		return false, err
	}
	id, uid := owner(by)
	res, err := db.ExecContext(ctx, `INSERT INTO follows (kind, tmdb_id, title, poster, user_id, user_uid, active, source, state_json, created_at)
		VALUES (?,?,?,?,?,?,0,?,?,?) ON CONFLICT(kind, tmdb_id) DO NOTHING`,
		in.Kind, in.TmdbID, in.Title, in.Poster, id, uid, in.Source, string(raw), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ActivateImported switches on the inactive follows imported from Sonarr
// and Radarr that were never checked, tells OnFollowed of each (the core
// tracks the title from now on) and checks each soon (Kick). Returns how
// many. A follow already checked once (switched on before) and then
// switched off by the user stays off when this is called again.
func (s *Service) ActivateImported(ctx context.Context) (int, error) {
	rows, err := s.DB.QueryContext(ctx, `UPDATE follows SET active=1
		WHERE active=0 AND source IN ('sonarr','radarr') AND last_checked IS NULL RETURNING id`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		// the row alone (no author: OnFollowed needs the title, and one
		// profile lookup per follow would be a core call each)
		f, _, err := scanFollow(s.DB.QueryRowContext(ctx, `SELECT `+followCols+` FROM follows WHERE id=?`, id))
		if err != nil {
			if ctx.Err() == nil {
				s.log().Warn("follow: a switched-on follow not told (the core tracks it at its next round)", "id", id, "err", err)
			}
		} else {
			s.followed(ctx, f)
		}
		s.Kick(id)
	}
	return len(ids), nil
}
