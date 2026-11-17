package acquire

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// PendingAcquisition is a download that is not in the library yet: queued,
// downloading, waiting for space or failed to organize. It covers what the
// release holds, not only what was asked for: Episode 0 = the whole
// season; EpisodeEnd > Episode = the range Episode..EpisodeEnd.
type PendingAcquisition struct {
	Kind                        string
	TMDBID                      int
	Season, Episode, EpisodeEnd int
	InfoHash                    string
}

// Pending lists acquisitions not organized yet («Следить» does not ask for
// them again); a pack downloading several seasons once per season asked
// for (acquisitionMeta.Seasons). Streams are left out (they are never
// organized; a stream that is kept turns into a download), and so are rows
// whose torrent is gone.
func (s *Service) Pending(ctx context.Context) ([]PendingAcquisition, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT a.kind, a.tmdb_id, a.season, a.episode, a.infohash, t.name, t.release_json, a.meta_json
		FROM acquisitions a JOIN torrents t ON lower(t.infohash)=lower(a.infohash)
		WHERE a.organized_at IS NULL AND t.mode<>'stream'
		ORDER BY a.created_at, a.infohash`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingAcquisition
	for rows.Next() {
		var p PendingAcquisition
		var name string
		var rel sql.NullString
		var meta string
		if err := rows.Scan(&p.Kind, &p.TMDBID, &p.Season, &p.Episode, &p.InfoHash, &name, &rel, &meta); err != nil {
			return nil, err
		}
		if p.Kind == KindSeries {
			coverage(&p, name, rel.String)
		}
		// a pack of several seasons: each season asked for and not placed
		// yet, whole (its selection holds the whole season, also when an
		// episode started it)
		var m acquisitionMeta
		if p.Kind == KindSeries && meta != "" && json.Unmarshal([]byte(meta), &m) == nil && len(m.Seasons) > 0 {
			for _, n := range m.Seasons {
				if !slices.Contains(m.Organized, n) {
					out = append(out, PendingAcquisition{Kind: p.Kind, TMDBID: p.TMDBID, Season: n, InfoHash: p.InfoHash})
				}
			}
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// coverage widens a series request to what its release holds: a season
// pack or an episode range (e.g. «S01E03-E08» picked for E03).
func coverage(p *PendingAcquisition, name, rel string) {
	var r struct {
		Title string `json:"title"`
	}
	if rel != "" {
		_ = json.Unmarshal([]byte(rel), &r) // a bad row: the torrent name below
	}
	for _, t := range []string{r.Title, name} {
		if t == "" {
			continue
		}
		info := release.Parse(t)
		if info.Season > 0 && p.Season > 0 && info.Season != p.Season {
			continue
		}
		switch {
		case info.Episode > 0 && info.EpisodeEnd > info.Episode:
			// a range (also «1-10 серии» packs): exactly those episodes
			if p.Episode == 0 || (p.Episode >= info.Episode && p.Episode <= info.EpisodeEnd) {
				p.Episode, p.EpisodeEnd = info.Episode, info.EpisodeEnd
			}
			return
		case info.Season > 0 && info.Episode == 0:
			p.Episode, p.EpisodeEnd = 0, 0 // the whole season
			return
		}
	}
}
