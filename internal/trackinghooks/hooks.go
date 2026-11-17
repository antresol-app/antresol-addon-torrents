// Package trackinghooks answers the core's tracking hooks: the
// tracking scanners of the pinned releases and the torrent search
// (tracking.scan:pinned, tracking.scan:torrent), the followed titles the
// core tracks besides the marked ones (tracking.titles), the followers it
// tells of new episodes (notify.interest) and a library file's original
// language for a voice-over's fit (voiceover.origLang).
package trackinghooks

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol/addonkit"
)

// Names are the hooks Register answers (the manifest declares them).
var Names = []string{"tracking.scan:pinned", "tracking.scan:torrent", "tracking.titles", "notify.interest", "voiceover.origLang"}

// Scanner is one tracking scanner (pinned.Scanner).
type Scanner interface {
	Scan(ctx context.Context, t tracking.Title, eps []tracking.Episode) ([]tracking.Source, error)
}

// Users is the server's profiles, each with its uid (coreadapt.Users).
type Users interface {
	Profiles(ctx context.Context) ([]addonkit.User, error)
}

// Deps are the hooks' parts (boot.Services.TrackingHooks).
type Deps struct {
	// Pinned is the pins' scanner (pinned.Service.Scanner), Torrent the
	// torrent search's (pinned.NewTorrentScanner: one for the process,
	// it keeps each title's day).
	Pinned, Torrent Scanner
	// Sync (optional: nil without follows) downloads a followed title's
	// missing episodes from its pin (pinned.Actions.Sync, under its
	// per-title lock); it runs after each tracking.scan:pinned answer, in
	// the background on Run.
	Sync func(ctx context.Context, t tracking.Title) error
	// Run is the addon's run context (Background when nil).
	Run context.Context
	// DB is the addon's database (follows).
	DB *sql.DB
	// Users tells which follows' authors are still profiles.
	Users Users
	// OrigLang is a library file's title's original language
	// (acquire.Service.OriginalLanguageOf), "" unknown.
	OrigLang func(ctx context.Context, fileID int64) string
	Log      *slog.Logger

	wait *sync.WaitGroup // tests: the background Syncs
}

// Register answers the tracking hooks on m.
func Register(m *addonkit.HookMux, d Deps) {
	m.Handle("tracking.scan:pinned", d.scanPinned)
	m.Handle("tracking.scan:torrent", d.scanTorrent)
	m.Handle("tracking.titles", d.titles)
	m.Handle("notify.interest", d.interest)
	m.Handle("voiceover.origLang", d.origLang)
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}
