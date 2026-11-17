package acquire

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

// «Найти озвучку…»:
// on a film or series page, the audio-track releases of the title (one
// search of the sources per request of the user), and a download of only
// the audio files the library's episodes need, placed beside their videos
// as external tracks (stage A) and fitted to them (internal/vofit).

// KindVoiceOver is the acquisition of a voice-over release's audio files
// for a title of the library (acquisitionMeta.VoiceOver).
const KindVoiceOver = "voiceover"

const (
	msgVoiceOverItem    = "Озвучку можно найти только для фильма или сериала медиатеки"
	msgVoiceOverStale   = "Список озвучек устарел — найдите их ещё раз"
	msgVoiceOverLists   = "Списки файлов можно открыть не больше чем у %d раздач за поиск"
	msgVoiceOverNoFiles = "Не удалось сопоставить серии"
	msgVoiceOverNoAudio = "В раздаче нет отдельных звуковых дорожек"
	msgVoiceOverNoMatch = "Дорожек для серий медиатеки в раздаче нет"
)

const (
	// voSearchTTL: how long a search's candidates can be downloaded or
	// expanded (the sheet stays open that long at most).
	voSearchTTL = 30 * time.Minute
	// voMaxLists: file lists fetched per search (the candidates the user
	// expands or picks), never more.
	voMaxLists = 5
	// voMaxVideo: video releases offered for a look at their files.
	voMaxVideo    = 5
	voMaxSearches = 16
)

// VoiceOverCoverage is what a voice-over release covers of the title on
// the server: Have of the Total episodes there (a film: 1 of 1, Film).
// Known: the release says which seasons (from its title, or Exact: from
// its file list).
type VoiceOverCoverage struct {
	Have  int  `json:"have"`
	Total int  `json:"total"`
	Known bool `json:"known"`
	Exact bool `json:"exact"`
	Film  bool `json:"film,omitempty"`
}

// VoiceOverCandidate is a release offered by «Найти озвучку…». ID names it
// in the search (POST …/voiceovers takes it back; the link never leaves
// the server). Video: a video release whose file list may hold separate
// tracks (its coverage is known once the list is fetched).
type VoiceOverCandidate struct {
	ID         string            `json:"id"`
	SourceID   int64             `json:"sourceId"`
	SourceName string            `json:"sourceName"`
	Title      string            `json:"title"`
	Studio     string            `json:"studio"`
	Studios    []string          `json:"studios"`
	Seeders    int               `json:"seeders"`
	Size       int64             `json:"size"`
	InfoHash   string            `json:"infoHash,omitempty"`
	Coverage   VoiceOverCoverage `json:"coverage"`
	Video      bool              `json:"video,omitempty"`
}

// VoiceOvers is the answer of FindVoiceOvers: the voice-over releases, the
// video releases naming a studio, and the sources that did not answer.
type VoiceOvers struct {
	Candidates    []VoiceOverCandidate  `json:"candidates"`
	VideoReleases []VoiceOverCandidate  `json:"videoReleases"`
	Failed        []indexer.SourceError `json:"failed"`
}

// voSearch is a search's releases kept for the download and the file
// lists (by candidate id).
type voSearch struct {
	at    time.Time
	rels  map[string]indexer.ScoredRelease
	lists map[string]bool // candidates whose file list was fetched
}

type voSearches struct {
	mu sync.Mutex
	m  map[int64]*voSearch // by item
}

func (c *voSearches) put(item int64, s *voSearch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[int64]*voSearch{}
	}
	for k, v := range c.m {
		if time.Since(v.at) > voSearchTTL {
			delete(c.m, k)
		}
	}
	for len(c.m) >= voMaxSearches {
		var oldest int64 = -1
		for k, v := range c.m {
			if oldest < 0 || v.at.Before(c.m[oldest].at) {
				oldest = k
			}
		}
		delete(c.m, oldest)
	}
	c.m[item] = s
}

// release returns the release id of the item's last search; list counts
// a file-list fetch (false: over voMaxLists for the search).
func (c *voSearches) release(item int64, id string, list bool) (indexer.ScoredRelease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[item]
	if !ok || time.Since(s.at) > voSearchTTL {
		return indexer.ScoredRelease{}, uerr(ErrInvalid, msgVoiceOverStale)
	}
	r, ok := s.rels[id]
	if !ok {
		return indexer.ScoredRelease{}, uerr(ErrInvalid, msgVoiceOverStale)
	}
	if list && !s.lists[id] {
		if len(s.lists) >= voMaxLists {
			return indexer.ScoredRelease{}, uerr(ErrInvalid, fmt.Sprintf(msgVoiceOverLists, voMaxLists))
		}
		s.lists[id] = true
	}
	return r, nil
}

var (
	// voWords: what names an audio-only release after its title part
	voWords = []string{"звуковая дорожка", "звуковые дорожки", "дорожк", "озвучк", "[rus sound]", "only audio"}
	voLatin = regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])(sound|audio)([^\p{L}\p{N}]|$)`)
	voCodec = regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])(ac3|dts)([^\p{L}\p{N}]|$)`)
)

// voiceOverRelease reports whether a release title names an audio-track
// release (the words after its title part, release.Tail: «Звуковая
// дорожка», «RUS Sound», «.mka», «AC3» without a video resolution…) and
// its studio (the first release.Studios names; "" when none).
func voiceOverRelease(title string) (bool, string) {
	tail := strings.ToLower(release.Tail(title))
	// «Sound» / «Audio» beside a resolution name a video release's audio
	// («1080p Multi Audio»), as AC3/DTS do
	ok := strings.Contains(strings.ToLower(title), ".mka") || voLatin.MatchString(tail) && release.Parse(title).Resolution == 0
	for _, w := range voWords {
		ok = ok || strings.Contains(tail, w)
	}
	if !ok && voCodec.MatchString(tail) && release.Parse(title).Resolution == 0 {
		ok = true
	}
	if !ok {
		return false, ""
	}
	return true, firstStudio(title)
}

func firstStudio(title string) string {
	if s := release.Studios(title); len(s) > 0 {
		return s[0]
	}
	return ""
}

// voItem is a film or series of the library a voice-over is looked for.
type voItem struct {
	id              int64
	kind            string // KindMovie | KindSeries
	title, original string
	// aliases: the library's other titles of the item (the core's
	// Item.aliases), searched beside title and original
	aliases []string
	year    int
	tmdbID  int64
}

// voTarget is a library file a voice-over track is fitted to: an
// episode's (season, episode) or the film's.
type voTarget struct {
	FileID     int64  `json:"fileId"`
	Path       string `json:"path"`
	Season     int    `json:"season,omitempty"`
	Episode    int    `json:"episode,omitempty"`
	DurationMs int64  `json:"durationMs,omitempty"`
}

func (s *Service) voItem(ctx context.Context, id int64) (voItem, error) {
	if s.o.Library == nil {
		return voItem{}, uerr(ErrUnavailable, "Медиатека недоступна")
	}
	li, err := s.o.Library.Item(ctx, id)
	if errors.Is(err, library.ErrNotFound) {
		return voItem{}, uerr(ErrNotFound, msgVoiceOverItem)
	}
	if err != nil {
		return voItem{}, err
	}
	it, err := voItemOf(li)
	if err != nil {
		return it, err
	}
	// the alias titles («Деньги» of «Миллиарды»): without them the search
	// still runs on the title and original title
	switch al, err := s.o.Library.Aliases(ctx, id); {
	case err == nil:
		it.aliases = al
	case ctx.Err() == nil:
		s.log.Warn("acquire: voice-over search: alias titles", "item", id, "err", err)
	}
	return it, nil
}

// voItemOf is the library's film or series li as a voItem.
func voItemOf(li library.Item) (voItem, error) {
	it := voItem{id: li.ID, title: li.Title, original: li.OriginalTitle, year: li.Year, tmdbID: li.TmdbID}
	switch li.Kind {
	case "movie":
		it.kind = KindMovie
	case "series":
		it.kind = KindSeries
	default:
		return it, uerr(ErrInvalid, msgVoiceOverItem)
	}
	return it, nil
}

// voTargets are the files of a film, or the episode files of a series
// (season and episode known), present on the server.
func (s *Service) voTargets(ctx context.Context, it voItem) ([]voTarget, error) {
	if s.o.Library == nil {
		return nil, nil
	}
	files, err := s.o.Library.ItemFiles(ctx, it.id)
	if errors.Is(err, library.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []voTarget
	if it.kind != KindSeries {
		for _, f := range files {
			if f.ItemID == it.id {
				out = append(out, voTarget{FileID: f.ID, Path: f.Path, DurationMs: f.DurationMs})
			}
		}
		return out, nil
	}
	// a series: its episodes' files, with the episode's numbers
	eps := map[int64]library.Item{}
	for _, f := range files {
		if f.ItemID == it.id {
			continue
		}
		ep, ok := eps[f.ItemID]
		if !ok {
			switch ep, err = s.o.Library.Item(ctx, f.ItemID); {
			case errors.Is(err, library.ErrNotFound):
				continue // gone meanwhile
			case err != nil:
				return nil, err
			}
			eps[f.ItemID] = ep
		}
		if ep.Kind != library.KindEpisode || ep.SeasonNo <= 0 || ep.EpisodeNo <= 0 {
			continue
		}
		out = append(out, voTarget{FileID: f.ID, Path: f.Path, Season: ep.SeasonNo, Episode: ep.EpisodeNo, DurationMs: f.DurationMs})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Season != out[b].Season {
			return out[a].Season < out[b].Season
		}
		if out[a].Episode != out[b].Episode {
			return out[a].Episode < out[b].Episode
		}
		return out[a].Path < out[b].Path
	})
	return out, nil
}

// voSearchRequest is the one search of the title: its library names (the
// title, the original title and the aliases, as the old build's
// item_titles query gave them).
func voSearchRequest(it voItem) indexer.SearchRequest {
	q := indexer.SearchRequest{Text: it.title, Kind: it.kind, Year: it.year, VoiceOvers: true}
	seen := map[string]bool{release.Normalize(it.title): true}
	for _, t := range append([]string{it.original}, it.aliases...) {
		if k := release.Normalize(t); k != "" && !seen[k] {
			seen[k] = true
			q.AltTitles = append(q.AltTitles, t)
		}
	}
	return q
}

// candidateID names a release within a search.
func candidateID(r indexer.ScoredRelease) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%d|%s|%s|%s", r.SourceID, r.Link, r.Magnet, strings.ToLower(r.InfoHash))
	return fmt.Sprintf("%016x", h.Sum64())
}

// FindVoiceOvers searches the sources once for the film or series itemID
// and returns its voice-over releases (voiceOverRelease), the household's
// favourite studios first, then by seeders; and up to voMaxVideo video
// releases of the title naming a studio, whose file lists the user may
// look into. Releases of another title are left out (rejected or not:
// an audio release has no resolution and few seeders, which the release
// score holds against it).
func (s *Service) FindVoiceOvers(ctx context.Context, itemID int64) (VoiceOvers, error) {
	it, err := s.voItem(ctx, itemID)
	if err != nil {
		return VoiceOvers{}, err
	}
	targets, err := s.voTargets(ctx, it)
	if err != nil {
		return VoiceOvers{}, err
	}
	sr := voSearchRequest(it)
	resp, err := s.search(ctx, sr)
	if err != nil {
		return VoiceOvers{}, err
	}
	var favourites []string
	if resp.Prefs != nil {
		favourites = resp.Prefs.Studios
	}
	titles := append([]string{sr.Text}, sr.AltTitles...)
	out := VoiceOvers{Candidates: []VoiceOverCandidate{}, VideoReleases: []VoiceOverCandidate{}, Failed: resp.Failed}
	if out.Failed == nil {
		out.Failed = []indexer.SourceError{}
	}
	search := &voSearch{at: time.Now(), rels: map[string]indexer.ScoredRelease{}, lists: map[string]bool{}}
	seen := map[string]bool{}
	for _, r := range append(append([]indexer.ScoredRelease{}, resp.Releases...), resp.Rejected...) {
		if r.Direct || !indexer.TitleMatches(r.Title, titles) || release.Parse(r.Title).IsCam {
			continue
		}
		id := candidateID(r)
		if seen[id] {
			continue
		}
		seen[id] = true
		c := VoiceOverCandidate{ID: id, SourceID: r.SourceID, SourceName: r.SourceName, Title: r.Title, Studios: release.Studios(r.Title),
			Seeders: r.Seeders, Size: r.Size, InfoHash: strings.ToLower(r.InfoHash)}
		if c.Studios == nil {
			c.Studios = []string{}
		}
		ok, studio := voiceOverRelease(r.Title)
		switch {
		case ok:
			c.Studio = studio
			c.Coverage = titleCoverage(it, targets, r.Title)
			out.Candidates = append(out.Candidates, c)
		case len(c.Studios) > 0:
			c.Studio, c.Video = c.Studios[0], true
			c.Coverage = titleCoverage(it, targets, r.Title)
			out.VideoReleases = append(out.VideoReleases, c)
		default:
			continue
		}
		search.rels[id] = r
	}
	sortCandidates(out.Candidates, favourites)
	sortCandidates(out.VideoReleases, favourites)
	if len(out.VideoReleases) > voMaxVideo {
		for _, c := range out.VideoReleases[voMaxVideo:] {
			delete(search.rels, c.ID)
		}
		out.VideoReleases = out.VideoReleases[:voMaxVideo]
	}
	s.voSearch.put(itemID, search)
	return out, nil
}

// studioRank is the rank of a release's best favourite studio (0 =
// first; len(favourites): none).
func studioRank(title string, studios []string, favourites []string) int {
	for r, f := range favourites {
		k := release.StudioKey(f)
		for _, n := range studios {
			if release.StudioKey(n) == k {
				return r
			}
		}
		if release.NamesStudio(release.Tail(title), f) {
			return r
		}
	}
	return len(favourites)
}

func sortCandidates(cs []VoiceOverCandidate, favourites []string) {
	rank := make(map[string]int, len(cs))
	for _, c := range cs {
		rank[c.ID] = studioRank(c.Title, c.Studios, favourites)
	}
	sort.SliceStable(cs, func(a, b int) bool {
		if ra, rb := rank[cs[a].ID], rank[cs[b].ID]; ra != rb {
			return ra < rb
		}
		return cs[a].Seeders > cs[b].Seeders
	})
}

// titleCoverage is the coverage a release's title tells: a film is
// covered; a series release covers the episodes of the seasons it names
// (an episode range: those), unknown when it names none.
func titleCoverage(it voItem, targets []voTarget, title string) VoiceOverCoverage {
	if it.kind == KindMovie {
		return VoiceOverCoverage{Have: 1, Total: 1, Known: true, Film: true}
	}
	eps := map[[2]int]bool{}
	for _, t := range targets {
		eps[[2]int{t.Season, t.Episode}] = true
	}
	cov := VoiceOverCoverage{Total: len(eps)}
	lo, hi := indexer.SeasonRange(title)
	if lo == 0 {
		return cov
	}
	info := release.Parse(title)
	cov.Known = true
	for k := range eps {
		if k[0] < lo || k[0] > hi {
			continue
		}
		if lo == hi && info.Episode > 0 && !info.IsSeasonPack && (k[1] < info.Episode || k[1] > max(info.EpisodeEnd, info.Episode)) {
			continue
		}
		cov.Have++
	}
	return cov
}
