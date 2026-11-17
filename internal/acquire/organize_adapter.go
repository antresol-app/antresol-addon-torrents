package acquire

import (
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// OrganizeJob is what the organizer takes (internal/organize): a finished
// file set and the item it is for.
type OrganizeJob = organize.Job

// filesOf is a download's files as the organizer's file set.
func filesOf(files []torrent.FileInfo) []organize.File {
	out := make([]organize.File, len(files))
	for i, f := range files {
		out[i] = organize.File{Path: f.Path, FullPath: f.FullPath, Size: f.Size}
	}
	return out
}
