package libraryhooks

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol/addonkit"
)

// pendingKinds maps acquisition kinds to the library kinds of their cards.
var pendingKinds = map[string]string{
	acquire.KindMovie: library.KindMovie, acquire.KindSeries: library.KindSeries, acquire.KindMusic: library.KindAlbum,
	acquire.KindBook: library.KindBook, acquire.KindAudiobook: library.KindAudiobook, acquire.KindComic: library.KindComic,
}

// downloadsPage is where an admin's card of a title without a page opens.
const downloadsPage = "/addons/torrents/downloads"

// pending is library.pending {scope}: the titles on their way into the
// library, newest first, as cards; scope "mine" those the profile
// (X-Antresol-User) asked for. Error cards go too: the core shows them to
// admins only. The hint is in the profile's language; a movie or series
// card opens its title page, another one the downloads page for an admin
// and nothing for others. The poster is the core's image name the
// download's cover was cached under when it started.
func (d Deps) pending(ctx context.Context, u *addonkit.User, in json.RawMessage) (any, error) {
	var q addonkit.PendingQuery
	if err := json.Unmarshal(in, &q); err != nil {
		return nil, err
	}
	mine, owner := false, "" // «Моя медиатека»: the profile's own, by uid
	lang, admin := i18n.RU, false
	if u != nil {
		if q.Scope == "mine" {
			mine, owner = true, u.UID
		}
		if u.Lang == i18n.EN {
			lang = i18n.EN
		}
		admin = u.IsAdmin
	}
	cards := []addonkit.PendingCard{}
	for _, dl := range d.Acquire.LibraryDownloads(ctx) {
		kind, ok := pendingKinds[dl.Kind]
		if !ok || (mine && (owner == "" || !slices.Contains(dl.By, owner))) {
			continue
		}
		c := addonkit.PendingCardInfo{ID: dl.ItemID, Kind: kind, Title: dl.Title, Subtitle: dl.Creator, Year: dl.Year, Poster: dl.Poster}
		st := addonkit.PendingState{Key: dl.Key, Status: dl.Status, Done: dl.Done, Size: dl.Size}
		switch {
		case (dl.Kind == acquire.KindMovie || dl.Kind == acquire.KindSeries) && dl.TMDBID > 0:
			c.TmdbKind, c.TmdbID = dl.Kind, int64(dl.TMDBID)
			st.Href = "/title/" + dl.Kind + "/" + strconv.Itoa(dl.TMDBID)
		case admin: // other kinds, and a movie or series without a TMDB id
			st.Href = downloadsPage
		}
		if dl.Status == acquire.DownloadActive {
			if h := acquire.StallHint(dl.Stalled, dl.Liveness, dl.TrackerErrorKind); h != nil {
				st.Hint = h.RU
				if lang == i18n.EN {
					st.Hint = h.EN
				}
			}
		}
		cards = append(cards, addonkit.PendingCard{Card: c, Pending: st})
	}
	return addonkit.PendingAnswer{Cards: cards}, nil
}
