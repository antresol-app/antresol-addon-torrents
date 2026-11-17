package acquire

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/openlibrary"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/wikidata"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// Wikidata is the part of *wikidata.Client the service uses: the catalog
// of book titles in Russian (Open Library has few Russian editions and
// finds nothing for «Шантарам»).
type Wikidata interface {
	SearchBooks(ctx context.Context, q, lang string) ([]wikidata.Work, error)
}

// wikidataTimeout bounds the Wikidata part of a book search: Open Library
// answers alone when Wikidata is slow.
const wikidataTimeout = 8 * time.Second

// coverLookups: the Wikidata works that get a cover from Open Library (one
// request each).
const coverLookups = 3

// bookCandidate is a found book before it becomes a Candidate.
type bookCandidate struct {
	c      Candidate
	olKey  string
	cover  bool // has a poster
	tier   int  // relevance: 0 = the title is the query … 3 = neither holds the other
	pop    int  // Wikipedia articles (Wikidata sitelinks)
	author string
	query  string // Open Library lookup for a cover: title
	qAuth  string // and author
}

// relevance ranks a title against the query: the same, a prefix, holds it,
// or another; the best of the title and its aliases.
func relevance(q string, titles ...string) int {
	k := release.Normalize(q)
	best := 3
	for _, t := range titles {
		n := release.Normalize(t)
		switch {
		case n == "" || k == "":
		case n == k:
			best = min(best, 0)
		case strings.HasPrefix(n, k+" "):
			best = min(best, 1)
		case strings.Contains(" "+n+" ", " "+k+" "):
			best = min(best, 2)
		}
	}
	return best
}

// discoverBooks searches Open Library and Wikidata for books and
// audiobooks at once and merges them. Wikidata maps a title to its work in
// every language: a Russian query («Шантарам») finds the work with its
// original title («Shantaram», an alias), an English one gets the Russian
// title as an alias, so the release search asks the trackers for both. A
// Russian query lists the Wikidata works first (by relevance, then by
// popularity: Wikipedia articles), then Open Library's other finds; an
// English one keeps Open Library's order and adds the works it lacks.
func (s *Service) discoverBooks(ctx context.Context, kind, q string) ([]Candidate, error) {
	ru := release.HasCyrillic(q)
	var (
		docs          []openlibrary.Doc
		works         []wikidata.Work
		olErr, wdErr  error
		wg            sync.WaitGroup
		haveWD, hasOL = s.o.Wikidata != nil, s.o.OpenLibrary != nil
	)
	if !haveWD && !hasOL {
		return nil, uerr(ErrUnavailable, CatalogOffline)
	}
	if hasOL {
		wg.Go(func() { docs, olErr = s.o.OpenLibrary.SearchQ(ctx, q) })
	}
	if haveWD {
		wg.Go(func() {
			wctx, cancel := context.WithTimeout(ctx, wikidataTimeout)
			defer cancel()
			lang := "en"
			if ru {
				lang = "ru"
			}
			works, wdErr = s.o.Wikidata.SearchBooks(wctx, q, lang)
		})
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if wdErr != nil {
		s.log.Warn("catalog search", "source", "wikidata", "err", wdErr)
	}
	switch {
	case olErr != nil && (!haveWD || wdErr != nil):
		return nil, s.catalogErr(ctx, "openlibrary", olErr)
	case !hasOL && wdErr != nil:
		return nil, s.catalogErr(ctx, "wikidata", wdErr)
	case olErr != nil:
		s.log.Warn("catalog search", "source", "openlibrary", "err", olErr)
		docs = nil
	}

	fromDoc := func(d openlibrary.Doc) bookCandidate {
		author := ""
		if len(d.Authors) > 0 {
			author = d.Authors[0]
		}
		c := Candidate{Kind: kind, ExtID: d.Key, Title: d.Title, Subtitle: author, Year: d.Year, Explicit: ExplicitSubjects(d.Subjects)}
		if hasOL {
			c.Poster = s.o.OpenLibrary.CoverURL(d.CoverID)
		}
		return bookCandidate{c: c, olKey: d.Key, cover: c.Poster != "", tier: relevance(q, d.Title), author: author}
	}
	// the work of an Open Library doc: the same Open Library key, else the
	// same title (in English or the original language)
	workOf := func(d openlibrary.Doc) int {
		for i, w := range works {
			if w.OpenLibrary != "" && w.OpenLibrary == d.Key {
				return i
			}
		}
		t := release.Normalize(d.Title)
		for i, w := range works {
			if t != "" && (t == release.Normalize(w.TitleEN) || t == release.Normalize(w.Original)) {
				return i
			}
		}
		return -1
	}
	used := make([]bool, len(works))
	var list []bookCandidate
	if ru {
		for _, w := range works {
			title := cmp.Or(w.TitleRU, w.Original, w.TitleEN)
			author, authorEN := "", ""
			if len(w.AuthorsRU) > 0 {
				author, authorEN = w.AuthorsRU[0], w.AuthorsEN[0]
			}
			ext := cmp.Or(w.OpenLibrary, "wikidata:"+w.ID)
			c := Candidate{Kind: kind, ExtID: ext, Title: title, Subtitle: author, Year: w.Year,
				Aliases: otherTitles(title, w.Original, w.TitleEN)}
			list = append(list, bookCandidate{c: c, olKey: w.OpenLibrary, tier: relevance(q, title), pop: w.Sitelinks,
				author: author, query: cmp.Or(w.Original, w.TitleEN, title), qAuth: authorEN})
		}
		slices.SortStableFunc(list, func(a, b bookCandidate) int {
			return cmp.Or(cmp.Compare(a.tier, b.tier), cmp.Compare(b.pop, a.pop))
		})
		for _, d := range docs {
			if i := slices.IndexFunc(list, func(b bookCandidate) bool { return b.olKey != "" && b.olKey == d.Key }); i >= 0 {
				// the doc of a work: its cover, its year
				if hasOL && !list[i].cover && d.CoverID > 0 {
					list[i].c.Poster, list[i].cover = s.o.OpenLibrary.CoverURL(d.CoverID), true
				}
				list[i].c.Year = cmp.Or(list[i].c.Year, d.Year)
				continue
			}
			list = append(list, fromDoc(d))
		}
	} else {
		for _, d := range docs {
			b := fromDoc(d)
			if i := workOf(d); i >= 0 {
				used[i] = true
				b.c.Aliases = otherTitles(d.Title, works[i].TitleRU, works[i].Original)
			}
			list = append(list, b)
		}
		for i, w := range works {
			if used[i] || w.OpenLibrary != "" && slices.ContainsFunc(list, func(b bookCandidate) bool { return b.olKey == w.OpenLibrary }) {
				continue
			}
			title := cmp.Or(w.TitleEN, w.Original, w.TitleRU)
			author := ""
			if len(w.AuthorsEN) > 0 {
				author = w.AuthorsEN[0]
			}
			c := Candidate{Kind: kind, ExtID: cmp.Or(w.OpenLibrary, "wikidata:"+w.ID), Title: title, Subtitle: author, Year: w.Year,
				Aliases: otherTitles(title, w.Original, w.TitleRU)}
			list = append(list, bookCandidate{c: c, olKey: w.OpenLibrary, tier: relevance(q, title), pop: w.Sitelinks,
				author: author, query: title, qAuth: author})
		}
	}
	list = dedupBooks(list)
	if len(list) > discoverLimit {
		list = list[:discoverLimit]
	}
	if hasOL {
		s.coversFromOpenLibrary(ctx, list)
	}
	out := make([]Candidate, 0, len(list))
	for _, b := range list {
		c := b.c
		if it, ok := s.libraryItem(ctx, acquisition{Kind: kind, Title: c.Title, Meta: acquisitionMeta{Author: b.author}}); ok {
			id := it.ID
			c.InLibrary, c.LibraryItemID = true, &id
		}
		out = append(out, c)
	}
	return out, nil
}

// coversFromOpenLibrary looks the first Wikidata works without a cover up
// on Open Library by their original title and author.
func (s *Service) coversFromOpenLibrary(ctx context.Context, list []bookCandidate) {
	n := 0
	for i := range list {
		b := &list[i]
		if b.cover || b.query == "" || n == coverLookups {
			continue
		}
		n++
		docs, err := s.o.OpenLibrary.Search(ctx, b.query, b.qAuth)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue // a cover is a nicety
		}
		k := slices.IndexFunc(docs, func(d openlibrary.Doc) bool { return b.olKey != "" && d.Key == b.olKey && d.CoverID > 0 })
		if k < 0 {
			k = slices.IndexFunc(docs, func(d openlibrary.Doc) bool {
				return d.CoverID > 0 && release.Normalize(d.Title) == release.Normalize(b.query)
			})
		}
		if k >= 0 {
			b.c.Poster, b.cover = s.o.OpenLibrary.CoverURL(docs[k].CoverID), true
			b.c.Year = cmp.Or(b.c.Year, docs[k].Year)
		}
	}
}

// otherTitles: the titles that are not title (folded), without repeats.
func otherTitles(title string, more ...string) []string {
	seen := map[string]bool{release.Normalize(title): true}
	var out []string
	for _, t := range more {
		if k := release.Normalize(t); k != "" && !seen[k] {
			seen[k] = true
			out = append(out, strings.TrimSpace(t))
		}
	}
	return out
}

// dedupBooks drops a later candidate of the same title and author (Open
// Library lists editions of one book as several works).
func dedupBooks(list []bookCandidate) []bookCandidate {
	seen := map[string]bool{}
	out := list[:0]
	for _, b := range list {
		k := release.Normalize(b.c.Title) + "\x00" + release.Normalize(b.c.Subtitle)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, b)
	}
	return out
}
