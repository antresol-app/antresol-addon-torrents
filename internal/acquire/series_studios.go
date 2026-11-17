package acquire

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol/addonkit"
)

// seriesStudios returns the voice-over studios of a series already on the
// server, best first: the studios of its episode files' audio tracks (the
// latest season first, more episodes first within a season), then those
// named by the releases of its acquisitions (latest first), then the audio
// tracks profiles chose for it (weakest). A series with
// nothing on the server, or a library that fails to answer, gives nil: the
// search goes on with the household's list alone.
func (s *Service) seriesStudios(ctx context.Context, tmdbID int) []string {
	if s.o.DB == nil || tmdbID <= 0 {
		return nil
	}
	var sa addonkit.SeriesAudio // the audio of its episode files, the voices profiles chose
	if s.o.Library != nil {
		var err error
		if sa, err = s.o.Library.SeriesAudio(ctx, int64(tmdbID)); err != nil {
			s.log.Warn("acquire: series studios", "tmdb", tmdbID, "err", err)
			return nil
		}
	}
	var out []string
	seen := map[string]bool{}
	add := func(names []string) {
		for _, n := range names {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	for _, step := range []func(context.Context, int) ([]string, error){
		func(context.Context, int) ([]string, error) { return fileStudios(sa), nil },
		s.acquisitionStudios,
		func(context.Context, int) ([]string, error) { return profileStudios(sa), nil },
	} {
		names, err := step(ctx, tmdbID)
		if err != nil {
			s.log.Warn("acquire: series studios", "tmdb", tmdbID, "err", err)
			return nil
		}
		add(names)
	}
	return out
}

// trackStudios: the studios an audio track title names ("LostFilm",
// "MVO | Кураж-Бамбей").
func trackStudios(title string) []string {
	if c, ok := release.CanonicalStudio(title); ok {
		return []string{c}
	}
	return release.Studios(title)
}

// fileStudios: the studios of the series' episode files by season (latest
// first; the latest two seasons with audio track titles) and, within a
// season, by the number of episodes.
func fileStudios(sa addonkit.SeriesAudio) []string {
	type tally struct {
		season, count, first int
		name                 string
	}
	type key struct {
		season int
		name   string
	}
	var seasons []int
	for _, f := range sa.Files {
		if len(f.Titles) > 0 && !slices.Contains(seasons, f.Season) {
			seasons = append(seasons, f.Season)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(seasons)))
	latest := seasons[:min(len(seasons), 2)]
	byKey := map[key]*tally{}
	var all []*tally
	inFile := map[string]bool{} // studios counted for the current file
	for _, f := range sa.Files {
		if !slices.Contains(latest, f.Season) {
			continue
		}
		clear(inFile)
		for _, title := range f.Titles {
			for _, n := range trackStudios(title) {
				if inFile[n] {
					continue
				}
				inFile[n] = true
				k := key{f.Season, n}
				t := byKey[k]
				if t == nil {
					t = &tally{season: f.Season, first: len(all), name: n}
					byKey[k] = t
					all = append(all, t)
				}
				t.count++
			}
		}
	}
	sort.SliceStable(all, func(a, b int) bool {
		if all[a].season != all[b].season {
			return all[a].season > all[b].season
		}
		if all[a].count != all[b].count {
			return all[a].count > all[b].count
		}
		return all[a].first < all[b].first
	})
	out := make([]string, 0, len(all))
	for _, t := range all {
		out = append(out, t.name)
	}
	return out
}

// acquisitionStudios: the studios named by the releases that brought the
// series' seasons and episodes, the latest season (then the latest
// acquisition) first.
func (s *Service) acquisitionStudios(ctx context.Context, tmdbID int) ([]string, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT t.name, t.release_json
		FROM acquisitions a JOIN torrents t ON lower(t.infohash)=lower(a.infohash)
		WHERE a.kind='series' AND a.tmdb_id=?
		ORDER BY a.season DESC, a.created_at DESC, a.infohash`, tmdbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		var rel sql.NullString
		if err := rows.Scan(&name, &rel); err != nil {
			return nil, err
		}
		var r struct {
			Title string `json:"title"`
		}
		if rel.String != "" {
			_ = json.Unmarshal([]byte(rel.String), &r) // a bad row: the torrent name
		}
		if r.Title == "" {
			r.Title = name
		}
		out = append(out, release.Parse(r.Title).Studios...)
	}
	return out, rows.Err()
}

// profileStudios: the audio tracks profiles chose for the series (any
// profile, the latest choice first).
func profileStudios(sa addonkit.SeriesAudio) []string {
	var out []string
	for _, p := range sa.Profiles {
		if p.AudioTitle != "" {
			out = append(out, trackStudios(p.AudioTitle)...)
		}
	}
	return out
}
