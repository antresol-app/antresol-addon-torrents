// Package pinned keeps the release a household follows for a tracked title
// (a pin): its file list, a refresh of the tracker's topic (a re-upload with
// new episodes replaces the old torrent in the engine, data kept) and the
// tracking scanner of kind "pinned".
package pinned

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/extaudio"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/subtitles"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// Pin is the release pinned for a title (one per title, household-wide).
type Pin struct {
	ID, TitleID int64
	// SourceID is the source (indexer.Source.ID as text) whose Link is
	// refreshed; "" for a magnet or an own .torrent file (never refreshed).
	SourceID                                          string
	Link, DetailsURL, ReleaseTitle, InfoHash, Quality string
	Files                                             []torrent.FileEntry
	PinnedBy                                          int64  // 0 = unknown (profile gone)
	PinnedByUID                                       string // its uid ("" = unknown)
	PinnedAt, LastChecked                             time.Time
	LastError                                         string // why the last refresh kept the pin ("" = fine)
	Suggestion                                        *Suggestion
}

// Suggestion is a newer release of the title found elsewhere: offered to
// the household, applied only by AcceptSuggestion.
type Suggestion struct {
	Link, SourceID, ReleaseTitle, InfoHash string
	Episodes                               int
	Found                                  time.Time
}

// Indexer is what a pin needs of the sources (*indexer.Manager through
// ManagerIndexer).
type Indexer interface {
	// Sources are the torrent sources a pasted topic link may belong to.
	Sources(ctx context.Context) ([]indexer.Source, error)
	// CheckReleaseLink: nil when link is on a site of the source.
	CheckReleaseLink(ctx context.Context, sourceID int64, link string) error
	Download(ctx context.Context, sourceID int64, link string) (cardigann.DownloadResult, error)
}

// Engine is what a pin needs of the torrent engine (*torrent.Engine).
type Engine interface {
	Get(infohash string) (torrent.Handle, bool)
	FetchInfo(ctx context.Context, magnet string) ([]byte, error)
	Replace(ctx context.Context, oldIH string, metafile []byte, keepSelected func(path string) bool) (torrent.Handle, error)
}

// Service pins releases and refreshes them.
type Service struct {
	DB       *sql.DB
	Tracking Tracking
	Indexer  Indexer
	Engine   Engine
	Now      func() time.Time
	Log      *slog.Logger
	// Library (optional) tells the core's library files: an old file of a
	// replaced release the library names is never deleted (stale.go);
	// without it none is.
	Library Library
	// Replaced (optional) is told of each torrent the engine swapped for a
	// pin's new release (acquire.Service.Rekey: its download record moves
	// along); an error is logged.
	Replaced func(ctx context.Context, oldIH, newIH string) error
	// Imported (optional) tells whether the download ih was put into the
	// library, and when (acquire.Service.Imported); without it the old
	// files of a renamed re-upload are never deleted (stale.go).
	Imported func(ctx context.Context, ih string) (at time.Time, ok bool, err error)
	// AfterScan (optional) runs after each pinned scan of a pinned title
	// (Actions.Sync); an error is logged.
	AfterScan func(ctx context.Context, t tracking.Title) error
}

// User-facing errors (catalog keys).
const (
	msgNoSource     = "Этот трекер не подключён в «Источниках»"
	msgNotTorrent   = "Трекер прислал не торрент-файл — возможно, нужно снова войти на сайт"
	msgFewer        = "В обновлённой раздаче нет части серий — оставлена прежняя"
	msgFilesGone    = "В обновлённой раздаче нет части файлов — оставлена прежняя"
	msgNoSuggestion = "Новой раздачи не предложено"
	msgNotPinned    = "Раздача не закреплена"
	msgNoInput      = "Укажите magnet-ссылку, ссылку на раздачу или торрент-файл"
	msgNoTitle      = "Этот фильм или сериал не отслеживается"
)

var (
	// ErrNoSource: a pasted topic link is on no connected tracker.
	ErrNoSource = i18n.Errorf(msgNoSource)
	// ErrNotPinned: the title has no pin.
	ErrNotPinned = i18n.Errorf(msgNotPinned)
)

// fetchTimeout bounds a magnet's metadata fetch.
const fetchTimeout = 2 * time.Minute

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// got is a release read from a link: its metafile (nil when only the engine
// has it) and what it lists.
type got struct {
	metafile []byte
	ih, name string
	files    []torrent.FileEntry
}

func parseMetafile(mf []byte) (got, error) {
	ih, name, files, err := torrent.ParseFiles(mf)
	if err != nil {
		return got{}, i18n.Errorf(msgNotTorrent)
	}
	return got{metafile: mf, ih: strings.ToLower(ih), name: name, files: files}, nil
}

func magnetIH(magnet string) (string, error) {
	m, err := metainfo.ParseMagnetUri(magnet)
	if err != nil {
		return "", err
	}
	return strings.ToLower(m.InfoHash.HexString()), nil
}

func isMagnet(s string) bool { return strings.HasPrefix(strings.ToLower(s), "magnet:") }

// fromMagnet fetches a magnet's metafile from the swarm; a torrent the
// engine has already is read from the engine (no metafile then).
func (s *Service) fromMagnet(ctx context.Context, magnet string) (got, error) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	mf, err := s.Engine.FetchInfo(fctx, magnet)
	if errors.Is(err, torrent.ErrKnown) {
		ih, perr := magnetIH(magnet)
		if perr != nil {
			return got{}, perr
		}
		h, ok := s.Engine.Get(ih)
		if !ok || h == nil {
			return got{}, err
		}
		g := got{ih: ih}
		for _, f := range h.Files() {
			g.files = append(g.files, torrent.FileEntry{Index: f.Index, Path: f.Path, Size: f.Size})
			if g.name == "" {
				g.name, _, _ = strings.Cut(f.Path, "/")
			}
		}
		return g, nil
	}
	if err != nil {
		return got{}, err
	}
	return parseMetafile(mf)
}

// fetch reads the release behind a source's link (or a magnet).
func (s *Service) fetch(ctx context.Context, sourceID, link string) (got, error) {
	if isMagnet(link) {
		return s.fromMagnet(ctx, link)
	}
	id, err := strconv.ParseInt(sourceID, 10, 64)
	if err != nil {
		return got{}, fmt.Errorf("pinned: source %q: %w", sourceID, err)
	}
	res, err := s.Indexer.Download(ctx, id, link)
	if err != nil {
		return got{}, err
	}
	if len(res.Torrent) > 0 {
		return parseMetafile(res.Torrent)
	}
	if res.Magnet != "" {
		return s.fromMagnet(ctx, res.Magnet)
	}
	return got{}, i18n.Errorf(msgNotTorrent)
}

func qualityOf(names ...string) string {
	for _, n := range names {
		if r := release.Parse(n).Resolution; r > 0 {
			return strconv.Itoa(r) + "p"
		}
	}
	return ""
}

func (s *Service) title(ctx context.Context, titleID int64) error {
	_, ok, err := s.Tracking.TitleByID(ctx, titleID)
	if err != nil {
		return err
	}
	if !ok {
		return i18n.Errorf(msgNoTitle)
	}
	return nil
}

// pin stores g as the title's pin, replacing any other (the household has
// one pin per title, whoever pinned it).
func (s *Service) pin(ctx context.Context, titleID int64, sourceID, link, details, releaseTitle string, g got, by addonkit.User) (Pin, error) {
	if releaseTitle == "" {
		releaseTitle = g.name
	}
	now := s.now().UTC().Truncate(time.Second)
	p := Pin{TitleID: titleID, SourceID: sourceID, Link: link, DetailsURL: details, ReleaseTitle: releaseTitle,
		InfoHash: g.ih, Quality: qualityOf(releaseTitle, g.name), Files: g.files, PinnedAt: now, LastChecked: now}
	fj, err := json.Marshal(nonNil(p.Files))
	if err != nil {
		return Pin{}, err
	}
	// by its uid: without one the pin names nobody
	var pinnedBy, pinnedByUID any
	if by.ID > 0 && by.UID != "" {
		pinnedBy, pinnedByUID = by.ID, by.UID
	}
	if _, err := s.DB.ExecContext(ctx,
		`INSERT INTO pinned_releases(title_id, source_id, link, details_url, release_title, infohash, quality, files_json, pinned_by, pinned_by_uid, pinned_at, last_checked, last_error, suggestion_json)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'','')
		 ON CONFLICT(title_id) DO UPDATE SET source_id=excluded.source_id, link=excluded.link, details_url=excluded.details_url,
		   release_title=excluded.release_title, infohash=excluded.infohash, quality=excluded.quality, files_json=excluded.files_json,
		   pinned_by=excluded.pinned_by, pinned_by_uid=excluded.pinned_by_uid, pinned_at=excluded.pinned_at, last_checked=excluded.last_checked, last_error='', suggestion_json=''`,
		titleID, sourceID, link, details, releaseTitle, p.InfoHash, p.Quality, string(fj), pinnedBy, pinnedByUID,
		now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		return Pin{}, err
	}
	got, ok, err := s.ByTitle(ctx, titleID)
	if err != nil {
		return Pin{}, err
	}
	if !ok {
		return Pin{}, sql.ErrNoRows
	}
	return got, nil
}

func nonNil(f []torrent.FileEntry) []torrent.FileEntry {
	if f == nil {
		return []torrent.FileEntry{}
	}
	return f
}

// PinRelease pins a search result of a source: link is its download link
// (a .torrent link or a magnet), details its topic page.
func (s *Service) PinRelease(ctx context.Context, titleID int64, sourceID, link, details, releaseTitle string, by addonkit.User) (Pin, error) {
	if err := s.title(ctx, titleID); err != nil {
		return Pin{}, err
	}
	link = strings.TrimSpace(link)
	if isMagnet(link) {
		sourceID = "" // a magnet is never refreshed: nothing to ask the source for
	} else {
		// Download would fetch any link with the source's session: a link
		// from a profile must be on the source's site
		id, err := strconv.ParseInt(sourceID, 10, 64)
		if err != nil {
			return Pin{}, fmt.Errorf("pinned: source %q: %w", sourceID, err)
		}
		if err := s.Indexer.CheckReleaseLink(ctx, id, link); err != nil {
			return Pin{}, err
		}
	}
	g, err := s.fetch(ctx, sourceID, link)
	if err != nil {
		return Pin{}, err
	}
	return s.pin(ctx, titleID, sourceID, link, details, releaseTitle, g, by)
}

// PinOwn pins a release the household brings: a magnet (its metafile from
// the swarm), a .torrent file (as is; input ignored) or a topic URL of a
// connected tracker (the source whose site the host is downloads it;
// ErrNoSource when none is).
func (s *Service) PinOwn(ctx context.Context, titleID int64, input string, torrentFile []byte, by addonkit.User) (Pin, error) {
	if err := s.title(ctx, titleID); err != nil {
		return Pin{}, err
	}
	input = strings.TrimSpace(input)
	switch {
	case len(torrentFile) > 0:
		g, err := parseMetafile(torrentFile)
		if err != nil {
			return Pin{}, err
		}
		return s.pin(ctx, titleID, "", "", "", "", g, by)
	case isMagnet(input):
		g, err := s.fromMagnet(ctx, input)
		if err != nil {
			return Pin{}, err
		}
		return s.pin(ctx, titleID, "", input, "", "", g, by)
	case input == "":
		return Pin{}, i18n.Errorf(msgNoInput)
	}
	srcs, err := s.Indexer.Sources(ctx)
	if err != nil {
		return Pin{}, err
	}
	var lastErr error
	for _, src := range srcs {
		if !src.Enabled || s.Indexer.CheckReleaseLink(ctx, src.ID, input) != nil {
			continue
		}
		id := strconv.FormatInt(src.ID, 10)
		g, err := s.fetch(ctx, id, input)
		if err != nil {
			lastErr = err
			continue
		}
		return s.pin(ctx, titleID, id, input, input, "", g, by)
	}
	if lastErr != nil {
		return Pin{}, lastErr
	}
	return Pin{}, ErrNoSource
}

// Unpin drops the title's pin (nothing in the engine changes).
func (s *Service) Unpin(ctx context.Context, titleID int64) error {
	_, err := s.DB.ExecContext(ctx, `DELETE FROM pinned_releases WHERE title_id=?`, titleID)
	return err
}

const pinCols = `id, title_id, source_id, link, details_url, release_title, infohash, quality, files_json, pinned_by, pinned_at, last_checked, last_error, suggestion_json, COALESCE(pinned_by_uid, '')`

func scanPin(r interface{ Scan(...any) error }) (Pin, error) {
	var p Pin
	var fj, sj, at string
	var by sql.NullInt64
	var lc sql.NullString
	if err := r.Scan(&p.ID, &p.TitleID, &p.SourceID, &p.Link, &p.DetailsURL, &p.ReleaseTitle, &p.InfoHash, &p.Quality,
		&fj, &by, &at, &lc, &p.LastError, &sj, &p.PinnedByUID); err != nil {
		return Pin{}, err
	}
	if err := json.Unmarshal([]byte(fj), &p.Files); err != nil {
		return Pin{}, fmt.Errorf("pinned: files of %d: %w", p.ID, err)
	}
	if sj != "" {
		var sg Suggestion
		if err := json.Unmarshal([]byte(sj), &sg); err == nil {
			p.Suggestion = &sg
		}
	}
	p.PinnedBy = by.Int64
	p.PinnedAt, _ = time.Parse(time.RFC3339, at)
	if lc.Valid {
		p.LastChecked, _ = time.Parse(time.RFC3339, lc.String)
	}
	return p, nil
}

// ByTitle returns the title's pin.
func (s *Service) ByTitle(ctx context.Context, titleID int64) (Pin, bool, error) {
	p, err := scanPin(s.DB.QueryRowContext(ctx, `SELECT `+pinCols+` FROM pinned_releases WHERE title_id=?`, titleID))
	if errors.Is(err, sql.ErrNoRows) {
		return Pin{}, false, nil
	}
	if err != nil {
		return Pin{}, false, err
	}
	return p, true, nil
}

// SetSuggestion offers a newer release for the title's pin (nil clears the
// offer).
func (s *Service) SetSuggestion(ctx context.Context, titleID int64, sg *Suggestion) error {
	sj := ""
	if sg != nil {
		b, err := json.Marshal(sg)
		if err != nil {
			return err
		}
		sj = string(b)
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE pinned_releases SET suggestion_json=? WHERE title_id=?`, sj, titleID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotPinned
	}
	return nil
}

// AcceptSuggestion makes the offered release the title's pin; the engine's
// torrent of the old pin, if any, is replaced keeping its data and its
// selection. The pin keeps who pinned it and when.
func (s *Service) AcceptSuggestion(ctx context.Context, titleID int64) (Pin, error) {
	p, ok, err := s.ByTitle(ctx, titleID)
	if err != nil {
		return Pin{}, err
	}
	if !ok {
		return Pin{}, ErrNotPinned
	}
	if p.Suggestion == nil {
		return Pin{}, i18n.Errorf(msgNoSuggestion)
	}
	sg := *p.Suggestion
	srcID := sg.SourceID
	if isMagnet(sg.Link) {
		srcID = ""
	}
	g, err := s.fetch(ctx, srcID, sg.Link)
	if err != nil {
		return Pin{}, err
	}
	// fetched again now: the release may have lost episodes since it was
	// suggested; refused as Refresh refuses it (the pin stays)
	if g.ih != p.InfoHash {
		if err := covers(g, p); err != nil {
			if _, uerr := s.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE pinned_releases SET last_error=? WHERE id=?`,
				i18n.MessageL(i18n.RU, err), p.ID); uerr != nil {
				return Pin{}, errors.Join(err, uerr)
			}
			return Pin{}, err
		}
	}
	replaced, old, err := s.replaceInEngine(ctx, p.InfoHash, g)
	if err != nil {
		return Pin{}, err
	}
	np := p
	np.SourceID, np.Link, np.DetailsURL, np.ReleaseTitle = srcID, sg.Link, "", sg.ReleaseTitle
	if np.ReleaseTitle == "" {
		np.ReleaseTitle = g.name
	}
	np.Quality = qualityOf(np.ReleaseTitle, g.name)
	np.InfoHash, np.Files, np.Suggestion, np.LastError = g.ih, g.files, nil, ""
	np.LastChecked = s.now().UTC().Truncate(time.Second)
	ok, err = s.update(ctx, np, p.InfoHash)
	s.checkUpdated(replaced, ok, err, p, g.ih)
	if err != nil {
		return Pin{}, err
	}
	if ok {
		s.keepStale(ctx, p.ID, old, g)
	}
	out, _, err := s.ByTitle(ctx, titleID)
	return out, err
}

// keepStale records old (the files a replaced download left behind) for
// the pin; a failure is logged (the files then stay on disk).
func (s *Service) keepStale(ctx context.Context, pinID int64, old *stale, g got) {
	if err := s.recordStale(context.WithoutCancel(ctx), pinID, old, g); err != nil {
		s.log().Error("pinned: the old files of a replaced release are not recorded: they stay on disk", "pin", pinID, "err", err)
	} else if old != nil {
		s.log().Info("pinned: old files of a replaced release wait for the new one's import", "pin", pinID, "files", len(old.Paths), "root", old.Root)
	}
}

// update writes p's release over the row still holding oldIH; false when
// the row changed meanwhile (left as it is).
func (s *Service) update(ctx context.Context, p Pin, oldIH string) (bool, error) {
	fj, err := json.Marshal(nonNil(p.Files))
	if err != nil {
		return false, err
	}
	sj := ""
	if p.Suggestion != nil {
		b, _ := json.Marshal(p.Suggestion)
		sj = string(b)
	}
	res, err := s.DB.ExecContext(ctx,
		`UPDATE pinned_releases SET source_id=?, link=?, details_url=?, release_title=?, infohash=?, quality=?, files_json=?,
		   last_checked=?, last_error=?, suggestion_json=? WHERE id=? AND infohash=?`,
		p.SourceID, p.Link, p.DetailsURL, p.ReleaseTitle, p.InfoHash, p.Quality, string(fj),
		p.LastChecked.UTC().Format(time.RFC3339), p.LastError, sj, p.ID, oldIH)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// checkUpdated logs loudly a pin row left on the old release after the
// engine was moved to the new one (the row changed meanwhile, or the write
// failed): the pin and the engine disagree until it is pinned again.
func (s *Service) checkUpdated(replaced, ok bool, err error, p Pin, newIH string) {
	if replaced && (err != nil || !ok) {
		s.log().Error("pinned: the engine has the new release but the pin row was not updated",
			"title", p.TitleID, "pin", p.ID, "old", p.InfoHash, "new", newIH, "err", err)
	}
}

// replaceInEngine swaps the engine's torrent oldIH (when it has one) for
// g's release, keeping the old selection only (new episode files are not
// selected here). Replace runs on a ctx that does not end with the
// caller's: its check reads the whole release and an ended ctx rolls it
// back.
// replaced: the engine's torrent was swapped; old: the files of a
// download g no longer lists (stale.go; nil: none).
func (s *Service) replaceInEngine(ctx context.Context, oldIH string, g got) (replaced bool, old *stale, err error) {
	if g.ih == oldIH || s.Engine == nil {
		return false, nil, nil
	}
	h, ok := s.Engine.Get(oldIH)
	if !ok {
		return false, nil, nil
	}
	if g.metafile == nil {
		return false, nil, fmt.Errorf("pinned: %s is in the engine already", g.ih)
	}
	old = staleOf(h, g)
	bg := context.WithoutCancel(ctx)
	if _, err := s.Engine.Replace(bg, oldIH, g.metafile, nil); err != nil {
		return false, nil, err
	}
	if s.Replaced != nil {
		if err := s.Replaced(bg, oldIH, g.ih); err != nil {
			s.log().Error("pinned: the download record of a replaced release stays on the old infohash",
				"old", oldIH, "new", g.ih, "err", err)
		}
	}
	if old != nil {
		old.Since = s.now().UTC().Truncate(time.Second).Format(time.RFC3339)
	}
	return true, old, nil
}

var subtitleExts = subtitles.Codecs

// mediaFile: a file that can belong to an episode (a video, an external
// audio track or a subtitle).
func mediaFile(p string) bool {
	ext := strings.ToLower(path.Ext(p))
	_, sub := subtitleExts[ext]
	return organize.IsVideo(p) || extaudio.IsAudio(p) || sub
}

// fileEpisodes are the episodes a file's own name names: its SxxEyy code
// (a range counts each episode), an anime-style « - 08», the season from
// organize.FileSeason or defaultSeason.
func fileEpisodes(p string, defaultSeason int) [][2]int {
	base := path.Base(p)
	info := release.Parse(base)
	first, last := info.Episode, info.EpisodeEnd
	if first == 0 {
		// the same numbers organize places the file by
		first, last = release.DashEpisodes(base)
	}
	if first <= 0 {
		return nil
	}
	if last < first {
		last = first
	}
	season := info.Season
	if season == 0 {
		season = organize.FileSeason(p)
	}
	if season == 0 {
		season = defaultSeason
	}
	var out [][2]int
	for e := first; e <= last && e-first < 500; e++ {
		out = append(out, [2]int{season, e})
	}
	return out
}

// EpisodesOf maps each episode a release holds to its files' indexes (the
// videos and the external audio and subtitle tracks, each by its own name)
// in index order; padding and other files are left out.
func EpisodesOf(files []torrent.FileEntry, defaultSeason int) map[[2]int][]int {
	out := map[[2]int][]int{}
	for _, f := range files {
		if f.Padding || !mediaFile(f.Path) {
			continue
		}
		for _, k := range fileEpisodes(f.Path, defaultSeason) {
			out[k] = append(out[k], f.Index)
		}
	}
	for k := range out {
		slices.Sort(out[k])
	}
	return out
}
