package acquire

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// «Докачивать новые» of a pinned release (internal/pinned): the follow's
// resolution in the automatic pick, new episodes' files added to the pin's
// download, and the download record kept across the pin's re-upload.

const (
	msgNoResolution  = "Раздач в %s нет — скачивается лучшая раздача другого качества"
	msgNotADownload  = "Раздача не скачивается на сервер"
	msgNoEpisodeFile = "В раздаче нет файла %d"
	msgUnknownGrow   = "Эта раздача добавлена не через поиск — Antresol не знает, что в ней"
)

// resolutionReleases keeps the releases of resolution ("1080p") when the
// search has any (their order kept); otherwise all of them, with a note
// for the asker. A release's resolution is the indexer's parse of its
// name (its own parse when the indexer gave none).
func resolutionReleases(rels []indexer.ScoredRelease, resolution string) ([]indexer.ScoredRelease, string) {
	want, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(resolution)), "p"))
	if err != nil || want <= 0 {
		return rels, ""
	}
	var out []indexer.ScoredRelease
	for _, r := range rels {
		res := r.Info.Resolution
		if res == 0 {
			res = release.Parse(r.Title).Resolution
		}
		if res == want {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return rels, fmt.Sprintf(msgNoResolution, strconv.Itoa(want)+"p")
	}
	return out, ""
}

// GrowEpisodes adds files (engine indexes: the new episodes of a pinned
// release, their videos and external tracks) to the series download ih,
// as growSeason does for a season: the engine's selection grows
// (Engine.AddFiles) and the acquisition is organized afresh once the
// larger selection is complete (organize skips the episodes the library
// has already, so the files placed before are not placed twice). A pack
// of several seasons gets the files' seasons asked for again (one placed
// before is placed again). A whole torrent, or files selected already,
// change nothing. ErrNotFound: the engine lacks ih, or «На сервер» does
// not know it (no acquisition); ErrInvalid: not a download, or not a
// series.
func (s *Service) GrowEpisodes(ctx context.Context, ih string, files []int) (AcquireResult, error) {
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return AcquireResult{}, uerr(ErrNotFound, "Раздача не найдена")
	}
	ih = h.InfoHash()
	if h.Status().Mode != ModeDownload {
		return AcquireResult{}, uerr(ErrInvalid, msgNotADownload)
	}
	all := h.Files()
	for _, i := range files {
		if i < 0 || i >= len(all) {
			return AcquireResult{}, uerr(ErrInvalid, fmt.Sprintf(msgNoEpisodeFile, i))
		}
	}
	grew, err := s.growEpisodes(ctx, ih, all, files)
	if err != nil {
		return AcquireResult{}, err
	}
	if grew {
		s.checkDisk() // more to download: hold it if the disk is full
	}
	return s.existingResult(ih), nil
}

// growEpisodes is GrowEpisodes under s.mu: grew is false when nothing was
// added.
func (s *Service) growEpisodes(ctx context.Context, ih string, all []torrent.FileInfo, files []int) (grew bool, err error) {
	// No organize starts while the selection grows (see growSelection).
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.acquisition(ctx, ih)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, uerr(ErrNotFound, msgUnknownGrow)
	case err != nil:
		return false, err
	case a.Kind != KindSeries:
		return false, uerr(ErrInvalid, msgNotADownload)
	}
	if !partial(all) {
		return false, nil // a whole torrent has them already
	}
	var add []int
	for _, i := range files {
		if !all[i].Selected && !slices.Contains(add, i) {
			add = append(add, i)
		}
	}
	if len(add) == 0 {
		return false, nil
	}
	slices.Sort(add)
	g, ok := s.o.Engine.(selectionGrower)
	if !ok {
		return false, uerr(ErrInvalid, msgNotADownload)
	}
	meta := a.Meta
	meta.SelRev++
	if len(meta.Seasons) > 0 {
		// a pack: the added files' seasons are asked for and placed again
		for _, i := range add {
			n := organize.FileSeason(all[i].Path)
			if n <= 0 {
				n = release.Parse(path.Base(all[i].Path)).Season
			}
			if n <= 0 {
				continue
			}
			if !slices.Contains(meta.Seasons, n) {
				meta.Seasons = union(meta.Seasons, []int{n})
			}
			meta.Organized = slices.DeleteFunc(slices.Clone(meta.Organized), func(x int) bool { return x == n })
		}
	}
	// the selection grows first: an organize that ran meanwhile records
	// nothing once the revision moves (see growSelection)
	if err := g.AddFiles(ctx, ih, add); err != nil {
		return false, growErr(err)
	}
	seasons, _ := json.Marshal(meta.Seasons)
	organized, _ := json.Marshal(meta.Organized)
	q := `UPDATE acquisitions SET organized_at=NULL, organize_error='', organize_attempts=0,
		meta_json=json_set(COALESCE(NULLIF(meta_json,''),'{}'), '$.selRev', ?`
	args := []any{meta.SelRev}
	if len(meta.Seasons) > 0 {
		q += `, '$.seasons', json(?), '$.organized', json(?)`
		args = append(args, string(seasons), string(organized))
	}
	q += `) WHERE infohash=?`
	if _, err := s.o.DB.ExecContext(ctx, q, append(args, ih)...); err != nil {
		return false, err
	}
	s.log.Info("new episodes added to a download", "infohash", ih, "files", len(add))
	return true, nil
}

// Rekey moves the acquisition of oldIH to newIH: the engine replaced that
// torrent by a re-upload of the same release (pinned.Service.Replaced). It
// is organized afresh (the re-upload's files, renamed or new, are imported
// once complete; organize skips the episodes the library has). A row newIH
// has already is kept (the old one is left for reconcile).
func (s *Service) Rekey(ctx context.Context, oldIH, newIH string) error {
	oldIH, newIH = strings.ToLower(oldIH), strings.ToLower(newIH)
	if oldIH == newIH {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET infohash=?, organized_at=NULL, organize_error='', organize_attempts=0
		WHERE infohash=? AND NOT EXISTS (SELECT 1 FROM acquisitions WHERE infohash=?)`, newIH, oldIH, newIH)
	return err
}

// Imported tells whether the download ih was put into the library (its
// whole current selection), and when; ok false when it was not, or «На
// сервер» does not know it.
func (s *Service) Imported(ctx context.Context, ih string) (at time.Time, ok bool, err error) {
	a, err := s.acquisition(ctx, strings.ToLower(ih))
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil || !a.OrganizedAt.Valid || a.Error != "" {
		return time.Time{}, false, err
	}
	at, err = time.Parse(time.RFC3339, a.OrganizedAt.String)
	if err != nil {
		return time.Time{}, false, nil
	}
	return at, true, nil
}
