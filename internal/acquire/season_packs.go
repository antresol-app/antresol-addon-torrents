package acquire

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	atorrent "github.com/anacrolix/torrent"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// Season requests and packs of several seasons.
//
// A season request (a series, Season > 0, «На сервер») whose release
// holds several seasons downloads only that season's files: the engine's
// file selection (AddOptions.Files), so the disk guard and «Загрузки» see
// the selected size and organize places only those files. A later request
// of another season the pack has adds that season's files to the same
// download (Engine.AddFiles) — also when the release found this time is
// the same files under another infohash (torrent.PathInUseError). The
// seasons a download was asked for are acquisitionMeta.Seasons.

const (
	msgNoSeasonFiles  = "В раздаче нет файлов %d сезона. Выберите другую раздачу"
	msgOrganizingWait = "Подождите — раскладываю файлы"
)

// seasonPackRequest reports whether req is a season request a pack's
// files are selected for.
func seasonPackRequest(req AcquireRequest) bool {
	return req.Mode == ModeDownload && req.Kind == KindSeries && req.Season > 0 && req.Files == nil
}

// seasonFiles selects the files of season from a release's file list: nil
// (the whole torrent) unless its files name several seasons; then the
// files of that season (padding and files of no season left out), or
// ErrNoFiles when it has none.
func seasonFiles(entries []torrent.FileEntry, season int) ([]int, error) {
	seasons := map[int]bool{}
	of := make([]int, len(entries))
	for i, f := range entries {
		if f.Padding {
			continue
		}
		if of[i] = organize.FileSeason(f.Path); of[i] > 0 {
			seasons[of[i]] = true
		}
	}
	if len(seasons) < 2 {
		return nil, nil
	}
	var out []int
	for i, f := range entries {
		if of[i] == season && !f.Padding {
			out = append(out, f.Index)
		}
	}
	if len(out) == 0 {
		return nil, uerr(ErrNoFiles, fmt.Sprintf(msgNoSeasonFiles, season))
	}
	return out, nil
}

// releaseTitle is the release name of a pick (rel), else the magnet's
// display name.
func releaseTitle(rel json.RawMessage, magnet string) string {
	var r struct {
		Title string `json:"title"`
	}
	if len(rel) > 0 && json.Unmarshal(rel, &r) == nil && strings.TrimSpace(r.Title) != "" {
		return r.Title
	}
	if spec, err := atorrent.TorrentSpecFromMagnetUri(magnet); err == nil {
		return spec.DisplayName
	}
	return ""
}

// mayBePack reports whether a magnet named title must be read before a
// season request adds it: its name says several seasons
// (indexer.MultiSeason), or names no season at all («Suits Remux 1080p»
// may be the whole series). The latter is read rather than guessed from
// its size against TMDB's episode count: a 1080p episode is 1 GB in x265
// and 10+ GB in a remux, so any such guess either misses packs or reads
// most releases anyway; reads are bounded (maxPackFetches).
func mayBePack(title string) bool {
	if indexer.MultiSeason(title) {
		return true
	}
	lo, _ := indexer.SeasonRange(title)
	return lo == 0
}

// packFactor: a release this many times the median size of the season's
// own releases in the same search is read as a pack, whatever its name.
const packFactor = 2.5

// maxSeasonCandidates bounds the releases addSeasonPick tries, and
// maxPackFetches the file lists it waits for (each up to
// packFetchTimeout): a request takes at most 3 × 40 s on file lists.
const (
	maxSeasonCandidates = 8
	maxPackFetches      = 3
)

// packFetchTimeout bounds one pack's file-list fetch (tests shorten it).
var packFetchTimeout = releaseFilesTimeout

// webFetchBudget bounds all the pack file-list reads of one request a web
// client waits for (the reverse proxy cuts a request at 60 s; the search
// takes some of it too): each read gets min(packFetchTimeout, what is
// left), and none starts once it is spent. A Background request
// («Следить») has maxPackFetches full reads instead. Tests shorten it.
var webFetchBudget = 30 * time.Second

// addSeasonPick is addSeason of the best release of a search (alts, best
// first): a magnet that may be a pack (its name says several seasons, or
// it is packFactor times the season's median size) is read first, each
// within its own packFetchTimeout, and when its file list does not come,
// or a pack has no file of the season, the next release is taken; after
// maxPackFetches such reads, magnets that need one are passed over. The
// last error is returned when nothing could be added.
func (s *Service) addSeasonPick(ctx context.Context, req AcquireRequest, a *acquisition, opt torrent.AddOptions, alts []indexer.ScoredRelease) (torrent.Handle, *AcquireResult, error) {
	median := seasonMedian(alts, req.Season)
	var err error
	fetches := 0
	budgetEnd := time.Now().Add(webFetchBudget) // a web request's reads (not Background)
	for i, r := range alts {
		if i == maxSeasonCandidates {
			break
		}
		o := opt
		if rel, merr := json.Marshal(r); merr == nil {
			o.Release = rel
		}
		o.SourceURL = r.Details
		link, magnet := strings.TrimSpace(r.Link), strings.TrimSpace(r.Magnet)
		if strings.HasPrefix(strings.ToLower(link), "magnet:") {
			magnet, link = link, ""
		}
		var mf []byte
		if link != "" {
			if s.o.Indexer == nil {
				return nil, nil, uerr(ErrUnavailable, "Источники раздач не настроены")
			}
			res, derr := s.o.Indexer.Download(ctx, r.SourceID, link)
			if derr != nil {
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				return nil, nil, wrapErr(ErrSource, derr.Error(), derr)
			}
			mf, magnet = res.Torrent, res.Magnet
			if magnet != "" {
				mf = nil
			}
		}
		fetch := mayBePack(r.Title) || median > 0 && float64(r.Size) > packFactor*float64(median)
		deadline := time.Now().Add(packFetchTimeout)
		if fetch && mf == nil {
			if fetches == maxPackFetches || !req.Background && !time.Now().Before(budgetEnd) {
				continue // enough waiting: a release that needs no read may follow
			}
			fetches++
			if !req.Background && budgetEnd.Before(deadline) {
				deadline = budgetEnd
			}
		}
		var h torrent.Handle
		var joined *AcquireResult
		h, joined, err = s.addSeason(ctx, req, a, o, mf, magnet, fetch, deadline)
		if err == nil || joined != nil || !errors.Is(err, ErrNoFiles) || ctx.Err() != nil {
			return h, joined, err
		}
		s.log.Info("season request: no file list of the season in a release, trying the next one", "release", r.Title, "season", req.Season, "err", err)
	}
	return nil, nil, err
}

// seasonMedian is the median size of the releases of season alone in a
// search (0 when there is none with a size).
func seasonMedian(alts []indexer.ScoredRelease, season int) int64 {
	var sizes []int64
	for _, r := range alts {
		if lo, hi := indexer.SeasonRange(r.Title); lo == season && hi == season && r.Size > 0 && !indexer.MultiSeason(r.Title) {
			sizes = append(sizes, r.Size)
		}
	}
	if len(sizes) == 0 {
		return 0
	}
	slices.Sort(sizes)
	if n := len(sizes); n%2 == 0 {
		return (sizes[n/2-1] + sizes[n/2]) / 2
	}
	return sizes[len(sizes)/2]
}

// addSeason adds the release of a season request (seasonPackRequest):
// its metafile mf, or a magnet (fetched first, by deadline, when fetch
// says it may be a pack: only then can the files be chosen). A pack gets
// the season's files only (a.Meta.Seasons is set); a release the engine
// has, or the same files under another infohash, gets the season's files
// added instead: then the answer is done.
func (s *Service) addSeason(ctx context.Context, req AcquireRequest, a *acquisition, opt torrent.AddOptions, mf []byte, magnet string, fetch bool, deadline time.Time) (torrent.Handle, *AcquireResult, error) {
	if mf == nil {
		if !fetch {
			h, err := s.o.Engine.AddMagnet(ctx, magnet, opt)
			return h, nil, err
		}
		c, added, err := s.releaseMetafile(ctx, 0, magnet, deadline)
		if err != nil {
			return nil, nil, err
		}
		if added != nil {
			return s.joinSeason(ctx, added.InfoHash, *a, req, func() (torrent.Handle, error) { return s.o.Engine.AddMagnet(ctx, magnet, opt) })
		}
		mf = c.metafile
	}
	ih, _, entries, err := torrent.ParseFiles(mf)
	if err != nil {
		// the engine says what is wrong with it
		h, err := s.o.Engine.AddTorrentFile(ctx, mf, opt)
		return h, nil, err
	}
	if _, ok := s.o.Engine.Get(ih); ok {
		return s.joinSeason(ctx, ih, *a, req, func() (torrent.Handle, error) { return s.o.Engine.AddTorrentFile(ctx, mf, opt) })
	}
	files, err := seasonFiles(entries, req.Season)
	if err != nil {
		return nil, nil, err
	}
	opt.Files = files
	h, err := s.o.Engine.AddTorrentFile(ctx, mf, opt)
	if pe := (*torrent.PathInUseError)(nil); errors.As(err, &pe) {
		res, ok, gerr := s.growSeason(ctx, pe.Owner, *a, req.Season)
		switch {
		case gerr != nil:
			return nil, nil, gerr
		case ok:
			return nil, &res, nil
		}
	}
	if err == nil && files != nil {
		a.Meta.Seasons = []int{req.Season}
	}
	return h, nil, err
}

// joinSeason is addSeason of a release the engine has (ih): the season
// joins its download, or (another title's, a stream) the release is
// added as before: add (the engine answers with the torrent it has).
func (s *Service) joinSeason(ctx context.Context, ih string, a acquisition, req AcquireRequest, add func() (torrent.Handle, error)) (torrent.Handle, *AcquireResult, error) {
	res, ok, err := s.growSeason(ctx, ih, a, req.Season)
	if err != nil {
		return nil, nil, err
	}
	if ok {
		return nil, &res, nil
	}
	h, err := add()
	return h, nil, err
}

// growSeason adds season's files to the download ih of the same series
// (a pack of several seasons downloading some of them): ok false when ih
// is no download of that series. A whole torrent has the season already.
func (s *Service) growSeason(ctx context.Context, ih string, want acquisition, season int) (AcquireResult, bool, error) {
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return AcquireResult{}, false, nil
	}
	// No organize starts while the selection grows (see growSelection).
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.acquisition(ctx, ih)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return AcquireResult{}, false, nil
	case err != nil:
		return AcquireResult{}, false, err
	case a.Kind != KindSeries || a.TMDBID != want.TMDBID || h.Status().Mode != ModeDownload:
		return AcquireResult{}, false, nil
	case a.Season == 0:
		// a whole-series download has every season: nothing to record (a
		// season's cancel must never remove it, see CancelTitle)
		return s.existingResult(ih), true, nil
	}
	files := h.Files()
	if !partial(files) {
		// a whole torrent (or no file list yet: a magnet, added whole) has
		// the season already; it records no seasons and is organized whole
		return s.existingResult(ih), true, nil
	}
	var add []int
	sel, err := seasonFiles(entriesOf(files), season)
	if err != nil {
		return AcquireResult{}, false, err
	}
	for _, i := range sel {
		if !files[i].Selected {
			add = append(add, i)
		}
	}
	seasons := a.Meta.Seasons
	if len(seasons) == 0 && a.Season > 0 {
		seasons = []int{a.Season}
	}
	asked := slices.Contains(seasons, season)
	if len(add) == 0 && asked {
		return s.existingResult(ih), true, nil
	}
	if !asked {
		seasons = append(slices.Clone(seasons), season)
		slices.Sort(seasons)
	}
	a.Meta.Seasons = seasons
	reset := ""
	if len(add) > 0 {
		g, ok := s.o.Engine.(selectionGrower)
		if !ok {
			return AcquireResult{}, false, nil
		}
		a.Meta.SelRev++
		// the selection grows first: an organize that ran meanwhile records
		// nothing once the revision moves (see growSelection)
		if err := g.AddFiles(ctx, ih, add); err != nil {
			return AcquireResult{}, false, growErr(err)
		}
		reset = `, organized_at=NULL, organize_error='', organize_attempts=0`
	}
	// only the fields growing changes: an organize finishing meanwhile
	// records its placed seasons (meta organized) without being undone
	list, _ := json.Marshal(seasons)
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET meta_json=json_set(COALESCE(NULLIF(meta_json,''),'{}'), '$.seasons', json(?), '$.selRev', ?)`+reset+` WHERE infohash=?`,
		string(list), a.Meta.SelRev, ih); err != nil {
		return AcquireResult{}, false, err
	}
	s.log.Info("season added to a download of several seasons", "infohash", ih, "season", season, "files", len(add))
	return s.existingResult(ih), true, nil
}

// coversSeason reports whether a was asked for season (its own, or one a
// pack's selection grew by).
func (a acquisition) coversSeason(season int) bool {
	return a.Season == season || slices.Contains(a.Meta.Seasons, season)
}

// isPack reports whether a is a series pack downloading the seasons asked
// for (acquisitionMeta.Seasons).
func (a acquisition) isPack() bool { return a.Kind == KindSeries && len(a.Meta.Seasons) > 0 }

// union is a ∪ b, sorted.
func union(a, b []int) []int {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// packToPlace keeps the files of a pack's seasons asked for and not placed
// yet, and returns those seasons.
func packToPlace(files []torrent.FileInfo, m acquisitionMeta) ([]torrent.FileInfo, []int) {
	var seasons []int
	for _, n := range m.Seasons {
		if !slices.Contains(m.Organized, n) {
			seasons = append(seasons, n)
		}
	}
	var out []torrent.FileInfo
	for _, f := range files {
		if slices.Contains(seasons, organize.FileSeason(f.Path)) {
			out = append(out, f)
		}
	}
	return out, seasons
}

// packPlaced records a pack whose seasons asked for are all placed (one
// cancelled, or nothing left to place) as organized.
func (s *Service) packPlaced(ctx context.Context, a acquisition) {
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET organized_at=?, organize_error='' WHERE infohash=? AND organized_at IS NULL AND `+selRevIs,
		time.Now().UTC().Format(time.RFC3339), a.InfoHash, a.Meta.SelRev); err != nil {
		s.log.Error("save organize result", "infohash", a.InfoHash, "err", err)
	}
}

// seasonOf narrows st, a pack's torrent, to the files of season: their
// size and progress, and seeding once the season is placed (the ring of
// that season).
func (s *Service) seasonOf(st torrent.Status, a acquisition, season int) torrent.Status {
	h, ok := s.o.Engine.Get(st.InfoHash)
	if !ok {
		return st
	}
	st.Size, st.Done = 0, 0
	for _, f := range h.Files() {
		if f.Selected && organize.FileSeason(f.Path) == season {
			st.Size += f.Size
			st.Done += int64(f.Progress * float64(f.Size))
		}
	}
	if slices.Contains(a.Meta.Organized, season) {
		st.Status = torrent.StatusSeeding
	}
	return st
}

// cancelSeason is CancelTitle of season of a pack that has other seasons
// asked for: the season's files leave the selection (their data, which is
// not in the library, is deleted by the engine) and the season leaves the
// acquisition; a placed season stays. done: the season was taken out.
func (s *Service) cancelSeason(ctx context.Context, a acquisition, season int) (done bool, err error) {
	if slices.Contains(a.Meta.Organized, season) {
		return false, nil
	}
	sh, ok := s.o.Engine.(selectionShrinker)
	if !ok {
		return false, nil
	}
	h, ok := s.o.Engine.Get(a.InfoHash)
	if !ok {
		return false, nil
	}
	// No organize starts while the season goes (startOrganizeJob takes
	// s.mu), and none may be placing its files now.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.organizing[a.InfoHash] {
		return false, uerr(ErrInProgress, msgOrganizingWait)
	}
	var drop []int
	for _, f := range h.Files() {
		if f.Selected && organize.FileSeason(f.Path) == season {
			drop = append(drop, f.Index)
		}
	}
	if len(drop) > 0 {
		// the engine empties a file only when it has no other link (a hard
		// link in the library); and never one a library row names
		if err := sh.RemoveFiles(ctx, a.InfoHash, drop, func(p string) bool { return !s.libraryFile(ctx, p) }); err != nil {
			return false, err
		}
	}
	rest := slices.DeleteFunc(slices.Clone(a.Meta.Seasons), func(n int) bool { return n == season })
	list, _ := json.Marshal(rest)
	q := `UPDATE acquisitions SET meta_json=json_set(COALESCE(NULLIF(meta_json,''),'{}'), '$.seasons', json(?))`
	if len(rest) > 0 && a.Season == season {
		q += `, season=` + strconv.Itoa(rest[0]) // the first season still asked for
	}
	if _, err := s.o.DB.ExecContext(ctx, q+` WHERE infohash=?`, string(list), a.InfoHash); err != nil {
		return false, err
	}
	a.Meta.Seasons = rest
	if !a.OrganizedAt.Valid {
		left := false
		for _, n := range rest {
			left = left || !slices.Contains(a.Meta.Organized, n)
		}
		if !left {
			s.packPlaced(ctx, a)
		}
	}
	s.log.Info("season taken out of a download of several seasons", "infohash", a.InfoHash, "season", season, "files", len(drop))
	return true, nil
}

// seriesLocks serialise the season requests of one series (by TMDB id)
// from the release's add to its acquisition row: two seasons of one new
// pack asked at once would both add it, and the engine keeps the first
// selection only.
type seriesLocks struct {
	mu sync.Mutex
	m  map[int]*seriesLock
}

type seriesLock struct {
	mu   sync.Mutex
	refs int
}

// lock takes the series' lock and returns its release.
func (l *seriesLocks) lock(tmdbID int) (unlock func()) {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[int]*seriesLock{}
	}
	sl := l.m[tmdbID]
	if sl == nil {
		sl = &seriesLock{}
		l.m[tmdbID] = sl
	}
	sl.refs++
	l.mu.Unlock()
	sl.mu.Lock()
	return func() {
		sl.mu.Unlock()
		l.mu.Lock()
		if sl.refs--; sl.refs == 0 {
			delete(l.m, tmdbID)
		}
		l.mu.Unlock()
	}
}

// libraryFile reports whether a library row names path (true when the
// library cannot be asked: keep the data).
func (s *Service) libraryFile(ctx context.Context, path string) bool {
	if s.o.Library == nil {
		return true
	}
	ok, err := s.o.Library.FileExists(ctx, path)
	return ok || err != nil
}

// selectionShrinker is implemented by *torrent.Engine: RemoveFiles shrinks
// a torrent's file selection.
type selectionShrinker interface {
	RemoveFiles(ctx context.Context, infohash string, files []int, mayEmpty func(path string) bool) error
}

var _ selectionShrinker = (*torrent.Engine)(nil)
