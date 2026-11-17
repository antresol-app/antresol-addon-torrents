package studios

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// Core read timing: the core's list is read again once a minute (a
// change reaches the scoring up to that late); one read waits at
// most readTimeout.
const (
	coreTTL     = 60 * time.Second
	readTimeout = 5 * time.Second
)

// Core is the household's favourite studios («Любимые озвучки») as the
// core keeps them (GET /settings/studios, addonkit.Client.Studios): read
// at most once a minute; a failed read keeps the last answer (none before
// the first) and is tried again only after the minute too, so a core that
// is down costs one search one timeout a minute. Safe for concurrent use.
type Core struct {
	Studios func(ctx context.Context) ([]string, error)
	Now     func() time.Time // tests
	Log     *slog.Logger

	mu   sync.Mutex // held over a read: concurrent searches ask once
	list []string
	next time.Time // when to read again
}

// List is the core's list, cached.
func (c *Core) List(ctx context.Context) []string {
	if c == nil || c.Studios == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if t := now(); !t.Before(c.next) {
		rctx, cancel := context.WithTimeout(ctx, readTimeout)
		l, err := c.Studios(rctx)
		cancel()
		c.next = t.Add(coreTTL)
		switch {
		case err == nil:
			c.list = l
		case ctx.Err() != nil:
			c.next = time.Time{} // the search's own end: not the core's fault, ask the next one
		default:
			log := c.Log
			if log == nil {
				log = slog.Default()
			}
			log.Warn("torrents: the core's favourite studios (the last list kept)", "err", err, "kept", len(c.list))
		}
	}
	return append([]string(nil), c.list...)
}

// Merge is the studios a search scores with: the core's first, then own
// (the addon's release prefs) without the ones already named (by
// release.StudioKey). A nil Core gives own as it is.
func (c *Core) Merge(ctx context.Context, own []string) []string {
	if c == nil || c.Studios == nil {
		return own
	}
	return release.MergeStudios(c.List(ctx), own)
}
