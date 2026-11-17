package acquire

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/events"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/extaudio"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/subtitles"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

const msgVoiceOverOtherDownload = "Эта раздача уже в «Загрузках»"

// voiceOverMeta is acquisitionMeta.VoiceOver: the title and the release's
// files chosen for it, each with the library file its track goes beside.
type voiceOverMeta struct {
	ItemID int64  `json:"itemId"`
	Studio string `json:"studio,omitempty"`
	// OrigLang: the title's original language (TMDB, ISO 639-1; "" unknown),
	// the track the fit measures against.
	OrigLang string `json:"origLang,omitempty"`
	// Picks: the selected files by engine index.
	Picks map[int]voTarget `json:"picks"`
}

// PlacedVoiceOver is a track of «Найти озвучку…» about to be linked at
// Path beside the library file FileID (Options.VoiceOverPlaced).
type PlacedVoiceOver struct {
	Path   string
	FileID int64
	// OrigLang: voiceOverMeta.OrigLang.
	OrigLang string
}

// VoiceOverRequest asks for a candidate (VoiceOverCandidate.ID) of the
// item's last search. Force: admin — download even when the disk is full.
type VoiceOverRequest struct {
	ItemID int64  `json:"-"`
	ID     string `json:"id"`
	UserID int64  `json:"-"`
	// UserUID: UserID's uid ("" = the one the core lists now).
	UserUID string `json:"-"`
	Force   bool   `json:"force,omitempty"`
}

// The states of a voice-over's episode («Загрузки» of the sheet).
const (
	VoiceDownloading = "downloading" // its track is downloading
	VoicePlaced      = "placed"      // beside its video, being fitted
	VoiceFitted      = "fitted"      // fitted: plays (OffsetMs, Tempo)
	VoiceUnmeasured  = "unmeasured"  // its sync could not be checked: not offered
	VoiceMisfit      = "misfit"      // another version of the film: not offered
	VoiceSkipped     = "skipped"     // not placed (another length, the episode gone)
	VoiceHad         = "had"         // not placed: a track of its studio is there already
)

// voiceRank orders an episode's states, best last (its best track shows).
var voiceRank = map[string]int{VoiceSkipped: 0, VoiceHad: 0, VoiceMisfit: 1, VoicePlaced: 2, VoiceUnmeasured: 3, VoiceFitted: 4}

// VoiceOverEpisode is an episode (or the film: Season and Episode 0) a
// voice-over's track is for, and what became of it.
type VoiceOverEpisode struct {
	Season   int     `json:"season,omitempty"`
	Episode  int     `json:"episode,omitempty"`
	FileID   int64   `json:"fileId"`
	State    string  `json:"state,omitempty"`
	OffsetMs float64 `json:"offsetMs,omitempty"`
	Tempo    float64 `json:"tempo,omitempty"`
}

// VoiceOverPreview is a candidate's file list as coverage: the episodes
// it has tracks for.
type VoiceOverPreview struct {
	ID       string             `json:"id"`
	Coverage VoiceOverCoverage  `json:"coverage"`
	Episodes []VoiceOverEpisode `json:"episodes"`
	// Message: why nothing can be taken from it ("" when something can).
	Message string `json:"message,omitempty"`
}

// VoiceOverStarted is the answer of DownloadVoiceOver. Existing: the
// release was downloading already (its selection grown when needed).
type VoiceOverStarted struct {
	InfoHash string             `json:"infoHash"`
	Existing bool               `json:"existing,omitempty"`
	Episodes []VoiceOverEpisode `json:"episodes"`
}

// voPick is a file of a release chosen for a target.
type voPick struct {
	index  int
	path   string
	target voTarget
}

// audioEpisode is the season and episode of a release's audio file: the
// code in its name (SxxEyy), the season from the nearest folder naming one
// when the name has none; a bare number («01.ac3») only inside a folder
// naming the season. 0, 0: none.
func audioEpisode(p string) (int, int) {
	base := path.Base(p)
	info := release.Parse(base)
	// the nearest folder naming seasons: exactly one is the season; a range
	// or several («S01-S03», «1-3 сезон») name none
	folder := 0
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if lo, hi := organize.FolderSeasons(path.Base(d)); lo > 0 {
			if lo == hi {
				folder = lo
			}
			break
		}
	}
	season, ep := info.Season, info.Episode
	if ep > 0 {
		if season == 0 {
			season = folder
		}
		return season, ep
	}
	if folder > 0 {
		if n, ok := organize.LeadNumber(base); ok {
			return folder, n
		}
	}
	return 0, 0
}

// voicePicks chooses a release's audio files for the targets: for a series
// those whose episode code is an episode the server has; for a film those
// that name no episode (an mp3 or flac only when it names a studio: an
// album otherwise), checked against the film's length once downloaded.
// The release's videos and every other file stay out.
func voicePicks(entries []torrent.FileEntry, it voItem, targets []voTarget) ([]voPick, error) {
	byEp := map[[2]int]voTarget{}
	for _, t := range targets {
		if _, ok := byEp[[2]int{t.Season, t.Episode}]; !ok {
			byEp[[2]int{t.Season, t.Episode}] = t
		}
	}
	var picks []voPick
	audio := 0
	for _, f := range entries {
		if f.Padding || !extaudio.IsAudio(f.Path) {
			continue
		}
		if _, ok := tfileOf(f); !ok {
			continue
		}
		audio++
		season, ep := audioEpisode(f.Path)
		switch {
		case it.kind == KindSeries:
			if t, ok := byEp[[2]int{season, ep}]; ok && ep > 0 {
				picks = append(picks, voPick{index: f.Index, path: f.Path, target: t})
			}
		case len(targets) > 0 && ep == 0:
			if music := map[string]bool{".mp3": true, ".flac": true}[strings.ToLower(path.Ext(f.Path))]; music && len(release.Studios(path.Base(f.Path))) == 0 {
				continue
			}
			picks = append(picks, voPick{index: f.Index, path: f.Path, target: targets[0]})
		}
	}
	switch {
	case audio == 0:
		return nil, uerr(ErrNoFiles, msgVoiceOverNoAudio)
	case len(picks) > 0:
		return picks, nil
	case it.kind == KindSeries && !anyEpisode(entries):
		return nil, uerr(ErrNoFiles, msgVoiceOverNoFiles)
	}
	return nil, uerr(ErrNoFiles, msgVoiceOverNoMatch)
}

// anyEpisode: some audio file of the release has an episode.
func anyEpisode(entries []torrent.FileEntry) bool {
	for _, f := range entries {
		if extaudio.IsAudio(f.Path) {
			if _, ep := audioEpisode(f.Path); ep > 0 {
				return true
			}
		}
	}
	return false
}

func picksEpisodes(picks map[int]voTarget) []VoiceOverEpisode {
	seen := map[int64]bool{}
	var out []VoiceOverEpisode
	for _, t := range picks {
		if !seen[t.FileID] {
			seen[t.FileID] = true
			out = append(out, VoiceOverEpisode{Season: t.Season, Episode: t.Episode, FileID: t.FileID})
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Season != out[b].Season {
			return out[a].Season < out[b].Season
		}
		if out[a].Episode != out[b].Episode {
			return out[a].Episode < out[b].Episode
		}
		return out[a].FileID < out[b].FileID
	})
	if out == nil {
		out = []VoiceOverEpisode{}
	}
	return out
}

func pickMap(picks []voPick) map[int]voTarget {
	m := make(map[int]voTarget, len(picks))
	for _, p := range picks {
		m[p.index] = p.target
	}
	return m
}

// voRelease returns the candidate's release, its metafile (cached
// s.files, else from the source or the swarm: a file-list fetch, at most
// voMaxLists per search) and the infohash of the torrent the engine has
// of it ("" = none).
func (s *Service) voRelease(ctx context.Context, itemID int64, id string) (indexer.ScoredRelease, []byte, string, error) {
	r, err := s.voSearch.release(itemID, id, false)
	if err != nil {
		return r, nil, "", err
	}
	link := strings.TrimSpace(r.Link)
	if link == "" {
		link = strings.TrimSpace(r.Magnet)
	}
	key := cacheKey(r.SourceID, link)
	c, ok := s.files.get(key, time.Now())
	if !ok {
		if _, err := s.voSearch.release(itemID, id, true); err != nil {
			return r, nil, "", err
		}
		var added *ReleaseFiles
		c, added, err = s.releaseMetafile(ctx, r.SourceID, link, time.Now().Add(releaseFilesTimeout))
		if err != nil {
			return r, nil, "", err
		}
		if added != nil {
			return r, nil, added.InfoHash, nil
		}
		s.files.put(key, c, time.Now())
	}
	if _, ok := s.o.Engine.Get(c.infoHash); ok {
		return r, c.metafile, c.infoHash, nil
	}
	return r, c.metafile, "", nil
}

// voEntries is the file list of a release: its metafile's, else the
// engine's torrent's.
func (s *Service) voEntries(mf []byte, ih string) ([]torrent.FileEntry, error) {
	if len(mf) > 0 {
		_, _, entries, err := torrent.ParseFiles(mf)
		if err != nil {
			return nil, wrapErr(ErrInvalid, msgNoFileList, err)
		}
		return entries, nil
	}
	if h, ok := s.o.Engine.Get(ih); ok {
		if files := h.Files(); len(files) > 0 {
			return entriesOf(files), nil
		}
	}
	return nil, uerr(ErrNoFiles, msgFileListFailed)
}

// PreviewVoiceOver fetches a candidate's file list (one of voMaxLists per
// search; a second look reads the cached list) and says which episodes of
// the server it has tracks for.
func (s *Service) PreviewVoiceOver(ctx context.Context, itemID int64, id string) (VoiceOverPreview, error) {
	it, err := s.voItem(ctx, itemID)
	if err != nil {
		return VoiceOverPreview{}, err
	}
	targets, err := s.voTargets(ctx, it)
	if err != nil {
		return VoiceOverPreview{}, err
	}
	_, mf, ih, err := s.voRelease(ctx, itemID, id)
	if err != nil {
		return VoiceOverPreview{}, err
	}
	entries, err := s.voEntries(mf, ih)
	if err != nil {
		return VoiceOverPreview{}, err
	}
	out := VoiceOverPreview{ID: id, Episodes: []VoiceOverEpisode{}}
	out.Coverage = VoiceOverCoverage{Total: len(picksEpisodes(pickMap(targetPicks(targets)))), Known: true, Exact: true, Film: it.kind == KindMovie}
	picks, err := voicePicks(entries, it, targets)
	if err != nil {
		msg, ok := UserMessage(err)
		if !ok {
			return VoiceOverPreview{}, err
		}
		out.Message = msg
		return out, nil
	}
	out.Episodes = picksEpisodes(pickMap(picks))
	out.Coverage.Have = len(out.Episodes)
	return out, nil
}

// targetPicks lists the targets as picks (for counting them).
func targetPicks(ts []voTarget) []voPick {
	out := make([]voPick, len(ts))
	for i, t := range ts {
		out[i] = voPick{index: i, target: t}
	}
	return out
}

// DownloadVoiceOver downloads a candidate's audio files the library's
// episodes (or film) need (voicePicks) into the torrents folder; once
// complete they are linked beside their videos and fitted. The same
// release again is that download: its selection grows by the tracks of
// episodes that came since, nothing else is added.
func (s *Service) DownloadVoiceOver(ctx context.Context, req VoiceOverRequest) (VoiceOverStarted, error) {
	req.UserID, req.UserUID = s.asker(ctx, req.UserID, req.UserUID)
	it, err := s.voItem(ctx, req.ItemID)
	if err != nil {
		return VoiceOverStarted{}, err
	}
	targets, err := s.voTargets(ctx, it)
	if err != nil {
		return VoiceOverStarted{}, err
	}
	r, mf, ih, err := s.voRelease(ctx, req.ItemID, req.ID)
	if err != nil {
		return VoiceOverStarted{}, err
	}
	entries, err := s.voEntries(mf, ih)
	if err != nil {
		return VoiceOverStarted{}, err
	}
	picks, err := voicePicks(entries, it, targets)
	if err != nil {
		return VoiceOverStarted{}, err
	}
	studio := firstStudio(r.Title)
	if ih != "" {
		return s.growVoiceOver(ctx, ih, it, picks)
	}
	opt := torrent.AddOptions{Mode: ModeDownload, SaveDir: s.downloadDir(it.kind), AddedBy: req.UserID, AddedByUID: req.UserUID, SourceURL: r.Details, ItemID: &it.id}
	if rel, err := json.Marshal(r); err == nil {
		opt.Release = rel
	}
	if s.o.Disk != nil && s.o.Disk.Level() == LevelFull && !req.Force {
		opt.WaitingSpace = true
	}
	for _, p := range picks {
		opt.Files = append(opt.Files, p.index)
	}
	sort.Ints(opt.Files)
	h, err := s.o.Engine.AddTorrentFile(ctx, mf, opt)
	if err != nil {
		switch {
		case errors.Is(err, torrent.ErrPathInUse):
			return VoiceOverStarted{}, wrapErr(ErrInvalid, "Эти файлы уже скачиваются другой раздачей", err)
		case errors.Is(err, torrent.ErrStorageOffline):
			return VoiceOverStarted{}, wrapErr(ErrUnavailable, "Медиадиск недоступен: загрузки остановлены, пока его не подключат", err)
		}
		return VoiceOverStarted{}, wrapErr(ErrSource, "Не удалось открыть раздачу. Попробуйте другую", err)
	}
	a := acquisition{InfoHash: h.InfoHash(), Kind: KindVoiceOver, TMDBID: int(it.tmdbID), Title: it.title, Year: it.year,
		Meta: acquisitionMeta{Release: r.Title, VoiceOver: &voiceOverMeta{ItemID: it.id, Studio: studio, OrigLang: s.originalLanguage(ctx, it), Picks: pickMap(picks)}}}
	if err := s.saveAcquisition(ctx, a, req.UserID, req.UserUID, true); err != nil {
		return VoiceOverStarted{}, err
	}
	if req.Force {
		s.setForced(a.InfoHash)
	}
	s.checkDisk()
	return VoiceOverStarted{InfoHash: a.InfoHash, Episodes: picksEpisodes(a.Meta.VoiceOver.Picks)}, nil
}

// originalLanguage is the title's original language from TMDB ("" when
// it has no TMDB id, without a key, or TMDB does not answer).
func (s *Service) originalLanguage(ctx context.Context, it voItem) string {
	if it.tmdbID <= 0 || s.o.Metadata == nil {
		return ""
	}
	d, err := s.title(ctx, it.kind, int(it.tmdbID))
	if err != nil {
		s.log.Info("voice-over: the title's original language is unknown", "item", it.id, "err", err)
		return ""
	}
	return d.OriginalLanguage
}

// OriginalLanguageOf is the original language of the title of the library
// file fileID (a film, or an episode's series; TMDB): the fit asks again
// when the download stored none (TMDB did not answer then). "" unknown.
func (s *Service) OriginalLanguageOf(ctx context.Context, fileID int64) string {
	if s.o.Library == nil {
		return ""
	}
	li, ok, err := s.o.Library.ItemOfFile(ctx, fileID)
	if err != nil || !ok {
		return ""
	}
	// up from an episode through its season to the series
	for range 3 {
		if li.Kind == "movie" || li.Kind == "series" {
			if l := metaOriginalLanguage(li.MetaJSON); l != "" {
				return l
			}
			it, err := voItemOf(li)
			if err != nil {
				return ""
			}
			return s.originalLanguage(ctx, it)
		}
		if li.ParentID == 0 {
			return ""
		}
		if li, err = s.o.Library.Item(ctx, li.ParentID); err != nil {
			return ""
		}
	}
	return ""
}

// metaOriginalLanguage is the "original_language" of an item's meta ("":
// none).
func metaOriginalLanguage(meta string) string {
	var m struct {
		OriginalLanguage string `json:"original_language"`
	}
	if meta != "" {
		_ = json.Unmarshal([]byte(meta), &m) // a bad meta: none
	}
	return m.OriginalLanguage
}

// growVoiceOver is DownloadVoiceOver of a release the engine has: a
// voice-over download of the same title takes the picks it lacks
// (organized again once they complete); another download of it is a
// conflict.
func (s *Service) growVoiceOver(ctx context.Context, ih string, it voItem, picks []voPick) (VoiceOverStarted, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.acquisition(ctx, ih)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (a.Kind != KindVoiceOver || a.Meta.VoiceOver == nil || a.Meta.VoiceOver.ItemID != it.id) {
		return VoiceOverStarted{}, uerr(ErrConflict, msgVoiceOverOtherDownload)
	}
	if err != nil {
		return VoiceOverStarted{}, err
	}
	vo := a.Meta.VoiceOver
	var add []int
	for _, p := range picks {
		if _, ok := vo.Picks[p.index]; !ok {
			add = append(add, p.index)
			vo.Picks[p.index] = p.target
		}
	}
	out := VoiceOverStarted{InfoHash: ih, Existing: true}
	if len(add) > 0 {
		g, ok := s.o.Engine.(selectionGrower)
		if !ok {
			return VoiceOverStarted{}, uerr(ErrConflict, msgVoiceOverOtherDownload)
		}
		sort.Ints(add)
		if err := g.AddFiles(ctx, ih, add); err != nil {
			return VoiceOverStarted{}, growErr(err)
		}
		a.Meta.SelRev++
		meta, err := json.Marshal(a.Meta)
		if err != nil {
			return VoiceOverStarted{}, err
		}
		if _, err := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET meta_json=?, organized_at=NULL, organize_error='', organize_attempts=0 WHERE infohash=?`,
			string(meta), ih); err != nil {
			return VoiceOverStarted{}, err
		}
	}
	out.Episodes = picksEpisodes(vo.Picks)
	return out, nil
}

// voiceTrack describes a release's audio file (its in-torrent path p, on
// disk at full) as a track: the language and studio its name or folders
// state (else Russian and the release's studio).
func voiceTrack(p, full, studio string) extaudio.Track {
	t := extaudio.Track{Path: full}
	parts := strings.Split(p, "/")
	base := parts[len(parts)-1]
	rest := strings.TrimSuffix(base, path.Ext(base))
	t.Lang = subtitles.LangOf(rest + ".x")
	for i := len(parts) - 2; i >= 0 && t.Lang == ""; i-- {
		t.Lang = subtitles.LangOf("x." + strings.NewReplacer(".", " ").Replace(parts[i]) + ".x")
	}
	if t.Lang == "" {
		t.Lang = "ru"
	}
	for i := len(parts) - 1; i >= 0 && t.Studio == ""; i-- {
		t.Studio = firstStudio(parts[i])
	}
	if t.Studio == "" {
		t.Studio = studio
	}
	t.Title = t.Studio
	return t
}

// organizeVoiceOver links each complete track of a voice-over download
// beside the video it was chosen for, as placeAudio names a release's
// tracks (a track of another length than its video, or of a video the
// library no longer has, is left in the torrent), and records the paths.
func (s *Service) organizeVoiceOver(ctx context.Context, a acquisition, files []torrent.FileInfo) {
	vo := a.Meta.VoiceOver
	if vo == nil {
		return
	}
	var probe extaudio.Prober
	if s.o.FFprobe != "" {
		probe = organize.MediaProber(s.o.FFprobe)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	used := map[string]int{} // stem + tag → tracks placed in this run
	paths := []string{}
	for _, f := range files {
		if ctx.Err() != nil {
			return
		}
		t, ok := vo.Picks[f.Index]
		if !ok || f.FullPath == "" {
			continue
		}
		if st, err := os.Stat(t.Path); err != nil || !st.Mode().IsRegular() {
			continue // the episode is gone (or the disk is away)
		}
		tr := voiceTrack(f.Path, f.FullPath, vo.Studio)
		if probe != nil {
			videoMs := t.DurationMs
			if videoMs <= 0 {
				if p, err := probe(ctx, t.Path); err == nil {
					videoMs = p.DurationMs
				}
			}
			if _, ok := extaudio.Describe(ctx, tr, probe, videoMs); !ok {
				s.log.Info("voice-over track not placed: unreadable or another length", "track", f.Path, "video", t.Path)
				continue
			}
		}
		// a track of its language and studio beside the video already (any
		// source; not a misfit): the same file reused (a release added
		// again), another one kept alone
		size := f.Size
		if st, err := os.Stat(f.FullPath); err == nil {
			size = st.Size()
		}
		if reuse, had := s.besideTrack(ctx, t.Path, tr, size); had {
			if reuse != "" {
				paths = append(paths, reuse)
			}
			continue
		}
		stem := strings.TrimSuffix(t.Path, filepath.Ext(t.Path))
		key := stem + organize.AudioTag(tr)
		var before func(string) error
		if s.o.VoiceOverPlaced != nil {
			before = func(dst string) error {
				return s.o.VoiceOverPlaced(ctx, PlacedVoiceOver{Path: dst, FileID: t.FileID, OrigLang: vo.OrigLang})
			}
		}
		n, dst, _ := organize.PlaceAudioFileAt(ctx, f.FullPath, key, strings.ToLower(filepath.Ext(f.Path)), used[key], before)
		used[key] = n
		if dst != "" {
			paths = append(paths, dst)
		}
	}
	raw, _ := json.Marshal(paths)
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET organized_at=?, organize_error='', organize_attempts=organize_attempts+1, paths_json=? WHERE infohash=?
		AND `+selRevIs, time.Now().UTC().Format(time.RFC3339), string(raw), a.InfoHash, a.Meta.SelRev); err != nil {
		s.log.Error("save voice-over organize result", "infohash", a.InfoHash, "err", err)
		return
	}
	s.log.Info("voice-over tracks placed beside their videos", "infohash", a.InfoHash, "tracks", len(paths))
	s.publish(events.LibraryUpdated, map[string]any{"infoHash": a.InfoHash, "kind": a.Kind, "tmdbId": a.TMDBID, "itemId": vo.ItemID, "title": a.Title})
	s.finished(Finished{Key: a.InfoHash, Kind: a.Kind, Title: a.Title, Year: a.Year})
}

// langKey is a track language as compared (ISO 639-1 when known).
func langKey(l string) string {
	if c := subtitles.NormLang(l); c != "" {
		return c
	}
	return strings.ToLower(strings.TrimSpace(l))
}

// besideTrack looks beside video for a track standing for tr (size: its
// file's): one of the same language and size that this stage placed
// before (a fit is known of it: a release added again) is reused (its
// path); another of the same language and studio, from any source, is
// there already (had, reuse ""). A misfit there counts as none.
func (s *Service) besideTrack(ctx context.Context, video string, tr extaudio.Track, size int64) (reuse string, had bool) {
	for _, e := range extaudio.Find(video) {
		if langKey(e.Lang) != langKey(tr.Lang) {
			continue
		}
		var state string
		known := false
		if s.o.FitState != nil {
			state, _, _, known = s.o.FitState(ctx, e.Path)
		}
		if known && state == VoiceMisfit {
			continue
		}
		sameStudio := tr.Studio != "" && release.StudioKey(e.Studio) == release.StudioKey(tr.Studio)
		if st, err := os.Stat(e.Path); err == nil && st.Size() == size && (sameStudio || e.Studio == tr.Studio) && (known || s.o.FitState == nil) {
			return e.Path, true
		}
		if sameStudio && standsFor(known, state) {
			return "", true
		}
	}
	return "", false
}

// standsFor: a track of the studio beside the video counts as the
// studio's track there — one of a release (no fit: stage A), or one of
// stage B fitted or still being fitted; one that is never offered (a
// misfit, unmeasured, a failed fit given up) lets a new one in.
func standsFor(known bool, state string) bool {
	return !known || state == VoiceFitted || state == VoicePlaced
}

// studioBeside: a track of the studio lies beside video (standsFor).
func (s *Service) studioBeside(ctx context.Context, video, studio string) bool {
	if studio == "" {
		return false
	}
	for _, e := range extaudio.Find(video) {
		if release.StudioKey(e.Studio) != release.StudioKey(studio) {
			continue
		}
		if s.o.FitState != nil {
			if st, _, _, ok := s.o.FitState(ctx, e.Path); !standsFor(ok, st) {
				continue
			}
		}
		return true
	}
	return false
}

// SetVoiceOverFit sets Options.VoiceOverPlaced and FitState (the fit is
// made after the service); before Run.
func (s *Service) SetVoiceOverFit(placed func(ctx context.Context, p PlacedVoiceOver) error,
	state func(ctx context.Context, path string) (string, float64, float64, bool)) {
	s.o.VoiceOverPlaced, s.o.FitState = placed, state
}

// VoiceOverDownload is a voice-over download of a title: its torrent's
// progress and its episodes' states.
type VoiceOverDownload struct {
	InfoHash string             `json:"infoHash"`
	Title    string             `json:"title"`
	Studio   string             `json:"studio"`
	Status   string             `json:"status"`
	Progress float64            `json:"progress"`
	Done     bool               `json:"done"`
	Episodes []VoiceOverEpisode `json:"episodes"`
}

// VoiceOverDownloads lists the voice-over downloads of a title, newest
// first, with their episodes' states: downloading, placed (being fitted),
// skipped; FitState (nil: none) turns a placed track into fitted,
// unchecked or misfit.
func (s *Service) VoiceOverDownloads(ctx context.Context, itemID int64) ([]VoiceOverDownload, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT `+acquisitionCols+`, paths_json FROM acquisitions WHERE kind=? ORDER BY rowid DESC`, KindVoiceOver)
	if err != nil {
		return nil, err
	}
	type row struct {
		a     acquisition
		paths []string
	}
	var list []row
	for rows.Next() {
		var a acquisition
		var meta, paths string
		var noFiles bool
		if err := rows.Scan(&a.InfoHash, &a.Kind, &a.TMDBID, &a.Title, &a.Year, &a.Season, &a.Episode, &a.OrganizedAt, &a.Error, &a.Attempts,
			&a.ExtID, &meta, &noFiles, &a.Poster, &a.AddedBy, &a.HadPeers, &a.AutoNote, &a.AddedByUID, &paths); err != nil {
			rows.Close()
			return nil, err
		}
		if json.Unmarshal([]byte(meta), &a.Meta) != nil || a.Meta.VoiceOver == nil || a.Meta.VoiceOver.ItemID != itemID {
			continue
		}
		r := row{a: a}
		_ = json.Unmarshal([]byte(paths), &r.paths)
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []VoiceOverDownload{}
	for _, r := range list {
		vo := r.a.Meta.VoiceOver
		d := VoiceOverDownload{InfoHash: r.a.InfoHash, Title: r.a.Meta.Release, Studio: vo.Studio, Episodes: picksEpisodes(vo.Picks)}
		h, ok := s.o.Engine.Get(r.a.InfoHash)
		if !ok {
			continue // removed in «Загрузки»
		}
		st := h.Status()
		d.Status = st.Status
		if st.Size > 0 {
			d.Progress = float64(st.Done) / float64(st.Size)
		}
		organized := r.a.OrganizedAt.Valid
		d.Done = organized
		if organized {
			d.Progress = 1
		}
		for i := range d.Episodes {
			ep := &d.Episodes[i]
			ep.State = VoiceDownloading
			if !organized {
				continue
			}
			ep.State = VoiceSkipped
			stem, video := "", ""
			for _, t := range vo.Picks {
				if t.FileID == ep.FileID {
					stem, video = strings.TrimSuffix(t.Path, filepath.Ext(t.Path))+".", t.Path
					break
				}
			}
			// several tracks for one episode: the best of their states
			for _, p := range r.paths {
				if stem == "" || !strings.HasPrefix(p, stem) {
					continue
				}
				e := VoiceOverEpisode{State: VoicePlaced}
				if s.o.FitState != nil {
					if st, off, tempo, ok := s.o.FitState(ctx, p); ok {
						e.State, e.OffsetMs, e.Tempo = st, off, tempo
					}
				}
				if voiceRank[e.State] > voiceRank[ep.State] {
					ep.State, ep.OffsetMs, ep.Tempo = e.State, e.OffsetMs, e.Tempo
				}
			}
			if ep.State == VoiceSkipped && video != "" && s.studioBeside(ctx, video, vo.Studio) {
				ep.State = VoiceHad
			}
		}
		out = append(out, d)
	}
	return out, nil
}
