// Package channelfetch answers the core's channels.fetch hook: «ТВ»
// channels with fetching want programmes on disk before they air. Each
// wanted programme the addon does not fetch yet is downloaded («На
// сервер», never a stream) as an admin's «Скачать» would, in the
// background; the hook answers at once with how each one is. Downloads
// started here are remembered (the settings row channel_fetches), so a
// dropped key's unfinished download goes with its data and nothing else
// is touched: a download that ran already for someone else is reported
// as existing and never dropped. Upload follows the addon's settings like
// any download (the live server forces «Не раздавать»).
package channelfetch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// Acquire is what the hook uses of acquire.Service.
type Acquire interface {
	Acquire(ctx context.Context, req acquire.AcquireRequest) (acquire.AcquireResult, error)
	Pending(ctx context.Context) ([]acquire.PendingAcquisition, error)
	Remove(ctx context.Context, ih string, deleteData bool) error
}

// Follows says whether a title is followed («Следить», «Хочу посмотреть»).
type Follows interface {
	ByTMDB(ctx context.Context, kind string, tmdbID int) (follow.Follow, error)
}

// Deps are the hook's parts.
type Deps struct {
	Acquire Acquire
	// Torrents lists the engine's torrents (Engine.List).
	Torrents func() []torrent.Status
	Follows  Follows // nil: nothing is followed
	DB       *sql.DB // the addon's settings table
	Log      *slog.Logger
}

const settingsKey = "channel_fetches"

// acquireTimeout bounds one background start (the search of every source).
const acquireTimeout = 5 * time.Minute

// maxSearches background starts search at once; the rest wait (their
// programmes show «searching» meanwhile).
const maxSearches = 2

// entry is a programme fetched for channels.
type entry struct {
	InfoHash string `json:"ih,omitempty"`
	// Complete: its torrent was seen finished (a torrent gone after that
	// was organized, not lost).
	Complete bool   `json:"complete,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Hook is the channels.fetch handler.
type Hook struct {
	d         Deps
	mu        sync.Mutex
	searching map[string]bool
	slots     chan struct{}   // maxSearches places for the background starts
	ctx       context.Context // the addon's lifetime, for the background starts
}

// Register answers channels.fetch on m; ctx bounds the background starts.
func Register(ctx context.Context, m *addonkit.HookMux, d Deps) *Hook {
	h := &Hook{d: d, searching: map[string]bool{}, slots: make(chan struct{}, maxSearches), ctx: ctx}
	m.Handle("channels.fetch", h.serve)
	return h
}

func (h *Hook) log() *slog.Logger {
	if h.d.Log != nil {
		return h.d.Log
	}
	return slog.Default()
}

func (h *Hook) load(ctx context.Context) (map[string]entry, error) {
	out := map[string]entry{}
	var v string
	err := h.d.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, settingsKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		h.log().Warn("channel fetches: bad settings row, starting afresh", "err", err)
		return map[string]entry{}, nil
	}
	return out, nil
}

func (h *Hook) save(ctx context.Context, m map[string]entry) error {
	b, _ := json.Marshal(m)
	_, err := h.d.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, settingsKey, string(b))
	return err
}

// covers: pending acquisition p holds programme w.
func covers(p acquire.PendingAcquisition, w addonkit.ChannelFetchTitle) bool {
	if p.TMDBID != w.TMDBID {
		return false
	}
	if w.Kind == "movie" {
		return p.Kind == acquire.KindMovie
	}
	if p.Kind != acquire.KindSeries || p.Season != w.Season {
		return false
	}
	if p.Episode == 0 {
		return true // the whole season
	}
	end := max(p.EpisodeEnd, p.Episode)
	return w.Episode >= p.Episode && w.Episode <= end
}

func (h *Hook) serve(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var req addonkit.ChannelFetch
	if err := json.Unmarshal(in, &req); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	m, err := h.load(ctx)
	if err != nil {
		return nil, err
	}
	pending, err := h.d.Acquire.Pending(ctx)
	if err != nil {
		return nil, err
	}
	pendingIH := map[string]bool{}
	for _, p := range pending {
		pendingIH[strings.ToLower(p.InfoHash)] = true
	}
	torrents := map[string]torrent.Status{}
	if h.d.Torrents != nil {
		for _, st := range h.d.Torrents() {
			torrents[strings.ToLower(st.InfoHash)] = st
		}
	}
	// what no channel needs any more: an unfinished download of ours goes
	for _, k := range req.Drop {
		e, ok := m[k]
		if !ok {
			continue
		}
		delete(m, k)
		if ih := strings.ToLower(e.InfoHash); ih != "" && pendingIH[ih] && !e.Complete {
			if err := h.d.Acquire.Remove(ctx, e.InfoHash, true); err != nil {
				h.log().Warn("channel fetches: removing a dropped download", "key", k, "err", err)
			}
		}
	}
	out := addonkit.ChannelFetchAnswer{States: []addonkit.ChannelFetchState{}}
	for _, w := range req.Want {
		if w.Key == "" || w.TMDBID <= 0 || (w.Kind != "movie" && w.Kind != "episode") {
			continue
		}
		st := addonkit.ChannelFetchState{Key: w.Key, Keep: h.followed(ctx, w)}
		e, mine := m[w.Key]
		switch {
		case h.searching[w.Key]:
			st.State = "searching"
		case mine && e.Error != "":
			st.State, st.Error = "failed", e.Error
			delete(m, w.Key) // asked again later: a new search
		case mine && e.InfoHash != "":
			ih := strings.ToLower(e.InfoHash)
			t, has := torrents[ih]
			if has {
				st.Size, st.Done = t.Size, t.Done
				if t.Size > 0 && t.Done >= t.Size {
					e.Complete = true
					m[w.Key] = e
				}
			}
			switch {
			case pendingIH[ih] && has:
				st.State, st.Rate = "downloading", t.DownRate
				if e.Complete {
					st.State = "done" // finished, being put into the library
				}
			case e.Complete || (has && !pendingIH[ih]):
				st.State = "done"
			default:
				st.State, st.Error = "failed", "Загрузка пропала"
				delete(m, w.Key)
			}
		default:
			// someone's download of it runs already: not the channel's
			found := false
			for _, p := range pending {
				if covers(p, w) {
					found = true
					st.State, st.Existing = "downloading", true
					if t, ok := torrents[strings.ToLower(p.InfoHash)]; ok {
						st.Size, st.Done, st.Rate = t.Size, t.Done, t.DownRate
					}
					break
				}
			}
			if !found {
				st.State = "searching"
				h.start(w)
			}
		}
		out.States = append(out.States, st)
	}
	if err := h.save(ctx, m); err != nil {
		return nil, err
	}
	return out, nil
}

func (h *Hook) followed(ctx context.Context, w addonkit.ChannelFetchTitle) bool {
	if h.d.Follows == nil {
		return false
	}
	kind := follow.KindMovie
	if w.Kind == "episode" {
		kind = follow.KindSeries
	}
	f, err := h.d.Follows.ByTMDB(ctx, kind, w.TMDBID)
	return err == nil && f.Active
}

// start downloads w in the background (h.mu held).
func (h *Hook) start(w addonkit.ChannelFetchTitle) {
	h.searching[w.Key] = true
	go func() {
		res, err := h.acquire(w)
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.searching, w.Key)
		if h.ctx.Err() != nil {
			return // stopping: asked again after the restart
		}
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		m, lerr := h.load(sctx)
		if lerr != nil {
			h.log().Warn("channel fetches: load", "err", lerr)
			return
		}
		e := entry{InfoHash: res.InfoHash}
		if err != nil {
			e = entry{Error: errorText(err)}
			h.log().Info("channel fetches: could not start", "key", w.Key, "err", err)
		}
		m[w.Key] = e
		if serr := h.save(sctx, m); serr != nil {
			h.log().Warn("channel fetches: save", "err", serr)
		}
	}()
}

// acquire runs the search and start of w once a place is free (at most
// maxSearches at once); its time limit counts from then.
func (h *Hook) acquire(w addonkit.ChannelFetchTitle) (acquire.AcquireResult, error) {
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	case <-h.ctx.Done():
		return acquire.AcquireResult{}, h.ctx.Err()
	}
	ctx, cancel := context.WithTimeout(h.ctx, acquireTimeout)
	defer cancel()
	req := acquire.AcquireRequest{TMDBID: w.TMDBID, Kind: acquire.KindMovie, Mode: acquire.ModeDownload, Background: true}
	if w.Kind == "episode" {
		req.Kind, req.Season, req.Episode = acquire.KindSeries, w.Season, w.Episode
	}
	return h.d.Acquire.Acquire(ctx, req)
}

// errorText is what the admin is shown of a failed start (Russian).
func errorText(err error) string {
	switch {
	case errors.Is(err, acquire.ErrNoRelease):
		return "Раздача не найдена"
	case errors.Is(err, acquire.ErrUnavailable):
		return "Источники недоступны"
	}
	if msg := err.Error(); msg != "" && len(msg) < 300 {
		return msg
	}
	return "Не удалось начать загрузку"
}
