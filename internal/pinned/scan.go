package pinned

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// scanner is the tracking scanner of kind "pinned".
type scanner struct{ s *Service }

// Scanner is the tracking scanner of the pinned releases: it refreshes a
// title's pin first when due (checked over 6 h ago), then gives one source
// per episode the pin holds.
func (s *Service) Scanner() Scanner { return scanner{s} }

func (scanner) Kind() string { return "pinned" }

// Scan: one Source per episode of the title's pin, Ref "pin:<id>:<file
// index>" of its video (its first file when it has none), Quality from the
// file's name or the release's, Labels the release's audio, studios and
// subtitles. A film's pin gives one source (season and episode 0) on its
// largest video. No pin: none.
func (c scanner) Scan(ctx context.Context, t tracking.Title, eps []tracking.Episode) ([]tracking.Source, error) {
	s := c.s
	p, ok, err := s.ByTitle(ctx, t.ID)
	if err != nil || !ok {
		return nil, err
	}
	s.refreshDue(ctx, p)
	if p, ok, err = s.ByTitle(ctx, t.ID); err != nil || !ok {
		return nil, err
	}
	// old files of a renamed re-upload whose time has come (also after a
	// restart that missed the import's hook)
	if err := s.SweepStale(ctx, p.InfoHash); err != nil {
		s.log().Warn("pinned: old files of a replaced release", "pin", p.ID, "err", err)
	}
	if s.AfterScan != nil {
		// the followed title's missing episodes (Actions.Sync): after the
		// pin's refresh, whatever the scan finds
		defer func() {
			if err := s.AfterScan(ctx, t); err != nil && ctx.Err() == nil {
				s.log().Warn("pinned: download the missing episodes", "title", t.ID, "err", err)
			}
		}()
	}
	info := release.Parse(p.ReleaseTitle)
	labels := slices.Concat(info.Audio, info.Studios, info.Subs)
	provider := s.providerName(ctx, p.SourceID)
	meta := tracking.SourceMeta{SourceID: p.SourceID, Lang: release.AudioLang(p.ReleaseTitle, info),
		Studios: info.Studios, Subs: info.Subs}
	if len(info.Studios) > 0 {
		meta.Voice = info.Studios[0]
	}
	if len(info.Audio) > 0 {
		meta.Audio = info.Audio[0]
	}
	src := func(season, ep int, f torrent.FileEntry) tracking.Source {
		q := qualityOf(f.Path)
		if q == "" {
			q = p.Quality
		}
		m := meta
		idx := f.Index
		m.FileIndex = &idx
		return tracking.Source{TitleID: t.ID, Season: season, Episode: ep, Kind: "pinned",
			Ref: fmt.Sprintf("pin:%d:%d", p.ID, f.Index), Provider: provider, Quality: q, Labels: labels, Meta: m}
	}
	byIndex := map[int]torrent.FileEntry{}
	for _, f := range p.Files {
		byIndex[f.Index] = f
	}
	if t.Kind == "movie" {
		var best *torrent.FileEntry
		for i, f := range p.Files {
			if !f.Padding && organize.IsVideo(f.Path) && (best == nil || f.Size > best.Size) {
				best = &p.Files[i]
			}
		}
		if best == nil {
			return nil, nil
		}
		return []tracking.Source{src(0, 0, *best)}, nil
	}
	byEp := EpisodesOf(p.Files, DefaultSeason(eps))
	keys := make([][2]int, 0, len(byEp))
	for k := range byEp {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b [2]int) int {
		if a[0] != b[0] {
			return a[0] - b[0]
		}
		return a[1] - b[1]
	})
	out := make([]tracking.Source, 0, len(keys))
	for _, k := range keys {
		idx := byEp[k]
		f := byIndex[idx[0]]
		for _, i := range idx {
			if organize.IsVideo(byIndex[i].Path) {
				f = byIndex[i]
				break
			}
		}
		out = append(out, src(k[0], k[1], f))
	}
	return out, nil
}

// DefaultSeason is the season of a file naming none: the title's only
// season when its list has one, else 1.
func DefaultSeason(eps []tracking.Episode) int {
	season := 0
	for _, e := range eps {
		if e.Season == 0 {
			continue
		}
		if season != 0 && e.Season != season {
			return 1
		}
		season = e.Season
	}
	if season == 0 {
		return 1
	}
	return season
}

// providerName is the name of the pin's source ("" for none).
func (s *Service) providerName(ctx context.Context, sourceID string) string {
	id, err := strconv.ParseInt(sourceID, 10, 64)
	if err != nil || s.Indexer == nil {
		return ""
	}
	srcs, err := s.Indexer.Sources(ctx)
	if err != nil {
		return ""
	}
	for _, x := range srcs {
		if x.ID == id {
			return x.Name
		}
	}
	return ""
}

// ManagerIndexer is an Indexer of the sources manager: its Sources are the
// enabled torrent sources (no OPDS catalog or direct-download site, which
// accept any link and download no torrent).
func ManagerIndexer(m *indexer.Manager) Indexer { return managerIndexer{m} }

type managerIndexer struct{ *indexer.Manager }

func (m managerIndexer) Sources(ctx context.Context) ([]indexer.Source, error) {
	all, err := m.Manager.Sources(ctx)
	if err != nil {
		return nil, err
	}
	var out []indexer.Source
	for _, s := range all {
		if !s.Enabled || s.DefinitionID == indexer.OPDSDefinitionID {
			continue
		}
		if def, err := m.Catalog().Definition(s.DefinitionID); err == nil && def.Download != nil && def.Download.Direct {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}
