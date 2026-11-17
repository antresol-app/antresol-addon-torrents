package coreadapt

import (
	"context"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol/addonkit"
)

// Tracking is the core's tracked titles through addonkit (permission
// tracking): pinned.Tracking. The copied model carries no Shikimori,
// MyShows or AniDB id, next check or refresh time: the API sends none.
type Tracking struct{ C *addonkit.Client }

// ToTitle is the copied model of an SDK tracked title.
func ToTitle(t addonkit.TrackingTitle) tracking.Title {
	return tracking.Title{ID: t.ID, Kind: t.Kind, TMDBID: t.TMDBID, IMDbID: t.IMDbID, Name: t.Name,
		Poster: t.Poster, Status: t.Status, Cert: t.Cert}
}

// ToSource is the copied model of an SDK source.
func ToSource(s addonkit.TrackingSource) tracking.Source {
	m := s.Meta
	return tracking.Source{TitleID: s.TitleID, Season: s.Season, Episode: s.Episode, Kind: s.Kind,
		Ref: s.Ref, Provider: s.Provider, Quality: s.Quality, Labels: s.Labels,
		Meta: tracking.SourceMeta{ItemID: m.ItemID, FileID: m.FileID, SourceID: m.SourceID, Provider: m.Provider,
			Voice: m.Voice, Lang: m.Lang, Audio: m.Audio, Studios: m.Studios, Subs: m.Subs, FileIndex: m.FileIndex},
		FirstSeen: s.FirstSeen, LastSeen: s.LastSeen}
}

// FromSource is the SDK form of a copied source (a scanner's answer).
func FromSource(s tracking.Source) addonkit.TrackingSource {
	m := s.Meta
	return addonkit.TrackingSource{TitleID: s.TitleID, Season: s.Season, Episode: s.Episode, Kind: s.Kind,
		Ref: s.Ref, Provider: s.Provider, Quality: s.Quality, Labels: s.Labels,
		Meta: addonkit.TrackingSourceMeta{ItemID: m.ItemID, FileID: m.FileID, SourceID: m.SourceID, Provider: m.Provider,
			Voice: m.Voice, Lang: m.Lang, Audio: m.Audio, Studios: m.Studios, Subs: m.Subs, FileIndex: m.FileIndex},
		FirstSeen: s.FirstSeen, LastSeen: s.LastSeen}
}

func title(t addonkit.TrackingTitle, ok bool, err error) (tracking.Title, bool, error) {
	if err != nil || !ok {
		return tracking.Title{}, false, err
	}
	return ToTitle(t), true, nil
}

func (t Tracking) TitleByID(ctx context.Context, id int64) (tracking.Title, bool, error) {
	return title(t.C.TitleByID(ctx, id))
}

func (t Tracking) TitleByTMDB(ctx context.Context, kind string, tmdbID int64) (tracking.Title, bool, error) {
	return title(t.C.TitleByTMDB(ctx, kind, tmdbID))
}

// Episodes is the title's episode list; none for a title the core does
// not track (as the core's store answers).
func (t Tracking) Episodes(ctx context.Context, id int64) ([]tracking.Episode, error) {
	eps, err := t.C.TitleEpisodes(ctx, id)
	if addonkit.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]tracking.Episode, len(eps))
	for i, e := range eps {
		out[i] = tracking.Episode{TitleID: e.TitleID, Season: e.Season, Number: e.Number, Name: e.Name, AirDate: e.AirDate}
	}
	return out, nil
}

// Sources is every source of the title, of every kind; none for a title
// the core does not track.
func (t Tracking) Sources(ctx context.Context, id int64) ([]tracking.Source, error) {
	srcs, err := t.C.TitleSources(ctx, id)
	if addonkit.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]tracking.Source, len(srcs))
	for i, s := range srcs {
		out[i] = ToSource(s)
	}
	return out, nil
}

func (t Tracking) EnsureTitle(ctx context.Context, kind string, tmdbID int64, name, poster string) (tracking.Title, error) {
	x, err := t.C.EnsureTitle(ctx, kind, tmdbID, name, poster)
	if err != nil {
		return tracking.Title{}, err
	}
	return ToTitle(x), nil
}

// Kick asks the core to refresh the title soon; an unknown id is the
// SDK's 404 *addonkit.APIError (addonkit.IsNotFound: ensure it again).
func (t Tracking) Kick(ctx context.Context, id int64) error { return t.C.KickTitle(ctx, id) }
