package trackinghooks

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/pinned"
	"github.com/antresol-app/antresol/addonkit"
)

// syncTimeout bounds one after-scan Sync (as the core's scan timeout
// bounded it when it ran inside the scan). Its downloads themselves run
// on (start under) a context that does not end with it.
var syncTimeout = 2 * time.Minute

// sourcesAnswer is a scan's 200 with sources: the list always present, so
// none is {"sources": []} (the kind's rows go), never a bare {}.
type sourcesAnswer struct {
	Sources []addonkit.TrackingSource `json:"sources"`
}

// errBadScan: a body without a title (a failure: the core keeps the rows).
var errBadScan = errors.New("trackinghooks: a scan request without its title")

// scan answers a tracking.scan:<kind> hook with sc: its sources, or
// {unchanged: true} exactly when it returns pinned.ErrUnchanged; another
// error is a failure (500: the core logs it and keeps the rows).
func scan(ctx context.Context, sc Scanner, in json.RawMessage) (tracking.Title, any, error) {
	ctx = indexer.Background(ctx) // the core's round of scans: no one waits on its searches
	var req addonkit.ScanRequest
	if err := json.Unmarshal(in, &req); err != nil || req.Title.ID <= 0 {
		return tracking.Title{}, nil, errBadScan
	}
	t := coreadapt.ToTitle(req.Title)
	eps := make([]tracking.Episode, len(req.Episodes))
	for i, e := range req.Episodes {
		eps[i] = tracking.Episode{TitleID: e.TitleID, Season: e.Season, Number: e.Number, Name: e.Name, AirDate: e.AirDate}
	}
	srcs, err := sc.Scan(ctx, t, eps)
	if errors.Is(err, pinned.ErrUnchanged) {
		return t, addonkit.ScanAnswer{Unchanged: true}, nil
	}
	if err != nil {
		return t, nil, err
	}
	out := sourcesAnswer{Sources: make([]addonkit.TrackingSource, len(srcs))}
	for i, s := range srcs {
		out.Sources[i] = coreadapt.FromSource(s)
	}
	return t, out, nil
}

// scanPinned: the pin's sources, then the followed title's missing
// episodes from the pin: Sync runs once per scan, after the answer
// is made, in the background on the run context (the core stores the
// rows meanwhile; Sync reads only library rows), also when the scan
// failed (as the deferred AfterScan did once the pin was refreshed).
func (d Deps) scanPinned(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	t, out, err := scan(ctx, d.Pinned, in)
	if t.ID > 0 && d.Sync != nil {
		d.afterScan(t)
	}
	return out, err
}

func (d Deps) afterScan(t tracking.Title) {
	run := d.Run
	if run == nil {
		run = context.Background()
	}
	if run.Err() != nil {
		return // stopping
	}
	if d.wait != nil {
		d.wait.Add(1)
	}
	go func() {
		if d.wait != nil {
			defer d.wait.Done()
		}
		ctx, cancel := context.WithTimeout(indexer.Background(run), syncTimeout)
		defer cancel()
		if err := d.Sync(ctx, t); err != nil && run.Err() == nil {
			d.log().Warn("torrents: download the missing episodes of a pinned release", "title", t.ID, "err", err)
		}
	}()
}

// scanTorrent: the torrent search's sources; it never syncs.
func (d Deps) scanTorrent(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	_, out, err := scan(ctx, d.Torrent, in)
	return out, err
}
