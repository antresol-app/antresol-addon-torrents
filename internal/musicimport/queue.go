//go:build ignore

package musicimport

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// AcquireAPI is the part of *acquire.Service the queue uses.
type AcquireAPI interface {
	PlanTracks(ctx context.Context, req acquire.TrackAcquireRequest) (acquire.TrackPlan, error)
	AcquireTracks(ctx context.Context, req acquire.TrackAcquireRequest, p acquire.TrackPlan) (acquire.TrackAcquireResult, error)
	TrackDownload(ctx context.Context, ih string) (acquire.TrackDownload, error)
	InProgress(ctx context.Context, extID string, albumID int64, except string) (string, bool, error)
	Disk() (acquire.DiskUsage, error)
	Remove(ctx context.Context, ih string, deleteData bool) error
}

// LibraryAPI is the part of *library.Repo the queue uses.
type LibraryAPI interface {
	FindAlbumByMBID(ctx context.Context, rgid string) (library.Item, bool, error)
	FindAlbum(ctx context.Context, artist, album string) (library.Item, bool, error)
	AlbumFolderTracks(ctx context.Context, albumID int64) ([]library.Track, error)
	TracksOfTorrent(ctx context.Context, ih string) ([]library.Track, error)
}

var (
	_ AcquireAPI = (*acquire.Service)(nil)
	_ LibraryAPI = (*library.Repo)(nil)
)

const (
	defaultMaxGroups = 3
	defaultGap       = 5 * time.Second
	// retryAfter: a failed search is tried again once, this long after.
	retryAfter = 30 * time.Minute
	// spaceShare: a download starts only if the disk stays under this
	// share of its size (the disk guard's 95 %).
	spaceShare = 0.95
	// stallAfter: a download an album waits for that made no progress this
	// long is given up (the album searches its own release).
	stallAfter = 6 * time.Hour
	// The album's own download is given up when, counting only the time
	// it runs (downloading or fetching its metadata while the server is
	// up), it got no byte for noStartAfter (peers that connect but hold
	// none of the chosen files, a swarm that never sends the metadata) or
	// made no progress for stallAfter — dead: removed with its files, its
	// release never searched again — or, running slowAfter already, would
	// at its average rate need more than slowETA — slow: removed keeping
	// its files, its release taken again only when no other holds the
	// songs. A paused, held or failed one is not counted, and nothing is
	// given up before it has run upTime since this server started.
	//
	// The download is the import's own (AcquireTracks refuses one that
	// runs already). A «На сервер» of the same album meanwhile gets that
	// download as its own (Existing) and asks nothing else of it; giving
	// up a dead one serves that request too: the album is searched again,
	// as the automatic switch away from a dead swarm does for any
	// download.
	noStartAfter = time.Hour
	slowAfter    = 12 * time.Hour
	slowETA      = 72 * time.Hour
	upTime       = time.Hour
	// maxTickGap: the most one gap between two ticks counts (a slow
	// search holds a tick up; a longer gap is the server asleep).
	maxTickGap = 5 * time.Minute
	// maxDead: the album fails (only «Повторить» is left) once this many
	// of its downloads were given up as dead.
	maxDead = 3

	msgError     = "Ошибка"
	msgNoRelease = "Подходящих раздач не нашлось"
	msgNoFormat  = "Нет раздачи в нужном формате"
	msgRemoved   = "Раздачу удалили в «Загрузках»"
	msgDead      = "Раздачи альбома не качаются: их никто не раздаёт"
	msgDeadOnly  = "Подходящих вариантов не нашлось: найденные раздачи не качаются"
)

// Queue hands a job's albums to the acquire pipeline: at most Max at a
// time are searched or downloaded, searches at least Gap apart, never past
// the disk's 95 % (a group that does not fit waits for space). Songs the
// library has already are linked, not downloaded — every album of a job
// is looked up once before its first download starts, so they need no
// slot; the rest of an album the library has go into its folder. An
// album downloading already (another job's, or «На сервер») is waited
// for, never fetched twice. Every state is in the database: a restart
// resumes downloads without adding them again.
type Queue struct {
	Acq   AcquireAPI
	Lib   LibraryAPI
	Store *playlistimport.Store
	Max   int           // 0: 3
	Gap   time.Duration // 0: 5 s
	Now   func() time.Time
	// Rebuild runs after an album finished (the job's playlists and stars).
	Rebuild func(ctx context.Context, jobID int64) error
	Log     *slog.Logger

	mu       sync.Mutex
	lastPlan time.Time
	heldAt   map[int64]int64 // group → disk used when it was held for space
	swept    map[int64]bool  // jobs whose albums were all looked up in the library
	obs      map[int64]*observed
}

// qs is the download-only SQL over the queue's store.
func (q *Queue) qs() queueStore { return queueStore{q.Store} }

func (q *Queue) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

func (q *Queue) log() *slog.Logger {
	if q.Log != nil {
		return q.Log
	}
	return slog.Default()
}

// Step advances the job's albums once (called every tick in downloading):
// downloads that ended are linked to the library, then new albums start.
func (q *Queue) Step(ctx context.Context, jobID int64) error {
	if q == nil || q.Store == nil || q.Acq == nil || q.Lib == nil {
		return nil
	}
	j, err := q.Store.Job(ctx, jobID)
	if err != nil || j.Status != playlistimport.StatusDownloading {
		return err
	}
	reserved, err := q.follow(ctx, j)
	if err != nil {
		return err
	}
	return q.start(ctx, j, reserved)
}

// Downloaded is how many bytes of the download ih are on disk
// (playlistimport.DownloadProgress); known false without the acquire
// service.
func (q *Queue) Downloaded(ctx context.Context, ih string) (int64, bool, error) {
	if q == nil || q.Acq == nil {
		return 0, false, nil
	}
	d, err := q.Acq.TrackDownload(ctx, ih)
	return d.Done, err == nil, err
}

// follow checks the job's downloads; it returns the bytes they still need.
func (q *Queue) follow(ctx context.Context, j playlistimport.Job) (int64, error) {
	gs, err := q.Store.GroupsWhere(ctx, `g.job_id = ? AND g.state = ?`, j.ID, playlistimport.GroupDownloading)
	if err != nil {
		return 0, err
	}
	var reserved int64
	for _, g := range gs {
		d, err := q.Acq.TrackDownload(ctx, g.InfoHash)
		if err != nil {
			return reserved, err
		}
		at := q.now()
		switch {
		case d.Organized:
			err = q.finish(ctx, g, at)
		case !d.Present:
			err = q.gone(ctx, g, at)
		case d.OrganizeError != "" && d.Exhausted:
			err = q.failDownload(ctx, g, d.OrganizeError, at)
		default:
			gaveUp, err := q.headway(ctx, g, d, at)
			if err != nil {
				return reserved, err
			}
			if !gaveUp {
				reserved += max(g.Bytes-d.Done, 0)
			}
			continue
		}
		if err != nil {
			return reserved, err
		}
		q.rebuild(ctx, j.ID)
	}
	return reserved, nil
}

// headway keeps the running clock of the album's own download and gives
// it up when it makes none (see noStartAfter). The clock only runs while
// the engine is downloading the torrent or fetching its metadata and this
// server is up (ticks of this process; a gap is counted as maxTickGap at
// most); nothing is given up before it has run upTime in this process.
// A dead download is removed with its partial files — the import added it
// (see the note at noStartAfter on «На сервер») — and its release is left
// out of the album's next search; a slow one is removed keeping its files
// and taken again only when no other release holds the songs.
// gaveUp: it was given up.
func (q *Queue) headway(ctx context.Context, g playlistimport.Group, d acquire.TrackDownload, now time.Time) (gaveUp bool, err error) {
	c := g.Clock
	changed := false
	if c.Base < 0 { // a download from before this version: its clock starts now
		c.Base, changed = d.Done, true
	}
	running := d.Status == torrent.StatusDownloading || d.Status == torrent.StatusQueued
	up := q.observe(g.ID, g.InfoHash, now, running)
	if up.dt > 0 {
		c.Run += up.dt
		c.Quiet += up.dt
	}
	if d.Done > c.Done {
		c.Done, c.Quiet, changed = d.Done, 0, true
	}
	if changed || now.Sub(up.saved) >= time.Minute {
		if err := q.qs().saveClock(ctx, g.ID, g.InfoHash, c); err != nil {
			return false, err
		}
		q.saved(g.ID, now)
	}
	if !running || up.ran < upTime {
		return false, nil
	}
	size := d.Size
	if size <= 0 {
		size = g.Bytes
	}
	why, slow := "", false
	switch {
	case c.Done == 0 && c.Quiet >= noStartAfter:
		why = "no byte"
	case c.Quiet >= stallAfter:
		why = "no progress"
	case c.Run >= slowAfter && c.Done > c.Base && size > c.Done &&
		time.Duration(float64(size-c.Done)/float64(c.Done-c.Base)*float64(c.Run)) > slowETA:
		why, slow = "too slow", true
	default:
		return false, nil
	}
	q.log().Info("import: giving up a download that makes no headway", "job", g.JobID, "album", g.Album, "infohash", g.InfoHash,
		"why", why, "done", c.Done, "size", size, "running", c.Run.Round(time.Minute), "status", d.Status)
	// listed first: a removal whose requeue fails is not taken for an
	// admin's removal on the next tick (gone)
	if err := q.qs().markGivenUp(ctx, g.ID, g.InfoHash, slow); err != nil {
		return false, err
	}
	if err := q.Acq.Remove(ctx, g.InfoHash, !slow); err != nil && !errors.Is(err, acquire.ErrNotFound) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// organizing right now (it ended after all), or the disk is away:
		// tried again on the next tick
		q.log().Warn("import: remove a stalled download", "infohash", g.InfoHash, "err", err)
		return false, nil
	}
	q.forget(g.ID)
	return true, q.qs().requeue(ctx, g.ID, g.InfoHash, msgDead, maxDead, now)
}

// tick is what observe tells of a download: the running time not saved
// yet (since the last save of its clock; 0 on the first tick of this
// process), its running time in this process, and when its clock was
// last saved.
type tick struct {
	dt, ran time.Duration
	saved   time.Time
}

// observe notes a tick of the group's download ih.
func (q *Queue) observe(groupID int64, ih string, now time.Time, running bool) tick {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.obs == nil {
		q.obs = map[int64]*observed{}
	}
	o := q.obs[groupID]
	if o == nil || o.ih != ih {
		o = &observed{ih: ih, at: now, saved: now}
		q.obs[groupID] = o
		return tick{saved: now}
	}
	if running {
		dt := min(max(now.Sub(o.at), 0), maxTickGap)
		o.ran += dt
		o.unsaved += dt
	}
	o.at = now
	return tick{dt: o.unsaved, ran: o.ran, saved: o.saved}
}

func (q *Queue) saved(groupID int64, at time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if o := q.obs[groupID]; o != nil {
		o.saved, o.unsaved = at, 0
	}
}

func (q *Queue) forget(groupID int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.obs, groupID)
}

// observed is the queue's memory of a download's ticks in this process.
type observed struct {
	ih        string
	at, saved time.Time
	ran       time.Duration
	// unsaved: running time since the clock was last saved (each tick
	// reads the saved clock, so it adds all of it, not its own gap)
	unsaved time.Duration
}

func (q *Queue) rebuild(ctx context.Context, jobID int64) {
	if q.Rebuild == nil {
		return
	}
	if err := q.Rebuild(ctx, jobID); err != nil && ctx.Err() == nil {
		q.log().Error("import: rebuild after an album", "job", jobID, "err", err)
	}
}

// gone: the album's download is no longer in the engine. Another download
// of the album took its place (the automatic switch away from a dead
// swarm, a pick in «Выбрать релиз…»): the album waits for that one. The
// queue gave it up (headway) and its requeue did not happen: now. Else an
// admin removed it: its songs fail.
func (q *Queue) gone(ctx context.Context, g playlistimport.Group, at time.Time) error {
	if _, ok, err := q.Acq.InProgress(ctx, g.ReleaseGroup, g.AlbumItemID, g.InfoHash); err != nil || ok {
		if err != nil {
			return err
		}
		q.log().Info("import: the album's download was replaced, waiting for the new one", "job", g.JobID, "album", g.Album, "infohash", g.InfoHash)
		return q.qs().requeue(ctx, g.ID, g.InfoHash, "", maxDead, at)
	}
	if slices.Contains(g.Dead, g.InfoHash) || slices.Contains(g.Slow, g.InfoHash) {
		return q.qs().requeue(ctx, g.ID, g.InfoHash, msgDead, maxDead, at) // given up, its requeue failed
	}
	return q.failDownload(ctx, g, msgRemoved, at)
}

// failDownload ends a download that went wrong: its songs fail.
func (q *Queue) failDownload(ctx context.Context, g playlistimport.Group, msg string, at time.Time) error {
	ts, err := q.Store.GroupTracks(ctx, g.ID, playlistimport.TrackDownloading)
	if err != nil {
		return err
	}
	return q.qs().failGroup(ctx, g.ID, playlistimport.TrackIDs(ts), msg, false, at)
}

// finish links an organized download's songs to the library tracks of its
// torrent by title; a song not among them failed.
func (q *Queue) finish(ctx context.Context, g playlistimport.Group, at time.Time) error {
	lib, err := q.Lib.TracksOfTorrent(ctx, g.InfoHash)
	if err != nil {
		return err
	}
	ts, err := q.Store.GroupTracks(ctx, g.ID, playlistimport.TrackDownloading)
	if err != nil {
		return err
	}
	links := playlistimport.LinkTracks(ts, lib)
	var failed []int64
	for _, t := range ts {
		if _, ok := links[t.ID]; !ok {
			failed = append(failed, t.ID)
		}
	}
	album := g.AlbumItemID
	for _, l := range lib {
		if album == 0 && l.AlbumID != 0 {
			album = l.AlbumID
		}
	}
	return q.qs().finishGroup(ctx, g.ID, links, failed, album, at)
}

// start starts waiting albums, oldest first, while fewer than Max are
// active. Songs the library has are linked first (no search, no space);
// at most one search runs per tick, Gap after the last one; a full or offline disk starts
// nothing. A group held for space is searched again only once the disk
// has freed.
func (q *Queue) start(ctx context.Context, j playlistimport.Job, reserved int64) error {
	limit, gap := q.Max, q.Gap
	if limit <= 0 {
		limit = defaultMaxGroups
	}
	if gap <= 0 {
		gap = defaultGap
	}
	du, derr := q.Acq.Disk()
	if derr != nil && ctx.Err() == nil {
		q.log().Warn("import: disk usage", "err", derr)
	}
	canStart := derr == nil && du.Offline == "" && du.Level != acquire.LevelFull
	active, err := q.qs().activeGroups(ctx)
	if err != nil {
		return err
	}
	cands, err := q.qs().queueCandidates(ctx, j.ID, q.now().Add(-retryAfter))
	if err != nil {
		return err
	}
	if err := q.sweep(ctx, j.ID, cands); err != nil {
		return err
	}
	for _, g := range cands {
		if active >= limit || ctx.Err() != nil {
			break
		}
		if g.State == playlistimport.GroupWaitingSpace && !q.freed(g.ID, du) {
			continue
		}
		album, rest, err := q.linkHave(ctx, g)
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			continue
		}
		// downloading already (a cancelled job's, or «На сервер»): wait
		// for it, shown, then link what it brought — unless it is paused,
		// failed or stalled: then the album's own search
		if waiting, err := q.waitElsewhere(ctx, g, album); err != nil || waiting {
			if err != nil {
				return err
			}
			continue
		}
		if g.State == playlistimport.GroupWaitingDownload {
			g.State = playlistimport.GroupWaiting // the download it waited for ended or went (giveUpWait)
		}
		if !canStart || !q.paced(gap) {
			break // the later albums wait their turn (no lookups every tick)
		}
		if moved, err := q.qs().moveGroup(ctx, g.ID, g.State, playlistimport.GroupSearching, q.now()); err != nil || !moved {
			if err != nil {
				return err
			}
			continue
		}
		// one search per tick: a slow one (a magnet's file list waits up to
		// 40 s) does not hold up the downloads' follow-up for the next ones
		return q.acquire(ctx, j, g, album, rest, du, reserved)
	}
	return nil
}

// waitElsewhere: the album downloads elsewhere and the group waits for it
// (waiting_download, named). A download that is paused, failed or has
// made no progress for stallAfter is given up (the group waits for its
// own search, which leaves it out).
func (q *Queue) waitElsewhere(ctx context.Context, g playlistimport.Group, album int64) (bool, error) {
	ih, ok, err := q.Acq.InProgress(ctx, g.ReleaseGroup, album, g.SkipInfoHash)
	if err != nil || !ok {
		if err == nil && g.State == playlistimport.GroupWaitingDownload {
			// it ended or went: its own search now (from waiting)
			err = q.qs().giveUpWait(ctx, g.ID, g.SkipInfoHash, q.now())
		}
		return false, err
	}
	d, err := q.Acq.TrackDownload(ctx, ih)
	if err != nil {
		return false, err
	}
	now := q.now()
	stalled := g.State == playlistimport.GroupWaitingDownload && g.InfoHash == ih && d.Done <= g.WaitDone && !g.WaitAt.IsZero() &&
		now.Sub(g.WaitAt) >= stallAfter
	if !d.Present || d.Status == torrent.StatusPaused || d.Status == torrent.StatusError || stalled {
		q.log().Info("import: no longer waiting for a download", "group", g.ID, "infohash", ih, "status", d.Status, "stalled", stalled)
		return true, q.qs().giveUpWait(ctx, g.ID, ih, now) // searched on a later tick
	}
	return true, q.qs().waitFor(ctx, g, ih, d.Name, d.Done, now)
}

// sweep links, once per job (and restart), the songs of every album the
// library has, before the first download starts: they need no slot.
func (q *Queue) sweep(ctx context.Context, jobID int64, cands []playlistimport.Group) error {
	q.mu.Lock()
	done := q.swept[jobID]
	q.mu.Unlock()
	if done {
		return nil
	}
	for _, g := range cands {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, _, err := q.linkHave(ctx, g); err != nil {
			return err
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.swept == nil {
		q.swept = map[int64]bool{}
	}
	q.swept[jobID] = true
	return nil
}

// linkHave links the album's songs the library has (the group is have
// when that is all of them); it returns the library album (0: none) and
// the songs still to download.
func (q *Queue) linkHave(ctx context.Context, g playlistimport.Group) (int64, []playlistimport.Track, error) {
	states := []string{playlistimport.TrackMatched}
	if g.State == playlistimport.GroupFailed {
		states = append(states, playlistimport.TrackFailed) // the automatic retry
	}
	want, err := q.Store.GroupTracks(ctx, g.ID, states...)
	if err != nil {
		return 0, nil, err
	}
	album, lib, err := q.existing(ctx, g)
	if err != nil {
		return 0, nil, err
	}
	have := playlistimport.LinkTracks(want, lib)
	rest := make([]playlistimport.Track, 0, len(want))
	for _, t := range want {
		if _, ok := have[t.ID]; !ok {
			rest = append(rest, t)
		}
	}
	if len(have) > 0 || len(rest) == 0 {
		if err := q.Store.MarkHave(ctx, g.ID, have, album, len(rest) == 0, q.now()); err != nil {
			return 0, nil, err
		}
	}
	return album, rest, nil
}

// paced: a search may start now (Gap since the last); it is then counted.
func (q *Queue) paced(gap time.Duration) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := q.now()
	if !q.lastPlan.IsZero() && now.Sub(q.lastPlan) < gap {
		return false
	}
	q.lastPlan = now
	return true
}

// freed: a group held for space may be searched again — the disk has
// freed since it was held (or the hold is from before a restart).
func (q *Queue) freed(groupID int64, du acquire.DiskUsage) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	used, ok := q.heldAt[groupID]
	return !ok || du.Used < used
}

func (q *Queue) hold(groupID int64, du acquire.DiskUsage) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.heldAt == nil {
		q.heldAt = map[int64]int64{}
	}
	q.heldAt[groupID] = du.Used
}

func (q *Queue) unhold(groupID int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.heldAt, groupID)
}

// fits: the disk stays under spaceShare with the running downloads'
// remaining bytes and these (an unknown disk size: yes).
func fits(du acquire.DiskUsage, reserved, bytes int64) bool {
	if du.Total <= 0 {
		return true
	}
	return float64(du.Used+reserved+bytes) <= spaceShare*float64(du.Total)
}

// acquire searches a release for the album's songs rest (the group is
// searching) and starts it, or records why not (not found, failed, held
// for space).
func (q *Queue) acquire(ctx context.Context, j playlistimport.Job, g playlistimport.Group, album int64, rest []playlistimport.Track, du acquire.DiskUsage, reserved int64) error {
	req := acquire.TrackAcquireRequest{ExtID: g.ReleaseGroup, Title: g.Album, Artist: g.Artist, Year: g.Year, Aliases: g.Aliases,
		AlbumID: album, UserID: j.UserID, IgnoreDownload: g.SkipInfoHash, AnyFormat: g.AnyFormat, Exclude: g.Dead, Avoid: g.Slow}
	byKey := map[string]int64{}
	for _, t := range rest {
		key := keyOf(t.ID)
		byKey[key] = t.ID
		req.Tracks = append(req.Tracks, acquire.WantedTrack{Key: key, Title: titleOf(t), DurationMs: t.DurationMs})
	}
	plan, err := q.Acq.PlanTracks(ctx, req)
	if err == nil && !fits(du, reserved, plan.Bytes) {
		q.hold(g.ID, du)
		_, err := q.qs().moveGroup(ctx, g.ID, playlistimport.GroupSearching, playlistimport.GroupWaitingSpace, q.now())
		return err
	}
	if err == nil {
		if !q.qs().stillDownloading(ctx, j.ID) {
			return nil // cancelled meanwhile (the group is gone)
		}
		var res acquire.TrackAcquireResult
		if res, err = q.Acq.AcquireTracks(ctx, req, plan); err == nil {
			q.unhold(g.ID)
			var downloading, missing []int64
			for k := range res.Files {
				downloading = append(downloading, byKey[k])
			}
			for _, k := range res.Missing {
				missing = append(missing, byKey[k])
			}
			// recorded even when a stop comes now: the torrent is added
			_, err := q.qs().startGroup(context.WithoutCancel(ctx), g.ID, res.InfoHash, res.Bytes, downloading, missing, q.now())
			return err
		}
	}
	if ctx.Err() != nil {
		return ctx.Err() // a stop: the search starts again after the restart
	}
	if errors.Is(err, acquire.ErrInProgress) {
		// the album began downloading meanwhile: wait for it
		_, err := q.qs().moveGroup(ctx, g.ID, playlistimport.GroupSearching, playlistimport.GroupWaiting, q.now())
		return err
	}
	ids := playlistimport.TrackIDs(rest)
	msg, ok := acquire.UserMessage(err)
	if errors.Is(err, acquire.ErrNoFormat) {
		if !ok {
			msg = msgNoFormat
		}
		return q.qs().noFormat(ctx, g.ID, ids, msg, q.now())
	}
	if errors.Is(err, acquire.ErrNoRelease) && !errors.Is(err, acquire.ErrSourcesFailed) {
		switch {
		case len(g.Dead) > 0:
			msg = msgDeadOnly // the releases left were given up
		case !ok:
			msg = msgNoRelease
		}
		return q.Store.NotFound(ctx, g.ID, ids, msg, q.now())
	}
	if !ok {
		msg = msgError
	}
	q.log().Warn("import: album acquisition failed", "job", j.ID, "album", g.Album, "err", err)
	return q.qs().failGroup(ctx, g.ID, ids, msg, true, q.now())
}

// existing finds the album in the library (playlistimport.FindExisting,
// also by the download the group waited for).
func (q *Queue) existing(ctx context.Context, g playlistimport.Group) (int64, []library.Track, error) {
	return playlistimport.FindExisting(ctx, q.Lib, g, q.Lib.TracksOfTorrent)
}

// titleOf is the title a release names a song by: MusicBrainz's, else the
// service's.
func titleOf(t playlistimport.Track) string {
	if t.MBTitle != "" {
		return t.MBTitle
	}
	return t.Title
}

// keyOf is a song's key in a TrackAcquireRequest (its track id).
func keyOf(id int64) string { return strconv.FormatInt(id, 10) }
