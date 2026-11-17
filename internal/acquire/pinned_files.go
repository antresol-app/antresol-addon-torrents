package acquire

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// AcquirePinnedFiles downloads only files (engine indexes: the missing
// episodes of a pinned release, their videos and external tracks) of the
// release ih that req names by its link (ReleaseLink, SourceID; TMDBID,
// Season of a series). It is internal/pinned's «Докачивать новые» and
// never a client's: AcquireRequest.Files stays music-only.
//   - ih downloading: the files join its selection (GrowEpisodes);
//   - ih streamed: its selection becomes the files (the stream's data is
//     kept), then it is downloaded («На сервер», Promote);
//   - not in the engine: the release is added in download mode with the
//     files selected, never whole.
//
// A download «На сервер» does not know gets its acquisition from req.
func (s *Service) AcquirePinnedFiles(ctx context.Context, ih string, req AcquireRequest, files []int) (AcquireResult, error) {
	req.UserID, req.UserUID = s.asker(ctx, req.UserID, req.UserUID)
	if req.Kind != KindSeries || req.TMDBID <= 0 || len(files) == 0 || strings.TrimSpace(req.ReleaseLink) == "" {
		return AcquireResult{}, uerr(ErrInvalid, msgNotADownload)
	}
	ih = strings.ToLower(ih)
	if _, ok := s.o.Engine.Get(ih); !ok {
		c, added, err := s.releaseMetafile(ctx, req.SourceID, strings.TrimSpace(req.ReleaseLink), time.Now().Add(releaseFilesTimeout))
		if err != nil {
			return AcquireResult{}, err
		}
		switch {
		case added != nil:
			ih = strings.ToLower(added.InfoHash) // the engine has it after all
		case c.metafile == nil:
			return AcquireResult{}, uerr(ErrInvalid, msgNoFileList)
		default:
			if got := strings.ToLower(c.infoHash); got != "" {
				ih = got
			}
			if _, ok := s.o.Engine.Get(ih); !ok {
				return s.addPinnedFiles(ctx, req, c.metafile, files)
			}
		}
	}
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return AcquireResult{}, uerr(ErrNotFound, "Раздача не найдена")
	}
	if err := s.ensureAcquisition(ctx, ih, req); err != nil {
		return AcquireResult{}, err
	}
	switch h.Status().Mode {
	case ModeDownload:
		return s.GrowEpisodes(ctx, ih, files)
	case ModeStream:
		return s.promotePinnedFiles(ctx, h, files, req.Force)
	}
	return AcquireResult{}, uerr(ErrInvalid, msgNotADownload)
}

// addPinnedFiles adds the release mf in download mode with files selected
// and records its acquisition (a series request of req.Season).
func (s *Service) addPinnedFiles(ctx context.Context, req AcquireRequest, mf []byte, files []int) (AcquireResult, error) {
	d, err := s.title(ctx, KindSeries, req.TMDBID)
	if err != nil {
		return AcquireResult{}, err
	}
	a := acquisition{Kind: KindSeries, TMDBID: req.TMDBID, Title: d.Title, Year: d.Year, Season: req.Season}
	opt := torrent.AddOptions{Mode: ModeDownload, AddedBy: req.UserID, AddedByUID: req.UserUID, SaveDir: s.downloadDir(KindSeries), Files: files,
		WaitingSpace: s.o.Disk != nil && s.o.Disk.Level() == LevelFull && !req.Force}
	if it, ok := s.libraryItem(ctx, a); ok {
		id := it.ID
		opt.ItemID = &id
	}
	defer s.series.lock(req.TMDBID)()
	h, err := s.o.Engine.AddTorrentFile(ctx, mf, opt)
	if err != nil {
		if errors.Is(err, torrent.ErrStorageOffline) {
			return AcquireResult{}, wrapErr(ErrUnavailable, "Медиадиск недоступен: загрузки остановлены, пока его не подключат", err)
		}
		return AcquireResult{}, wrapErr(ErrSource, "Не удалось открыть раздачу. Попробуйте другую", err)
	}
	a.InfoHash = h.InfoHash()
	if err := s.saveAcquisition(ctx, a, req.UserID, req.UserUID, false); err != nil {
		return AcquireResult{}, err
	}
	s.capturePoster(a.InfoHash, s.posterSource(a, d.Poster, ""))
	s.log.Info("pinned release added for its missing episodes", "infohash", a.InfoHash, "files", len(files))
	s.checkDisk()
	return AcquireResult{InfoHash: a.InfoHash, FileIndex: -1, Status: h.Status().Status}, nil
}

// ensureAcquisition records req as the acquisition of ih when «На сервер»
// has none (a torrent added elsewhere); one it has is kept.
func (s *Service) ensureAcquisition(ctx context.Context, ih string, req AcquireRequest) error {
	_, err := s.acquisition(ctx, ih)
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	d, err := s.title(ctx, KindSeries, req.TMDBID)
	if err != nil {
		return err
	}
	return s.saveAcquisition(ctx, acquisition{InfoHash: ih, Kind: KindSeries, TMDBID: req.TMDBID, Title: d.Title, Year: d.Year, Season: req.Season}, req.UserID, req.UserUID, false)
}

// promotePinnedFiles narrows a streamed torrent to files (a stream's
// data is never deleted by the engine) and downloads it: only those files
// are fetched, organized once complete.
func (s *Service) promotePinnedFiles(ctx context.Context, h torrent.Handle, files []int, force bool) (AcquireResult, error) {
	ih := h.InfoHash()
	sh, ok := s.o.Engine.(selectionShrinker)
	if !ok {
		return AcquireResult{}, uerr(ErrInvalid, msgNotADownload)
	}
	var drop []int
	for _, f := range h.Files() {
		if f.Selected && !slices.Contains(files, f.Index) {
			drop = append(drop, f.Index)
		}
	}
	var add []int
	for _, f := range h.Files() {
		if !f.Selected && slices.Contains(files, f.Index) {
			add = append(add, f.Index)
		}
	}
	if len(add) > 0 {
		g, ok := s.o.Engine.(selectionGrower)
		if !ok {
			return AcquireResult{}, uerr(ErrInvalid, msgNotADownload)
		}
		if err := g.AddFiles(ctx, ih, add); err != nil {
			return AcquireResult{}, growErr(err)
		}
	}
	if len(drop) > 0 {
		if err := sh.RemoveFiles(ctx, ih, drop, func(string) bool { return false }); err != nil {
			return AcquireResult{}, growErr(err)
		}
	}
	if _, err := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET organized_at=NULL, organize_error='', organize_attempts=0 WHERE infohash=?`, ih); err != nil {
		return AcquireResult{}, err
	}
	if _, err := s.Promote(ctx, ih, force); err != nil {
		return AcquireResult{}, err
	}
	s.log.Info("streamed pinned release downloaded for its missing episodes", "infohash", ih, "files", len(files))
	return s.existingResult(ih), nil
}
