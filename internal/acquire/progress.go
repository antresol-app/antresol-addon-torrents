package acquire

import (
	"context"
	"encoding/json"
	"hash/fnv"
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

// progressIdleEvery: while nothing is moving and the engine's list is as
// at the last build, the download list is rebuilt this often (the library
// side of it, the acquisitions and the core's titles, can change on its
// own) and sent when it differs (or after progressResendEvery).
const progressIdleEvery = 15 * time.Second

// progressResendEvery: a quiet list is sent again after this long though it
// did not change, so a page whose event stream was down, or an event the
// core refused, catches up (a page loads the list once, then follows the
// events).
const progressResendEvery = 30 * time.Second

// progressState is what publishProgress remembers between ticks (guarded
// by Service.mu).
type progressState struct {
	sig     uint64    // the engine's list at the last build (0: not taken)
	at      time.Time // that build
	working bool      // something was moving then
	admin   uint64    // the admins' list last sent
	adminAt time.Time // when
	adultAt time.Time // when the adults' list was last sent
}

// working: something moves now, so the list changes every tick: a torrent
// queued or downloading, an organize running, a direct download unfinished.
func (s *Service) working(list []torrent.Status) bool {
	for _, st := range list {
		if st.Status == torrent.StatusQueued || st.Status == torrent.StatusDownloading {
			return true
		}
	}
	s.mu.Lock()
	n := len(s.organizing)
	s.mu.Unlock()
	if n > 0 {
		return true
	}
	s.direct.mu.Lock()
	defer s.direct.mu.Unlock()
	for _, j := range s.direct.jobs {
		j.mu.Lock()
		done := j.completed != nil
		j.mu.Unlock()
		if !done {
			return true
		}
	}
	return false
}

// hashOf is a short fingerprint of v's JSON.
func hashOf(v any) uint64 {
	b, _ := json.Marshal(v)
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

// publishProgress is the progress tick: the list is built once (when
// anyone listens) and goes out as EventProgress (admins) and, derived from
// it, as EventLibraryProgress (adults) — every tick while something moves
// or the engine's list changed, else (a quiet system) only every
// progressIdleEvery, and then only what differs.
func (s *Service) publishProgress() {
	if s.o.Bus == nil {
		return
	}
	admins, adults := s.o.Bus.Wants(EventProgress), s.o.Bus.Wants(EventLibraryProgress)
	if !admins && !adults {
		return
	}
	engine := s.o.Engine.List()
	working := s.working(engine)
	var sig uint64
	if !working {
		sig = hashOf(engine)
	}
	s.mu.Lock()
	// the tick after the last one that moved builds too: it carries the end
	quiet := !working && !s.prog.working && sig == s.prog.sig
	if quiet && time.Since(s.prog.at) < progressIdleEvery {
		s.mu.Unlock()
		return
	}
	s.prog.sig, s.prog.at, s.prog.working = sig, time.Now(), working
	s.mu.Unlock()
	list := s.torrentViews(engine)
	if admins {
		s.publishAdmins(list, quiet)
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
	same := string(b) == s.lastAdult && time.Since(s.prog.adultAt) < progressResendEvery
	s.lastAdult = string(b)
	if active || !same {
		s.prog.adultAt = time.Now()
	}
	s.mu.Unlock()
	if active || !same {
		s.publish(EventLibraryProgress, lp)
	}
}

// publishAdmins sends the admins' list; on a quiet build only when it
// differs from the last one sent or progressResendEvery has passed.
func (s *Service) publishAdmins(list []TorrentView, quiet bool) {
	h := hashOf(list)
	s.mu.Lock()
	same := quiet && h == s.prog.admin && time.Since(s.prog.adminAt) < progressResendEvery
	s.prog.admin = h
	if !same {
		s.prog.adminAt = time.Now()
	}
	s.mu.Unlock()
	if !same {
		s.publish(EventProgress, list)
	}
}
