package acquire

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// The automatic switch away from a release nobody seeds: indexers (JacRed
// above all, which caches) show stale seeder counts, and a download of a
// dead swarm would wait forever. A download-mode torrent that has had no
// peer and no byte for autoReplaceAfter (and never had one: a NATed seeder
// may connect in late, and a torrent that ever had a peer is never
// touched) is replaced by the next best release of the same title — the
// same search and scoring, releases tried for the title left out, the
// top few probed one by one and the first live one taken — at most
// maxAutoSwitches times
// per title. With no live alternative the dead one stays, with a note.

// autoReplaceAfter: how long a download goes without a peer or a byte
// before it is switched (tests shorten it).
var autoReplaceAfter = 10 * time.Minute

// autoCheckEvery: how often Run looks for downloads to switch.
var autoCheckEvery = 30 * time.Second

const (
	// maxAutoSwitches: automatic switches per title.
	maxAutoSwitches = 3
	// probeCandidates: releases looked at (fetched, probed) per switch,
	// one at a time, stopping at the first live one.
	probeCandidates = 3
	// switchTimeout bounds one switch (search, metafiles, probes, start).
	switchTimeout = 3 * time.Minute
)

// AutoReplace notes (acquisitions.auto_note, TorrentView.AutoReplace).
const (
	AutoSwitched  = "switched"
	AutoNoneAlive = "none_alive"
	// autoQuiet: a none_alive note was dismissed, or the download got a
	// peer while being switched; the switch does not try it again.
	autoQuiet = "quiet"
	// autoReplaced: a switch started another release in its place; set
	// before it is removed, so a failed removal is never switched again.
	autoReplaced = "replaced"
)

// switchBackoff: the wait after a failed switch, doubled for each further
// failure (at most switchBackoffMax).
var (
	switchBackoff    = 30 * time.Minute
	switchBackoffMax = 24 * time.Hour
)

// swarmProber is the part of *torrent.Engine liveness needs (an Engine
// without it — a test fake — gets no probe).
type swarmProber interface {
	CheckLiveness(infohash string)
	NoteProbe(infohash string, res torrent.SwarmProbe)
	ProbeSwarm(ctx context.Context, tg torrent.ProbeTarget) torrent.SwarmProbe
}

// idleRemover is the engine's removal of a download that never had a peer
// or a byte (checked under its lock), its files kept.
type idleRemover interface {
	RemoveIfIdle(ctx context.Context, infohash string) error
}

// peerNotifier tells of a torrent's first peer as it connects.
type peerNotifier interface {
	OnPeer(f func(infohash string))
}

// watchPeers records had_peers the moment a download's first peer
// connects (not on the next check): such a download is never switched.
func (s *Service) watchPeers() {
	if n, ok := s.o.Engine.(peerNotifier); ok {
		n.OnPeer(s.peerSeen)
	}
}

func (s *Service) peerSeen(ih string) {
	if _, err := s.o.DB.Exec(`UPDATE acquisitions SET had_peers=1 WHERE infohash=? AND had_peers=0`, strings.ToLower(ih)); err != nil {
		s.log.Warn("save had peers", "infohash", ih, "err", err)
	}
}

// switchFailures returns the failed switches of a title and when the next
// may run.
func (s *Service) switchFailures(ctx context.Context, key string) (int, time.Time) {
	var n int
	var next string
	err := s.o.DB.QueryRowContext(ctx, `SELECT failures, next_at FROM release_switch_failures WHERE title_key=?`, key).Scan(&n, &next)
	if err != nil {
		return 0, time.Time{}
	}
	at, _ := time.Parse(time.RFC3339, next)
	return n, at
}

// switchFailed records a failed switch of a title: the next waits
// switchBackoff × 2^(failures−1); n is the failures now.
func (s *Service) switchFailed(ctx context.Context, key string, now time.Time) int {
	n, _ := s.switchFailures(ctx, key)
	n++
	wait := min(switchBackoff<<min(n-1, 10), switchBackoffMax)
	_, err := s.o.DB.ExecContext(ctx, `INSERT INTO release_switch_failures (title_key, failures, next_at) VALUES (?,?,?)
		ON CONFLICT(title_key) DO UPDATE SET failures=excluded.failures, next_at=excluded.next_at`, key, n, now.Add(wait).UTC().Format(time.RFC3339))
	if err != nil {
		s.log.Warn("save switch failure", "err", err)
	}
	return n
}

// autoState is the switch's memory of this run (Service.auto).
type autoState struct {
	mu sync.Mutex
	// quiet: since when a download has been seen running with no peer and
	// no byte (this run; a restart starts the window afresh).
	quiet map[string]time.Time
	// switching: titles (attemptKey) a switch runs for.
	switching map[string]bool
}

// attemptKey names the title of an acquisition for release_attempts: the
// downloadKey plus the season and episode of a series.
func attemptKey(a acquisition) string {
	k := downloadKey(a)
	if a.Season != 0 || a.Episode != 0 {
		k += ":s" + strconv.Itoa(a.Season) + ":e" + strconv.Itoa(a.Episode)
	}
	return k
}

// recordAttempt remembers that ih was tried for a title.
func (s *Service) recordAttempt(ctx context.Context, key, ih string, auto bool) {
	_, err := s.o.DB.ExecContext(ctx, `INSERT INTO release_attempts (title_key, infohash, auto) VALUES (?,?,?)
		ON CONFLICT(title_key, infohash) DO UPDATE SET auto = MAX(auto, excluded.auto)`, key, strings.ToLower(ih), auto)
	if err != nil {
		s.log.Warn("record release attempt", "err", err)
	}
}

// releaseAttempts returns the infohashes tried for a title and how many of them
// the switch started.
func (s *Service) releaseAttempts(ctx context.Context, key string) (map[string]bool, int, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT infohash, auto FROM release_attempts WHERE title_key=?`, key)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out, n := map[string]bool{}, 0
	for rows.Next() {
		var ih string
		var auto bool
		if err := rows.Scan(&ih, &auto); err != nil {
			return nil, 0, err
		}
		out[ih] = true
		if auto {
			n++
		}
	}
	return out, n, rows.Err()
}

func (s *Service) setAutoNote(ctx context.Context, ih, note string) {
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET auto_note=? WHERE infohash=?`, note, strings.ToLower(ih)); err != nil {
		s.log.Warn("save switch note", "infohash", ih, "err", err)
	}
}

// DismissAutoNote hides the switch's note of a download (admin).
func (s *Service) DismissAutoNote(ctx context.Context, ih string) error {
	a, err := s.acquisition(ctx, strings.ToLower(ih))
	if err != nil {
		return uerr(ErrNotFound, "Раздача не найдена")
	}
	switch a.AutoNote {
	case AutoSwitched:
		s.setAutoNote(ctx, a.InfoHash, "")
	case AutoNoneAlive:
		s.setAutoNote(ctx, a.InfoHash, autoQuiet)
	}
	return nil
}

// autoReplaceNote is TorrentView.AutoReplace: the switched note until the
// download completes, the none-alive one while it still has no peer.
func autoReplaceNote(st torrent.Status, a acquisition) string {
	running := st.Status == torrent.StatusQueued || st.Status == torrent.StatusDownloading || st.Status == torrent.StatusWaitingSpace || st.Status == torrent.StatusPaused
	switch {
	case a.AutoNote == AutoSwitched && running && !a.OrganizedAt.Valid:
		return AutoSwitched
	case a.AutoNote == AutoNoneAlive && running && !headway(st) && !a.HadPeers:
		return AutoNoneAlive
	}
	return ""
}

// headway: the torrent has (or had, this run) a peer, or has a byte.
func headway(st torrent.Status) bool {
	return st.HadPeers || st.Peers > 0 || st.Done > 0 || st.Downloaded > 0
}

// checkAutoReplace (Run, every autoCheckEvery) starts a switch for each
// download quiet for autoReplaceAfter. It never blocks on a switch.
func (s *Service) checkAutoReplace(ctx context.Context, now time.Time) {
	acqs, err := s.acquisitions(ctx)
	if err != nil {
		s.log.Warn("load acquisitions", "err", err)
		return
	}
	s.auto.mu.Lock()
	defer s.auto.mu.Unlock()
	if s.auto.quiet == nil {
		s.auto.quiet, s.auto.switching = map[string]time.Time{}, map[string]bool{}
	}
	listed := map[string]bool{}
	for _, st := range s.o.Engine.List() {
		a, ok := acqs[st.InfoHash]
		// a voice-over's audio files are no title to switch («Найти озвучку…»
		// lists the other releases)
		if !ok || st.Mode != torrent.ModeDownload || a.OrganizedAt.Valid || a.Kind == KindVoiceOver {
			continue
		}
		ih := st.InfoHash
		if headway(st) {
			delete(s.auto.quiet, ih)
			if !a.HadPeers {
				if _, err := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET had_peers=1 WHERE infohash=?`, ih); err != nil {
					s.log.Warn("save had peers", "infohash", ih, "err", err)
				}
			}
			continue
		}
		// Only a download that should be running and has never had a
		// peer; a hold, an error or a note that the switch gave up ends
		// (or never starts) the window.
		if a.HadPeers || a.AutoNote == AutoNoneAlive || a.AutoNote == autoQuiet || a.AutoNote == autoReplaced ||
			(st.Status != torrent.StatusQueued && st.Status != torrent.StatusDownloading) {
			delete(s.auto.quiet, ih)
			continue
		}
		listed[ih] = true
		since, ok := s.auto.quiet[ih]
		if !ok {
			s.auto.quiet[ih] = now
			continue
		}
		if now.Sub(since) < autoReplaceAfter || st.Stalled == "" {
			continue
		}
		key := attemptKey(a)
		if s.auto.switching[key] {
			continue
		}
		s.auto.switching[key] = true
		s.auto.quiet[ih] = now // a failed switch is retried after another window
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.auto.mu.Lock()
				delete(s.auto.switching, key)
				s.auto.mu.Unlock()
			}()
			sctx, cancel := context.WithTimeout(s.root, switchTimeout)
			defer cancel()
			s.autoReplace(sctx, a, key, now)
		}()
	}
	for ih := range s.auto.quiet {
		if !listed[ih] {
			delete(s.auto.quiet, ih)
		}
	}
}

// candidate is a release the switch may start.
type candidate struct {
	rel    indexer.ScoredRelease
	magnet string                    // or
	dl     *cardigann.DownloadResult // the fetched metafile (or magnet)
	target torrent.ProbeTarget
	probe  torrent.SwarmProbe
}

// autoReplace switches the dead download a to the best live release of
// its title, or leaves it with the none-alive note. A switch that fails
// (search error, the start refused, out of time) is counted per title,
// toward the title's switches, and puts the next try off (switchFailed).
func (s *Service) autoReplace(ctx context.Context, a acquisition, key string, now time.Time) {
	log := s.log.With("infohash", a.InfoHash, "title", a.Title)
	// writes outlive ctx's deadline: a timed-out switch is still recorded
	db := context.WithoutCancel(ctx)
	failures, next := s.switchFailures(ctx, key)
	if now.Before(next) {
		return // backing off after a failed switch
	}
	tried, switched, err := s.releaseAttempts(ctx, key)
	if err != nil {
		log.Warn("auto switch: load attempts", "err", err)
		return
	}
	tried[strings.ToLower(a.InfoHash)] = true
	if switched+failures >= maxAutoSwitches {
		log.Info("auto switch: no switches left for the title")
		s.setAutoNote(db, a.InfoHash, AutoNoneAlive)
		return
	}
	failed := func(what string, err error) {
		if s.root.Err() != nil {
			return // shutting down: not the release's fault
		}
		n := s.switchFailed(db, key, now)
		log.Warn("auto switch: "+what, "err", err, "failures", n)
		if switched+n >= maxAutoSwitches {
			s.setAutoNote(db, a.InfoHash, AutoNoneAlive)
		}
	}
	req := autoRequest(a)
	sr, err := s.searchFor(ctx, req)
	if err != nil {
		failed("search", err)
		return
	}
	resp, err := s.search(ctx, sr)
	if err != nil {
		failed("search", err)
		return
	}
	if req.Kind == KindMusic {
		resp.Releases, _ = formatReleases(resp, false)
	}
	c, probed, ok := s.liveCandidate(ctx, resp.Releases, tried)
	if !ok {
		if ctx.Err() != nil {
			failed("out of time", ctx.Err())
			return
		}
		log.Info("auto switch: no live release", "probed", probed)
		s.setAutoNote(db, a.InfoHash, AutoNoneAlive)
		return
	}
	req.SourceID, req.ReleaseTitle = c.rel.SourceID, c.rel.Title
	if c.magnet != "" {
		req.ReleaseLink = c.magnet
	} else {
		req.ReleaseLink, req.preloaded = c.rel.Link, c.dl
	}
	req.replaces, req.probed = a.InfoHash, &c.probe
	res, err := s.Acquire(ctx, req)
	if err != nil {
		failed("start", err)
		return
	}
	if strings.EqualFold(res.InfoHash, a.InfoHash) {
		failed("start", errors.New("the same release"))
		return
	}
	s.setAutoNote(db, res.InfoHash, AutoSwitched)
	// Noted first: a dead download whose removal fails is not switched
	// again. Removed only if still idle (checked by the engine, under its
	// lock: a seeder may have connected during the switch), its files kept
	// (it has no byte; a magnet's display name may be another folder's).
	s.setAutoNote(db, a.InfoHash, autoReplaced)
	if err := s.removeIdle(db, a.InfoHash); err != nil {
		if errors.Is(err, torrent.ErrNotIdle) {
			log.Info("auto switch: the old download came alive; both are kept", "new", res.InfoHash)
			s.setAutoNote(db, a.InfoHash, autoQuiet)
			return
		}
		log.Warn("auto switch: remove the dead download", "err", err)
		return
	}
	log.Info("auto switch: started another release", "new", res.InfoHash)
}

// removeIdle removes a download only if the engine finds it still idle.
func (s *Service) removeIdle(ctx context.Context, ih string) error {
	r, ok := s.o.Engine.(idleRemover)
	if !ok {
		return errors.New("acquire: the engine cannot remove an idle download")
	}
	return s.removeWith(ctx, ih, func() error { return r.RemoveIfIdle(ctx, ih) })
}

// autoRequest is the download request of an acquisition, as its profile
// made it, with no release chosen.
func autoRequest(a acquisition) AcquireRequest {
	req := AcquireRequest{Kind: a.Kind, TMDBID: a.TMDBID, ExtID: a.ExtID, Title: a.Title, Season: a.Season, Episode: a.Episode,
		Mode: ModeDownload, UserID: a.AddedBy, UserUID: a.AddedByUID, Aliases: a.Meta.Aliases, albumDir: a.Meta.AlbumDir}
	if isMediaKind(a.Kind) {
		req.Artist = a.Meta.Artist
		if req.Artist == "" {
			req.Artist = a.Meta.Author
		}
		req.Year = a.Meta.Year
		if req.Year == 0 {
			req.Year = a.Year
		}
	}
	return req
}

// searchFor builds the search of a request, as Acquire does.
func (s *Service) searchFor(ctx context.Context, req AcquireRequest) (indexer.SearchRequest, error) {
	if isMediaKind(req.Kind) {
		title, artist, err := mediaTitle(req.Kind, req.Title, req.Artist)
		if err != nil {
			return indexer.SearchRequest{}, err
		}
		sr := withAliases(mediaSearchRequest(req.Kind, title, artist), req.Aliases)
		sr.Year = albumYear(req.Kind, title, req.Year)
		return sr, nil
	}
	d, err := s.title(ctx, req.Kind, req.TMDBID)
	if err != nil {
		return indexer.SearchRequest{}, err
	}
	return s.titleSearch(ctx, req.Kind, req.TMDBID, d, req.Season, req.Episode), nil
}

// liveCandidate goes through the best releases not tried yet, in score
// order, one at a time: each is resolved (a metafile is fetched from its
// source only when it is its turn) and probed, and the first live one is
// returned. At most probeCandidates releases are looked at, so a switch
// costs the indexers and trackers a few requests, not a burst. probed: how
// many were probed.
func (s *Service) liveCandidate(ctx context.Context, rels []indexer.ScoredRelease, tried map[string]bool) (c candidate, probed int, ok bool) {
	p, canProbe := s.o.Engine.(swarmProber)
	if !canProbe {
		return candidate{}, 0, false
	}
	looked := 0
	for _, r := range rels {
		if looked >= probeCandidates || ctx.Err() != nil {
			break
		}
		if r.Direct || (r.InfoHash != "" && tried[strings.ToLower(r.InfoHash)]) {
			continue
		}
		c, fetched, ok := s.resolve(ctx, r)
		if fetched {
			looked++ // a request to the source, whatever came of it
		}
		if !ok || tried[c.target.InfoHash] {
			continue
		}
		if _, ok := s.o.Engine.Get(c.target.InfoHash); ok {
			continue // the engine has it already (another download of it)
		}
		tried[c.target.InfoHash] = true // a release listed by two sources
		if !fetched {
			looked++
		}
		probed++
		if c.probe = p.ProbeSwarm(ctx, c.target); c.probe.Alive() {
			return c, probed, true
		}
	}
	return candidate{}, probed, false
}

// resolve reads a release's probe target: from its magnet, else from the
// metafile (or magnet) its source gives for the link (fetched: the source
// was asked).
func (s *Service) resolve(ctx context.Context, r indexer.ScoredRelease) (c candidate, fetched, ok bool) {
	c = candidate{rel: r}
	m := r.Magnet
	if m == "" && strings.HasPrefix(strings.ToLower(r.Link), "magnet:") {
		m = r.Link
	}
	var err error
	switch {
	case m != "":
		c.magnet = m
		c.target, err = torrent.TargetOf(m, nil)
	case s.o.Indexer == nil || r.Link == "":
		return c, false, false
	default:
		fetched = true
		dl, derr := s.o.Indexer.Download(ctx, r.SourceID, r.Link)
		if derr != nil {
			return c, true, false
		}
		c.dl = &dl
		if dl.Magnet != "" {
			c.target, err = torrent.TargetOf(dl.Magnet, nil)
		} else {
			c.target, err = torrent.TargetOf("", dl.Torrent)
		}
	}
	return c, fetched, err == nil
}

// The real engine has all of it.
var (
	_ swarmProber  = (*torrent.Engine)(nil)
	_ idleRemover  = (*torrent.Engine)(nil)
	_ peerNotifier = (*torrent.Engine)(nil)
)
