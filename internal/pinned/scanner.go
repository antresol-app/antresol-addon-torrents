package pinned

import (
	"context"
	"errors"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
)

// Scanner is a tracking scanner of the addon: the core asks it (the
// tracking.scan:<kind> hook) where each episode of a title can be got.
// The core's own Scanner lives with its Store; this one mirrors it.
type Scanner interface {
	Kind() string
	Scan(ctx context.Context, t tracking.Title, eps []tracking.Episode) ([]tracking.Source, error)
}

// ErrUnchanged: the scanner did not look this time; the core keeps the
// title's sources of its kind as they are (the hook answers Unchanged).
var ErrUnchanged = errors.New("pinned: unchanged")

// Tracking is what pins need of the core's tracked titles (the tracking
// API, coreadapt.Tracking in production).
type Tracking interface {
	TitleByID(ctx context.Context, id int64) (tracking.Title, bool, error)
	TitleByTMDB(ctx context.Context, kind string, tmdbID int64) (tracking.Title, bool, error)
	Episodes(ctx context.Context, id int64) ([]tracking.Episode, error)
	Sources(ctx context.Context, id int64) ([]tracking.Source, error)
	// EnsureTitle is the tracked title of kind and TMDB id, made when the
	// core has none.
	EnsureTitle(ctx context.Context, kind string, tmdbID int64, name, poster string) (tracking.Title, error)
	// Kick asks the core to refresh the title soon.
	Kick(ctx context.Context, id int64) error
}

// Library is what pins need of the core's library (coreadapt.Library).
type Library interface {
	FileExists(ctx context.Context, path string) (bool, error)
}
