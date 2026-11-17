package acquire

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// Statuses of a LibraryDownload.
const (
	DownloadActive     = "active"     // queued or downloading
	DownloadWaiting    = "waiting"    // waiting for disk space
	DownloadPaused     = "paused"     // paused, or its data went missing
	DownloadOrganizing = "organizing" // downloaded, being put into the library
	DownloadError      = "error"      // the torrent or organize failed
)

// organizedGrace is how long an organized title stays a LibraryDownload
// («organizing») while the scanner has not indexed it yet: the card must
// not vanish between organize and the scan that brings the library card.
const organizedGrace = 10 * time.Minute

// LibraryDownload is one title on its way into the library: the
// download-mode torrents of one movie, series, album, book, audiobook or
// comic that are not organized yet, summed up. The library pages show it
// as a pending card at once.
type LibraryDownload struct {
	// Key identifies the title among the downloads ("movie:42", "music:<mbid>").
	Key    string
	Kind   string // acquisition kind (KindMovie … KindComic)
	TMDBID int    // movies and series
	Title  string
	Year   int
	// Creator: the artist of an album, the author of a book ("" for video).
	Creator string
	// Poster: the cover, a file of the image cache ("" = none yet).
	Poster string
	// ItemID: the title's library item when it is in the library already
	// (a new season of a series, another album version); 0 = not yet.
	ItemID int64
	Status string // Download* above
	// Done and Size: bytes, summed over the title's torrents.
	Done, Size int64
	AddedAt    time.Time
	// By: the uids of the profiles that asked for the title's torrents
	// (none known: nil; never ids, which a new profile may get).
	By []string
	// Stalled: torrent.StalledNoPeers or StalledNoMetadata when every
	// active torrent of the title is stalled ("" = some headway, or not
	// active).
	Stalled string
	// Liveness, TrackerError, TrackerErrorKind: see TitleProgress.
	Liveness, TrackerError, TrackerErrorKind string
	// Season: the one season all its torrents are for (0: the whole
	// title, or several seasons).
	Season int
}

// stallSum merges the Stalled hints of a title's active torrents: the
// title is stalled only when each of them is (one moving torrent is
// headway); no_peers wins over no_metadata.
type stallSum struct {
	hint   string
	moving bool
	// unseen: every torrent so far is torrent.LivenessUnseen (seen: one
	// was, checking: one is still being probed, alive: one is not
	// unseen); trackerError/Kind: a refusal of a stalled torrent's tracker.
	seen, checking, alive     bool
	trackerError, trackerKind string
}

func (s *stallSum) add(st torrent.Status) {
	if st.Stalled == "" {
		s.moving = true
	} else if s.hint == "" || st.Stalled == torrent.StalledNoPeers {
		s.hint = st.Stalled
	}
	switch st.Liveness {
	case torrent.LivenessUnseen:
		s.seen = true
	case torrent.LivenessChecking:
		s.checking = true
	default:
		s.alive = true // alive, or never probed: nothing to say
	}
	if st.Stalled != "" && st.TrackerErrorKind != "" && s.trackerKind == "" {
		s.trackerError, s.trackerKind = st.TrackerError, st.TrackerErrorKind
	}
}

// liveness: torrent.LivenessUnseen when every torrent is, LivenessChecking
// while one is probed and none is alive, else "".
func (s stallSum) liveness() string {
	switch {
	case s.alive:
		return ""
	case s.checking:
		return torrent.LivenessChecking
	case s.seen:
		return torrent.LivenessUnseen
	}
	return ""
}

func (s stallSum) result() string {
	if s.moving {
		return ""
	}
	return s.hint
}

// statusRank orders the statuses of a title with several torrents: the
// most telling one wins (one season downloading reads as «downloading»).
var statusRank = map[string]int{DownloadActive: 0, DownloadWaiting: 1, DownloadOrganizing: 2, DownloadPaused: 3, DownloadError: 4}

// LibraryDownloads lists the titles being downloaded into the library,
// newest first. Streams and seeds of existing data are left out, and so is
// an organized title once the library has it (or organizedGrace after
// organize). Direct downloads are not listed: they take seconds.
func (s *Service) LibraryDownloads(ctx context.Context) []LibraryDownload {
	return s.collectDownloads(ctx, s.o.Engine.List(), true)
}

// collectDownloads is LibraryDownloads over the torrents sts (the engine's
// list, or the progress tick's): the one rule of which torrents are a
// title on its way and of their keys, shared by the pending cards and the
// adults' progress event. withItems asks the library for every title (the
// cards' ItemID); without, only for an organized one (it is dropped once
// the library has it), so the 2 s tick asks the core almost never.
func (s *Service) collectDownloads(ctx context.Context, sts []torrent.Status, withItems bool) []LibraryDownload {
	acqs, err := s.acquisitions(ctx)
	if err != nil {
		s.log.Warn("load acquisitions", "err", err)
		return nil
	}
	// the library lookups first: one per title, a few at a time (a
	// household with many downloads still answers within a hook's 2 s)
	need := map[titleLookup]acquisition{}
	for _, st := range sts {
		a, ok := acqs[st.InfoHash]
		if !ok {
			continue
		}
		if _, ok := onTheWay(st, a); ok && (withItems || a.OrganizedAt.Valid) {
			need[lookupOf(a)] = a
		}
	}
	found := s.libraryItems(ctx, need)
	byKey := map[string]*LibraryDownload{}
	stalls := map[string]*stallSum{}
	mixed := map[string]bool{} // keys whose torrents are for several seasons
	var order []string
	for _, st := range sts {
		a, ok := acqs[st.InfoHash]
		if !ok {
			continue
		}
		status, ok := onTheWay(st, a)
		if !ok {
			continue
		}
		var it library.Item
		var inLib bool
		if withItems || a.OrganizedAt.Valid {
			r := found[lookupOf(a)]
			it, inLib = r.item, r.ok
		}
		if a.OrganizedAt.Valid && inLib {
			continue // in the library now
		}
		key := downloadKey(a)
		d, seen := byKey[key]
		switch {
		case !seen:
		case d.Season != a.Season:
			mixed[key] = true
		}
		if !seen {
			d = &LibraryDownload{Key: key, Kind: a.Kind, TMDBID: a.TMDBID, Title: a.Title, Year: a.Year, Status: status, Season: a.Season}
			if d.Year == 0 {
				d.Year = a.Meta.Year
			}
			d.Creator = a.Meta.Artist
			if a.Kind != KindMusic {
				d.Creator = a.Meta.Author
			}
			if d.Title == "" { // a whole artist (no album asked for)
				d.Title, d.Creator = a.Meta.Artist, ""
			}
			byKey[key] = d
			order = append(order, key)
		}
		if inLib {
			d.ItemID = it.ID
		}
		if d.Poster == "" {
			d.Poster = a.Poster
		}
		if statusRank[status] < statusRank[d.Status] {
			d.Status = status
		}
		if a.AddedByUID != "" && !slices.Contains(d.By, a.AddedByUID) {
			d.By = append(d.By, a.AddedByUID)
		}
		if status == DownloadActive {
			if stalls[key] == nil {
				stalls[key] = &stallSum{}
			}
			stalls[key].add(st)
		}
		d.Done += st.Done
		d.Size += st.Size
		if st.AddedAt.After(d.AddedAt) {
			d.AddedAt = st.AddedAt
		}
	}
	out := make([]LibraryDownload, 0, len(order))
	for _, k := range order {
		d := *byKey[k]
		if mixed[k] {
			d.Season = 0
		}
		if sum := stalls[k]; sum != nil && d.Status == DownloadActive {
			d.Stalled = sum.result()
			d.Liveness = sum.liveness()
			if d.Stalled != "" {
				d.TrackerError, d.TrackerErrorKind = sum.trackerError, sum.trackerKind
			}
		}
		out = append(out, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt.After(out[j].AddedAt) })
	return out
}

// onTheWay is the status of torrent st of acquisition a when it is a title
// on its way into the library: a download, not organized long ago, not
// organized with nothing placed, not an organized comic (no library
// lookup by title: the scan has it at once). An organized one is dropped
// by the caller once the library has the title.
func onTheWay(st torrent.Status, a acquisition) (string, bool) {
	if st.Mode != torrent.ModeDownload {
		return "", false
	}
	status, ok := downloadStatus(st, a)
	if !ok || (a.OrganizedAt.Valid && (a.NoFiles || a.Kind == KindComic || !recent(a.OrganizedAt.String))) {
		return "", false
	}
	return status, true
}

// titleLookup is what libraryItem asks the library with.
type titleLookup struct {
	kind           string
	tmdbID         int
	title, creator string
}

func lookupOf(a acquisition) titleLookup {
	creator := a.Meta.Author
	if a.Kind == KindMusic {
		creator = a.Meta.Artist
	}
	return titleLookup{a.Kind, a.TMDBID, a.Title, creator}
}

type lookedUp struct {
	item library.Item
	ok   bool
}

// libraryLookups bounds the library lookups of one listing that run at once.
const libraryLookups = 4

// libraryItems runs libraryItem for each title of need, libraryLookups at
// a time.
func (s *Service) libraryItems(ctx context.Context, need map[titleLookup]acquisition) map[titleLookup]lookedUp {
	out := make(map[titleLookup]lookedUp, len(need))
	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, libraryLookups)
	for k, a := range need {
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			it, ok := s.libraryItem(ctx, a)
			mu.Lock()
			out[k] = lookedUp{it, ok}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// downloadStatus is the LibraryDownload status of one torrent; false: it
// is not on its way (an organized torrent counts as organizing, the
// caller drops it once the library has the title).
func downloadStatus(st torrent.Status, a acquisition) (string, bool) {
	if a.OrganizedAt.Valid {
		return DownloadOrganizing, true
	}
	switch st.Status {
	case torrent.StatusQueued, torrent.StatusDownloading:
		if st.DataMissing {
			return DownloadPaused, true
		}
		return DownloadActive, true
	case torrent.StatusWaitingSpace:
		return DownloadWaiting, true
	case torrent.StatusPaused:
		return DownloadPaused, true
	case torrent.StatusSeeding:
		if a.Error != "" {
			return DownloadError, true
		}
		return DownloadOrganizing, true
	case torrent.StatusError:
		return DownloadError, true
	}
	return "", false
}

// downloadKey groups the torrents of one title.
func downloadKey(a acquisition) string {
	switch {
	case a.TMDBID > 0:
		return a.Kind + ":" + strconv.Itoa(a.TMDBID)
	case a.ExtID != "":
		return a.Kind + ":" + a.ExtID
	}
	return a.Kind + ":" + strings.ToLower(a.Meta.Artist+"|"+a.Meta.Author+"|"+a.Title)
}

// recent reports whether an RFC 3339 time is within organizedGrace.
func recent(at string) bool {
	t, err := time.Parse(time.RFC3339, at)
	return err == nil && time.Since(t) < organizedGrace
}
