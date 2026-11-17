package pinned

import (
	"context"
	"errors"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
)

// refreshAfter: a pin checked longer ago is refreshed before a scan.
const refreshAfter = 6 * time.Hour

// Refresh fetches the pin's release again (a magnet or an own file is never
// fetched: only last_checked moves). The same infohash moves last_checked
// only. A release holding every episode the pin has (and maybe more)
// becomes the pin, and the engine's torrent of the old one, if any, is
// replaced keeping its data and its selection. Anything else — a body that
// is not a metafile (an error or login page), a release missing some of
// the pin's episodes, a failed download or Replace — keeps the pin as it
// was, records the reason in last_error and returns it.
func (s *Service) Refresh(ctx context.Context, p Pin) (changed bool, err error) {
	now := s.now().UTC().Truncate(time.Second)
	if p.SourceID == "" || isMagnet(p.Link) || p.Link == "" {
		return false, s.touch(ctx, p.ID, now, "")
	}
	g, err := s.fetch(ctx, p.SourceID, p.Link)
	if err == nil && g.ih != p.InfoHash {
		err = covers(g, p)
	}
	replaced := false
	var old *stale
	if err == nil && g.ih != p.InfoHash {
		replaced, old, err = s.replaceInEngine(ctx, p.InfoHash, g)
	}
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if terr := s.touch(ctx, p.ID, now, i18n.MessageL(i18n.RU, err)); terr != nil {
			return false, errors.Join(err, terr)
		}
		return false, err
	}
	if g.ih == p.InfoHash {
		return false, s.touch(ctx, p.ID, now, "")
	}
	np := p
	np.InfoHash, np.Files, np.LastChecked, np.LastError = g.ih, g.files, now, ""
	np.Quality = qualityOf(np.ReleaseTitle, g.name)
	ok, err := s.update(ctx, np, p.InfoHash)
	s.checkUpdated(replaced, ok, err, p, g.ih)
	if err == nil && ok {
		s.keepStale(ctx, p.ID, old, g)
	}
	return ok, err
}

// covers: g holds every episode p does. A pin naming no episodes (a film,
// files whose names parse to none) needs each of its media files still
// there by path, at least as large.
func covers(g got, p Pin) error {
	old := EpisodesOf(p.Files, 1)
	if len(old) == 0 {
		size := map[string]int64{}
		for _, f := range g.files {
			if !f.Padding {
				size[f.Path] = f.Size
			}
		}
		for _, f := range p.Files {
			if f.Padding || !mediaFile(f.Path) {
				continue
			}
			if n, ok := size[f.Path]; !ok || n < f.Size {
				return i18n.Errorf(msgFilesGone)
			}
		}
		return nil
	}
	have := EpisodesOf(g.files, 1)
	for k := range old {
		if _, ok := have[k]; !ok {
			return i18n.Errorf(msgFewer)
		}
	}
	return nil
}

// touch records a refresh that left the release as it is.
func (s *Service) touch(ctx context.Context, id int64, now time.Time, lastErr string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE pinned_releases SET last_checked=?, last_error=? WHERE id=?`,
		now.Format(time.RFC3339), lastErr, id)
	return err
}

// refreshDue refreshes the title's pin when it was checked over
// refreshAfter ago; a failed refresh is logged (the pin stays).
func (s *Service) refreshDue(ctx context.Context, p Pin) {
	if !p.LastChecked.IsZero() && s.now().Sub(p.LastChecked) < refreshAfter {
		return
	}
	if _, err := s.Refresh(ctx, p); err != nil && ctx.Err() == nil {
		s.log().Warn("pinned: refresh", "title", p.TitleID, "pin", p.ID, "err", err)
	}
}
