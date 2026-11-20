package acquire

import (
	"context"
	"database/sql"
	"errors"
	"path"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// WatchTitle is the title a «Смотреть сейчас» input of file (its path in
// the torrent) belongs to: the acquisition's kind, TMDB id, season and
// episode, the episode of a season pack read from the file's name. false:
// the torrent has no acquisition of a movie or series.
func (s *Service) WatchTitle(ctx context.Context, ih, file string) (kind string, tmdbID, season, episode int, ok bool) {
	a, err := s.acquisition(ctx, strings.ToLower(ih))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Warn("watch title", "infohash", ih, "err", err)
		}
		return "", 0, 0, 0, false
	}
	if a.TMDBID <= 0 || (a.Kind != KindMovie && a.Kind != KindSeries) {
		return "", 0, 0, 0, false
	}
	season, episode = a.Season, a.Episode
	if a.Kind == KindSeries && episode == 0 {
		if info := release.Parse(path.Base(file)); info.Episode > 0 {
			season, episode = info.Season, info.Episode
		}
	}
	return a.Kind, a.TMDBID, season, episode, true
}

// SetOnRemoved sets the function called with a download's key after
// Remove took it away (a torrent or a direct download): the watch inputs
// of it are dropped there.
func (s *Service) SetOnRemoved(fn func(key string)) { s.onRemoved.Store(&fn) }

func (s *Service) removed(key string) {
	s.releases.Delete(key)
	if fn := s.onRemoved.Load(); fn != nil {
		(*fn)(strings.ToLower(key))
	}
}

// RequestTitleInfo is a catalog title the addon knows from its own
// downloads (requests.title).
type RequestTitleInfo struct {
	Title, Creator string
	Year           int
	Poster         string // a catalog's cover URL ("" = none)
}

// RequestTitle names the album, book, audiobook or comic of catalog id
// extID from the newest acquisition of it; false: the addon never got it
// (its catalogs have no lookup by id; the core keeps the kid's text then).
func (s *Service) RequestTitle(ctx context.Context, kind, extID string) (RequestTitleInfo, bool) {
	if extID == "" || kind == KindMovie || kind == KindSeries {
		return RequestTitleInfo{}, false
	}
	a, err := scanAcquisition(s.o.DB.QueryRowContext(ctx, `SELECT `+acquisitionCols+` FROM acquisitions WHERE kind=? AND ext_id=? ORDER BY created_at DESC LIMIT 1`, kind, extID))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Warn("request title", "kind", kind, "extId", extID, "err", err)
		}
		return RequestTitleInfo{}, false
	}
	out := RequestTitleInfo{Title: a.Title, Creator: a.Meta.Author, Year: a.Year, Poster: s.RequestPoster(kind, extID, "")}
	if kind == KindMusic {
		out.Creator = a.Meta.Artist
	}
	if out.Year == 0 {
		out.Year = a.Meta.Year
	}
	if out.Title == "" {
		return RequestTitleInfo{}, false
	}
	return out, true
}
