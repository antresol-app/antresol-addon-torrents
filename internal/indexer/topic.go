package indexer

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// A Russian tracker's release name often leaves out its voice-overs
// («… WEB-DL 1080p | P») while the release page lists them («Перевод:
// Профессиональный (многоголосый) LostFilm»). So for the releases of such
// a source whose name names no studio, the search reads their pages —
// the source's own session, never a sign-in — and scores them by what the
// page says too. It is bounded: the topicMax with most seeders, topicWorkers
// at a time, topicBudget for all of a source's pages; a page that does not
// answer leaves the release scored by its name alone.
const (
	topicMax      = 12
	topicWorkers  = 4
	topicBudget   = 6 * time.Second
	topicTTL      = 12 * time.Hour
	topicMissTTL  = 30 * time.Minute // a page that did not answer is asked again after this
	topicCacheMax = 4096
)

// topicInfo is what a release page says of its voice-overs.
type topicInfo struct {
	Studios []string // known studios, canonical names
	Audio   []string // "Dub" | "MVO" | "AVO", as release.Parse names them
	Voice   string   // the voice-over lines, for studios the household typed
}

func (t topicInfo) empty() bool { return len(t.Studios) == 0 && len(t.Audio) == 0 && t.Voice == "" }

// voiceLine is a release page line about its voice-overs: «Перевод:»,
// «Перевод 1:», «Озвучивание:», «Озвучка:», «Звук:», «Аудио 2:» and their
// like (a label at the line's start, a colon).
var voiceLine = regexp.MustCompile(`(?im)^[\s*•·\-]*(перевод|озвучивание|озвучание|озвучка|озвучено|звук|аудио|аудиодорожк[аи]|дорожк[аи]|audio|translation)\s*(?:#?\s*\d+)?\s*[:：]\s*(.+)$`)

// parseTopic reads the voice-over lines of a release page's text.
func parseTopic(text string) topicInfo {
	var t topicInfo
	var lines []string
	for _, m := range voiceLine.FindAllStringSubmatch(text, 40) {
		v := strings.TrimSpace(m[2])
		if v == "" {
			continue
		}
		lines = append(lines, v)
	}
	if len(lines) == 0 {
		return t
	}
	voice := strings.Join(lines, "\n")
	low := strings.ToLower(voice)
	for _, a := range []struct{ word, tag string }{{"дубл", "Dub"}, {"многоголос", "MVO"}, {"двухголос", "MVO"}, {"авторск", "AVO"}, {"одноголос", "AVO"}} {
		if strings.Contains(low, a.word) && !slices.Contains(t.Audio, a.tag) {
			t.Audio = append(t.Audio, a.tag)
		}
	}
	t.Studios = release.MergeStudios(release.Studios(voice))
	t.Voice = voice
	return t
}

// withTopic is info with what the release page said: its studios after
// the name's own, its voice-over kinds when the name has none, and a
// studio of want the household typed (outside the table) when the page
// names it.
func withTopic(info release.Info, t topicInfo, want []string) release.Info {
	if t.empty() {
		return info
	}
	info.Studios = release.MergeStudios(info.Studios, t.Studios)
	for _, f := range want {
		if _, known := release.CanonicalStudio(f); !known && release.NamesStudio(t.Voice, f) {
			info.Studios = release.MergeStudios(info.Studios, []string{f})
		}
	}
	if !slices.ContainsFunc(info.Audio, func(a string) bool { return a == "Dub" || a == "MVO" || a == "AVO" }) {
		info.Audio = append(slices.Clone(info.Audio), t.Audio...)
	}
	return info
}

type topicEntry struct {
	info    topicInfo
	expires time.Time
}

// topicCache keeps release pages' voice-overs by page link.
type topicCache struct {
	mu sync.Mutex
	m  map[string]topicEntry
}

func (c *topicCache) get(link string, now time.Time) (topicInfo, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[link]
	if !ok || now.After(e.expires) {
		return topicInfo{}, false
	}
	return e.info, true
}

func (c *topicCache) put(link string, t topicInfo, ttl time.Duration, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]topicEntry{}
	}
	if len(c.m) >= topicCacheMax {
		for k, e := range c.m { // the stale ones first, else any
			if now.After(e.expires) {
				delete(c.m, k)
			}
		}
		for k := range c.m {
			if len(c.m) < topicCacheMax {
				break
			}
			delete(c.m, k)
		}
	}
	c.m[link] = topicEntry{info: t, expires: now.Add(ttl)}
}

// topicKind: the kinds whose releases have voice-overs.
func topicKind(kind string) bool {
	switch kind {
	case "music", "book", "audiobook":
		return false
	}
	return true
}

// topicSource: the search reads the release pages of s (a Russian
// tracker, a kind with voice-overs).
func (m *Manager) topicSource(s Source, q SearchRequest) bool {
	if !topicKind(q.Kind) || m.isDirect(s) {
		return false
	}
	def, err := m.cat.Definition(s.DefinitionID)
	return err == nil && isRussian(def.Language)
}

// readTopics reads the voice-overs of s's releases from their pages (see
// the comment above topicMax), by Details link. nil when s is not a
// Russian tracker or nothing was read.
func (m *Manager) readTopics(ctx context.Context, s Source, q SearchRequest, results []Result) map[string]topicInfo {
	if !m.topicSource(s, q) {
		return nil
	}
	var cand []Result
	seen := map[string]bool{}
	for _, r := range results {
		if r.Details == "" || seen[r.Details] || len(release.Parse(r.Title).Studios) > 0 {
			continue
		}
		seen[r.Details] = true
		cand = append(cand, r)
	}
	if len(cand) == 0 {
		return nil
	}
	slices.SortStableFunc(cand, func(a, b Result) int { return cmp.Compare(b.Seeders, a.Seeders) })
	cand = cand[:min(len(cand), topicMax)]

	out := map[string]topicInfo{}
	var mu sync.Mutex
	var todo []string
	now := m.now()
	for _, r := range cand {
		if t, ok := m.topics.get(r.Details, now); ok {
			if !t.empty() {
				out[r.Details] = t
			}
			continue
		}
		todo = append(todo, r.Details)
	}
	if len(todo) == 0 {
		return out
	}
	run, err := m.runner(s)
	if err != nil {
		return out
	}
	tctx, cancel := context.WithTimeout(ctx, topicBudget)
	defer cancel()
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range min(topicWorkers, len(todo)) {
		wg.Go(func() {
			for link := range jobs {
				text, err := run.TopicText(tctx, link)
				if err != nil {
					if tctx.Err() == nil { // not the budget's end: the page itself failed
						m.topics.put(link, topicInfo{}, topicMissTTL, m.now())
					}
					continue
				}
				t := parseTopic(text)
				m.topics.put(link, t, topicTTL, m.now())
				if !t.empty() {
					mu.Lock()
					out[link] = t
					mu.Unlock()
				}
			}
		})
	}
feed:
	for _, l := range todo {
		select {
		case jobs <- l:
		case <-tctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	return out
}
