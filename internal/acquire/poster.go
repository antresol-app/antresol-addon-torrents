package acquire

import (
	"context"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata"
)

// Images is the image cache (*metadata.Images): Fetch downloads a cover once
// and names the cache file served by /api/images/{name}.
type Images interface {
	Fetch(ctx context.Context, url string) (string, error)
}

const posterTimeout = 30 * time.Second

// posterSource is the third-party cover of an acquisition, "" when none is
// known. TMDB and MusicBrainz covers come from the server's own catalogs; a
// book's cover is the one the client saw, accepted only from Open Library
// (the server fetches it, so no other host may be named).
func (s *Service) posterSource(a acquisition, tmdbPoster, clientPoster string) string {
	switch a.Kind {
	case KindMovie, KindSeries:
		if tmdbPoster != "" && s.o.Metadata != nil {
			return s.o.Metadata.ImageURL("w342", tmdbPoster)
		}
	case KindMusic:
		if a.ExtID != "" && s.o.MusicBrainz != nil {
			return s.o.MusicBrainz.CoverURL(a.ExtID)
		}
	case KindBook, KindAudiobook, KindComic:
		if clientPoster != "" && s.o.OpenLibrary != nil {
			// the folder of a cover URL: https://covers.openlibrary.org/b/id/
			one := s.o.OpenLibrary.CoverURL(1)
			if dir := one[:strings.LastIndex(one, "/")+1]; strings.Count(dir, "/") >= 3 && strings.HasPrefix(clientPoster, dir) {
				return clientPoster
			}
		}
	}
	return ""
}

// RequestPoster is the cover a kid's request of an album, book or manga
// keeps: the server's own (posterSource: the MusicBrainz cover of extID, an
// Open Library cover), else the client's only when it is an https address
// of a catalog of posterHosts or one of this server's /api/images; "" otherwise.
func (s *Service) RequestPoster(kind, extID, clientPoster string) string {
	if p := s.posterSource(acquisition{Kind: kind, ExtID: extID}, "", clientPoster); p != "" {
		return p
	}
	return CatalogPoster(clientPoster)
}

// CatalogPoster is metadata.CatalogPoster: p when it is a cover of a
// catalog a client may name, "" otherwise.
func CatalogPoster(p string) string { return metadata.CatalogPoster(p) }

// capturePoster caches the cover of a started download in the background
// and records its cache file on the acquisition.
func (s *Service) capturePoster(ih, url string) {
	if url == "" || s.o.Images == nil {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ctx, cancel := context.WithTimeout(s.root, posterTimeout)
		defer cancel()
		s.storePoster(ctx, ih, url)
	}()
}

func (s *Service) storePoster(ctx context.Context, ih, url string) {
	name, err := s.o.Images.Fetch(ctx, url)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("download poster", "infohash", ih, "err", err)
		}
		return
	}
	s.setPoster(ctx, ih, name)
}

func (s *Service) setPoster(ctx context.Context, ih, name string) {
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET poster=? WHERE infohash=?`, name, ih); err != nil {
		s.log.Warn("save download poster", "infohash", ih, "err", err)
	}
}

// backfillPosters gives the acquisitions made before posters were captured
// theirs: the library item's cached poster when the title is there, else the
// catalog cover (TMDB by id, Cover Art Archive by release group). A failure
// is left for the next start. Books have no catalog id to ask with.
func (s *Service) backfillPosters(ctx context.Context) {
	if s.o.Images == nil {
		return
	}
	acqs, err := s.acquisitions(ctx)
	if err != nil {
		s.log.Warn("backfill posters", "err", err)
		return
	}
	for ih, a := range acqs {
		if ctx.Err() != nil {
			return
		}
		if a.Poster != "" {
			continue
		}
		if it, ok := s.libraryItem(ctx, a); ok && library.ImageCacheName.MatchString(it.Poster) {
			s.setPoster(ctx, ih, it.Poster)
			continue
		}
		tmdbPoster := ""
		if isVideoKind(a.Kind) && a.TMDBID > 0 && s.o.Metadata != nil {
			if d, err := s.title(ctx, a.Kind, a.TMDBID); err == nil {
				tmdbPoster = d.Poster
			}
		}
		if url := s.posterSource(a, tmdbPoster, ""); url != "" {
			fctx, cancel := context.WithTimeout(ctx, posterTimeout)
			s.storePoster(fctx, ih, url)
			cancel()
		}
	}
}
