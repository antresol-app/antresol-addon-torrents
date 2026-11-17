package followapi

import (
	"context"
	"errors"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol/addonkit"
)

// The rating gate of the core's routes, kept here as the legacy handlers
// kept it: a profile with a rating limit (X-Antresol-User maxRating, the
// limit the core's own gate applies) does not reach a pinned title or a
// voice-over item above it; the routes answer 404 «Не найдено», as for an
// item that is not there. The rule is the core's library.RatingAllowed
// (corecopy).

// rootItem walks it up to the top of its hierarchy (library.Repo.Root).
func rootItem(ctx context.Context, lib Library, it library.Item) (library.Item, error) {
	for i := 0; i < 4 && library.HasRatedParent(it.Kind) && it.ParentID != 0; i++ {
		p, err := lib.Item(ctx, it.ParentID)
		if err != nil {
			return it, err
		}
		it = p
	}
	return it, nil
}

// itemVisible: u may see the library item it (library.Repo.Visible).
func itemVisible(ctx context.Context, lib Library, u addonkit.User, it library.Item) (bool, error) {
	if u.MaxRating == "" {
		return true, nil
	}
	root, err := rootItem(ctx, lib, it)
	if err != nil {
		return false, err
	}
	return library.RatingAllowed(u.MaxRating, root), nil
}

// titleVisible: u may see the tracked title t (the core's trackGate):
// a title in the library by its item's rating; else by its stored
// certification, a title without one hidden from a limited profile.
func titleVisible(ctx context.Context, lib Library, u addonkit.User, t tracking.Title) (bool, error) {
	if u.MaxRating == "" {
		return true, nil
	}
	if lib != nil {
		kind := library.KindSeries
		if t.Kind == "movie" {
			kind = library.KindMovie
		}
		it, err := lib.ItemByTMDB(ctx, kind, t.TMDBID)
		switch {
		case err == nil:
			return itemVisible(ctx, lib, u, it)
		case !errors.Is(err, library.ErrNotFound):
			return false, err
		}
	}
	if u.IsKid || t.Cert == "" {
		return false, nil
	}
	return library.RatingAllowed(u.MaxRating, library.Item{Kind: t.Kind, Rating: t.Cert}), nil
}
