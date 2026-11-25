package indexer

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// What one source answered to one query is reused for searchTTL, and
// identical searches that run at once share one (searchCacheMax answers
// are kept): the same title asked twice within seconds (the picker, a
// phone, a run) is not a second fan-out to every tracker with its logins.
// What is cached is the source's raw answer; scoring, deduplication and the
// preferences are applied to it each time, so they are never stale.
const (
	searchTTL      = 90 * time.Second
	searchCacheMax = 64
)

type cachedAnswer struct {
	results []Result
	at      time.Time
}

// flight is a search of one source that others wait for.
type flight struct {
	done    chan struct{}
	results []Result
	err     error
}

type searchCache struct {
	mu      sync.Mutex
	answers map[string]cachedAnswer
	flights map[string]*flight
}

// searchKey is what a source's answer depends on: the source as set up, the
// kind, the voice-over switch, the season (a non-Russian tracker is asked
// "<title> S01") and the queries.
func searchKey(s Source, q SearchRequest, queries []string) string {
	return fmt.Sprintf("%s\x00%d\x00%s\x00%t\x00%d\x00%s",
		runnerKey(s.DefinitionID, s.Settings), s.ID, q.Kind, q.VoiceOvers, q.Season, strings.Join(queries, "\x00"))
}

// cachedSearch is searchSource for a search a person waits for. Background
// searches (they must see a source's present state, and a paused one's
// pause) and settling ones (a start does not take old answers) go straight
// to the source, and only an answer without an error is kept.
func (m *Manager) cachedSearch(ctx context.Context, s Source, q SearchRequest, queries []string) ([]Result, error) {
	if IsBackground(ctx) || Settling(ctx) {
		return m.searchSource(ctx, s, q, queries)
	}
	c, key := &m.searches, searchKey(s, q, queries)
	for {
		c.mu.Lock()
		if a, ok := c.answers[key]; ok && m.now().Sub(a.at) < searchTTL {
			c.mu.Unlock()
			return slices.Clone(a.results), nil
		}
		if f, ok := c.flights[key]; ok {
			c.mu.Unlock()
			select {
			case <-f.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if f.err != nil && (errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded)) && ctx.Err() == nil {
				continue // the asker it waited for left: ask again, as the leader now
			}
			return slices.Clone(f.results), f.err
		}
		f := &flight{done: make(chan struct{})}
		if c.flights == nil {
			c.flights = map[string]*flight{}
		}
		c.flights[key] = f
		c.mu.Unlock()

		f.results, f.err = m.searchSource(ctx, s, q, queries)

		c.mu.Lock()
		delete(c.flights, key)
		if f.err == nil {
			c.keep(key, f.results, m.now())
		}
		c.mu.Unlock()
		close(f.done)
		return f.results, f.err
	}
}

// keep stores an answer, dropping the expired ones and, when full, the
// oldest. c.mu must be held.
func (c *searchCache) keep(key string, results []Result, now time.Time) {
	if c.answers == nil {
		c.answers = map[string]cachedAnswer{}
	}
	for k, a := range c.answers {
		if now.Sub(a.at) >= searchTTL {
			delete(c.answers, k)
		}
	}
	for len(c.answers) >= searchCacheMax {
		oldest, at := "", now
		for k, a := range c.answers {
			if a.at.Before(at) || oldest == "" {
				oldest, at = k, a.at
			}
		}
		delete(c.answers, oldest)
	}
	c.answers[key] = cachedAnswer{results: slices.Clone(results), at: now}
}
