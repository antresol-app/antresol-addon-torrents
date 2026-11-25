// Package followapi is the profiles' «Следить», pinned releases and «Найти
// озвучку…»: /api/follow…, /api/pinned/{kind}/{tmdbId}… and
// /api/items/{id}/voiceovers…, shapes as the core served them before the
// move (legacy follow_handlers, pinned_handlers, voiceovers_handlers).
// Every route is for adult profiles (kids are refused here too, as
// defence in depth: the core keeps them from the proxy); none is
// admin-only, the admins' differences stay inside the handlers (all
// follows, no link check, «force»). main mounts them on the profiles'
// /api/ mux, served once the addon is ready.
package followapi

import (
	"context"
	"net/http"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/httpx"
	"github.com/antresol-app/antresol-addon-torrents/internal/pinned"
	"github.com/antresol-app/antresol/addonkit"
)

// Acquire is what the pin and voice-over routes use of acquire.Service.
type Acquire interface {
	Acquire(ctx context.Context, req acquire.AcquireRequest) (acquire.AcquireResult, error)
	AcquirePinnedFiles(ctx context.Context, ih string, req acquire.AcquireRequest, files []int) (acquire.AcquireResult, error)
	FindVoiceOvers(ctx context.Context, itemID int64) (acquire.VoiceOvers, error)
	DownloadVoiceOver(ctx context.Context, req acquire.VoiceOverRequest) (acquire.VoiceOverStarted, error)
	PreviewVoiceOver(ctx context.Context, itemID int64, id string) (acquire.VoiceOverPreview, error)
	VoiceOverDownloads(ctx context.Context, itemID int64) ([]acquire.VoiceOverDownload, error)
}

// Library is what the routes read of the core's library
// (coreadapt.Library).
type Library interface {
	Item(ctx context.Context, id int64) (library.Item, error)
	ItemByTMDB(ctx context.Context, kind string, tmdbID int64) (library.Item, error)
}

var _ Library = coreadapt.Library{}

// Deps are what the routes need.
type Deps struct {
	// Follows: nil when the core has no TMDB (no follow service): the
	// follow routes are not mounted, as before the move.
	Follows *follow.Service
	// Pins: nil, no pin routes. Its Tracking is the core's tracked titles.
	Pins    *pinned.Service
	Acquire Acquire
	// Library is the core's library: the voice-over routes' item, and the
	// rating gate of both families (a title's library item).
	Library Library
	// Watch registers a started stream's file as the profile's input and
	// returns its id (watch.Deps.Watch); its errors go to WatchError.
	Watch      func(ctx context.Context, u addonkit.User, ih string, idx int) (string, error)
	WatchError func(w http.ResponseWriter, r *http.Request, what string, err error)
	// Added (optional): a download or stream was added (the holds go out
	// at once).
	Added func()
}

func (d Deps) added() {
	if d.Added != nil {
		d.Added()
	}
}

// Register adds the routes to mux (the profiles' /api/ routes).
func Register(mux *http.ServeMux, d Deps) {
	if d.Follows != nil {
		registerFollow(mux, d.Follows)
	}
	if d.Pins != nil && d.Pins.Tracking != nil && d.Acquire != nil && d.Library != nil {
		registerPinned(mux, &pinnedAPI{d})
	}
	if d.Acquire != nil && d.Library != nil {
		registerVoiceOvers(mux, &voiceAPI{d})
	}
}

// adults lets signed-in adult profiles through, in their language.
func adults(h http.HandlerFunc) http.Handler { return httpx.Adults(h) }

// user is the request's profile (adults let only those through).
func user(r *http.Request) addonkit.User {
	if u, ok := addonkit.UserFrom(r); ok {
		return *u
	}
	return addonkit.User{}
}
