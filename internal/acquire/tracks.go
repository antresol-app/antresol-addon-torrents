package acquire

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// The music import's hand-off (musicimport.Queue): some songs of one
// album, downloaded with the track selection of «Выбрать релиз…» into a new
// album folder or into the folder of the album the library has already.

// planReleases is how many releases PlanTracks opens at most.
const planReleases = 3

// WantedTrack is a song to download: Key names it in the result (the
// import's track id), Disc and No (0: unknown) break ties between files of
// the same title.
type WantedTrack struct {
	Key, Title string
	Disc, No   int
	DurationMs int64
}

// TrackAcquireRequest asks for some songs of an album (ExtID: its
// MusicBrainz release group). AlbumID is the library album they are added
// to; 0 = a new album.
type TrackAcquireRequest struct {
	ExtID, Title, Artist string
	Year                 int
	Aliases              []string
	Tracks               []WantedTrack
	AlbumID              int64
	UserID               int64
	UserUID              string // UserID's uid ("" = the one the core lists now)
	// IgnoreDownload: a running download of the album the import gave up
	// waiting for (paused, failed, stalled): it no longer holds the album.
	IgnoreDownload string
	// AnyFormat: «Скачать в другом формате» (AcquireRequest.AnyFormat).
	AnyFormat bool
	// Exclude: infohashes of releases the import gave up on (their swarm
	// sent nothing): never planned again. Avoid: releases given up for
	// being too slow: planned only when no other release holds the songs.
	Exclude, Avoid []string
}

// TrackPlan is the release chosen for a TrackAcquireRequest and its files:
// Matched maps a wanted key to its file index, Missing lists the keys no
// file holds, Bytes is the size of Files.
type TrackPlan struct {
	SourceID     int64
	ReleaseLink  string
	ReleaseTitle string
	Files        []int
	Matched      map[string]int
	Missing      []string
	Bytes        int64
}

// TrackAcquireResult is a started download of a plan.
type TrackAcquireResult struct {
	InfoHash string
	Files    map[string]int // key → file index
	Missing  []string
	Bytes    int64
}

// TrackDownload is how far a download of AcquireTracks is: Known (an
// acquisition row), Present (the engine has it), Organized, or the last
// OrganizeError and whether the automatic retries are over (Exhausted).
// Size and Done cover the selected files.
type TrackDownload struct {
	// Status is the engine's (downloading, paused, error…), Name the
	// download as «Загрузки» lists it ("" when the engine has it not).
	Status, Name   string
	Known, Present bool
	Organized      bool
	OrganizeError  string
	Exhausted      bool
	Size, Done     int64
}

// msgInProgress: PlanTracks or AcquireTracks of an album that downloads.
const msgInProgress = "Альбом уже скачивается"

// PlanTracks searches the sources for the album and opens the file lists
// of the best planReleases torrent releases (direct downloads and torrents
// the engine has are skipped): the one whose files hold the most wanted
// songs wins, the better scored on a tie. Only releases of «Формат
// загрузки» count (unless AnyFormat): by the name, else by the file list;
// when only other formats hold the songs (and every file list opened
// loaded), ErrNoFormat. Releases of req.Exclude are skipped, those of
// req.Avoid taken only when no other release holds the songs. When none
// holds any: ErrConflict
// msgReleaseAdded if a torrent in «Загрузки» holds them (its selection
// cannot grow), else ErrNoRelease «Подходящих вариантов не нашлось» (with
// ErrSourcesFailed when a source or a file list did not answer: worth
// trying again later). An album downloading already (InProgress) is
// ErrInProgress, before any search.
func (s *Service) PlanTracks(ctx context.Context, req TrackAcquireRequest) (TrackPlan, error) {
	switch _, ok, err := s.inProgress(ctx, req.ExtID, req.AlbumID, req.IgnoreDownload); {
	case err != nil:
		return TrackPlan{}, err
	case ok:
		return TrackPlan{}, uerr(ErrInProgress, msgInProgress)
	}
	resp, err := s.Releases(ctx, ReleaseRequest{Kind: KindMusic, ExtID: req.ExtID, Title: req.Title, Artist: req.Artist,
		Year: req.Year, Aliases: req.Aliases})
	if err != nil {
		return TrackPlan{}, err
	}
	unanswered := len(resp.Failed) > 0
	listFailed := false // a file list did not load: what it holds is unknown
	inEngine := false   // a release in «Загрузки» holds wanted songs
	releases, otherFormat := formatReleases(resp, req.AnyFormat)
	var best, fallback TrackPlan // fallback: of a release to avoid
	opened := 0
	in := func(list []string, ih string) bool {
		return ih != "" && slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(x, ih) })
	}
	// consider opens a release's file list and keeps the best plan
	// (avoided releases' apart)
	consider := func(r indexer.ScoredRelease) error {
		link := r.Link
		if link == "" || strings.HasPrefix(strings.ToLower(link), "magnet:") {
			link = r.Magnet
			if link == "" {
				link = r.Link
			}
		}
		if r.Direct || strings.TrimSpace(link) == "" {
			return nil
		}
		opened++
		rf, err := s.ReleaseFiles(ctx, ReleaseFilesRequest{SourceID: r.SourceID, ReleaseLink: link, Title: req.Title,
			Artist: req.Artist, Year: req.Year, Aliases: req.Aliases})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.log.Warn("import: release file list", "release", r.Title, "err", err)
			unanswered, listFailed = true, true
			return nil
		}
		if in(req.Exclude, rf.InfoHash) {
			return nil
		}
		if rf.Added || rf.InDownload {
			if s.addedHolds(rf.InfoHash, req) {
				inEngine = true
			}
			return nil
		}
		idx, matched, missing := matchTrackFiles(rf.Files, req.Tracks, req.AlbumID == 0)
		if len(matched) > 0 && !req.AnyFormat && filesMismatch(r.Title, rf.Files, resp.Prefs) {
			otherFormat = true // the name told nothing, the files do
			return nil
		}
		into := &best
		if in(req.Avoid, rf.InfoHash) {
			into = &fallback
		}
		if len(matched) > len(into.Matched) {
			*into = TrackPlan{SourceID: r.SourceID, ReleaseLink: link, ReleaseTitle: r.Title, Files: idx, Matched: matched,
				Missing: missing, Bytes: filesBytes(rf.Files, idx)}
		}
		return nil
	}
	var avoided []indexer.ScoredRelease
	for _, r := range releases {
		if opened == planReleases {
			break
		}
		switch {
		case in(req.Exclude, r.InfoHash):
		case in(req.Avoid, r.InfoHash):
			avoided = append(avoided, r) // only when nothing else holds the songs
		default:
			if err := consider(r); err != nil {
				return TrackPlan{}, err
			}
		}
	}
	for _, r := range avoided {
		if len(best.Matched) > 0 || len(fallback.Matched) > 0 || opened == planReleases {
			break
		}
		if err := consider(r); err != nil {
			return TrackPlan{}, err
		}
	}
	if len(best.Matched) == 0 {
		best = fallback
	}
	if len(best.Matched) == 0 && inEngine {
		// its files cannot be added to the running torrent's selection
		return TrackPlan{}, uerr(ErrConflict, msgReleaseAdded)
	}
	if len(best.Matched) == 0 && otherFormat && !listFailed {
		// a release of the format whose file list did not load may hold
		// them: that is a source that did not answer, not «no format»
		return TrackPlan{}, noFormatErr(resp.Prefs)
	}
	if len(best.Matched) == 0 {
		if unanswered {
			return TrackPlan{}, wrapErr(ErrNoRelease, msgNoMatchSourcesFailed, ErrSourcesFailed)
		}
		return TrackPlan{}, uerr(ErrNoRelease, msgNoMatch)
	}
	return best, nil
}

// addedHolds: the torrent ih the engine has holds a file of a wanted song
// (an album downloaded before, now asked for a song it left out).
func (s *Service) addedHolds(ih string, req TrackAcquireRequest) bool {
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return false
	}
	var entries []torrent.FileEntry
	for _, f := range h.Files() {
		entries = append(entries, torrent.FileEntry{Index: f.Index, Path: f.Path, Size: f.Size})
	}
	files := releaseFileList(entries, ReleaseFilesRequest{Title: req.Title, Artist: req.Artist, Year: req.Year, Aliases: req.Aliases})
	_, matched, _ := matchTrackFiles(files, req.Tracks, req.AlbumID == 0)
	return len(matched) > 0
}

func filesBytes(files []ReleaseFile, idx []int) int64 {
	in := map[int]bool{}
	for _, i := range idx {
		in[i] = true
	}
	var n int64
	for _, f := range files {
		if in[f.Index] {
			n += f.Size
		}
	}
	return n
}

// AcquireTracks downloads a plan: the files of its release, as «Выбрать
// релиз…» does (never forced: a full disk holds it). With AlbumID the
// tracks go into that library album's folder.
func (s *Service) AcquireTracks(ctx context.Context, req TrackAcquireRequest, p TrackPlan) (TrackAcquireResult, error) {
	ar := AcquireRequest{Kind: KindMusic, Mode: ModeDownload, ExtID: req.ExtID, Title: req.Title, Artist: req.Artist, Year: req.Year,
		Aliases: req.Aliases, ReleaseLink: p.ReleaseLink, SourceID: p.SourceID, ReleaseTitle: p.ReleaseTitle, Files: p.Files,
		UserID: req.UserID, UserUID: req.UserUID, auto: true, ignoreRunning: req.IgnoreDownload}
	if req.AlbumID != 0 {
		ad := s.o.Library
		if ad == nil {
			return TrackAcquireResult{}, uerr(ErrUnavailable, "Медиатека недоступна")
		}
		dir, err := ad.AlbumDir(ctx, req.AlbumID)
		switch {
		case errors.Is(err, library.ErrNotFound):
			// the album is gone meanwhile: a new one
		case err != nil:
			return TrackAcquireResult{}, err
		default:
			ar.albumDir = dir
		}
	}
	res, err := s.Acquire(ctx, ar)
	if err != nil {
		return TrackAcquireResult{}, err
	}
	if res.Existing {
		// it began downloading meanwhile: its files are not this plan's
		return TrackAcquireResult{}, uerr(ErrInProgress, msgInProgress)
	}
	return TrackAcquireResult{InfoHash: res.InfoHash, Files: p.Matched, Missing: p.Missing, Bytes: p.Bytes}, nil
}

// InProgress returns a music download the engine has that is not
// organized yet (nor given up on) of the release group extID or into the
// library album albumID (0: none) — the import's, of any job, or one of
// «На сервер» — other than except ("": none). ok false: none.
func (s *Service) InProgress(ctx context.Context, extID string, albumID int64, except string) (ih string, ok bool, err error) {
	return s.inProgress(ctx, extID, albumID, except)
}

func (s *Service) inProgress(ctx context.Context, extID string, albumID int64, except string) (string, bool, error) {
	dir := ""
	if albumID != 0 {
		if ad := s.o.Library; ad != nil {
			switch d, err := ad.AlbumDir(ctx, albumID); {
			case errors.Is(err, library.ErrNotFound):
			case err != nil:
				return "", false, err
			default:
				dir = d
			}
		}
	}
	return s.runningMusic(ctx, extID, dir, except)
}

// runningMusic is InProgress by the album's library folder (albumDir, ""
// = none), the download except left out.
func (s *Service) runningMusic(ctx context.Context, extID, albumDir, except string) (string, bool, error) {
	extID = strings.TrimSpace(extID)
	if extID == "" && albumDir == "" {
		return "", false, nil
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT infohash FROM acquisitions WHERE kind = ?1 AND organized_at IS NULL
		AND NOT (organize_error != '' AND organize_attempts >= ?2)
		AND (?3 != '' AND ext_id = ?3 OR ?4 != '' AND json_extract(meta_json, '$.albumDir') = ?4) ORDER BY rowid`,
		KindMusic, maxOrganizeAttempts, extID, albumDir)
	if err != nil {
		return "", false, err
	}
	var ihs []string
	for rows.Next() {
		var ih string
		if err := rows.Scan(&ih); err != nil {
			rows.Close()
			return "", false, err
		}
		ihs = append(ihs, ih)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", false, err
	}
	for _, ih := range ihs {
		if ih == except {
			continue
		}
		if _, ok := s.o.Engine.Get(ih); ok {
			return ih, true, nil
		}
	}
	return "", false, nil
}

// TrackDownload reports a download of AcquireTracks (any acquisition).
func (s *Service) TrackDownload(ctx context.Context, ih string) (TrackDownload, error) {
	var out TrackDownload
	a, err := s.acquisition(ctx, ih)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return out, err
	default:
		out.Known = true
		out.Organized = a.OrganizedAt.Valid
		out.OrganizeError = a.Error
		out.Exhausted = a.Attempts >= maxOrganizeAttempts
	}
	if h, ok := s.o.Engine.Get(ih); ok {
		st := h.Status()
		out.Present, out.Size, out.Done, out.Status, out.Name = true, st.Size, st.Done, st.Status, st.Name
		if out.Name == "" {
			out.Name = a.Title
		}
	}
	return out, nil
}

// matchTrackFiles picks the files of a release preview that hold the
// wanted songs: a track file whose name (without its number and an
// «Artist - » prefix, editions dropped) is the song's title. Of several,
// the one with the song's disc and number wins, else one of the requested
// album (Checked), else the first. idx is sorted and also holds the
// requested album's extras (cover, booklet) when newAlbum.
func matchTrackFiles(files []ReleaseFile, wanted []WantedTrack, newAlbum bool) (idx []int, matched map[string]int, missing []string) {
	parsed := make([]organize.TrackFile, len(files))
	for i, f := range files {
		if f.Kind == fileKindTrack {
			parsed[i] = organize.ParseTrackFile(f.Path)
		}
	}
	allowed := allowedTracks(files, newAlbum)
	matched = map[string]int{}
	used := map[int]bool{}
	for _, w := range wanted {
		key := organize.SongKey(w.Title)
		best, bestRank := -1, -1
		for i, f := range files {
			if f.Kind != fileKindTrack || !allowed[i] || used[f.Index] || key == "" || !containsStr(parsed[i].Names, key) {
				continue
			}
			rank := 0
			if w.No > 0 && parsed[i].No == w.No && (w.Disc == 0 || parsed[i].Disc == w.Disc || parsed[i].Disc == 0 && w.Disc == 1) {
				rank += 2
			}
			if f.Checked {
				rank++
			}
			if rank > bestRank {
				best, bestRank = i, rank
			}
		}
		if best < 0 {
			missing = append(missing, w.Key)
			continue
		}
		used[files[best].Index] = true
		matched[w.Key] = files[best].Index
		idx = append(idx, files[best].Index)
	}
	if newAlbum && len(idx) > 0 {
		for _, f := range files {
			if f.Kind == fileKindExtra && f.Checked {
				idx = append(idx, f.Index)
			}
		}
	}
	sort.Ints(idx)
	return idx, matched, missing
}

// allowedTracks marks the track files a song may come from. A new album
// may take any (each album of the torrent gets its own folder). Into an
// album the library has, only the requested album's: with one album in
// the release that one, with several the one the preview checks (the
// requested root, see releaseFileList) — none when the preview checks
// them all (no album of the release is the requested one).
func allowedTracks(files []ReleaseFile, newAlbum bool) map[int]bool {
	out := map[int]bool{}
	roots := map[string]bool{}
	rootOf := make([]string, len(files))
	unchecked := false
	for i, f := range files {
		if f.Kind != fileKindTrack {
			continue
		}
		tf, ok := tfileOf(torrent.FileEntry{Index: f.Index, Path: f.Path, Size: f.Size})
		if !ok {
			continue
		}
		rootOf[i], _ = organize.AlbumRootOf(tf)
		roots[rootOf[i]] = true
		unchecked = unchecked || !f.Checked
	}
	for i, f := range files {
		if f.Kind == fileKindTrack && (newAlbum || len(roots) == 1 || unchecked && f.Checked) {
			out[i] = true
		}
	}
	return out
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
