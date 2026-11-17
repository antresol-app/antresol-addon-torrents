package acquire

// EventProgress is the event the download list goes out on (every 2 s
// while someone listens): the core's events.TorrentProgress, which only
// this family named.
const EventProgress = "torrent.progress"

// EventLibraryProgress is the adults' view of the same tick
// (LibraryProgress): built from the list EventProgress carries, never a
// second walk of the engine.
const EventLibraryProgress = "torrent.library-progress"

// LibraryProgress is one library download as every adult may see it (the
// app's `progress` event): no infohash of a stream, no error text,
// no name of who added it.
type LibraryProgress struct {
	// Key: the title's, as its pending card has it (LibraryDownload.Key,
	// kind:tmdbId and the like): the torrents of one title are one entry.
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	TMDBID int    `json:"tmdbId"`
	Season int    `json:"season"` // the one season of its torrents (0: the whole title or several)
	Status string `json:"status"` // DownloadActive … DownloadError
	Done   int64  `json:"done"`
	Size   int64  `json:"size"`
	// Hint: why it makes no headway, in both languages (nil: nothing to say).
	Hint *Hint `json:"hint,omitempty"`
}
