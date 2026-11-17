package pinned

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// What tracking does with a followed title's pinned release. The owner's
// rule: a title only marked is never downloaded — its new episodes are in
// the feed and the letters (tracking does that), nothing more. Only an
// active follow («Докачивать новые», turned on by hand) downloads, and
// then from the pinned release itself, only the episodes it lacks.

// FollowLookup tells the active follows (*follow.Service).
type FollowLookup interface {
	// Following: the title has an active follow, and its author (zero =
	// unknown; its uid keys the download).
	Following(ctx context.Context, kind string, tmdbID int64) (by addonkit.User, active bool, err error)
}

// Grower is what the actions need of «На сервер» (*acquire.Service).
type Grower interface {
	// AcquirePinnedFiles downloads only files of the pinned release ih.
	AcquirePinnedFiles(ctx context.Context, ih string, req acquire.AcquireRequest, files []int) (acquire.AcquireResult, error)
	// Acquire: a film's pinned release, whole.
	Acquire(ctx context.Context, req acquire.AcquireRequest) (acquire.AcquireResult, error)
	// Pending: the downloads not imported yet (the follow's own among them).
	Pending(ctx context.Context) ([]acquire.PendingAcquisition, error)
}

// Actions download the missing episodes of followed titles' pins.
type Actions struct {
	Pins    *Service
	Follows FollowLookup
	Acquire Grower

	mu    sync.Mutex
	locks map[int64]*titleLock // one Sync per title at a time
}

type titleLock struct {
	mu   sync.Mutex
	refs int
}

// lock takes the title's Sync lock and returns its release.
func (a *Actions) lock(titleID int64) func() {
	a.mu.Lock()
	if a.locks == nil {
		a.locks = map[int64]*titleLock{}
	}
	l := a.locks[titleID]
	if l == nil {
		l = &titleLock{}
		a.locks[titleID] = l
	}
	l.refs++
	a.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		a.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(a.locks, titleID)
		}
		a.mu.Unlock()
	}
}

// Sync runs after each pinned scan of a title (Service.AfterScan) and when
// its follow is turned on. Nothing happens unless the title has an active
// follow. Then the episodes the pin holds that have aired (an unknown air
// date counts as aired: the file is there), the library lacks (no library
// source) and the pin's download does not select yet are wanted: their
// files (video and external tracks, EpisodesOf) are downloaded from the
// pin and nothing else (AcquirePinnedFiles: grown into the pin's download,
// a stream narrowed to them and downloaded, or the release added with only
// them selected). A wanted episode whose download failed is wanted again
// at the next scan. Episodes another download not imported yet holds
// (Pending: the follow's own pick of an episode, or a season pack) are not
// wanted either; nor are files of a pin download that failed counted as
// selected. A film's pin is downloaded whole when the library lacks the
// film and no download of it is pending. One Sync per title runs at a
// time. The calls run on a ctx that does not end with the scan's.
func (a *Actions) Sync(ctx context.Context, t tracking.Title) error {
	if a.Follows == nil || a.Acquire == nil || a.Pins == nil {
		return nil
	}
	defer a.lock(t.ID)()
	by, active, err := a.Follows.Following(ctx, t.Kind, t.TMDBID)
	if err != nil || !active {
		return err // marked only: never downloaded
	}
	p, ok, err := a.Pins.ByTitle(ctx, t.ID)
	if err != nil || !ok {
		return err
	}
	have, err := a.libraryEpisodes(ctx, t.ID)
	if err != nil {
		return err
	}
	req, err := pinRequest(t, p, by)
	if err != nil {
		return err
	}
	bg := context.WithoutCancel(ctx)
	var h torrent.Handle
	inEngine := false
	if a.Pins.Engine != nil {
		h, inEngine = a.Pins.Engine.Get(p.InfoHash)
	}
	downloading := inEngine && h != nil && h.Status().Mode == torrent.ModeDownload
	pending, err := a.Acquire.Pending(ctx)
	if err != nil {
		return err
	}
	if t.Kind == "movie" {
		if have[[2]int{0, 0}] || downloading {
			return nil
		}
		for _, x := range pending {
			if x.Kind == acquire.KindMovie && int64(x.TMDBID) == t.TMDBID {
				return nil // the film downloads already (the follow's own pick)
			}
		}
		_, err := a.Acquire.Acquire(bg, req)
		return err
	}
	// what other downloads of the series hold: episodes and whole seasons
	// (a multi-season pack comes as one row per season); a whole-series
	// pack (season 0, no episode) holds every season
	packs := map[int]bool{}
	wholeSeries := false
	for _, x := range pending {
		if x.Kind != acquire.KindSeries || int64(x.TMDBID) != t.TMDBID || strings.EqualFold(x.InfoHash, p.InfoHash) {
			continue
		}
		if x.Season == 0 && x.Episode == 0 {
			wholeSeries = true
			continue
		}
		if x.Episode == 0 {
			packs[x.Season] = true
			continue
		}
		for e := x.Episode; e <= max(x.Episode, x.EpisodeEnd); e++ {
			have[[2]int{x.Season, e}] = true
		}
	}
	selected := map[int]bool{}
	if downloading && h.Status().Status != torrent.StatusError {
		for _, f := range h.Files() {
			if f.Selected {
				selected[f.Index] = true
			}
		}
	}
	eps, err := a.Pins.Tracking.Episodes(ctx, t.ID)
	if err != nil {
		return err
	}
	today := a.Pins.now().Format(time.DateOnly)
	future := map[[2]int]bool{}
	for _, e := range eps {
		if e.AirDate != "" && e.AirDate > today {
			future[[2]int{e.Season, e.Number}] = true
		}
	}
	var files []int
	firstSeason := 0
	for k, idx := range EpisodesOf(p.Files, DefaultSeason(eps)) {
		if have[k] || future[k] || packs[k[0]] || wholeSeries {
			continue
		}
		wanted := false
		for _, i := range idx {
			if !selected[i] {
				files, wanted = append(files, i), true
			}
		}
		if wanted && (firstSeason == 0 || k[0] < firstSeason) {
			firstSeason = k[0]
		}
	}
	if len(files) == 0 {
		return nil
	}
	slices.Sort(files)
	files = slices.Compact(files)
	req.Season = firstSeason
	if _, err := a.Acquire.AcquirePinnedFiles(bg, p.InfoHash, req, files); err != nil {
		return fmt.Errorf("pinned: missing episodes of %d: %w", t.ID, err)
	}
	return nil
}

// pinRequest is «На сервер» of the pin's own link (a magnet of its
// infohash when the pin has no link: an own .torrent file).
func pinRequest(t tracking.Title, p Pin, by addonkit.User) (acquire.AcquireRequest, error) {
	link, src := p.Link, int64(0)
	if link == "" {
		link = "magnet:?xt=urn:btih:" + p.InfoHash
	}
	if !isMagnet(link) {
		var err error
		if src, err = strconv.ParseInt(p.SourceID, 10, 64); err != nil {
			return acquire.AcquireRequest{}, fmt.Errorf("pinned: source %q: %w", p.SourceID, err)
		}
	}
	req := acquire.AcquireRequest{TMDBID: int(t.TMDBID), Kind: acquire.KindSeries, Mode: acquire.ModeDownload,
		ReleaseLink: link, SourceID: src, UserID: by.ID, UserUID: by.UID, Background: true}
	if t.Kind == "movie" {
		req.Kind = acquire.KindMovie
	}
	return req, nil
}

// libraryEpisodes are the title's episodes with a library source.
func (a *Actions) libraryEpisodes(ctx context.Context, titleID int64) (map[[2]int]bool, error) {
	srcs, err := a.Pins.Tracking.Sources(ctx, titleID)
	if err != nil {
		return nil, err
	}
	out := map[[2]int]bool{}
	for _, s := range srcs {
		if s.Kind == "library" {
			out[[2]int{s.Season, s.Episode}] = true
		}
	}
	return out, nil
}

// Covered says which episodes of a series the title's pin holds (none for
// a title not tracked or not pinned): follow.Service.Covered, so the
// follow's own search leaves them to the pin.
func (s *Service) Covered(ctx context.Context, tmdbID int64) (map[[2]int]bool, error) {
	out := map[[2]int]bool{}
	t, ok, err := s.Tracking.TitleByTMDB(ctx, "series", tmdbID)
	if err != nil || !ok {
		return out, err
	}
	p, ok, err := s.ByTitle(ctx, t.ID)
	if err != nil || !ok {
		return out, err
	}
	eps, err := s.Tracking.Episodes(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	for k := range EpisodesOf(p.Files, DefaultSeason(eps)) {
		out[k] = true
	}
	return out, nil
}

// FollowedTitles answers the tracking.titles hook: the titles with an
// active follow, so the core tracks (makes and refreshes) a followed title
// though nobody marked it. Name and poster are the follow's (the poster a
// full TMDB URL, which the core keeps as it is).
func FollowedTitles(db *sql.DB) func(ctx context.Context) ([]addonkit.ExtraTitle, error) {
	return func(ctx context.Context) ([]addonkit.ExtraTitle, error) {
		rows, err := db.QueryContext(ctx, `SELECT kind, tmdb_id, title, poster FROM follows
			WHERE active = 1 AND kind IN ('series','movie') ORDER BY id`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []addonkit.ExtraTitle{}
		for rows.Next() {
			var x addonkit.ExtraTitle
			if err := rows.Scan(&x.Kind, &x.TMDBID, &x.Name, &x.Poster); err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, rows.Err()
	}
}

// FilmPinned says a film has a pinned release (follow.Service.FilmPinned:
// the follow's own search leaves it to the pin).
func (s *Service) FilmPinned(ctx context.Context, tmdbID int64) (bool, error) {
	t, ok, err := s.Tracking.TitleByTMDB(ctx, "movie", tmdbID)
	if err != nil || !ok {
		return false, err
	}
	_, ok, err = s.ByTitle(ctx, t.ID)
	return ok, err
}
