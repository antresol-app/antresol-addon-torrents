//go:build ignore

package musicimport

import (
	"context"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
)

// Catalog is «Найти и добавить»'s music search (acquire.Service.DiscoverMedia)
// as the import's review list reads it (playlistimport.Catalog).
type Catalog struct{ Acq *acquire.Service }

// DiscoverMedia searches the catalog of a kind.
func (c Catalog) DiscoverMedia(ctx context.Context, kind, q string) ([]playlistimport.Candidate, error) {
	cs, err := c.Acq.DiscoverMedia(ctx, kind, q)
	if err != nil {
		return nil, err
	}
	out := make([]playlistimport.Candidate, len(cs))
	for i, c := range cs {
		out[i] = playlistimport.Candidate{TMDBID: c.TMDBID, ExtID: c.ExtID, Kind: c.Kind, Title: c.Title, Subtitle: c.Subtitle, Year: c.Year,
			Poster: c.Poster, Overview: c.Overview, InLibrary: c.InLibrary, LibraryItemID: c.LibraryItemID, Aliases: c.Aliases,
			MatchedTrack: c.MatchedTrack, ByArtist: c.ByArtist}
	}
	return out, nil
}
