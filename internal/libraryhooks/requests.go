package libraryhooks

import (
	"context"
	"encoding/json"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol/addonkit"
)

// requestKinds are the catalog kinds a kid may ask for through the addon
// (movies and series are the core's own TMDB).
var requestKinds = map[string]bool{acquire.KindMusic: true, acquire.KindBook: true, acquire.KindAudiobook: true, acquire.KindComic: true}

// requestsOpen is requests.open {}: kids' requests are taken while any
// release source is on (whoever approves one can then get it).
func (d Deps) requestsOpen(ctx context.Context, _ *addonkit.User, _ json.RawMessage) (any, error) {
	on, err := d.Sources.HasEnabled(ctx)
	if err != nil {
		d.log().Warn("torrents: requests.open", "err", err)
		return addonkit.RequestsOpenAnswer{Open: false}, nil
	}
	return addonkit.RequestsOpenAnswer{Open: on}, nil
}

// requestsTitle is requests.title {kind, extId}: the album's or book's
// name from the addon's own download of it; 204 when it never got it.
func (d Deps) requestsTitle(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var q addonkit.RequestTitleQuery
	if err := json.Unmarshal(in, &q); err != nil {
		return nil, err
	}
	if !requestKinds[q.Kind] {
		return nil, nil
	}
	t, ok := d.Acquire.RequestTitle(ctx, q.Kind, q.ExtID)
	if !ok {
		return nil, nil
	}
	return addonkit.RequestTitleAnswer{Title: t.Title, Subtitle: t.Creator, Year: t.Year, Poster: t.Poster}, nil
}

// requestsSearch is requests.search {kind, q}: the kind's catalog (as
// «Найти и добавить» finds it) as request candidates, the catalog's adult
// mark kept (the core keeps those from kids).
func (d Deps) requestsSearch(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var q addonkit.RequestSearchQuery
	if err := json.Unmarshal(in, &q); err != nil {
		return nil, err
	}
	out := addonkit.RequestSearchAnswer{Candidates: []addonkit.RequestCandidate{}}
	if !requestKinds[q.Kind] || q.Q == "" {
		return out, nil
	}
	cs, err := d.Acquire.DiscoverMedia(ctx, q.Kind, q.Q)
	if err != nil {
		return nil, err
	}
	for _, c := range cs {
		aliases := c.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		out.Candidates = append(out.Candidates, addonkit.RequestCandidate{Kind: c.Kind, TMDBID: c.TMDBID, ExtID: c.ExtID,
			Title: c.Title, Subtitle: c.Subtitle, Year: c.Year, Poster: c.Poster, Explicit: c.Explicit, Aliases: aliases})
	}
	return out, nil
}
