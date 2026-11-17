package holds

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

const pendingFor = 5 * time.Minute

// Hooks answer the core's files.* hooks (contract: "File hooks") for the
// holds the Pusher sent: what follows a move, holding and releasing the
// downloads around it, stopping the ones whose data moved away, and
// re-pointing the ones that follow (BeginRelocate on detach, Finish on
// attach).
type Hooks struct {
	Engine *torrent.Engine
	// Remove drops a download, its data kept (acquire.Service.Remove with
	// deleteData false).
	Remove func(ctx context.Context, ih string) error
	// Pending (5 min): a detached relocation not attached in time is
	// finished by what is on disk (renamed: from gone and to there).
	Pending time.Duration
	// Changed (optional) is told after a hook changed a download (the
	// Pusher's Changed).
	Changed func()
	Log     *slog.Logger

	mu      sync.Mutex
	pending map[[2]string]*pendingMove
	paused  map[string]string // held by files.pause → the engine's hold before ("" or waiting_space)
}

type pendingMove struct {
	r     *torrent.Relocation
	timer *time.Timer
}

type keysBody struct {
	Keys []string `json:"keys"`
}

type planBody struct {
	Moves []struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"moves"`
	Keys []string `json:"keys"`
}

type moveBody struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Keys    []string `json:"keys"`
	Renamed bool     `json:"renamed"`
}

// Register handles the six files.* hooks on m.
func (h *Hooks) Register(m *addonkit.HookMux) {
	m.Handle("files.plan", h.plan)
	m.Handle("files.pause", h.pause)
	m.Handle("files.resume", h.resume)
	m.Handle("files.stop", h.stop)
	m.Handle("files.detach", h.detach)
	m.Handle("files.attach", h.attach)
}

func (h *Hooks) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.Default()
}

func (h *Hooks) changed() {
	if h.Changed != nil {
		h.Changed()
	}
}

func decode(in json.RawMessage, v any) error {
	if err := json.Unmarshal(in, v); err != nil {
		return fmt.Errorf("holds: bad hook body: %w", err) // a failure (500), not a refusal
	}
	return nil
}

// refusal is the 409 of a hook that could not hold the data, in the
// profile's language: today's file-operation conflict text.
func refusal(u *addonkit.User, err error) error {
	ctx := context.Background()
	if u != nil && u.Lang != "" {
		ctx = i18n.WithLanguage(ctx, u.Lang)
	}
	if m, ok := acquire.UserMessage(err); ok {
		return addonkit.Refuse(i18n.T(ctx, "%s. Файлы остались на месте", i18n.Message(ctx, userMsg{m, err})))
	}
	return addonkit.Refuse(i18n.T(ctx, "Не удалось остановить раздачу этих файлов, поэтому они остались на месте. Попробуйте ещё раз"))
}

// userMsg is acquire's user message with its error chain (an *i18n.Error
// in it is rendered in the profile's language).
type userMsg struct {
	msg string
	err error
}

func (e userMsg) Error() string { return e.msg }
func (e userMsg) Unwrap() error { return e.err }

// files.plan {moves, keys} → {follow, stop}: per move what the engine can
// re-point and what it must stop, of the keys asked; a key a move stops
// does not follow.
func (h *Hooks) plan(_ context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var b planBody
	if err := decode(in, &b); err != nil {
		return nil, err
	}
	asked := map[string]bool{}
	for _, k := range b.Keys {
		asked[lower(k)] = true
	}
	fol, stop := map[string]bool{}, map[string]bool{}
	for _, mv := range b.Moves {
		f, s := h.Engine.PlanRelocate(mv.From, mv.To)
		for _, ih := range f {
			fol[lower(ih)] = true
		}
		for _, ih := range s {
			stop[lower(ih)] = true
		}
	}
	out := struct {
		Follow []string `json:"follow"`
		Stop   []string `json:"stop"`
	}{Follow: []string{}, Stop: []string{}}
	for _, k := range b.Keys {
		k = lower(k)
		switch {
		case stop[k] && !slices.Contains(out.Stop, k):
			out.Stop = append(out.Stop, k)
		case fol[k] && !stop[k] && !slices.Contains(out.Follow, k):
			out.Follow = append(out.Follow, k)
		}
	}
	return out, nil
}

// statuses maps the engine's torrents by lower-case infohash (one List
// per hook call).
func (h *Hooks) statuses() map[string]torrent.Status {
	m := map[string]torrent.Status{}
	for _, s := range h.Engine.List() {
		m[lower(s.InfoHash)] = s
	}
	return m
}

// files.pause {keys}: the downloads stop using their data. One already
// paused (by the admin) is left as it is, and files.resume leaves it so;
// one waiting for disk space goes back to waiting on resume.
func (h *Hooks) pause(ctx context.Context, u *addonkit.User, in json.RawMessage) (any, error) {
	var b keysBody
	if err := decode(in, &b); err != nil {
		return nil, err
	}
	all := h.statuses()
	for _, k := range b.Keys {
		k = lower(k)
		st, ok := all[k]
		if !ok || st.Status == torrent.StatusPaused {
			continue
		}
		before := ""
		if st.Status == torrent.StatusWaitingSpace {
			before = torrent.StatusWaitingSpace
		}
		if err := h.Engine.Pause(ctx, k); err != nil {
			if errors.Is(err, torrent.ErrNotFound) {
				continue
			}
			return nil, refusal(u, err)
		}
		h.mu.Lock()
		if h.paused == nil {
			h.paused = map[string]string{}
		}
		h.paused[k] = before
		h.mu.Unlock()
	}
	h.changed()
	return nil, nil
}

// files.resume {keys}: the downloads files.pause held use their data again
// (harmless for any other key).
func (h *Hooks) resume(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var b keysBody
	if err := decode(in, &b); err != nil {
		return nil, err
	}
	var errs []error
	for _, k := range b.Keys {
		k = lower(k)
		h.mu.Lock()
		before, was := h.paused[k]
		delete(h.paused, k)
		h.mu.Unlock()
		if !was {
			continue
		}
		var err error
		if before == torrent.StatusWaitingSpace {
			err = h.Engine.HoldForSpace(ctx, k)
		} else {
			err = h.Engine.Resume(ctx, k)
		}
		if err != nil && !errors.Is(err, torrent.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	h.changed()
	return nil, errors.Join(errs...)
}

// files.stop {keys}: the data moved away; the downloads go, the data kept.
func (h *Hooks) stop(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var b keysBody
	if err := decode(in, &b); err != nil {
		return nil, err
	}
	var errs []error
	for _, k := range b.Keys {
		k = lower(k)
		h.mu.Lock()
		delete(h.paused, k)
		h.mu.Unlock()
		if err := h.Remove(ctx, k); err != nil && !errors.Is(err, torrent.ErrNotFound) && !errors.Is(err, acquire.ErrNotFound) {
			h.log().Error("torrents: files.stop", "key", k, "err", err)
			errs = append(errs, err)
		}
	}
	h.changed()
	return nil, errors.Join(errs...)
}

// files.detach {from, to, keys}: the downloads that follow the move are
// dropped from the client until files.attach (or Pending).
func (h *Hooks) detach(ctx context.Context, u *addonkit.User, in json.RawMessage) (any, error) {
	var b moveBody
	if err := decode(in, &b); err != nil {
		return nil, err
	}
	r, err := h.Engine.BeginRelocate(context.WithoutCancel(ctx), b.From, b.To)
	if err != nil {
		return nil, refusal(u, err)
	}
	id := [2]string{b.From, b.To}
	h.mu.Lock()
	if h.pending == nil {
		h.pending = map[[2]string]*pendingMove{}
	}
	old := h.pending[id]
	pm := &pendingMove{r: r}
	h.pending[id] = pm
	wait := h.Pending
	if wait <= 0 {
		wait = pendingFor
	}
	pm.timer = time.AfterFunc(wait, func() { h.lost(id, pm) })
	h.mu.Unlock()
	if old != nil {
		// a second detach of the same move: only reachable when the first
		// held no torrent (one still moving makes BeginRelocate ErrBusy)
		old.timer.Stop()
		h.finish(old, onDisk(b.From, b.To))
	}
	return nil, nil
}

// files.attach {from, to, keys, renamed}: the move is over; the downloads
// are re-pointed (renamed) or restored. One with nothing pending is fine.
func (h *Hooks) attach(_ context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var b moveBody
	if err := decode(in, &b); err != nil {
		return nil, err
	}
	id := [2]string{b.From, b.To}
	h.mu.Lock()
	pm := h.pending[id]
	delete(h.pending, id)
	h.mu.Unlock()
	if pm == nil {
		h.log().Info("torrents: files.attach with no relocation pending", "from", b.From, "to", b.To)
		return nil, nil
	}
	pm.timer.Stop()
	h.finish(pm, b.Renamed)
	return nil, nil
}

// lost finishes a relocation never attached by what is on disk.
func (h *Hooks) lost(id [2]string, pm *pendingMove) {
	h.mu.Lock()
	if h.pending[id] != pm {
		h.mu.Unlock()
		return
	}
	delete(h.pending, id)
	h.mu.Unlock()
	renamed := onDisk(id[0], id[1])
	h.log().Warn("torrents: files.attach never came; finished by the disk", "from", id[0], "to", id[1], "renamed", renamed)
	h.finish(pm, renamed)
}

func (h *Hooks) finish(pm *pendingMove, renamed bool) {
	if err := pm.r.Finish(context.Background(), renamed); err != nil {
		h.log().Error("torrents: finish a relocation", "err", err)
	}
	h.changed()
}

// onDisk: the move happened (from is gone, to is there).
func onDisk(from, to string) bool {
	_, ferr := os.Lstat(from)
	_, terr := os.Lstat(to)
	return errors.Is(ferr, os.ErrNotExist) && terr == nil
}
