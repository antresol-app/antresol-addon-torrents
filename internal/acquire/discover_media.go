package acquire

import (
	"context"
	"errors"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
)

// isMediaKind: music, books, audiobooks and comics (not from TMDB, download
// only).
func isMediaKind(k string) bool { return organize.IsMediaKind(k) }

// mediaTitle checks a media request: music needs the artist (the album
// is optional: without it the whole artist is wanted, a discography first);
// books, audiobooks and comics need the title; a comic has no artist.
func mediaTitle(kind, title, artist string) (string, string, error) {
	title, artist = strings.TrimSpace(title), strings.TrimSpace(artist)
	if kind == KindComic {
		artist = ""
	}
	if kind == KindMusic && artist == "" {
		return "", "", uerr(ErrInvalid, "Укажите исполнителя")
	}
	if kind != KindMusic && title == "" {
		return "", "", uerr(ErrInvalid, "Укажите, что искать")
	}
	return title, artist, nil
}

// mediaSearchRequest searches the sources for "<artist> <title>" and the
// title alone (the artist alone when no title); Artist and Work tell
// indexer.ScoreMedia what release names must hold.
func mediaSearchRequest(kind, title, artist string) indexer.SearchRequest {
	sr := indexer.SearchRequest{Kind: kind, Text: strings.TrimSpace(artist + " " + title), Artist: artist, Work: title}
	if title != "" {
		sr.AltTitles = []string{title}
	}
	return sr
}

// withAliases sets the aliases of the work; a book's or audiobook's are
// also searched for (its original and Russian titles: a translation is
// named in its own language).
func withAliases(sr indexer.SearchRequest, aliases []string) indexer.SearchRequest {
	sr.Aliases = aliases
	if sr.Kind == KindBook || sr.Kind == KindAudiobook {
		sr.AltTitles = append(sr.AltTitles, release.UsefulAliases(aliases)...)
	}
	return sr
}

// albumYear is the year release names are checked against: the album's
// (music with an album named); 0 = not checked.
func albumYear(kind, title string, year int) int {
	if kind != KindMusic || title == "" {
		return 0
	}
	return max(year, 0)
}

// mediaMeta is the acquisitions.meta_json of a media request.
func mediaMeta(kind, artist string, year int) acquisitionMeta {
	m := acquisitionMeta{Year: max(year, 0)}
	switch kind {
	case KindMusic:
		m.Artist = artist
	case KindBook, KindAudiobook:
		m.Author = artist
	}
	return m
}

// libraryItem finds the library item an acquisition adds to (the movie,
// series, album or book already there).
func (s *Service) libraryItem(ctx context.Context, a acquisition) (library.Item, bool) {
	if s.o.Library == nil {
		return library.Item{}, false
	}
	creator := a.Meta.Author
	if a.Kind == KindMusic {
		creator = a.Meta.Artist
	}
	it, ok, err := s.o.Library.FindTitle(ctx, a.Kind, int64(a.TMDBID), a.Title, creator)
	if err != nil {
		return library.Item{}, false // a lookup that failed: only a hint
	}
	return it, ok
}

// CatalogOffline is the message when MusicBrainz or Open Library (and
// Wikidata) is off or does not answer (the core's playlist import says the
// same).
const CatalogOffline = "Каталог недоступен — проверьте интернет"

// ErrCatalogOffline is CatalogOffline as an error (the playlist import
// maps it).
var ErrCatalogOffline = errors.New(CatalogOffline)

// DiscoverMedia searches the catalog of a kind: MusicBrainz release groups
// for music (Subtitle = artist; MatchedTrack when the album was found by a
// song of that title; ByArtist on the albums of the artist the query names,
// listed first and not counted in the limit), Open Library works and Wikidata works for books
// and audiobooks (Subtitle = the first author; discoverBooks); a comic is the query itself
// (trackers name manga series consistently, no catalog needed). InLibrary
// marks albums and books already in the library.
func (s *Service) DiscoverMedia(ctx context.Context, kind, q string) ([]Candidate, error) {
	if !isMediaKind(kind) {
		return nil, uerr(ErrInvalid, "Неизвестный раздел")
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return []Candidate{}, nil
	}
	mark := func(c Candidate, a acquisition) Candidate {
		if it, ok := s.libraryItem(ctx, a); ok {
			id := it.ID
			c.InLibrary, c.LibraryItemID = true, &id
		}
		return c
	}
	switch kind {
	case KindComic:
		return []Candidate{{Kind: KindComic, Title: q}}, nil
	case KindMusic:
		if s.o.MusicBrainz == nil {
			return nil, uerr(ErrUnavailable, CatalogOffline)
		}
		groups, err := s.o.MusicBrainz.SearchFree(ctx, q)
		if err != nil {
			return nil, s.catalogErr(ctx, "musicbrainz", err)
		}
		out := []Candidate{}
		limit := discoverLimit
		for _, g := range groups {
			c := Candidate{Kind: KindMusic, ExtID: g.ID, Title: g.Title, Subtitle: g.Artist, Year: g.Year, Poster: s.o.MusicBrainz.CoverURL(g.ID),
				Explicit: ExplicitSubjects(append([]string{g.Disambiguation}, g.SecondaryTypes...))}
			c.Aliases = release.UsefulAliases(g.Aliases) // never the disambiguation: «live», «compilation»
			c.MatchedTrack, c.ByArtist = g.MatchedTrack, g.ByArtist
			if g.ByArtist != "" {
				limit++ // the whole discography, then the other matches
			}
			out = append(out, mark(c, acquisition{Kind: KindMusic, Title: g.Title, Meta: acquisitionMeta{Artist: g.Artist}}))
		}
		if len(out) > limit {
			out = out[:limit]
		}
		return out, nil
	default: // book, audiobook
		return s.discoverBooks(ctx, kind, q)
	}
}

func (s *Service) catalogErr(ctx context.Context, source string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	s.log.Warn("catalog search", "source", source, "err", err)
	return wrapErr(ErrUnavailable, CatalogOffline, err)
}
