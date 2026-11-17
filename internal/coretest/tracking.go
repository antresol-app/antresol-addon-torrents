package coretest

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
)

// Tracking is the fake core's tracked titles (pinned.Tracking over plain
// slices): EnsureTitle makes a title (ids from 1), Kick records the id in
// Kicks (an unknown id is ErrUnknown, as the API's 404). Refresh and
// UpsertSources stand in for the core's refresh of a title, so a test
// runs the addon's scanners as the core would. Use a *Tracking; the zero
// value is ready.
type Tracking struct {
	Titles   []tracking.Title
	Episodes map[int64][]tracking.Episode
	Sources  map[int64][]tracking.Source
	Kicks    []int64
	// Err, when set, is what every read answers (the core unreachable).
	Err error

	mu sync.Mutex
}

// ErrUnknown: a kick of a title the core does not track.
var ErrUnknown = errors.New("coretest: title not tracked")

// Scanner is a tracking scanner (pinned.Scanner).
type Scanner interface {
	Kind() string
	Scan(ctx context.Context, t tracking.Title, eps []tracking.Episode) ([]tracking.Source, error)
}

func (f *Tracking) title(id int64) (tracking.Title, bool) {
	for _, t := range f.Titles {
		if t.ID == id {
			return t, true
		}
	}
	return tracking.Title{}, false
}

func (f *Tracking) TitleByID(_ context.Context, id int64) (tracking.Title, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return tracking.Title{}, false, f.Err
	}
	t, ok := f.title(id)
	return t, ok, nil
}

func (f *Tracking) TitleByTMDB(_ context.Context, kind string, tmdbID int64) (tracking.Title, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return tracking.Title{}, false, f.Err
	}
	for _, t := range f.Titles {
		if t.Kind == kind && t.TMDBID == tmdbID {
			return t, true, nil
		}
	}
	return tracking.Title{}, false, nil
}

// EpisodesOf is the title's episode list, by season and number (the
// Episodes field is the data).
func (f *Tracking) EpisodesOf(id int64) []tracking.Episode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.Episodes[id])
}

// SourcesOf is the title's sources (the Sources field is the data).
func (f *Tracking) SourcesOf(id int64) []tracking.Source {
	f.mu.Lock()
	defer f.mu.Unlock()
	return sortedSources(f.Sources[id])
}

// Adapter is f as pinned.Tracking (its fields named Episodes and Sources
// hold the data, so the methods are on this view).
func (f *Tracking) Adapter() TrackingAdapter { return TrackingAdapter{f} }

// TrackingAdapter is the pinned.Tracking of a *Tracking.
type TrackingAdapter struct{ *Tracking }

func (a TrackingAdapter) Episodes(_ context.Context, id int64) ([]tracking.Episode, error) {
	if err := a.err(); err != nil {
		return nil, err
	}
	return a.EpisodesOf(id), nil
}

func (a TrackingAdapter) Sources(_ context.Context, id int64) ([]tracking.Source, error) {
	if err := a.err(); err != nil {
		return nil, err
	}
	return a.SourcesOf(id), nil
}

func (a TrackingAdapter) EnsureTitle(ctx context.Context, kind string, tmdbID int64, name, poster string) (tracking.Title, error) {
	return a.Tracking.EnsureTitle(ctx, kind, tmdbID, name, poster)
}

func (a TrackingAdapter) Kick(ctx context.Context, id int64) error { return a.Tracking.Kick(ctx, id) }

func (f *Tracking) err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Err
}

// EnsureTitle is the title of kind and TMDB id, made (status unknown) when
// there is none.
func (f *Tracking) EnsureTitle(_ context.Context, kind string, tmdbID int64, name, poster string) (tracking.Title, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return tracking.Title{}, f.Err
	}
	for _, t := range f.Titles {
		if t.Kind == kind && t.TMDBID == tmdbID {
			return t, nil
		}
	}
	id := int64(1)
	for _, t := range f.Titles {
		id = max(id, t.ID+1)
	}
	t := tracking.Title{ID: id, Kind: kind, TMDBID: tmdbID, Name: name, Poster: poster, Status: "unknown"}
	f.Titles = append(f.Titles, t)
	return t, nil
}

// Kick records the kick of a tracked title.
func (f *Tracking) Kick(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	if _, ok := f.title(id); !ok {
		return ErrUnknown
	}
	f.Kicks = append(f.Kicks, id)
	return nil
}

// UpsertSources replaces the title's sources of kind with srcs, as the
// core's store does: a known row (season, episode, ref) keeps its first
// seen, a new one takes the episode's earliest of that kind or now; every
// row's last seen is now. The rows new to the title and kind come back.
func (f *Tracking) UpsertSources(titleID int64, kind string, srcs []tracking.Source, now time.Time) []tracking.Source {
	f.mu.Lock()
	defer f.mu.Unlock()
	type key struct {
		s, e int
		ref  string
	}
	now = now.UTC().Truncate(time.Second)
	old := map[key]tracking.Source{}
	first := map[[2]int]time.Time{}
	var keep []tracking.Source
	for _, x := range f.Sources[titleID] {
		if x.Kind != kind {
			keep = append(keep, x)
			continue
		}
		old[key{x.Season, x.Episode, x.Ref}] = x
		if t, ok := first[[2]int{x.Season, x.Episode}]; !ok || x.FirstSeen.Before(t) {
			first[[2]int{x.Season, x.Episode}] = x.FirstSeen
		}
	}
	seen := map[key]bool{}
	var added []tracking.Source
	for _, x := range srcs {
		k := key{x.Season, x.Episode, x.Ref}
		if seen[k] {
			continue
		}
		seen[k] = true
		x.TitleID, x.Kind, x.LastSeen = titleID, kind, now
		if x.Labels == nil {
			x.Labels = []string{}
		}
		switch o, ok := old[k]; {
		case ok:
			x.FirstSeen = o.FirstSeen
		default:
			if t, ok := first[[2]int{x.Season, x.Episode}]; ok {
				x.FirstSeen = t // the same source under a new name: no news
			} else {
				x.FirstSeen = now
				added = append(added, x)
			}
		}
		keep = append(keep, x)
	}
	if f.Sources == nil {
		f.Sources = map[int64][]tracking.Source{}
	}
	f.Sources[titleID] = sortedSources(keep)
	return added
}

// Refresh is the core's refresh of a title: its episode list becomes eps,
// then each scanner's answer replaces the title's sources of its kind;
// Unchanged, or another error (which the core logs), keeps them.
func (f *Tracking) Refresh(ctx context.Context, titleID int64, eps []tracking.Episode, now time.Time, scanners ...Scanner) error {
	f.mu.Lock()
	t, ok := f.title(titleID)
	if f.Episodes == nil {
		f.Episodes = map[int64][]tracking.Episode{}
	}
	list := make([]tracking.Episode, len(eps))
	for i, e := range eps {
		e.TitleID = titleID
		list[i] = e
	}
	f.Episodes[titleID] = list
	f.mu.Unlock()
	if !ok {
		return ErrUnknown
	}
	for _, sc := range scanners {
		srcs, err := sc.Scan(ctx, t, slices.Clone(list))
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue // unchanged, or failed: the core keeps the rows
		}
		f.UpsertSources(titleID, sc.Kind(), srcs, now)
	}
	return nil
}

// sortedSources: by season, episode, kind and ref (the core's order).
func sortedSources(in []tracking.Source) []tracking.Source {
	out := slices.Clone(in)
	slices.SortStableFunc(out, func(a, b tracking.Source) int {
		switch {
		case a.Season != b.Season:
			return a.Season - b.Season
		case a.Episode != b.Episode:
			return a.Episode - b.Episode
		case a.Kind != b.Kind:
			return cmp.Compare(a.Kind, b.Kind)
		}
		return cmp.Compare(a.Ref, b.Ref)
	})
	return out
}
