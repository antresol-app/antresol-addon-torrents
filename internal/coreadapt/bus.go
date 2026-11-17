package coreadapt

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/events"
	"github.com/antresol-app/antresol/addonkit"
)

// The addon's own event types and the app events they go out as.
// The values are acquire.EventProgress, indexer.EventSourceStatus and
// follow.EventFollowQueued/EventFollowChecked: acquire imports this
// package (and follow imports acquire), so they cannot be imported here.
const (
	EventProgress        = "torrent.progress"         // acquire.EventProgress: the download list, every 2 s
	EventLibraryProgress = "torrent.library-progress" // acquire.EventLibraryProgress: its adults' view
	EventSourceStatus    = "source.status"            // indexer.EventSourceStatus: a source's new status
	EventFollowQueued    = "follow.queued"            // follow.EventFollowQueued: a check queued downloads
	EventFollowChecked   = "follow.checked"           // follow.EventFollowChecked: a check finished

	AppProgress        = "admin.progress"      // admins only
	AppLibraryProgress = "progress"            // adults
	AppSourceStatus    = "admin.source-status" // admins only
	AppFollowQueued    = "admin.follow-queued" // admins only
	AppFollowChecked   = "follow-checked"      // adults: the reload signal
)

// forwarded maps the bus's event types to the app's event names; others
// stay in the addon (library.updated: the core publishes it itself after
// /library/import).
var forwarded = map[string]string{
	EventProgress:        AppProgress,
	EventLibraryProgress: AppLibraryProgress,
	EventSourceStatus:    AppSourceStatus,
	EventFollowQueued:    AppFollowQueued,
	EventFollowChecked:   AppFollowChecked,
}

// shaped cuts an event's data before it goes out (nil: as it is).
// follow.checked reaches every adult profile: its episode codes and its
// note (which may name library titles, and is Russian) are for admins and
// the follow's author, so the app event carries only the follow's id;
// the page reloads the follow, whose list is cut to each profile and
// localized (GET /api/follow). Admins have the codes in follow-queued.
var shaped = map[string]func(any) any{
	EventFollowChecked: followIDOnly,
}

// followIDOnly is {"followId": …} of a follow event's data.
func followIDOnly(data any) any {
	var v struct {
		FollowID int64 `json:"followId"`
	}
	if b, err := json.Marshal(data); err == nil {
		json.Unmarshal(b, &v)
	}
	return v
}

// tooBigLogEvery spaces the log lines of events the core refuses as too
// big (413): the download list goes out every 2 s and would repeat it.
const tooBigLogEvery = 10 * time.Minute

// Forward publishes the bus's download list (admins), its adults' view
// (progress), source statuses and follow checks to the app through the core (POST
// /events) until ctx ends or the bus closes. It listens to all three at
// all times: acquire then builds the list every 2 s (once, for both).
// A failed call is logged and the next event goes on; one refused as too
// big (413) is logged once per tooBigLogEvery per name, with its size.
func Forward(ctx context.Context, bus *events.Bus, c *addonkit.Client) {
	ch, cancel := bus.Subscribe(func(typ string) bool { _, ok := forwarded[typ]; return ok })
	defer cancel()
	tooBigAt := map[string]time.Time{}
	tooBigSince := map[string]int{}
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-ch:
			if !ok {
				return
			}
			name, fwd := forwarded[e.Type]
			if !fwd {
				continue
			}
			data := e.Data
			if shape := shaped[e.Type]; shape != nil {
				data = shape(data)
			}
			err := c.Event(ctx, name, data)
			if err == nil || ctx.Err() != nil {
				continue
			}
			var ae *addonkit.APIError
			if !errors.As(err, &ae) || ae.Status != http.StatusRequestEntityTooLarge {
				slog.Warn("coreadapt: event", "name", name, "err", err)
				continue
			}
			if at, ok := tooBigAt[name]; ok && time.Since(at) < tooBigLogEvery {
				tooBigSince[name]++
				continue
			}
			b, _ := json.Marshal(map[string]any{"name": name, "data": data})
			slog.Warn("coreadapt: event too big", "name", name, "bytes", len(b), "refusedSinceLast", tooBigSince[name], "err", err)
			tooBigAt[name], tooBigSince[name] = time.Now(), 0
		}
	}
}
