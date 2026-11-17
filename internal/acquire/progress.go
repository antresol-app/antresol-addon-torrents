package acquire

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// Hint is a line in both interface languages.
type Hint struct {
	RU string `json:"ru"`
	EN string `json:"en"`
}

// StallHint is the line under a download that makes no headway, as the
// web's hintLine words it: a tracker's refusal of a known kind wins over
// the stall; with no stall yet, a probe that saw no seeds says so. The
// tracker's own words are never repeated (they may name the account).
// nil: nothing to say.
func StallHint(stalled, liveness, trackerErrorKind string) *Hint {
	var key string
	switch {
	case stalled != "" && trackerErrorKind == torrent.TrackerBadPasskey:
		key = "Трекер отклонил раздачу: неверный пасскей"
	case stalled != "" && trackerErrorKind == torrent.TrackerNotRegistered:
		key = "Раздача не зарегистрирована на трекере"
	case stalled == torrent.StalledNoPeers:
		key = "Нет участников раздачи — ждём…"
	case stalled == torrent.StalledNoMetadata:
		key = "Получение данных о раздаче…"
	case stalled == "" && liveness == torrent.LivenessUnseen:
		key = "Сидов пока не видно"
	default:
		return nil
	}
	return &Hint{RU: i18n.TL(i18n.RU, key), EN: i18n.TL(i18n.EN, key)}
}

// libraryProgress is the adults' view of the tick's list: the titles on
// their way into the library by the pending cards' own rule and key
// (collectDownloads): no stream, no organized title the library has, no
// finished download of long ago; nothing an admin alone may see (a failed
// download is an admins' card: they have admin.progress).
func (s *Service) libraryProgress(ctx context.Context, list []TorrentView) []LibraryProgress {
	sts := make([]torrent.Status, 0, len(list))
	for _, v := range list {
		sts = append(sts, v.Status)
	}
	out := []LibraryProgress{}
	for _, d := range s.collectDownloads(ctx, sts, false) {
		if d.Status == DownloadError {
			continue
		}
		p := LibraryProgress{Key: d.Key, Kind: d.Kind, TMDBID: d.TMDBID, Season: d.Season, Status: d.Status, Done: d.Done, Size: d.Size}
		if d.Status == DownloadActive {
			p.Hint = StallHint(d.Stalled, d.Liveness, d.TrackerErrorKind)
		}
		out = append(out, p)
	}
	return out
}

// progressLookups bounds the tick's library lookups: a slow or hung core
// never holds acquire's loop (a lookup that misses just means the title
// stays on the list one more tick).
const progressLookups = 1500 * time.Millisecond

// publishProgress is the progress tick: the list is built once (when
// anyone listens) and goes out as EventProgress (admins) and, derived from
// it, as EventLibraryProgress (adults) — every tick while one of them is
// active, else only when it changed.
func (s *Service) publishProgress() {
	if s.o.Bus == nil {
		return
	}
	admins, adults := s.o.Bus.Wants(EventProgress), s.o.Bus.Wants(EventLibraryProgress)
	if !admins && !adults {
		return
	}
	list := s.Torrents()
	if admins {
		s.publish(EventProgress, list)
	}
	if !adults {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), progressLookups)
	lp := s.libraryProgress(ctx, list)
	cancel()
	b, _ := json.Marshal(lp)
	active := slices.ContainsFunc(lp, func(p LibraryProgress) bool { return p.Status == DownloadActive })
	s.mu.Lock()
	same := string(b) == s.lastAdult
	s.lastAdult = string(b)
	s.mu.Unlock()
	if active || !same {
		s.publish(EventLibraryProgress, lp)
	}
}
