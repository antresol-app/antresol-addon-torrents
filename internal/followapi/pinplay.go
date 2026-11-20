package followapi

import (
	"context"
	"fmt"
	"strings"

	"github.com/antresol-app/antresol/addonkit"
)

// PlayPinned is «Смотреть» of {season, episode} (a film: neither) from the
// title's pinned release as u, what POST pinned/{kind}/{tmdbId}/play does
// without its HTTP: the stream's file becomes u's input, and its id comes
// back. found false: the title has no pin u may see, and nothing was
// started (the caller falls back to the server's own pick). The errors are
// those of the acquire and watch services, for their writers.
func (d Deps) PlayPinned(ctx context.Context, u addonkit.User, kind string, tmdbID int64, season, episode int) (id string, found bool, err error) {
	if d.Pins == nil || d.Pins.Tracking == nil || d.Acquire == nil || d.Library == nil || d.Watch == nil {
		return "", false, nil
	}
	p := &pinnedAPI{d}
	t, tracked, err := p.tracking().TitleByTMDB(ctx, kind, tmdbID)
	if err != nil {
		return "", false, fmt.Errorf("pinned title: %w", err)
	}
	if !tracked {
		return "", false, nil // nobody pins an untracked title
	}
	ok, err := titleVisible(ctx, p.Library, u, t)
	if err != nil {
		return "", false, fmt.Errorf("pinned gate: %w", err)
	}
	if !ok {
		return "", false, nil // a hidden title is not told apart
	}
	pin, has, err := p.Pins.ByTitle(ctx, t.ID)
	if err != nil {
		return "", false, fmt.Errorf("pinned get: %w", err)
	}
	if !has {
		return "", false, nil
	}
	req, err := streamRequest(u, kind, t, pin, season, episode)
	if err != nil {
		return "", true, fmt.Errorf("pinned play source: %w", err)
	}
	out, err := p.Acquire.Acquire(ctx, req)
	if err != nil {
		return "", true, err
	}
	p.added()
	id, err = p.Watch(ctx, u, strings.ToLower(out.InfoHash), out.FileIndex)
	return id, true, err
}
