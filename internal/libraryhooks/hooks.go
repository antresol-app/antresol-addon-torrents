// Package libraryhooks answers the core's library hooks: the
// pending cards of the library pages (library.pending), the addon's disk
// segments (disk.usage) and the kids' requests of albums and books
// (requests.open, requests.title, requests.search).
package libraryhooks

import (
	"context"
	"log/slog"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol/addonkit"
)

// Acquire is what the hooks use of acquire.Service.
type Acquire interface {
	LibraryDownloads(ctx context.Context) []acquire.LibraryDownload
	DiscoverMedia(ctx context.Context, kind, q string) ([]acquire.Candidate, error)
	RequestTitle(ctx context.Context, kind, extID string) (acquire.RequestTitleInfo, bool)
}

// Sources tells whether any release source is on (indexer.Manager).
type Sources interface {
	HasEnabled(ctx context.Context) (bool, error)
}

// Deps are the hooks' parts. DownloadDir and StreamDir are the addon's
// folders (boot.Conf), its disk segments.
type Deps struct {
	Acquire                Acquire
	Sources                Sources
	DownloadDir, StreamDir string
	Log                    *slog.Logger
}

// Register answers the library hooks on m.
func Register(m *addonkit.HookMux, d Deps) {
	m.Handle("library.pending", d.pending)
	m.Handle("disk.usage", d.disk)
	m.Handle("requests.open", d.requestsOpen)
	m.Handle("requests.title", d.requestsTitle)
	m.Handle("requests.search", d.requestsSearch)
}

func (d Deps) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}
