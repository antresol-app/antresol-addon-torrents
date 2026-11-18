package acquire

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
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
// them again). A pack downloading some of its seasons comes once per
// season asked for and not placed yet (acquisitionMeta.Seasons), whole.
// Any other series download covers the episodes its selected files will
// be placed as (organize.SeriesEpisodes), one row each, once the engine
// has its file list: a pack of several seasons added whole marks all of
// them; before that (or when no file tells) what its release name says —
// a range of seasons once per season («S01-S03»), else its season or
// episode range (coverage). Streams are left out (they are never
// organized; a stream that is kept turns into a download), and so are rows
// whose torrent is gone. A download whose organize failed for good
// (maxOrganizeAttempts) still covers what it holds while its files are on
// the disk — the admin sees the failure in «Загрузки» and in the notice,
// and a retry places them; looking for them again would download the same
// episodes once more — and stops covering once its files are gone.
func (s *Service) Pending(ctx context.Context) ([]PendingAcquisition, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT a.kind, a.tmdb_id, a.season, a.episode, a.infohash, t.name, t.release_json, a.meta_json,
		t.save_dir, a.organize_error != '' AND a.organize_attempts >= ?
		FROM acquisitions a JOIN torrents t ON lower(t.infohash)=lower(a.infohash)
		WHERE a.organized_at IS NULL AND t.mode<>'stream'
		ORDER BY a.created_at, a.infohash`, maxOrganizeAttempts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingAcquisition
	for rows.Next() {
		var p PendingAcquisition
		var name string
		var rel sql.NullString
		var meta, saveDir string
		var failed bool
		if err := rows.Scan(&p.Kind, &p.TMDBID, &p.Season, &p.Episode, &p.InfoHash, &name, &rel, &meta, &saveDir, &failed); err != nil {
			return nil, err
		}
		if failed && !s.dataOnDisk(p.InfoHash, saveDir, name) {
			continue // nothing left to place: those are wanted again
		}
		var m acquisitionMeta
		if p.Kind == KindSeries && meta != "" {
			_ = json.Unmarshal([]byte(meta), &m) // a bad row: no seasons
		}
		if p.Kind == KindSeries {
			// a pack of seasons asked for one by one covers those seasons
			// whole (below); any other download what its files hold
			if eps := s.fileCoverage(p); len(m.Seasons) == 0 && len(eps) > 0 {
				out = append(out, eps...)
				continue
			}
			coverage(&p, name, rel.String)
		}
		// a pack of several seasons: each season asked for and not placed
		// yet, whole (its selection holds the whole season, also when an
		// episode started it)
		if p.Kind == KindSeries && len(m.Seasons) == 0 {
			if lo, hi := seasonsNamed(name, rel.String); hi > lo {
				for n := lo; n <= hi; n++ {
					out = append(out, PendingAcquisition{Kind: p.Kind, TMDBID: p.TMDBID, Season: n, InfoHash: p.InfoHash})
				}
				continue
			}
		}
		if p.Kind == KindSeries && len(m.Seasons) > 0 {
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
		if se, ep := dashSeasonEpisode(t); se > 0 {
			// «S3 - 08»: one episode, not the whole season
			info.Season, info.Episode, info.EpisodeEnd = se, ep, ep
		}
		if info.Season > 0 && p.Season > 0 && info.Season != p.Season {
			continue
		}
		if p.Season == 0 && info.Season > 0 {
			// a download asked for with no season (a hand-picked release)
			// covers the season its name tells, not the whole series
			p.Season = info.Season
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
		case info.Episode > 0 && p.Episode == 0 && info.Season == p.Season:
			// one episode picked with no episode asked for: that episode
			p.Episode, p.EpisodeEnd = info.Episode, 0
			return
		}
	}
}

// fileCoverage is what a series download's selected files will be placed
// as (organize.SeriesEpisodes, as its organize reads them), one row per
// episode or multi-episode file, sorted. Nil before the engine has the file list,
// or when no file tells its episode.
func (s *Service) fileCoverage(p PendingAcquisition) []PendingAcquisition {
	if s.o.Engine == nil {
		return nil
	}
	h, ok := s.o.Engine.Get(p.InfoHash)
	if !ok || h == nil {
		return nil
	}
	var paths []string
	for _, f := range h.Files() {
		if f.Selected {
			paths = append(paths, f.Path)
		}
	}
	var out []PendingAcquisition
	seen := map[[3]int]bool{}
	for _, k := range organize.SeriesEpisodes(paths, p.Season, p.Episode) {
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, PendingAcquisition{Kind: p.Kind, TMDBID: p.TMDBID, Season: k[0], Episode: k[1], EpisodeEnd: k[2], InfoHash: p.InfoHash})
	}
	slices.SortFunc(out, func(a, b PendingAcquisition) int {
		if a.Season != b.Season {
			return a.Season - b.Season
		}
		return a.Episode - b.Episode
	})
	return out
}

// seasonsNamed is the range of seasons a release name says it holds
// (indexer.SeasonRange of its pick's title, else the torrent name): a
// pack added whole before its file list is known covers each of them.
func seasonsNamed(name, rel string) (lo, hi int) {
	var r struct {
		Title string `json:"title"`
	}
	if rel != "" {
		_ = json.Unmarshal([]byte(rel), &r)
	}
	for _, t := range []string{r.Title, name} {
		if t == "" {
			continue
		}
		if se, _ := dashSeasonEpisode(t); se > 0 {
			return 0, 0 // «S3 - 08»: season 3, episode 8
		}
		if lo, hi = indexer.SeasonRange(t); lo > 0 {
			return lo, hi
		}
	}
	return 0, 0
}

// reDashSeasonEpisode is the anime-style «S3 - 08»: a season, then the
// episode after a spaced dash (a range of seasons says «S1-S3», «S1-3» or
// «Seasons 1 - 3»).
var reDashSeasonEpisode = regexp.MustCompile(`(?i)(?:^|[\s\[(._])S(\d{1,2})\s+-\s+(\d{1,4})(?:v\d)?(?:$|[\s\[(.])`)

// dashSeasonEpisode is the season and episode of an «S3 - 08» name, 0, 0
// for any other.
func dashSeasonEpisode(title string) (season, episode int) {
	m := reDashSeasonEpisode.FindStringSubmatch(title)
	if m == nil {
		return 0, 0
	}
	season, _ = strconv.Atoi(m[1])
	episode, _ = strconv.Atoi(m[2])
	if season <= 0 || episode <= 0 {
		return 0, 0
	}
	return season, episode
}

// dataOnDisk: some of torrent ih's files are on the disk (its selected
// files once the engine knows them, else its top folder or file in
// saveDir).
func (s *Service) dataOnDisk(ih, saveDir, name string) bool {
	exists := func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	}
	if s.o.Engine != nil {
		if h, ok := s.o.Engine.Get(ih); ok && h != nil {
			if h.Status().DataMissing {
				return false
			}
			known := false
			for _, f := range h.Files() {
				if !f.Selected {
					continue
				}
				known = true
				p := f.FullPath
				if p == "" {
					p = filepath.Join(saveDir, filepath.FromSlash(f.Path))
				}
				if exists(p) {
					return true
				}
			}
			if known {
				return false
			}
		}
	}
	return name != "" && saveDir != "" && exists(filepath.Join(saveDir, name))
}
