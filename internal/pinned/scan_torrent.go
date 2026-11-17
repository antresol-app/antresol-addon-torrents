package pinned

import (
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

// Torrent scanner defaults.
const (
	defaultTop     = 5
	torrentCadence = 24 * time.Hour
	// defaultSearchPause spaces the scanner's searches, as follow spaces
	// its own (each one asks every tracker)
	defaultSearchPause = 10 * time.Second
)

// searcher is what the torrent scanner asks of the sources
// (*indexer.Manager).
type searcher interface {
	Search(ctx context.Context, q indexer.SearchRequest) (indexer.SearchResponse, error)
}

// torrentScanner is the tracking scanner of kind "torrent".
type torrentScanner struct {
	search searcher
	pins   *Service
	top    int
	// pause: the least time between two searches, across titles (0: none)
	pause time.Duration

	mu   sync.Mutex
	last map[int64]time.Time // when each title was searched (this process)
	// searched: when the last search was sent (wall clock); searchMu
	// keeps the searches one at a time
	searchMu sync.Mutex
	searched time.Time
}

// NewTorrentScanner is the tracking scanner of the torrent search: once a
// day per title it searches the title (a series: its latest aired season)
// and keeps the top (0 = 5) releases by score (its searches 10 s apart); one Source per episode a
// release's name covers (a season pack: the season's aired episodes), Ref
// its link (or magnet), Provider its source. For a magnet-only pin it
// stores a Suggestion of a release of the same family (parsed title,
// resolution and group) holding more episodes; it never replaces the pin.
func NewTorrentScanner(m *indexer.Manager, p *Service, top int) Scanner {
	if top <= 0 {
		top = defaultTop
	}
	sc := &torrentScanner{pins: p, top: top, pause: defaultSearchPause, last: map[int64]time.Time{}}
	if m != nil {
		sc.search = m
	}
	return sc
}

func (*torrentScanner) Kind() string { return "torrent" }

// Scan returns ErrUnchanged for a title searched within the day
// (by this process, or by the last_seen of its torrent rows).
func (c *torrentScanner) Scan(ctx context.Context, t tracking.Title, eps []tracking.Episode) ([]tracking.Source, error) {
	if c.search == nil || strings.TrimSpace(t.Name) == "" {
		return nil, nil
	}
	now := c.pins.now()
	all, err := c.pins.Tracking.Sources(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	prev := ofKind(all, "torrent")
	if c.recent(t.ID, prev, now) {
		return nil, ErrUnchanged
	}
	q := indexer.SearchRequest{Text: t.Name, Kind: "movie"}
	today := now.Format(time.DateOnly)
	if t.Kind != "movie" {
		q.Kind, q.Season = "series", latestSeason(eps, today)
	}
	res, err := c.paced(ctx, q)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.last[t.ID] = now
	c.mu.Unlock()

	// a release with no link is no source: left out before the top
	ranked := slices.DeleteFunc(slices.Clone(res.Releases), func(r indexer.ScoredRelease) bool { return releaseRef(r) == "" })
	slices.SortStableFunc(ranked, func(a, b indexer.ScoredRelease) int { return b.Score - a.Score })
	var out []tracking.Source
	seen := map[[2]int]map[string]bool{}
	for _, r := range ranked[:min(c.top, len(ranked))] {
		ref := releaseRef(r)
		info := parseName(r.Title)
		labels := slices.Concat(info.Audio, info.Studios, info.Subs)
		meta := tracking.SourceMeta{SourceID: strconv.FormatInt(r.SourceID, 10), Lang: release.AudioLang(r.Title, info),
			Studios: info.Studios, Subs: info.Subs}
		if len(info.Studios) > 0 {
			meta.Voice = info.Studios[0]
		}
		if len(info.Audio) > 0 {
			meta.Audio = info.Audio[0]
		}
		for _, k := range covered(t, info, eps, q.Season, today) {
			if seen[k] == nil {
				seen[k] = map[string]bool{}
			}
			if seen[k][ref] {
				continue
			}
			seen[k][ref] = true
			out = append(out, tracking.Source{TitleID: t.ID, Season: k[0], Episode: k[1], Kind: "torrent",
				Ref: ref, Provider: r.SourceName, Quality: qualityOf(r.Title), Labels: labels, Meta: meta})
		}
	}
	if t.Kind != "movie" {
		// only one season is searched: the others keep their rows
		given := map[int]bool{q.Season: true}
		for _, x := range out {
			given[x.Season] = true
		}
		out = keepPrev(out, prev, func(x tracking.Source) bool { return !given[x.Season] })
		if err := c.suggest(ctx, t, eps, res.Releases, q.Season, today); err != nil && ctx.Err() == nil {
			c.pins.log().Warn("pinned: suggestion", "title", t.ID, "err", err)
		}
	}
	return out, nil
}

// paced sends q once pause has passed since the last search (ctx ending
// first: its error, nothing sent).
func (c *torrentScanner) paced(ctx context.Context, q indexer.SearchRequest) (indexer.SearchResponse, error) {
	c.searchMu.Lock()
	defer c.searchMu.Unlock()
	if wait := c.pause - time.Since(c.searched); !c.searched.IsZero() && wait > 0 {
		tm := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			tm.Stop()
			return indexer.SearchResponse{}, ctx.Err()
		case <-tm.C:
		}
	}
	defer func() { c.searched = time.Now() }()
	return c.search.Search(ctx, q)
}

// recent: the title was searched less than a day before now (by this
// process, or by the last_seen of its torrent rows prev).
func (c *torrentScanner) recent(titleID int64, prev []tracking.Source, now time.Time) bool {
	c.mu.Lock()
	at, ok := c.last[titleID]
	c.mu.Unlock()
	if ok && now.Sub(at) < torrentCadence {
		return true
	}
	for _, s := range prev {
		if now.Sub(s.LastSeen) < torrentCadence {
			return true
		}
	}
	return false
}

func releaseRef(r indexer.ScoredRelease) string {
	if r.Link != "" {
		return r.Link
	}
	return r.Magnet
}

// aired: an episode out by today (an unknown air date is not).
func aired(e tracking.Episode, today string) bool { return e.AirDate != "" && e.AirDate <= today }

// latestSeason is the latest season with an aired episode (else the latest
// listed; 0 for none).
func latestSeason(eps []tracking.Episode, today string) int {
	latest, listed := 0, 0
	for _, e := range eps {
		listed = max(listed, e.Season)
		if aired(e, today) {
			latest = max(latest, e.Season)
		}
	}
	if latest == 0 {
		return listed
	}
	return latest
}

// covered are the episodes a release's name covers: a film (0, 0); a
// series its SxxEyy range (those the title's list has, when it has any: an
// anime's absolute numbers past the season are left out), or for a season
// pack the season's aired episodes (a name with no season is of season);
// none when it names neither.
func covered(t tracking.Title, info release.Info, eps []tracking.Episode, season int, today string) [][2]int {
	if t.Kind == "movie" {
		return [][2]int{{0, 0}}
	}
	if info.Season > 0 {
		season = info.Season
	}
	if season <= 0 {
		season = 1
	}
	var out [][2]int
	switch {
	case info.Episode > 0:
		listed := listedEpisodes(eps)
		last := max(info.EpisodeEnd, info.Episode)
		for e := info.Episode; e <= last && e-info.Episode < 500; e++ {
			if k := [2]int{season, e}; listed == nil || listed[k] {
				out = append(out, k)
			}
		}
	case info.Season > 0 || info.IsSeasonPack:
		for _, e := range eps {
			if e.Season == season && e.Number > 0 && aired(e, today) {
				out = append(out, [2]int{season, e.Number})
			}
		}
	}
	return out
}

// reAnimeName: an anime-style release name, «[Group] Show - 01-08
// [1080p]», «[Group] Show (01-08) (1080p)», «[Group] Show - 08 (1080p)».
var reAnimeName = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*(.+?)\s*(?:-\s*(\d{1,4})(?:v\d)?(?:\s*[-~]\s*(\d{1,4}))?|\((\d{1,4})\s*[-~]\s*(\d{1,4})\))(?:\s|\[|\(|$)`)

// reLeadGroup: a leading «[Group]».
var reLeadGroup = regexp.MustCompile(`^\s*\[([^\]]+)\]`)

// parseName is release.Parse with the anime style added: a leading
// «[Group]» is the group when the name has no «-GRP», and a « - 01-08» or
// «(01-08)» after the title is its episodes (the title without them).
func parseName(name string) release.Info {
	info := release.Parse(name)
	if info.Group == "" {
		if m := reLeadGroup.FindStringSubmatch(name); m != nil {
			info.Group = strings.TrimSpace(m[1])
		}
	}
	if info.Episode > 0 || info.Season > 0 {
		return info
	}
	m := reAnimeName.FindStringSubmatch(name)
	if m == nil {
		return info
	}
	first, last := m[3], m[4]
	if first == "" {
		first, last = m[5], m[6]
	}
	info.Title = strings.TrimSpace(m[2])
	info.Episode, _ = strconv.Atoi(first)
	info.EpisodeEnd, _ = strconv.Atoi(last)
	return info
}

// listedEpisodes is the set of the title's episodes (nil: none listed).
func listedEpisodes(eps []tracking.Episode) map[[2]int]bool {
	if len(eps) == 0 {
		return nil
	}
	out := make(map[[2]int]bool, len(eps))
	for _, e := range eps {
		out[[2]int{e.Season, e.Number}] = true
	}
	return out
}

// magnetOnly: a pin no source refreshes (a magnet, an own .torrent file),
// e.g. a bitmagnet find.
func magnetOnly(p Pin) bool { return p.SourceID == "" || isMagnet(p.Link) }

func sameFamily(a, b release.Info) bool {
	return a.Title != "" && a.Group != "" && strings.EqualFold(a.Title, b.Title) &&
		a.Resolution == b.Resolution && strings.EqualFold(a.Group, b.Group)
}

// holdsAll: every episode of want is in have.
func holdsAll(have, want [][2]int) bool {
	set := make(map[[2]int]bool, len(have))
	for _, k := range have {
		set[k] = true
	}
	for _, k := range want {
		if !set[k] {
			return false
		}
	}
	return true
}

// suggest offers, for a magnet-only pin, the release of its family holding
// every episode the pin holds and the most more (the pin itself
// is never changed here). An offer of the same link and count is kept.
func (c *torrentScanner) suggest(ctx context.Context, t tracking.Title, eps []tracking.Episode, rs []indexer.ScoredRelease, season int, today string) error {
	p, ok, err := c.pins.ByTitle(ctx, t.ID)
	if err != nil || !ok || !magnetOnly(p) {
		return err
	}
	pi := parseName(p.ReleaseTitle)
	// the pin's episodes (those the title lists, when it lists any)
	listed := listedEpisodes(eps)
	var pe [][2]int
	for k := range EpisodesOf(p.Files, DefaultSeason(eps)) {
		if listed == nil || listed[k] {
			pe = append(pe, k)
		}
	}
	var best *indexer.ScoredRelease
	bestN := len(pe)
	for i, r := range rs {
		ri := parseName(r.Title)
		ref := releaseRef(r)
		if ref == "" || (r.InfoHash != "" && strings.EqualFold(r.InfoHash, p.InfoHash)) || !sameFamily(pi, ri) {
			continue
		}
		// an update holds every episode of the pin and more (another
		// season of the same family is no update)
		rc := covered(t, ri, eps, season, today)
		if !holdsAll(rc, pe) {
			continue
		}
		n := len(rc)
		if n > bestN || (n == bestN && best != nil && r.Score > best.Score) {
			best, bestN = &rs[i], n
		}
	}
	if best == nil {
		return nil
	}
	ref := releaseRef(*best)
	if sg := p.Suggestion; sg != nil && sg.Link == ref && sg.Episodes == bestN {
		return nil
	}
	srcID := ""
	if !isMagnet(ref) {
		srcID = strconv.FormatInt(best.SourceID, 10)
	}
	return c.pins.SetSuggestion(ctx, t.ID, &Suggestion{Link: ref, SourceID: srcID, ReleaseTitle: best.Title,
		InfoHash: strings.ToLower(best.InfoHash), Episodes: bestN, Found: c.pins.now().UTC().Truncate(time.Second)})
}
