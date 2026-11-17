package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// SearchRequest asks every enabled source for a title. AltTitles are other
// names (original, TMDB aliases): sources are searched by each of them and
// releases named by any of them match.
type SearchRequest struct {
	Text      string   `json:"text"`
	AltTitles []string `json:"altTitles,omitempty"`
	Kind      string   `json:"kind"` // movie | series | music | book | audiobook | comic
	Year      int      `json:"year,omitempty"`
	Season    int      `json:"season,omitempty"`
	Episode   int      `json:"episode,omitempty"`
	// Artist and Work: music, books, audiobooks and comics — the artist or
	// author ("" for comics) and the album, book or series ("" = everything
	// of the artist). Release names must hold Work and a word of Artist.
	Artist string `json:"artist,omitempty"`
	Work   string `json:"work,omitempty"`
	// Aliases: other names of Work (music: the MusicBrainz disambiguation,
	// «The Black Album»); a release named by one of them matches.
	Aliases []string `json:"aliases,omitempty"`
	// Loose skips the title filter (free-text search on the admin screen:
	// «Дюна» should also show «Дюна: Часть вторая»).
	Loose bool `json:"loose,omitempty"`
	// Studios: voice-over studios ranked before Prefs.Studios (a series
	// keeps the studio of the seasons already on the server).
	Studios []string `json:"studios,omitempty"`
	// VoiceOvers: «Найти озвучку…» — the same one query also takes the
	// Audio category (RuTracker's «Звуковые дорожки и Переводы», 185 and
	// 809): audio-track releases of the title; scored as Kind.
	VoiceOvers bool `json:"voiceOvers,omitempty"`
}

// ScoredRelease is a found release with its score.
type ScoredRelease struct {
	Result
	SourceID   int64
	SourceName string
	Info       release.Info
	Score      int
	Reasons    []string
	// Direct: a file link of an OPDS catalog or a direct-download
	// definition, downloaded as is (no torrent).
	Direct    bool
	directTag directTag
	// Lang: books and audiobooks — the language of the release (ru | en |
	// ja; "" = unknown), from the source's metadata (an OPDS dc:language)
	// or its name (release.BookLang).
	Lang string
	// AudioFormat: music — the format badge («FLAC», «MP3 320», «MP3
	// V0»; "" = not named); FormatMismatch: another format than
	// «Формат загрузки» (listed last, never picked by itself).
	AudioFormat    string
	FormatMismatch bool
}

// MarshalJSON uses camelCase names (cardigann.Result has no tags).
func (r ScoredRelease) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Title       string    `json:"title"`
		Details     string    `json:"details,omitempty"`
		Link        string    `json:"link,omitempty"`
		Magnet      string    `json:"magnet,omitempty"`
		InfoHash    string    `json:"infoHash,omitempty"`
		Size        int64     `json:"size"`
		Seeders     int       `json:"seeders"`
		Leechers    int       `json:"leechers"`
		PublishDate time.Time `json:"publishDate,omitzero"`
		Freeleech   bool      `json:"freeleech,omitempty"`
		SourceID    int64     `json:"sourceId"`
		SourceName  string    `json:"sourceName"`
		Resolution  int       `json:"resolution,omitempty"`
		Source      string    `json:"source,omitempty"`
		Codec       string    `json:"codec,omitempty"`
		HDR         bool      `json:"hdr,omitempty"`
		Audio       []string  `json:"audio,omitempty"`
		Subs        []string  `json:"subs,omitempty"`
		Studios     []string  `json:"studios,omitempty"`
		Season      int       `json:"season,omitempty"`
		Episode     int       `json:"episode,omitempty"`
		EpisodeEnd  int       `json:"episodeEnd,omitempty"`
		SeasonPack  bool      `json:"seasonPack,omitempty"`
		Score       int       `json:"score"`
		Excluded    bool      `json:"excluded,omitempty"`
		Reasons     []string  `json:"reasons"`
		Direct      bool      `json:"direct,omitempty"`
		Lang        string    `json:"lang,omitempty"`
		AudioFormat string    `json:"audioFormat,omitempty"`
		Mismatch    bool      `json:"formatMismatch,omitempty"`
	}{
		r.Title, r.Details, r.Link, r.Magnet, r.InfoHash, r.Size, r.Seeders, r.Leechers, r.PublishDate,
		r.DownloadVolumeFactor == 0, r.SourceID, r.SourceName, r.Info.Resolution, r.Info.Source, r.Info.Codec,
		r.Info.HDR, r.Info.Audio, r.Info.Subs, r.Info.Studios, r.Info.Season, r.Info.Episode, r.Info.EpisodeEnd, r.Info.IsSeasonPack,
		r.Score, r.Score == Excluded, r.Reasons, r.Direct, r.Lang, r.AudioFormat, r.FormatMismatch,
	})
}

// SourceError: a source that failed during a search.
type SourceError struct {
	SourceID int64  `json:"sourceId"`
	Name     string `json:"name"`
	Message  string `json:"message"`
}

// SearchResponse: Releases are acceptable releases, best first; Rejected
// were excluded by Score (with the reason); Failed lists broken sources.
type SearchResponse struct {
	Releases []ScoredRelease `json:"releases"`
	Rejected []ScoredRelease `json:"rejected"`
	Failed   []SourceError   `json:"failed"`
	// Prefs: the release preferences the releases were scored by (nil
	// from a source of no preferences).
	Prefs *Prefs `json:"-"`
}

const maxQueries = 3

// PendingSource: a source a search still waits for.
type PendingSource struct {
	SourceID int64  `json:"sourceId"`
	Name     string `json:"name"`
}

// Progress is a search so far: the response of the sources that answered
// (scored, deduplicated and sorted as the final one would be) and those
// still pending.
type Progress struct {
	SearchResponse
	Pending []PendingSource
}

type progressKey struct{}

// WithProgress makes Search call f once before any source answers and then
// each time one answers while others are still pending. The calls are
// serialized and run apart from the sources: a slow f never holds up a
// source's answer, and a report superseded while f is busy is dropped for
// the newer one. They all end before Search returns; the final response is
// Search's own result, never passed to f.
func WithProgress(ctx context.Context, f func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, f)
}

// found is one source's answer to a search (done: it answered).
type found struct {
	src     Source
	results []Result
	err     error
	direct  bool                 // the source's links are direct downloads
	tags    map[string]directTag // OPDS: the format of each link
	done    bool
}

// Search queries all enabled sources in parallel (each limited by the
// search timeout). A failing source never fails the whole search: it is
// reported in Failed and marked with status error.
func (m *Manager) Search(ctx context.Context, q SearchRequest) (SearchResponse, error) {
	queries := searchQueries(q)
	if len(queries) == 0 {
		return SearchResponse{}, errors.New("Пустой запрос")
	}
	all, err := m.Sources(ctx)
	if err != nil {
		return SearchResponse{}, err
	}
	var sources []Source
	backup := map[int64]Source{} // primary id -> its fallback source
	for _, s := range all {
		if s.FallbackFor != 0 {
			backup[s.FallbackFor] = s
			continue // asked only when its primary fails
		}
		if s.Enabled && (!m.isDirect(s) || opdsKind(q.Kind)) {
			sources = append(sources, s)
		}
	}
	// a source resting after a refused sign-in is not asked; its backup
	// (the other way into the same tracker) answers for it
	// (without one it is reported failed, not asked: the search is still
	// incomplete, so an album is retried later, never «not found»)
	for i, s := range sources {
		if b, ok := backup[s.ID]; ok && m.isResting(s.ID) && !m.isResting(b.ID) {
			delete(backup, s.ID)
			b.Name = s.Name
			sources[i] = b
		}
	}
	sortSources(sources)
	prefs, err := m.Prefs(ctx)
	if err != nil {
		m.log.Warn("indexer: release prefs", "err", err)
	}
	if m.studios != nil {
		// the core's favourite studios first
		prefs.Studios = release.MergeStudios(m.studios(ctx), prefs.Studios)
	}

	progress, _ := ctx.Value(progressKey{}).(func(Progress))
	var mu sync.Mutex // out and left
	out := make([]found, len(sources))
	for i, s := range sources {
		out[i].src = s
	}
	left := len(sources)
	// deliver words a snapshot of out and hands it to progress. It runs
	// outside mu: a progress call writes to a client, and a slow client must
	// not hold up the sources that answer meanwhile.
	deliver := func(snap []found) {
		if ctx.Err() != nil {
			return
		}
		p := Progress{SearchResponse: m.assemble(ctx, q, prefs, snap), Pending: []PendingSource{}}
		for _, f := range snap {
			if !f.done {
				p.Pending = append(p.Pending, PendingSource{SourceID: f.src.ID, Name: f.src.Name})
			}
		}
		progress(p)
	}
	// snaps holds the latest snapshot not yet delivered (latest wins: a
	// snapshot superseded before its turn is dropped); one goroutine
	// delivers them in order, and Search returns only after it is done, so
	// the progress calls stay serialized and end before the final response.
	var snaps chan []found
	delivered := make(chan struct{})
	if progress != nil && left > 0 {
		deliver(slices.Clone(out)) // before any source is asked: nothing to hold up
		snaps = make(chan []found, 1)
		go func() {
			defer close(delivered)
			for snap := range snaps {
				deliver(snap)
			}
		}()
	} else {
		close(delivered)
	}
	report := func() { // under mu
		if snaps == nil || left == 0 {
			return // the last answer is the final response
		}
		select {
		case <-snaps: // superseded
		default:
		}
		snaps <- slices.Clone(out)
	}
	var wg sync.WaitGroup
	for i, s := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var f found
			if isOPDS(s) {
				res, tags, err := m.searchOPDS(ctx, s, q, queries)
				f = found{s, res, err, true, tags, true}
			} else {
				var res []Result
				var err error
				if m.isResting(s.ID) {
					err = cardigann.ErrLoginFailed // not asked: no login attempt the tracker sees
				} else {
					res, err = m.searchSource(ctx, s, q, queries)
				}
				f = found{s, res, err, m.directDef(s), nil, true}
				if b, ok := backup[s.ID]; ok && err != nil && ctx.Err() == nil && !m.isResting(b.ID) {
					// the second way in, only now: the tracker is asked
					// once per search, not twice
					if res, berr := m.searchSource(ctx, b, q, queries); berr == nil {
						m.log.Info("indexer: answered through the backup source", "source", s.Name, "backup", b.Name)
						b.Name = s.Name // one tracker to the user; downloads go through b
						f = found{b, res, nil, m.directDef(b), nil, true}
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			out[i] = f
			left--
			report()
		}()
	}
	wg.Wait()
	if snaps != nil {
		close(snaps)
	}
	<-delivered
	if err := ctx.Err(); err != nil {
		return SearchResponse{}, err
	}
	return m.assemble(ctx, q, prefs, out), nil
}

// assemble scores, deduplicates and sorts the releases of the sources that
// answered (out is in priority order; pending sources are skipped).
func (m *Manager) assemble(ctx context.Context, q SearchRequest, prefs Prefs, out []found) SearchResponse {
	want := Wanted{Titles: append([]string{q.Text}, q.AltTitles...), Year: q.Year, Season: q.Season, Episode: q.Episode, Kind: q.Kind,
		Artist: q.Artist, Work: q.Work, Aliases: q.Aliases, Studios: wantedStudios(q.Studios, prefs.Studios)}
	if q.Loose {
		want.Titles = nil
	}
	resp := SearchResponse{Releases: []ScoredRelease{}, Rejected: []ScoredRelease{}, Failed: []SourceError{}, Prefs: &prefs}
	best := map[string]int{} // dedup key -> index in candidates
	var candidates []ScoredRelease
	russian := map[int64]bool{} // sources of Russian-language trackers
	shared := sharedPages(out)
	for _, f := range out {
		if !f.done {
			continue
		}
		if def, err := m.cat.Definition(f.src.DefinitionID); err == nil && isRussian(def.Language) {
			russian[f.src.ID] = true
		}
		if f.err != nil {
			resp.Failed = append(resp.Failed, SourceError{SourceID: f.src.ID, Name: f.src.Name, Message: humanErrorIn(ctx, f.err)})
		}
		for _, r := range f.results {
			if r.Link == "" && r.Magnet == "" {
				continue // nothing to download
			}
			c := ScoredRelease{Result: r, SourceID: f.src.ID, SourceName: f.src.Name}
			if f.direct {
				c.Direct, c.directTag = true, f.tags[r.Link]
			}
			key := dedupKey(c, shared)
			if i, ok := best[key]; ok {
				// sources are in priority order: replace only with more seeders
				if r.Seeders > candidates[i].Seeders {
					candidates[i] = c
				}
				continue
			}
			best[key] = len(candidates)
			candidates = append(candidates, c)
		}
	}
	for _, c := range candidates {
		c.Info = release.Parse(c.Title)
		w := want
		if russian[c.SourceID] {
			w.DefaultLang = "ru" // a Russian tracker names a translation only when it is not Russian
		}
		if q.Kind == "book" || q.Kind == "audiobook" {
			c.Lang = releaseLang(c)
		}
		c.Score, c.Reasons = Score(c.Result, c.Info, w, prefs)
		if q.Kind == "music" {
			a := release.ParseAudio(c.Title)
			c.AudioFormat, c.FormatMismatch = a.Label(), MusicFit(a, prefs) == FitNo
		}
		if c.Direct {
			c.Score, c.Reasons = scoreDirect(c.Result, c.directTag, want, prefs)
		}
		// reasons are Russian keys or their renderings: the request's language
		c.Reasons = localizeReasons(ctx, c.Reasons)
		if c.Score == Excluded {
			resp.Rejected = append(resp.Rejected, c)
		} else {
			resp.Releases = append(resp.Releases, c)
		}
	}
	sortReleases(resp.Releases)
	sortReleases(resp.Rejected)
	return resp
}

// localizeReasons translates the score reasons (made in Russian by Score)
// into the language of ctx.
func localizeReasons(ctx context.Context, reasons []string) []string {
	if i18n.FromContext(ctx) == i18n.RU {
		return reasons
	}
	out := make([]string, len(reasons))
	for i, r := range reasons {
		out[i] = i18n.Localize(ctx, r)
	}
	return out
}

// sortReleases: a release of another music format last; best score first; on a tie a direct download, then more
// seeders.
func sortReleases(s []ScoredRelease) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].FormatMismatch != s[j].FormatMismatch {
			return s[j].FormatMismatch // another format last, whatever its score
		}
		if s[i].Score != s[j].Score {
			return s[i].Score > s[j].Score
		}
		if s[i].Direct != s[j].Direct {
			return s[i].Direct
		}
		return s[i].Seeders > s[j].Seeders
	})
}

func dedupKey(r ScoredRelease, sharedPage map[string]bool) string {
	if h := strings.ToLower(strings.TrimSpace(r.InfoHash)); h != "" {
		return "ih:" + h
	}
	// the same topic through two sources of one tracker (RuTracker by
	// login and by cookie): one release, whatever each names it
	if p := pageKey(r); p != "" && !sharedPage[p] {
		return p
	}
	link := r.Link
	if link == "" {
		link = r.Magnet
	}
	return fmt.Sprintf("src:%d:%s", r.SourceID, link)
}

// pageKey is a release's page (Details) as a dedup key; "" without one.
func pageKey(r ScoredRelease) string {
	u, err := url.Parse(strings.TrimSpace(r.Details))
	if err != nil || u.Host == "" || (u.Path == "" && u.RawQuery == "") {
		return ""
	}
	return "page:" + strings.TrimPrefix(strings.ToLower(u.Host), "www.") + u.Path + "?" + u.RawQuery
}

// sharedPages are the pages two different releases (downloads) of one
// source share: such a source's Details is not the release's own page, so
// it is no key. One release found by two queries is not two.
func sharedPages(out []found) map[string]bool {
	shared := map[string]bool{}
	for _, f := range out {
		if !f.done {
			continue
		}
		dl := map[string]string{} // page -> its download
		for _, r := range f.results {
			p := pageKey(ScoredRelease{Result: r})
			if p == "" {
				continue
			}
			d := r.Link + "\x00" + r.Magnet
			if prev, ok := dl[p]; ok && prev != d {
				shared[p] = true
			}
			dl[p] = d
		}
	}
	return shared
}

// searchQueries: the main title and alternative ones, without duplicates.
func searchQueries(q SearchRequest) []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range append([]string{q.Text}, q.AltTitles...) {
		t = strings.TrimSpace(t)
		k := foldTitle(t)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, t)
		if len(out) == maxQueries {
			break
		}
	}
	return out
}

// Newznab categories and Cardigann query type per kind.
func kindQuery(kind string) ([]int, string) {
	switch kind {
	case "movie":
		return []int{2000}, "movie"
	case "series":
		return []int{5000}, "tvsearch"
	case "music":
		return []int{3000}, "music"
	case "book":
		return []int{7000}, "book"
	case "comic":
		return []int{7030}, "book"
	case "audiobook":
		return []int{3030}, "search"
	}
	return nil, "search"
}

// keepCyrillic turns off the Sonarr/Radarr helpers of Russian trackers
// (Kinozal strips Cyrillic letters from titles by default and appends
// «RUSSIAN»): books, audiobooks, comics and music are matched by their
// Russian title and author, which those helpers erase.
var keepCyrillic = map[string]string{"stripcyrillic": "", "addrussian": "", "addrussiantotitle": ""}

func kindConfig(kind string) map[string]string {
	if isMediaKind(kind) {
		return keepCyrillic
	}
	return nil
}

// searchSource runs the queries on one source within the search timeout.
// Russian trackers name seasons «Сезон 1», not S01, so they get the title
// alone and Score filters seasons; other trackers get "<title> S01" (big
// international sites return only the first results for a long-running show).
func (m *Manager) searchSource(ctx context.Context, s Source, q SearchRequest, queries []string) ([]Result, error) {
	sctx, cancel := context.WithTimeout(ctx, m.searchTimeout)
	defer cancel()
	r, err := m.runner(s)
	if err != nil {
		m.setStatus(ctx, s.ID, StatusError, humanError(err))
		return nil, err
	}
	cats, typ := kindQuery(q.Kind)
	if q.VoiceOvers && len(cats) > 0 {
		cats = append(cats[:len(cats):len(cats)], 3000)
	}
	if def, err := m.cat.Definition(s.DefinitionID); err == nil && q.Season > 0 && !isRussian(def.Language) {
		suffixed := make([]string, len(queries))
		for i, kw := range queries {
			suffixed[i] = fmt.Sprintf("%s S%02d", kw, q.Season)
		}
		queries = suffixed
	}
	var results []Result
	ok := false
	for _, kw := range queries {
		res, err := r.Search(sctx, cardigann.Query{Keywords: kw, Categories: cats, Type: typ, Config: kindConfig(q.Kind)})
		if err != nil {
			if ctx.Err() != nil {
				return results, ctx.Err() // the whole search was cancelled: say nothing about the source
			}
			if sctx.Err() != nil {
				err = fmt.Errorf("%w: %w", errSourceTimeout, err)
			}
			m.log.Info("indexer: search failed", "source", s.Name, "query", kw, "err", cardigann.RedactText(err.Error()))
			if ok {
				return results, nil // partial results are still results
			}
			m.setStatus(ctx, s.ID, StatusError, humanError(err))
			if errors.Is(err, cardigann.ErrLoginFailed) || errors.Is(err, cardigann.ErrCaptcha) {
				m.rest(s.ID)
			}
			return nil, err
		}
		ok = true
		results = append(results, res...)
	}
	m.setStatus(ctx, s.ID, StatusOK, "")
	return results, nil
}

// releaseLang is ScoredRelease.Lang: a direct release's tag carries the
// catalog's dc:language («EPUB RUS»), a torrent's name its own tags.
func releaseLang(c ScoredRelease) string {
	if c.directTag != "" {
		if l := release.BookLang("[" + c.directTag + "]"); l != "" {
			return l
		}
	}
	return release.BookLang(c.Title)
}
