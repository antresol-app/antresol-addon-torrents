// Package acquire is the «Найти и добавить» orchestrator: TMDB discovery,
// release search, «Смотреть сейчас» (stream torrents) and «На сервер»
// (download, then organize into the library), plus the disk guard.
package acquire

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/events"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/musicbrainz"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/openlibrary"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// Kinds and modes. Music, books, audiobooks and comics come from their
// catalogs (MusicBrainz, Open Library, the query itself) and are only
// downloaded («На сервер»), never streamed.
const (
	KindMovie     = library.KindMovie
	KindSeries    = library.KindSeries
	KindMusic     = library.KindMusic
	KindBook      = library.KindBook
	KindAudiobook = library.KindAudiobook
	KindComic     = library.KindComic
	ModeStream    = torrent.ModeStream
	ModeDownload  = torrent.ModeDownload
)

// msgAlreadyInLibrary: a series torrent organized with nothing placed, as
// every episode it holds was in the library already.
const msgAlreadyInLibrary = "Уже есть в медиатеке"

// msgNoMatch / msgNoMatchSourcesFailed: no release found (some sources
// did not answer, in the second). Whole messages: the API localizes them.
const (
	msgNoMatch              = "Подходящих вариантов не нашлось"
	msgNoMatchSourcesFailed = "Подходящих вариантов не нашлось. Часть источников не ответила — попробуйте позже"
)

// maxOrganizeAttempts bounds automatic organize retries (completion and
// restarts); an admin can retry by hand after fixing the cause.
const maxOrganizeAttempts = 3

// Engine is the part of *torrent.Engine the service uses.
type Engine interface {
	CacheEngine
	Remove(ctx context.Context, infohash string, deleteData bool) error
	HoldForSpace(ctx context.Context, infohash string) error
	AddTorrentFile(ctx context.Context, data []byte, opt torrent.AddOptions) (torrent.Handle, error)
	AddMagnet(ctx context.Context, uri string, opt torrent.AddOptions) (torrent.Handle, error)
	Get(infohash string) (torrent.Handle, bool)
	SetModeTo(ctx context.Context, infohash, mode, saveDir string) error
	Pause(ctx context.Context, infohash string) error
	Resume(ctx context.Context, infohash string) error
	OnComplete(f func(torrent.Status))
	// FetchInfo fetches a magnet's metafile without adding the torrent
	// (torrent.ErrKnown: the engine has it; torrent.ErrNoInfo: ctx ended).
	FetchInfo(ctx context.Context, magnet string) ([]byte, error)
}

// Indexer is the part of *indexer.Manager the service uses.
type Indexer interface {
	Search(ctx context.Context, q indexer.SearchRequest) (indexer.SearchResponse, error)
	Download(ctx context.Context, sourceID int64, link string) (cardigann.DownloadResult, error)
	// CheckReleaseLink: the link is a magnet or on a host of the source.
	CheckReleaseLink(ctx context.Context, sourceID int64, link string) error
}

// Metadata is the part of *tmdb.Client the service uses.
type Metadata interface {
	SearchMovie(ctx context.Context, q string, year int) ([]tmdb.Result, error)
	SearchTV(ctx context.Context, q string, year int) ([]tmdb.Result, error)
	Movie(ctx context.Context, id int) (tmdb.Details, error)
	TV(ctx context.Context, id int) (tmdb.Details, error)
	ImageURL(size, p string) string
}

// Library is the core's library the service uses (coreadapt.Library over
// the core API, coretest.Library in tests).
type Library = coreadapt.Core

// Users is the server's profiles («Добавил: …», whose a download is):
// coreadapt.Users, GET /users with each profile's uid.
type Users interface {
	Profiles(ctx context.Context) ([]addonkit.User, error)
}

// MusicBrainz is the part of *musicbrainz.Client the service uses.
type MusicBrainz interface {
	SearchFree(ctx context.Context, q string) ([]musicbrainz.ReleaseGroup, error)
	CoverURL(id string) string
}

// OpenLibrary is the part of *openlibrary.Client the service uses.
type OpenLibrary interface {
	SearchQ(ctx context.Context, q string) ([]openlibrary.Doc, error)
	Search(ctx context.Context, title, author string) ([]openlibrary.Doc, error)
	CoverURL(coverID int) string
}

// Options wire a Service. Metadata may be nil (no TMDB key): discovery and
// acquisition then report ErrUnavailable; everything else works.
type Options struct {
	DB       *sql.DB
	Engine   Engine
	Indexer  Indexer
	Metadata Metadata
	Library  Library
	// Users is the profiles (optional: «Добавил» stays empty and a
	// download names nobody: rows are keyed by uid).
	Users   Users
	Changed func() // optional: the library changed (trigger probe/enrich)
	Disk    *Disk
	Bus     *events.Bus // optional
	// FFprobe (optional) checks a release's audio tracks before organize
	// places them beside its videos (OrganizeJob.Probe).
	FFprobe string

	// downloads go to TorrentsDir/movies, series, music, books, audiobooks
	// and comics
	TorrentsDir      string
	MoviesDir        string
	SeriesDir        string
	MusicDir         string
	EbooksDir        string
	AudiobooksDir    string
	ComicsDir        string
	StreamCacheBytes int64
	// MusicBrainz and OpenLibrary are the catalogs of DiscoverMedia; nil
	// (VAULT_*_BASEURL=off) means ErrUnavailable.
	MusicBrainz MusicBrainz
	OpenLibrary OpenLibrary
	// Images caches the covers of downloads (nil = «Загрузки» has none).
	Images Images
	// Wikidata: the second book catalog (Russian titles); nil = Open
	// Library alone.
	Wikidata Wikidata
	Log      *slog.Logger
	// Finished (optional) hears of each download put into the library or
	// that could not be (notification letters to admins).
	Finished func(Finished)
	// VoiceOverPlaced (optional) hears of each track of «Найти озвучку…»
	// just before it is linked beside its video (the fit queues it, so the
	// players never list it unfitted); an error places nothing.
	VoiceOverPlaced func(ctx context.Context, p PlacedVoiceOver) error
	// FitState (optional) is the fit of a placed voice-over track
	// (VoiceFitted, VoiceUnmeasured, VoiceMisfit, or VoicePlaced while it
	// waits; ok false: none known).
	FitState func(ctx context.Context, path string) (state string, offsetMs, tempo float64, ok bool)
}

// Finished is a download put into the library (Failed false) or that
// could not be (Error: why, in Russian).
type Finished struct {
	Key    string // the infohash, or the direct download's id
	Kind   string
	Title  string
	Year   int
	Failed bool
	Error  string
}

func (s *Service) finished(f Finished) {
	if fn := s.onFinished.Load(); fn != nil {
		(*fn)(f)
	} else if s.o.Finished != nil {
		s.o.Finished(f)
	}
}

// SetFinished sets the Finished hook of a running service (the notifier is
// made after it).
func (s *Service) SetFinished(fn func(Finished)) { s.onFinished.Store(&fn) }

// Service orchestrates acquisitions. Create with New, run Run in the
// background.
type Service struct {
	o   Options
	log *slog.Logger

	// series serialises the season requests of a series (seriesLock).
	series seriesLocks

	mu         sync.Mutex
	organizing map[string]bool
	// removing: Remove is dropping the torrent; no organize may start.
	removing map[string]bool
	// lastAdult: the last EventLibraryProgress sent (JSON; publishProgress)
	lastAdult string
	// forced: downloads the admin started or resumed on a full disk; the
	// disk guard does not hold them again (until a restart).
	forced map[string]bool
	// starting: albums (release group, or library folder) an Acquire is
	// starting a download of: a second one at the same time is refused.
	starting  map[string]bool
	wg        sync.WaitGroup
	closing   chan struct{}
	closeOnce sync.Once
	// root bounds organize jobs; cancelled by Close so a long copy does
	// not hold up shutdown (it is redone after the restart).
	root     context.Context
	rootStop context.CancelFunc
	// cacheBytes is the stream cache limit (Options.StreamCacheBytes,
	// changed live from the settings).
	cacheBytes atomic.Int64
	// onFinished: SetFinished's hook (else Options.Finished)
	onFinished atomic.Pointer[func(Finished)]
	onRemoved  atomic.Pointer[func(string)] // SetOnRemoved
	// direct: direct downloads of OPDS sources (direct_job.go)
	direct directJobs
	// files: metafiles of release previews (release_files.go)
	files metafileCache
	// tracks: the song counts of partial albums (single_track.go)
	tracks trackCounts
	// auto: the automatic switch away from dead releases (autoreplace.go)
	auto autoState
	// voSearch: the last «Найти озвучку…» search of each title (voiceovers.go)
	voSearch voSearches
}

// Candidate is a title offered by «Найти и добавить»: a TMDB movie or
// series, or an album (Subtitle = artist), a book (Subtitle = author) or a
// comic series from its catalog (ExtID: MusicBrainz release group, Open
// Library work key).
type Candidate struct {
	TMDBID        int    `json:"tmdbId"`
	ExtID         string `json:"extId"`
	Kind          string `json:"kind"`
	Title         string `json:"title"`
	Subtitle      string `json:"subtitle"`
	Year          int    `json:"year"`
	Poster        string `json:"poster"`
	Overview      string `json:"overview"`
	InLibrary     bool   `json:"inLibrary"`
	LibraryItemID *int64 `json:"libraryItemId"`
	// Aliases: other names of the album (the MusicBrainz disambiguation,
	// «The Black Album»), sent back with the request.
	Aliases []string `json:"aliases,omitempty"`
	// MatchedTrack: music found by a song title — the song of the query
	// the album holds («Sad But True» on «Metallica»); omitted otherwise.
	MatchedTrack string `json:"matchedTrack,omitempty"`
	// ByArtist: music of the artist the query names («metallica» → their
	// albums, listed first) — that artist; omitted for the other matches.
	ByArtist string `json:"byArtist,omitempty"`
	// Explicit: the catalog marks it adult content (Open Library subjects,
	// a MusicBrainz disambiguation): kept from kid profiles.
	Explicit bool `json:"-"`
	// Approx: TMDB found nothing for the query as typed; this title is
	// within a few edits of it («Джентельмены» → «Джентльмены»).
	Approx bool `json:"approx,omitempty"`
}

// ReleaseRequest asks for releases of a TMDB title, or of an album, book,
// audiobook (Title, Artist = artist or author) or comic (Title).
type ReleaseRequest struct {
	TMDBID          int
	ExtID           string
	Kind            string
	Season, Episode int
	Title, Artist   string
	Year            int      // music: releases of another year are excluded
	Aliases         []string // music: other names of the album
}

// AcquireRequest starts watching or downloading a title.
type AcquireRequest struct {
	TMDBID int    `json:"tmdbId"`
	ExtID  string `json:"extId,omitempty"`
	Kind   string `json:"kind"`
	// Title and Artist (artist or author) name an album, book, audiobook
	// or comic (Title only); Year names the album's folder.
	Title  string `json:"title,omitempty"`
	Artist string `json:"artist,omitempty"`
	Year   int    `json:"year,omitempty"`
	// ReleaseTitle: the name of the release ReleaseLink points to (music:
	// kept as evidence for organize).
	ReleaseTitle string `json:"releaseTitle,omitempty"`
	// Aliases: other names of the album (Candidate.Aliases).
	Aliases     []string `json:"aliases,omitempty"`
	Season      int      `json:"season,omitempty"`
	Episode     int      `json:"episode,omitempty"`
	Mode        string   `json:"mode"`                  // stream | download
	ReleaseLink string   `json:"releaseLink,omitempty"` // "" = pick the best
	SourceID    int64    `json:"sourceId,omitempty"`
	UserID      int64    `json:"-"`
	// UserUID is UserID's uid (X-Antresol-User uid); "" = the uid
	// the core lists for UserID now (a request the core just made).
	UserUID string `json:"-"`
	Force   bool   `json:"force,omitempty"` // admin: download even when the disk is full
	// CheckLink: ReleaseLink comes from a profile, not an admin; it is
	// accepted only if it is a magnet or on a host of SourceID (Download
	// would fetch any other URL with the source's session). Never from a client.
	CheckLink bool `json:"-"`
	// Background: no one waits on an HTTP answer («Следить»): a season
	// request may spend more than webFetchBudget on pack file lists.
	// Never from a client.
	Background bool `json:"-"`
	// Poster: the cover of a book, audiobook or comic the client showed
	// (kept only when it is an Open Library cover).
	Poster string `json:"poster,omitempty"`
	// Direct: ReleaseLink is a file of an OPDS source (ScoredRelease.Direct),
	// downloaded as is.
	Direct bool `json:"direct,omitempty"`
	// Replace: the new download takes the place of this title's unfinished
	// ones (CancelTitle after it started).
	Replace bool `json:"replace,omitempty"`
	// Files: the files of ReleaseLink to download (engine indexes, from the
	// preview of «Выбрать релиз…»; admin, music only); nil = all of them.
	Files []int `json:"files,omitempty"`
	// AnyFormat: music — «Скачать в другом формате»: the automatic pick
	// may take a release of another format than «Формат загрузки».
	AnyFormat bool `json:"anyFormat,omitempty"`
	// Resolution: a film or series — the automatic pick takes the best
	// release of this resolution ("1080p") when the search has one, else
	// the best of the others (AcquireResult.Note says so); "" = the
	// household's release preferences alone. «Докачивать новые» or the
	// client's quality (Quality, checked by the handler) sets it.
	Resolution string `json:"-"`
	// Quality: the quality the client asked for («Скачать» asks it):
	// '' | 720p | 1080p | 2160p; the handler copies it into Resolution.
	Quality string `json:"quality,omitempty"`
	// albumDir: AcquireTracks' existing album folder (acquisitionMeta.AlbumDir);
	// never from a client.
	albumDir string
	// auto: AcquireTracks (the import's choice, not the admin's): an album
	// downloading already is that download, as for a one-tap request.
	auto bool
	// ignoreRunning: a running download of the album the import gave up
	// waiting for (TrackAcquireRequest.IgnoreDownload).
	ignoreRunning string
	// replaces: the automatic switch (autoreplace.go) starts this download
	// in place of that dead one; preloaded: the release's metafile or
	// magnet it fetched already. Never from a client.
	replaces  string
	preloaded *cardigann.DownloadResult
	probed    *torrent.SwarmProbe
}

// AcquireResult: for stream, StreamURL plays FileIndex.
type AcquireResult struct {
	InfoHash  string `json:"infoHash"`
	FileIndex int    `json:"fileIndex"`
	StreamURL string `json:"streamUrl"`
	Status    string `json:"status"`
	// Existing: the album was downloading already; this is that download
	// (no second copy is started), or the picked songs were added to it.
	Existing bool `json:"existing,omitempty"`
	// Note: what the admin should know about where the songs go (Russian;
	// localized by the API), e.g. msgNoAlbumFolder.
	Note string `json:"note,omitempty"`
}

// TorrentView is a torrent status with the library side of it.
type TorrentView struct {
	torrent.Status
	Kind          string `json:"kind,omitempty"`
	Title         string `json:"title,omitempty"`
	Organized     bool   `json:"organized"`
	OrganizeError string `json:"organizeError,omitempty"`
	// Note: what organize did besides placing files, e.g. «Уже есть в
	// медиатеке» when every episode was there already (nothing placed).
	Note string `json:"note,omitempty"`
	// AddedByName is the profile that added it (spec: the admin sees who
	// added what); "" when unknown or the profile was deleted.
	AddedByName string `json:"addedByName,omitempty"`
	// Direct: a direct download of an OPDS source (InfoHash "direct-…"),
	// not a torrent; Status downloading, done or error.
	Direct bool `json:"direct,omitempty"`
	// Poster: the cover of the title, a file of the image cache
	// (/api/images/{poster}); "" = none known.
	Poster string `json:"poster,omitempty"`
	// Watched: the requesting profile finished the movie or every episode
	// of the series; set by GET /api/torrents only (nil for other kinds,
	// titles not in the library and the torrent.progress events).
	Watched *bool `json:"watched,omitempty"`
	// TmdbID and Season: the movie or series (and season, 0 = all) it was
	// added for; 0 = not a TMDB title. «Выбрать другую раздачу…» opens
	// that title's page.
	TmdbID int `json:"tmdbId,omitempty"`
	Season int `json:"season,omitempty"`
	// AutoReplace: AutoSwitched — Antresol started this download by
	// itself in place of a release nobody seeded (until it completes or
	// the note is dismissed); AutoNoneAlive — this download got no peer
	// and no live alternative was found. "" = nothing to say.
	AutoReplace string `json:"autoReplace,omitempty"`
	// SelectedTracks of TotalTracks: the songs of a music download whose
	// files were picked («Выбрать треки…»), covers and CUE sheets left
	// out; both 0 for a whole torrent and other kinds.
	SelectedTracks int `json:"selectedTracks,omitempty"`
	TotalTracks    int `json:"totalTracks,omitempty"`
}

// Error kinds; errors returned by the service carry a Russian message and
// match one of these with errors.Is.
var (
	ErrInvalid     = errors.New("acquire: invalid request")
	ErrUnavailable = errors.New("acquire: unavailable")
	ErrNoRelease   = errors.New("acquire: no release")
	ErrNotFound    = errors.New("acquire: not found")
	ErrNoFiles     = errors.New("acquire: no playable file")
	ErrSource      = errors.New("acquire: source failed")
	// ErrConflict: the request clashes with a torrent the engine has.
	ErrConflict = errors.New("acquire: conflict")
	// ErrInProgress: the album is downloading already (the music import or
	// «На сервер»): wait for that download instead of starting another.
	ErrInProgress = errors.New("acquire: album downloading")
	// ErrSourcesFailed comes with ErrNoRelease when some sources did not
	// answer: the release may exist after all.
	ErrSourcesFailed = errors.New("acquire: some sources failed")
)

type userError struct {
	kind error
	msg  string
	err  error
}

func (e *userError) Error() string { return e.msg }
func (e *userError) Is(t error) bool {
	return t == e.kind
}
func (e *userError) Unwrap() error { return e.err }

// UserMessage returns the Russian message of a service error.
func UserMessage(err error) (string, bool) {
	var u *userError
	if errors.As(err, &u) {
		return u.msg, true
	}
	return "", false
}

func uerr(kind error, msg string) error { return &userError{kind: kind, msg: msg} }

func wrapErr(kind error, msg string, err error) error {
	return &userError{kind: kind, msg: msg, err: err}
}

// New creates the service and subscribes it to torrent completion.
func New(o Options) *Service {
	s := &Service{o: o, log: o.Log, organizing: map[string]bool{}, removing: map[string]bool{}, forced: map[string]bool{}, closing: make(chan struct{})}
	s.root, s.rootStop = context.WithCancel(context.Background())
	s.cacheBytes.Store(o.StreamCacheBytes)
	if s.log == nil {
		s.log = slog.Default()
	}
	s.log = s.log.With("component", "acquire")
	cleanDirectTemp(o.TorrentsDir, s.log) // no job runs yet
	cleanPartials(s.log, o.EbooksDir, o.ComicsDir, o.AudiobooksDir, o.MusicDir, o.MoviesDir, o.SeriesDir)
	if o.Disk != nil && o.Disk.Engine == nil {
		o.Disk.Engine = o.Engine
	}
	if o.Disk != nil {
		o.Disk.removed = s.removed // a pruned stream is a removed download
		o.Disk.dropSuperseded = s.dropSuperseded
	}
	o.Engine.OnComplete(s.completed)
	s.watchPeers()
	return s
}

func (s *Service) publish(typ string, data any) {
	if s.o.Bus != nil {
		s.o.Bus.Publish(events.Event{Type: typ, Data: data})
	}
}

// Run organizes torrents that finished while Vault was down, then publishes
// torrent progress every 2 s, checks the disk and re-runs that reconcile
// every 30 s and prunes the stream cache every 5 min, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.backfillPosters(s.root) }()
	s.reconcile(ctx)
	s.checkDisk()
	progress := time.NewTicker(2 * time.Second)
	disk := time.NewTicker(30 * time.Second)
	prune := time.NewTicker(5 * time.Minute)
	auto := time.NewTicker(autoCheckEvery)
	defer auto.Stop()
	defer progress.Stop()
	defer disk.Stop()
	defer prune.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Close()
			return
		case <-progress.C:
			s.publishProgress()
		case <-disk.C:
			s.checkDisk()
			s.reconcile(ctx) // organize anything a missed hook or restart left
		case <-prune.C:
			s.prune(ctx)
		case now := <-auto.C:
			s.checkAutoReplace(ctx, now)
		}
	}
}

// Close stops running organize jobs and waits for them; an interrupted
// one is redone on the next start.
func (s *Service) Close() {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		close(s.closing)
		s.mu.Unlock()
		if s.o.Disk != nil {
			s.o.Disk.Close() // no delayed drop runs on a closed engine
		}
		s.rootStop()
	})
	s.wg.Wait()
}

func (s *Service) prune(ctx context.Context) {
	limit := s.streamCacheBytes()
	if s.o.Disk == nil || limit <= 0 {
		return
	}
	if err := s.o.Disk.PruneStreamCache(ctx, limit); err != nil {
		s.log.Warn("prune stream cache", "err", err)
	}
}

// replaceStreams removes the earlier «Смотреть сейчас» torrents through
// which the same viewer watched what stream a now plays (the same movie,
// or the same episode of a series, as asked for, not as the torrent was
// first added): picking another source replaces the one that viewer
// watched before. Another viewer's stream of the same title stays (paused
// or playing), and so do saved downloads (a promoted stream included); a
// stream being read right now goes a grace after its last reader, unless
// a viewer watches it again meanwhile.
func (s *Service) replaceStreams(ctx context.Context, a acquisition, req AcquireRequest) {
	if s.o.Disk == nil {
		return
	}
	s.o.Disk.Unsupersede(a.InfoHash) // picked again: it is the one watched now
	if a.TMDBID <= 0 || (a.Kind != KindMovie && a.Kind != KindSeries) {
		return
	}
	item := StreamItem{Kind: a.Kind, TMDBID: a.TMDBID}
	if a.Kind == KindSeries {
		item.Season, item.Episode = req.Season, req.Episode
	}
	for _, ih := range s.o.Disk.Watching(a.InfoHash, streamViewer(req), item) {
		h, ok := s.o.Engine.Get(ih)
		if !ok || h.Status().Mode != ModeStream {
			continue
		}
		err := s.dropStream(ctx, ih)
		switch {
		case err == nil:
			s.log.Info("stream replaced by another source", "infohash", ih, "by", a.InfoHash)
		case errors.Is(err, errStreamInUse):
			// the player still has the old source open: it goes once
			// nobody reads it
			s.o.Disk.Supersede(ih)
		case errors.Is(err, torrent.ErrModeChanged), errors.Is(err, ErrNotFound), errors.Is(err, ErrInvalid):
			// a promote or a remove meanwhile: kept
		default:
			s.log.Warn("replace stream", "infohash", ih, "err", err)
		}
	}
}

// streamViewer is who asks for a stream (the profile's uid, else its id;
// "" = an unnamed asker, one viewer).
func streamViewer(req AcquireRequest) string {
	if req.UserUID != "" {
		return "uid:" + req.UserUID
	}
	if req.UserID != 0 {
		return "id:" + strconv.FormatInt(req.UserID, 10)
	}
	return ""
}

// dropStream removes stream torrent ih with its data and acquisition
// unless a reader has it open (errStreamInUse); the core is told.
func (s *Service) dropStream(ctx context.Context, ih string) error {
	return s.dropStreamWith(ctx, ih, func() error { return s.o.Disk.DropStream(ctx, ih) })
}

func (s *Service) dropStreamWith(ctx context.Context, ih string, drop func() error) error {
	err := s.removeWith(ctx, ih, drop)
	if err == nil {
		s.removed(ih)
	}
	return err
}

// dropSuperseded is Disk's call for a superseded stream nobody has read
// since at for supersededGrace. Disk re-checks that under its lock: a
// reader or a viewer back meanwhile keeps it (marked: its release
// schedules it again). One being added to the library or removed right
// now is left to the stream cache limit.
func (s *Service) dropSuperseded(ih string, at time.Time) {
	ctx := s.root
	err := s.dropStreamWith(ctx, ih, func() error { return s.o.Disk.dropDue(ctx, ih, at) })
	switch {
	case err == nil:
		s.log.Info("superseded stream removed", "infohash", ih)
	case errors.Is(err, errStreamInUse), errors.Is(err, torrent.ErrModeChanged), errors.Is(err, ErrNotFound), ctx.Err() != nil:
	default:
		s.log.Warn("remove superseded stream", "infohash", ih, "err", err)
	}
}

// offliner is implemented by *torrent.Engine.
type offliner interface{ Offline() string }

// connectivityReporter is implemented by *torrent.Engine.
type connectivityReporter interface{ Connectivity() torrent.Connectivity }

// Connectivity is whether peers on the internet reach the torrent engine
// (admin); false: the engine cannot tell.
func (s *Service) Connectivity() (torrent.Connectivity, bool) {
	if c, ok := s.o.Engine.(connectivityReporter); ok {
		return c.Connectivity(), true
	}
	return torrent.Connectivity{}, false
}

// Disk returns the current disk usage.
func (s *Service) Disk() (DiskUsage, error) {
	if o, ok := s.o.Engine.(offliner); ok {
		if msg := o.Offline(); msg != "" {
			return DiskUsage{Level: LevelOK, Offline: msg}, nil
		}
	}
	if s.o.Disk == nil {
		return DiskUsage{Level: LevelOK}, nil
	}
	used, total, err := s.o.Disk.Usage()
	if err != nil {
		return DiskUsage{}, err
	}
	return DiskUsage{Used: used, Total: total, Level: LevelOf(used, total)}, nil
}

// checkDisk keeps downloads within the disk (see guardSpace); the
// disk.warning event is the disk monitor's (internal/disk).
func (s *Service) checkDisk() {
	if s.o.Disk == nil {
		return
	}
	used, total, err := s.o.Disk.Usage()
	if err != nil {
		return
	}
	s.guardSpace(context.Background(), used, total)
}

// resumeBelow: waiting_space downloads start again only under this share of
// the disk (a gap to the 95 % hold, so they do not flap).
const resumeBelow = 0.93

// guardSpace is the «ждёт места» queue: at ≥ 95 % every running
// download is put on waiting_space (seeding goes on; downloads the admin
// forced are left alone); under 93 % waiting downloads resume, oldest
// first, as many as fit below 95 % (at least one, which is held again if
// it fills the disk).
func (s *Service) guardSpace(ctx context.Context, used, total int64) {
	if total <= 0 {
		return
	}
	list := s.o.Engine.List()
	sort.SliceStable(list, func(i, j int) bool { return list[i].AddedAt.Before(list[j].AddedAt) })
	s.mu.Lock()
	forced := maps.Clone(s.forced)
	s.mu.Unlock()
	if LevelOf(used, total) == LevelFull {
		for _, st := range list {
			if st.Mode != ModeDownload || st.Status != torrent.StatusDownloading || st.CompletedAt != nil || forced[st.InfoHash] {
				continue
			}
			if err := s.o.Engine.HoldForSpace(ctx, st.InfoHash); err != nil {
				s.log.Warn("hold download for space", "infohash", st.InfoHash, "err", err)
				continue
			}
			s.log.Info("disk is full: download waits for space", "infohash", st.InfoHash)
		}
		return
	}
	if float64(used) >= resumeBelow*float64(total) {
		return
	}
	room := int64(float64(total)*0.95) - used
	started := 0
	for _, st := range list {
		if st.Status != torrent.StatusWaitingSpace {
			continue
		}
		left := max(st.Size-st.Done, 0)
		if started > 0 && left > room {
			continue // does not fit; a smaller later one may
		}
		if err := s.o.Engine.Resume(ctx, st.InfoHash); err != nil {
			s.log.Warn("resume download after space freed", "infohash", st.InfoHash, "err", err)
			continue
		}
		s.log.Info("space freed: download resumes", "infohash", st.InfoHash)
		started++
		room -= left
	}
}

// --- discovery and releases ---

func (s *Service) meta() (Metadata, error) {
	if s.o.Metadata == nil {
		return nil, uerr(ErrUnavailable, "Поиск фильмов и сериалов недоступен: не задан ключ TMDB")
	}
	return s.o.Metadata, nil
}

const discoverLimit = 20

// Discover searches TMDB for movies and series. One kind failing still
// returns the other. TMDB's search forgives no typo: when it finds nothing,
// discoverFuzzy looks again by the words' beginnings and keeps the titles
// within a few edits of the query (Approx).
func (s *Service) Discover(ctx context.Context, q string) ([]Candidate, error) {
	m, err := s.meta()
	if err != nil {
		return nil, err
	}
	q = strings.TrimSpace(q)
	if q == "" {
		return []Candidate{}, nil
	}
	movies, shows, err := searchBoth(ctx, m, q)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.log.Warn("tmdb search", "err", err)
		return nil, wrapErr(ErrUnavailable, "TMDB сейчас недоступен. Попробуйте позже", err)
	}
	approx := false
	if len(movies) == 0 && len(shows) == 0 {
		movies, shows = discoverFuzzy(ctx, m, q)
		approx = true
	}
	out := []Candidate{}
	add := func(kind string, r tmdb.Result) {
		if r.Adult {
			return // adult content: never offered
		}
		c := Candidate{TMDBID: r.ID, Kind: kind, Title: r.Title, Year: r.Year, Overview: r.Overview, Approx: approx}
		if r.Poster != "" {
			c.Poster = m.ImageURL("w342", r.Poster)
		}
		if s.o.Library != nil {
			if it, err := s.o.Library.ItemByTMDB(ctx, kind, int64(r.ID)); err == nil {
				id := it.ID
				c.InLibrary, c.LibraryItemID = true, &id
			}
		}
		out = append(out, c)
	}
	// interleave: TMDB orders each list by relevance
	for i := 0; i < discoverLimit && (i < len(movies) || i < len(shows)); i++ {
		if i < len(movies) {
			add(KindMovie, movies[i])
		}
		if i < len(shows) {
			add(KindSeries, shows[i])
		}
	}
	return out, nil
}

// searchBoth is TMDB's movie and TV search of q at once; err only when
// both failed.
func searchBoth(ctx context.Context, m Metadata, q string) (movies, shows []tmdb.Result, err error) {
	var merr, terr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); movies, merr = m.SearchMovie(ctx, q, 0) }()
	go func() { defer wg.Done(); shows, terr = m.SearchTV(ctx, q, 0) }()
	wg.Wait()
	if merr != nil && terr != nil {
		return nil, nil, merr
	}
	return movies, shows, nil
}

func isVideoKind(k string) bool { return k == KindMovie || k == KindSeries }

// title fetches TMDB details of a title.
func (s *Service) title(ctx context.Context, kind string, id int) (tmdb.Details, error) {
	m, err := s.meta()
	if err != nil {
		return tmdb.Details{}, err
	}
	if !isVideoKind(kind) || id <= 0 {
		return tmdb.Details{}, uerr(ErrInvalid, "Неизвестный фильм или сериал")
	}
	var d tmdb.Details
	if kind == KindMovie {
		d, err = m.Movie(ctx, id)
	} else {
		d, err = m.TV(ctx, id)
	}
	if errors.Is(err, tmdb.ErrNotFound) {
		return d, uerr(ErrNotFound, "Такого фильма или сериала нет в TMDB")
	}
	if err != nil {
		if ctx.Err() != nil {
			return d, ctx.Err()
		}
		return d, wrapErr(ErrUnavailable, "TMDB сейчас недоступен. Попробуйте позже", err)
	}
	if d.Title == "" {
		d.Title = d.OriginalTitle
	}
	return d, nil
}

func searchRequest(kind string, d tmdb.Details, season, episode int) indexer.SearchRequest {
	q := indexer.SearchRequest{Text: d.Title, Kind: kind, Year: d.Year, Season: season, Episode: episode}
	seen := map[string]bool{release.Normalize(d.Title): true}
	for _, t := range append([]string{d.OriginalTitle}, d.AltTitles...) {
		if k := release.Normalize(t); k != "" && !seen[k] {
			seen[k] = true
			q.AltTitles = append(q.AltTitles, t)
		}
	}
	return q
}

// Releases searches all sources for a TMDB title (with its aliases), or
// for an album, book, audiobook or comic by its title and artist/author.
func (s *Service) Releases(ctx context.Context, req ReleaseRequest) (indexer.SearchResponse, error) {
	if isMediaKind(req.Kind) {
		title, artist, err := mediaTitle(req.Kind, req.Title, req.Artist)
		if err != nil {
			return indexer.SearchResponse{}, err
		}
		sr := withAliases(mediaSearchRequest(req.Kind, title, artist), req.Aliases)
		sr.Year = albumYear(req.Kind, title, req.Year)
		return s.search(ctx, sr)
	}
	d, err := s.title(ctx, req.Kind, req.TMDBID)
	if err != nil {
		return indexer.SearchResponse{}, err
	}
	return s.search(ctx, s.titleSearch(ctx, req.Kind, req.TMDBID, d, req.Season, req.Episode))
}

// titleSearch is searchRequest of a film or series; a series ranks the
// voice-over studios of its seasons on the server first.
func (s *Service) titleSearch(ctx context.Context, kind string, tmdbID int, d tmdb.Details, season, episode int) indexer.SearchRequest {
	sr := searchRequest(kind, d, season, episode)
	if kind == KindSeries {
		sr.Studios = s.seriesStudios(ctx, tmdbID)
	}
	return sr
}

func (s *Service) search(ctx context.Context, sr indexer.SearchRequest) (indexer.SearchResponse, error) {
	if s.o.Indexer == nil {
		return indexer.SearchResponse{}, uerr(ErrUnavailable, "Источники раздач не настроены")
	}
	resp, err := s.o.Indexer.Search(ctx, sr)
	if err != nil && ctx.Err() == nil {
		return resp, wrapErr(ErrSource, err.Error(), err)
	}
	return resp, err
}

// --- acquisition ---

type acquisition struct {
	InfoHash        string
	Kind            string
	TMDBID          int
	Title           string
	Year            int
	Season, Episode int
	OrganizedAt     sql.NullString
	Error           string
	Attempts        int
	// ExtID and Meta: music, books, audiobooks and comics (TMDBID 0).
	ExtID string
	Meta  acquisitionMeta
	// NoFiles: organized with nothing placed (every episode was in the
	// library already).
	NoFiles bool
	// Poster: the cover, a file of the image cache ("" = none yet).
	Poster string
	// AddedBy: the profile that asked for it (0 = unknown: the admin's
	// torrent screen, an older row), the id it had then; AddedByUID its
	// uid, which every per-profile read goes by (the id may be
	// another profile's now).
	AddedBy    int64
	AddedByUID string
	// HadPeers: a peer was connected at some point; AutoNote: the
	// automatic switch's note (autoNote*).
	HadPeers bool
	AutoNote string
}

// acquisitionMeta is acquisitions.meta_json: what organize needs besides
// the title.
type acquisitionMeta struct {
	Artist string `json:"artist,omitempty"`
	Author string `json:"author,omitempty"`
	Year   int    `json:"year,omitempty"`
	// Music: the chosen release name and the album's aliases (organize's
	// other-album check).
	Release string   `json:"release,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	// HandPicked: a profile chose the release (AcquireRequest.ReleaseLink).
	HandPicked bool `json:"handPicked,omitempty"`
	// Picked: the admin chose the files (AcquireRequest.Files), dropping
	// some or keeping several albums; organize then skips the other-album
	// check (OrganizeJob.Picked). An untouched single-album default is not.
	Picked bool `json:"picked,omitempty"`
	// RequestedRoot: the album folder in the torrent of the requested album
	// (OrganizeJob.RequestedRoot); "" when none is. Set with Files.
	RequestedRoot string `json:"requestedRoot,omitempty"`
	// AlbumDir: the library folder of the album the tracks are added to
	// (OrganizeJob.AlbumDir); "" = a new album. Set by AcquireTracks only.
	AlbumDir string `json:"albumDir,omitempty"`
	// AlbumDirs: songs picked by hand of albums the library has, per album
	// folder of the torrent (OrganizeJob.AlbumDirs).
	AlbumDirs map[string]string `json:"albumDirs,omitempty"`
	// SelRev counts the times the file selection grew (growSelection). An
	// organize records its result only while the revision it started with
	// is current: an import of the smaller selection, finishing after the
	// growth, leaves the acquisition unorganized for the next one.
	SelRev int `json:"selRev,omitempty"`
	// Seasons: a series pack of several seasons downloading only some of
	// them (season_packs.go): the seasons asked for, sorted (Season is
	// the first). Nil for every other download.
	Seasons []int `json:"seasons,omitempty"`
	// Organized: the seasons of Seasons placed in the library. Growing the
	// pack by a season never takes one off: organized_at is cleared until
	// every season asked for is placed, but these stay done.
	Organized []int `json:"organized,omitempty"`
	// VoiceOver: a voice-over's audio files for a title of the library
	// (KindVoiceOver).
	VoiceOver *voiceOverMeta `json:"voiceOver,omitempty"`
}

const acquisitionCols = `infohash, kind, tmdb_id, title, year, season, episode, organized_at, organize_error, organize_attempts, ext_id, meta_json, paths_json='[]', poster, COALESCE(added_by,0), had_peers, auto_note, COALESCE(added_by_uid,'')`

func scanAcquisition(sc interface{ Scan(...any) error }) (acquisition, error) {
	var a acquisition
	var meta string
	err := sc.Scan(&a.InfoHash, &a.Kind, &a.TMDBID, &a.Title, &a.Year, &a.Season, &a.Episode, &a.OrganizedAt, &a.Error, &a.Attempts, &a.ExtID, &meta, &a.NoFiles, &a.Poster, &a.AddedBy, &a.HadPeers, &a.AutoNote, &a.AddedByUID)
	if err == nil && meta != "" {
		if jerr := json.Unmarshal([]byte(meta), &a.Meta); jerr != nil {
			// one broken row must not hide every torrent: organize goes on
			// without the artist/author
			slog.Warn("acquire: bad acquisitions.meta_json", "infohash", a.InfoHash, "err", jerr)
			a.Meta = acquisitionMeta{}
		}
	}
	return a, err
}

// saveAcquisition records an acquisition. An existing row of the infohash
// is kept, unless replace: then its kind, title, year, ext id and meta are
// replaced and it is organized afresh (a row left from an earlier add must not win over a selection).
// userID and uid are the asker (asker): both or neither are stored.
func (s *Service) saveAcquisition(ctx context.Context, a acquisition, userID int64, uid string, replace bool) error {
	var by, byUID any
	if userID != 0 && uid != "" {
		by, byUID = userID, uid
	}
	meta, err := json.Marshal(a.Meta)
	if err != nil {
		return err
	}
	conflict := `DO NOTHING`
	if replace {
		conflict = `DO UPDATE SET kind=excluded.kind, title=excluded.title, year=excluded.year, ext_id=excluded.ext_id, meta_json=excluded.meta_json, organized_at=NULL, organize_error='', organize_attempts=0`
	}
	_, err = s.o.DB.ExecContext(ctx, `INSERT INTO acquisitions (infohash, kind, tmdb_id, title, year, season, episode, added_by, added_by_uid, ext_id, meta_json)
		VALUES (?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(infohash) `+conflict,
		a.InfoHash, a.Kind, a.TMDBID, a.Title, a.Year, a.Season, a.Episode, by, byUID, a.ExtID, string(meta))
	return err
}

func (s *Service) acquisition(ctx context.Context, ih string) (acquisition, error) {
	return scanAcquisition(s.o.DB.QueryRowContext(ctx, `SELECT `+acquisitionCols+` FROM acquisitions WHERE infohash=?`, ih))
}

func (s *Service) acquisitions(ctx context.Context) (map[string]acquisition, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT `+acquisitionCols+` FROM acquisitions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]acquisition{}
	for rows.Next() {
		a, err := scanAcquisition(rows)
		if err != nil {
			return nil, err
		}
		out[a.InfoHash] = a
	}
	return out, rows.Err()
}

func (s *Service) downloadDir(kind string) string {
	sub := "movies"
	switch kind {
	case KindSeries:
		sub = "series"
	case KindMusic:
		sub = "music"
	case KindBook:
		sub = "books"
	case KindAudiobook:
		sub = "audiobooks"
	case KindComic:
		sub = "comics"
	}
	return filepath.Join(s.o.TorrentsDir, sub)
}

// metaTimeout bounds waiting for a magnet's file list before «Смотреть
// сейчас» gives up (the torrent stays and keeps trying).
var metaTimeout = 60 * time.Second

// Acquire starts streaming or downloading a title. Without ReleaseLink the
// best scored release is taken.
func (s *Service) Acquire(ctx context.Context, req AcquireRequest) (AcquireResult, error) {
	deadline := time.Now().Add(releaseFilesTimeout)
	req.UserID, req.UserUID = s.asker(ctx, req.UserID, req.UserUID)
	if req.Files != nil && (req.Kind != KindMusic || req.Mode != ModeDownload || strings.TrimSpace(req.ReleaseLink) == "" || req.Direct) {
		return AcquireResult{}, uerr(ErrInvalid, msgFilesAlbumOnly)
	}
	if req.Mode != ModeStream && req.Mode != ModeDownload {
		return AcquireResult{}, uerr(ErrInvalid, "Выберите: смотреть сейчас или скачать на сервер")
	}
	if req.Mode == ModeDownload && req.Kind == KindSeries && req.Season <= 0 && strings.TrimSpace(req.ReleaseLink) == "" {
		// a series is downloaded season by season: a whole-series search
		// picks whatever scores best, often a single episode (an admin may
		// still pick a complete-series pack by hand)
		return AcquireResult{}, uerr(ErrInvalid, "Выберите сезон, который нужно скачать")
	}
	var a acquisition
	var sr indexer.SearchRequest
	tmdbPoster := ""
	if isMediaKind(req.Kind) {
		if req.Mode == ModeStream {
			return AcquireResult{}, uerr(ErrInvalid, "Для музыки и книг доступна только загрузка на сервер")
		}
		title, artist, err := mediaTitle(req.Kind, req.Title, req.Artist)
		if err != nil {
			return AcquireResult{}, err
		}
		sr = withAliases(mediaSearchRequest(req.Kind, title, artist), req.Aliases)
		sr.Year = albumYear(req.Kind, title, req.Year)
		a = acquisition{Kind: req.Kind, Title: title, Year: max(req.Year, 0), ExtID: strings.TrimSpace(req.ExtID), Meta: mediaMeta(req.Kind, artist, req.Year)}
	} else {
		d, err := s.title(ctx, req.Kind, req.TMDBID)
		if err != nil {
			return AcquireResult{}, err
		}
		sr = s.titleSearch(ctx, req.Kind, req.TMDBID, d, req.Season, req.Episode)
		a = acquisition{Kind: req.Kind, TMDBID: req.TMDBID, Title: d.Title, Year: d.Year, Season: req.Season, Episode: req.Episode}
		tmdbPoster = d.Poster
	}
	// note: what the asker should know of the pick (AcquireResult.Note)
	note := ""
	// running: a download of this album that runs already ("": none)
	running := ""
	if req.Kind == KindMusic && req.Mode == ModeDownload {
		ih, ok, err := s.runningMusic(ctx, a.ExtID, req.albumDir, req.ignoreRunning)
		if err != nil {
			return AcquireResult{}, err
		}
		if ok {
			running = ih
		}
		// an automatic start (one tap, the import) never makes a second
		// copy: the running download is the answer. A release or tracks
		// picked by hand go ahead: the same release is that download,
		// another one is the admin's choice.
		if auto := req.auto || strings.TrimSpace(req.ReleaseLink) == "" && req.Files == nil; auto {
			if ok {
				return s.existingResult(ih), nil
			}
			release, ok := s.startAlbum(a.ExtID, req.albumDir)
			if !ok {
				return AcquireResult{}, uerr(ErrInProgress, msgInProgress)
			}
			defer release()
		}
	}
	var alts []indexer.ScoredRelease
	sourceID, link, magnet, rel, details, err := s.pick(ctx, req, sr, &alts, &note)
	if err != nil {
		return AcquireResult{}, err
	}
	if relTitle, ok := directRelease(req, rel); ok && isMediaKind(req.Kind) && magnet == "" {
		return s.fetchDirect(ctx, req, a.Title, req.Artist, sourceID, link, relTitle)
	}

	waiting := false
	if req.Mode == ModeDownload && s.o.Disk != nil && s.o.Disk.Level() == LevelFull && !req.Force {
		waiting = true
	}
	if req.Mode == ModeStream {
		s.prune(ctx)
	}
	opt := torrent.AddOptions{Mode: req.Mode, AddedBy: req.UserID, AddedByUID: req.UserUID, Release: rel, WaitingSpace: waiting, SourceURL: details}
	if req.Mode == ModeDownload {
		opt.SaveDir = s.downloadDir(req.Kind)
	}
	if s.o.Library != nil {
		if it, ok := s.libraryItem(ctx, a); ok {
			id := it.ID
			opt.ItemID = &id
		}
	}
	if seasonPackRequest(req) {
		// a second season of the same pack waits for the first to be added
		// and recorded, then joins it (growSeason) instead of being lost
		defer s.series.lock(req.TMDBID)()
	}
	var h torrent.Handle
	var sel selection
	var albumDirs map[string]string
	switch {
	case req.Files != nil:
		var mf []byte
		var conflict string
		mf, sel, conflict, err = s.selectedRelease(ctx, req, deadline)
		if err != nil {
			if conflict != "" && errors.Is(err, ErrConflict) {
				// the release is in «Загрузки» with some songs: these join it
				res, ok, gerr := s.growSelection(ctx, conflict, req)
				if gerr != nil {
					return AcquireResult{}, gerr
				}
				if ok {
					return res, nil
				}
			}
			if running != "" && conflict == running {
				return s.existingResult(running), nil // the release that downloads
			}
			return AcquireResult{}, err
		}
		opt.Files = req.Files
		if req.albumDir == "" && sel.picked {
			albumDirs = s.libraryAlbumDirs(ctx, sel.roots, sel.requestedRoot, a.Title, a.Meta.Artist)
			if opt.ItemID != nil && sel.requestedRoot == "" {
				// the library has the album, the release no folder of it
				note = fmt.Sprintf(msgNoAlbumFolder, a.Title)
				s.log.Info("picked songs: no folder of the library's album in the release, a new album", "album", a.Title, "release", req.ReleaseTitle)
			}
		}
		h, err = s.o.Engine.AddTorrentFile(ctx, mf, opt)
	case seasonPackRequest(req) && len(alts) > 0 && req.preloaded == nil:
		// the best release whose files can be chosen: a pack magnet whose
		// file list does not come is skipped, never added whole
		var joined *AcquireResult
		if h, joined, err = s.addSeasonPick(ctx, req, &a, opt, alts); joined != nil {
			s.checkDisk() // the season joined a running download: hold it if the disk is full
			return *joined, nil
		}
	case magnet != "" && seasonPackRequest(req):
		var joined *AcquireResult
		if h, joined, err = s.addSeason(ctx, req, &a, opt, nil, magnet, mayBePack(releaseTitle(rel, magnet)), deadline); joined != nil {
			s.checkDisk() // the season joined a running download: hold it if the disk is full
			return *joined, nil
		}
	case magnet != "":
		h, err = s.o.Engine.AddMagnet(ctx, magnet, opt)
	default:
		if s.o.Indexer == nil {
			return AcquireResult{}, uerr(ErrUnavailable, "Источники раздач не настроены")
		}
		var res cardigann.DownloadResult
		if req.preloaded != nil {
			res = *req.preloaded
		} else {
			res, err = s.o.Indexer.Download(ctx, sourceID, link)
		}
		if err != nil {
			if ctx.Err() != nil {
				return AcquireResult{}, ctx.Err()
			}
			return AcquireResult{}, wrapErr(ErrSource, err.Error(), err)
		}
		switch {
		case seasonPackRequest(req):
			var joined *AcquireResult
			var mf []byte
			if res.Magnet == "" {
				mf = res.Torrent
			}
			if h, joined, err = s.addSeason(ctx, req, &a, opt, mf, res.Magnet, mayBePack(releaseTitle(rel, res.Magnet)), deadline); joined != nil {
				s.checkDisk() // the season joined a running download: hold it if the disk is full
				return *joined, nil
			}
		case res.Magnet != "":
			h, err = s.o.Engine.AddMagnet(ctx, res.Magnet, opt)
		default:
			h, err = s.o.Engine.AddTorrentFile(ctx, res.Torrent, opt)
		}
	}
	if err != nil {
		if errors.Is(err, torrent.ErrPathInUse) {
			return AcquireResult{}, wrapErr(ErrInvalid, "Эти файлы уже скачиваются другой раздачей", err)
		}
		if _, ok := UserMessage(err); ok {
			return AcquireResult{}, err // a season pack's file list (addSeason)
		}
		if errors.Is(err, torrent.ErrStorageOffline) {
			return AcquireResult{}, wrapErr(ErrUnavailable, "Медиадиск недоступен: загрузки остановлены, пока его не подключат", err)
		}
		return AcquireResult{}, wrapErr(ErrSource, "Не удалось открыть раздачу. Попробуйте другую", err)
	}
	if req.Kind == KindMusic {
		a.Meta.Aliases = release.UsefulAliases(req.Aliases)
		a.Meta.Release = strings.TrimSpace(req.ReleaseTitle)
		a.Meta.HandPicked = strings.TrimSpace(req.ReleaseLink) != "" && req.replaces == ""
		a.Meta.Picked, a.Meta.RequestedRoot = sel.picked, sel.requestedRoot
		a.Meta.AlbumDir = req.albumDir
		a.Meta.AlbumDirs = albumDirs
		if t, _ := directRelease(AcquireRequest{}, rel); t != "" {
			a.Meta.Release = t
		}
	}
	ih := h.InfoHash()
	if req.Force && req.Mode == ModeDownload {
		s.setForced(ih)
	}
	a.InfoHash = ih
	if err := s.saveAcquisition(ctx, a, req.UserID, req.UserUID, req.Files != nil); err != nil {
		return AcquireResult{}, err
	}
	s.capturePoster(ih, s.posterSource(a, tmdbPoster, req.Poster))
	if req.Mode == ModeDownload && !(running != "" && ih == running) {
		s.recordAttempt(ctx, attemptKey(a), ih, req.replaces != "")
		if p, ok := s.o.Engine.(swarmProber); ok {
			if req.probed != nil {
				p.NoteProbe(ih, *req.probed) // the switch probed it already
			} else {
				p.CheckLiveness(ih) // in the background: the answer is not held up
			}
		}
	}
	// Already in the client (watched before, or downloading): reuse it.
	if st := h.Status(); req.Mode == ModeDownload && st.Mode == ModeStream {
		if _, err := s.Promote(ctx, ih, req.Force); err != nil {
			return AcquireResult{}, err
		}
	}
	out := AcquireResult{InfoHash: ih, FileIndex: -1, Existing: running != "" && ih == running, Note: note}
	if req.Mode == ModeStream {
		if s.o.Disk != nil {
			s.o.Disk.Touch(ih)
		}
		wctx, cancel := context.WithTimeout(ctx, metaTimeout)
		err := h.WaitInfo(wctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return AcquireResult{}, ctx.Err()
			}
			return AcquireResult{}, wrapErr(ErrNoFiles, "Раздача пока не отвечает — у неё нет участников. Попробуйте другую", err)
		}
		idx := PickFile(h.Files(), req.Season, req.Episode)
		if idx < 0 {
			return AcquireResult{}, uerr(ErrNoFiles, "В раздаче нет подходящего видео. Попробуйте другую")
		}
		out.FileIndex = idx
		out.StreamURL = StreamURL(ih, idx)
		s.replaceStreams(ctx, a, req)
	}
	out.Status = h.Status().Status
	s.checkDisk()
	return out, nil
}

// startAlbum marks an album (its release group and library folder, ""
// = none) as being started; ok false: another Acquire is starting it.
func (s *Service) startAlbum(extID, albumDir string) (release func(), ok bool) {
	var keys []string
	if extID != "" {
		keys = append(keys, "rg:"+extID)
	}
	if albumDir != "" {
		keys = append(keys, "dir:"+albumDir)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		if s.starting[k] {
			return nil, false
		}
	}
	if s.starting == nil {
		s.starting = map[string]bool{}
	}
	for _, k := range keys {
		s.starting[k] = true
	}
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, k := range keys {
			delete(s.starting, k)
		}
	}, true
}

// selection is what acquisitionMeta keeps of AcquireRequest.Files.
type selection struct {
	picked        bool
	requestedRoot string
	// roots: the album folders of the chosen tracks, sorted.
	roots []string
}

// selectedRelease returns the metafile of the release a selection was made
// in (the preview's, else from the source or the swarm within deadline)
// and checks the selection against its file list. A torrent the engine has
// is a conflict, found before any swarm fetch: its files are chosen already.
// conflict is the infohash of that torrent on an ErrConflict.
func (s *Service) selectedRelease(ctx context.Context, req AcquireRequest, deadline time.Time) ([]byte, selection, string, error) {
	link := strings.TrimSpace(req.ReleaseLink)
	c, ok := s.files.get(cacheKey(req.SourceID, link), time.Now())
	if !ok {
		var added *ReleaseFiles
		var err error
		c, added, err = s.releaseMetafile(ctx, req.SourceID, link, deadline)
		if err != nil {
			return nil, selection{}, "", err
		}
		if added != nil {
			return nil, selection{}, added.InfoHash, uerr(ErrConflict, msgReleaseAdded)
		}
	}
	if _, ok := s.o.Engine.Get(c.infoHash); ok {
		return nil, selection{}, c.infoHash, uerr(ErrConflict, msgReleaseAdded)
	}
	_, _, entries, err := torrent.ParseFiles(c.metafile)
	if err != nil {
		return nil, selection{}, "", wrapErr(ErrInvalid, msgNoFileList, err)
	}
	sel, err := selectionOf(entries, req.Files, req.Title, req.Aliases)
	return c.metafile, sel, "", err
}

// libraryAlbumDirs maps the album folders of a release (roots; requested:
// the requested album's, named title) to the library folders of the
// albums the library has: the requested one by its catalog title, the
// others by their folder's title («1989 - Звезда по имени Солнце»). Songs
// picked by hand go there (OrganizeJob.AlbumDirs), so a later pick of more
// of an album's songs adds them to it instead of making a second one.
func (s *Service) libraryAlbumDirs(ctx context.Context, roots []string, requested, title, artist string) map[string]string {
	if s.o.Library == nil {
		return nil
	}
	var out map[string]string
	for _, root := range roots {
		t := title
		if root != requested {
			if root == "." {
				continue
			}
			t = folderTitle(root)
		}
		it, ok, err := s.o.Library.FindAlbum(ctx, artist, t)
		if err != nil || !ok {
			continue
		}
		if dir := s.ownedAlbumDir(ctx, it.ID); dir != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[root] = dir
		}
	}
	return out
}

// folderTitle is the album title of a release's album folder: «1988 -
// Группа крови (Remastered 2012)» → «Группа крови».
func folderTitle(root string) string {
	folder := path.Base(root)
	_, parsed, _ := library.ParseAlbumFolder(folder)
	if strings.TrimSpace(parsed) == "" {
		parsed = folder
	}
	return organize.AlbumTitle(parsed)
}

// ownedAlbumDir is the folder of the library album id ("" = unknown).
func (s *Service) ownedAlbumDir(ctx context.Context, id int64) string {
	if s.o.Library == nil {
		return ""
	}
	dir, err := s.o.Library.AlbumDir(ctx, id)
	if err != nil {
		if !errors.Is(err, library.ErrNotFound) {
			s.log.Warn("library album folder", "album", id, "err", err)
		}
		return ""
	}
	return dir
}

// existingResult answers a request with the album's running download ih.
func (s *Service) existingResult(ih string) AcquireResult {
	out := AcquireResult{InfoHash: ih, FileIndex: -1, Existing: true}
	if h, ok := s.o.Engine.Get(ih); ok {
		out.Status = h.Status().Status
	}
	return out
}

// selectionOf checks files (engine indexes) against a release's file list:
// at least one track, no padding file, nothing out of range. Picked: the
// selection is not the untouched default (some file of the preview is not
// in it) or its tracks span several albums. requestedRoot: the requested
// album's folder, or the only album's (the preview names it so too).
func selectionOf(entries []torrent.FileEntry, files []int, title string, aliases []string) (selection, error) {
	if len(files) == 0 {
		return selection{}, uerr(ErrInvalid, msgPickSong)
	}
	chosen := map[int]bool{}
	for _, i := range files {
		if i < 0 || i >= len(entries) || entries[i].Padding {
			return selection{}, uerr(ErrInvalid, msgBadFileList)
		}
		chosen[i] = true
	}
	roots := map[string]bool{}
	changed := false
	for i, f := range entries {
		kind := releaseFileKind(f)
		if kind != "" && !chosen[i] {
			changed = true
		}
		if kind == fileKindTrack && chosen[i] {
			tf, _ := tfileOf(f)
			root, _ := organize.AlbumRootOf(tf)
			roots[root] = true
		}
	}
	if len(roots) == 0 {
		return selection{}, uerr(ErrInvalid, msgPickSong)
	}
	sel := selection{picked: changed || len(roots) > 1, requestedRoot: requestedRoot(entries, title, aliases)}
	for r := range roots {
		sel.roots = append(sel.roots, r)
	}
	sort.Strings(sel.roots)
	if all := albumRoots(entries); sel.requestedRoot == "" && len(all) == 1 {
		sel.requestedRoot = all[0]
	}
	return sel, nil
}

// pick chooses the release: the requested one or the best scored (of
// req.Resolution when the search has one: resolutionReleases, its note in
// note). alts (when not nil) gets the search's releases, best first (none
// for the requested one).
func (s *Service) pick(ctx context.Context, req AcquireRequest, sr indexer.SearchRequest, alts *[]indexer.ScoredRelease, note *string) (sourceID int64, link, magnet string, rel json.RawMessage, details string, err error) {
	if l := strings.TrimSpace(req.ReleaseLink); l != "" {
		if strings.HasPrefix(strings.ToLower(l), "magnet:") {
			return req.SourceID, "", l, nil, "", nil
		}
		if req.SourceID <= 0 {
			return 0, "", "", nil, "", uerr(ErrInvalid, "Не указан источник раздачи")
		}
		if req.CheckLink {
			if s.o.Indexer == nil {
				return 0, "", "", nil, "", uerr(ErrUnavailable, "Источники раздач не настроены")
			}
			if err := s.o.Indexer.CheckReleaseLink(ctx, req.SourceID, l); err != nil {
				return 0, "", "", nil, "", wrapErr(ErrInvalid, msgForeignRelease, err)
			}
		}
		return req.SourceID, l, "", nil, "", nil
	}
	resp, err := s.search(ctx, sr)
	if err != nil {
		return 0, "", "", nil, "", err
	}
	if sr.Kind == KindMusic {
		var dropped bool
		if resp.Releases, dropped = formatReleases(resp, req.AnyFormat); dropped && len(resp.Releases) == 0 {
			return 0, "", "", nil, "", noFormatErr(resp.Prefs)
		}
	}
	if isVideoKind(sr.Kind) && req.Resolution != "" && len(resp.Releases) > 0 {
		resp.Releases, *note = resolutionReleases(resp.Releases, req.Resolution)
	}
	if len(resp.Releases) == 0 {
		if len(resp.Failed) > 0 {
			return 0, "", "", nil, "", wrapErr(ErrNoRelease, msgNoMatchSourcesFailed, ErrSourcesFailed)
		}
		return 0, "", "", nil, "", uerr(ErrNoRelease, msgNoMatch)
	}
	if alts != nil {
		*alts = resp.Releases
	}
	best := resp.Releases[0]
	rel, _ = json.Marshal(best)
	if best.Link != "" && !strings.HasPrefix(strings.ToLower(best.Link), "magnet:") {
		return best.SourceID, best.Link, "", rel, best.Details, nil
	}
	m := best.Magnet
	if m == "" {
		m = best.Link
	}
	return best.SourceID, "", m, rel, best.Details, nil
}

// StreamURL is the browser URL of a torrent file.
func StreamURL(ih string, idx int) string {
	return "/api/torrent-stream/" + ih + "/" + strconv.Itoa(idx)
}

// PickFile returns the file to play: for an episode the video whose
// SxxEyy matches (else the only/largest video), otherwise the largest
// video. -1 when there is no video.
func PickFile(files []torrent.FileInfo, season, episode int) int {
	best, bestSize := -1, int64(-1)
	match, matchSize := -1, int64(-1)
	videos := 0
	for _, f := range files {
		if !organize.IsVideo(f.Path) {
			continue
		}
		videos++
		if f.Size > bestSize {
			best, bestSize = f.Index, f.Size
		}
		if episode > 0 {
			info := release.Parse(filepath.Base(f.Path))
			s := info.Season
			if s == 0 {
				s = release.Parse(filepath.Base(filepath.Dir(f.Path))).Season
			}
			end := max(info.EpisodeEnd, info.Episode)
			if info.Episode > 0 && info.Episode <= episode && episode <= end && (s == 0 || season == 0 || s == season) && f.Size > matchSize {
				match, matchSize = f.Index, f.Size
			}
		}
	}
	if match >= 0 {
		return match
	}
	if episode > 0 && videos > 1 {
		return -1 // a season pack without this episode: never play another one
	}
	return best
}

// Promote keeps a «Смотреть сейчас» torrent for good: it is downloaded in
// full into TorrentsDir and organized into the library when complete. On a
// full disk without force (admin only) an incomplete torrent is put on
// waiting_space first, so the rest is downloaded once space frees up (or
// an admin resumes it). Returns the torrent status afterwards.
func (s *Service) Promote(ctx context.Context, ih string, force bool) (string, error) {
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return "", uerr(ErrNotFound, "Раздача не найдена")
	}
	ih = h.InfoHash()
	st := h.Status()
	if st.Mode != ModeStream {
		return st.Status, nil // already kept
	}
	kind := KindMovie
	if a, err := s.acquisition(ctx, ih); err == nil {
		kind = a.Kind
	}
	if force {
		s.setForced(ih)
	}
	// A complete torrent has nothing left to download: it is only moved
	// and organized, so the disk guard does not hold it (nor the playback).
	if !force && st.CompletedAt == nil && s.o.Disk != nil && s.o.Disk.Level() == LevelFull {
		// hold before switching: DownloadAll must not start on a full disk
		if err := s.o.Engine.HoldForSpace(ctx, ih); err != nil {
			return "", notFound(err)
		}
	}
	if err := s.o.Engine.SetModeTo(ctx, ih, ModeDownload, s.downloadDir(kind)); err != nil {
		return "", notFound(err)
	}
	if s.o.Disk != nil {
		s.o.Disk.Unsupersede(ih) // saved: never dropped as a replaced stream
	}
	s.checkDisk()
	return h.Status().Status, nil
}

// OpenStream opens a reader over a torrent file and marks the torrent as
// in use (pruning skips it) until the reader is closed.
func (s *Service) OpenStream(ctx context.Context, ih string, idx int) (torrent.FileInfo, func(ctx context.Context) (io.ReadSeekCloser, error), func(), error) {
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return torrent.FileInfo{}, nil, nil, uerr(ErrNotFound, "Раздача не найдена")
	}
	ih = h.InfoHash()
	wctx, cancel := context.WithTimeout(ctx, metaTimeout)
	err := h.WaitInfo(wctx)
	cancel()
	if err != nil {
		return torrent.FileInfo{}, nil, nil, wrapErr(ErrNoFiles, "Раздача пока не отвечает", err)
	}
	files := h.Files()
	// a file left out of a partial download is not there: a reader would
	// raise its piece priorities and download (and seed) it
	if idx < 0 || idx >= len(files) || !files[idx].Selected {
		return torrent.FileInfo{}, nil, nil, uerr(ErrNotFound, "Файл не найден")
	}
	release := func() {}
	if s.o.Disk != nil {
		release = s.o.Disk.Use(ih)
	}
	open := func(ctx context.Context) (io.ReadSeekCloser, error) {
		// look the torrent up again: after a promote/move the handle's
		// anacrolix torrent is replaced
		h, ok := s.o.Engine.Get(ih)
		if !ok {
			return nil, torrent.ErrNotFound
		}
		return h.NewReader(ctx, idx)
	}
	return files[idx], open, release, nil
}

// StreamFiles are the files of torrent ih as the engine has them (after
// OpenStream, its info is known); nil when it is gone.
func (s *Service) StreamFiles(ih string) []torrent.FileInfo {
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return nil
	}
	return h.Files()
}

// --- torrents (admin) ---

// Torrents lists all torrents with their library state.
func (s *Service) Torrents() []TorrentView {
	list := s.o.Engine.List()
	acqs, err := s.acquisitions(context.Background())
	if err != nil {
		s.log.Warn("load acquisitions", "err", err)
	}
	names := s.userNames(context.Background())
	out := make([]TorrentView, 0, len(list))
	for _, st := range list {
		v := TorrentView{Status: st}
		if st.AddedByUID != "" {
			v.AddedByName = names[st.AddedByUID]
		}
		if a, ok := acqs[st.InfoHash]; ok {
			v.Kind, v.Title, v.Poster = a.Kind, a.Title, a.Poster
			if a.Kind == KindMovie || a.Kind == KindSeries {
				v.TmdbID, v.Season = a.TMDBID, a.Season
			}
			if v.Title == "" {
				v.Title = a.Meta.Artist // a whole artist (no album asked for)
			}
			v.Organized = a.OrganizedAt.Valid
			v.OrganizeError = a.Error
			if v.Organized && a.NoFiles && a.Kind == KindSeries {
				v.Note = msgAlreadyInLibrary
			}
			if v.Error == "" && a.Error != "" {
				v.Error = a.Error
			}
			v.AutoReplace = autoReplaceNote(st, a)
			if a.Kind == KindMusic && st.TotalFiles > 0 && st.SelectedFiles < st.TotalFiles {
				v.SelectedTracks, v.TotalTracks = s.pickedTracks(st.InfoHash, st.SelectedFiles)
			}
		}
		out = append(out, v)
	}
	out = append(out, s.directViews(names)...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt.Before(out[j].AddedAt) })
	return out
}

// VideoItems maps the torrents of movies and series to their library
// items: the item the torrent was added for, else the one with the TMDB id
// of its acquisition (organized after the torrent started).
func (s *Service) VideoItems(ctx context.Context) map[string]int64 {
	out := map[string]int64{}
	acqs, err := s.acquisitions(ctx)
	if err != nil {
		s.log.Warn("load acquisitions", "err", err)
	}
	byKind := map[string]map[int64][]string{} // kind → tmdb id → infohashes
	for _, st := range s.o.Engine.List() {
		a, ok := acqs[st.InfoHash]
		if !ok || (a.Kind != KindMovie && a.Kind != KindSeries) {
			continue
		}
		if st.ItemID != nil {
			out[st.InfoHash] = *st.ItemID
			continue
		}
		if byKind[a.Kind] == nil {
			byKind[a.Kind] = map[int64][]string{}
		}
		byKind[a.Kind][int64(a.TMDBID)] = append(byKind[a.Kind][int64(a.TMDBID)], st.InfoHash)
	}
	for kind, hashes := range byKind {
		if batch := s.o.Library; batch != nil {
			ids := make([]int64, 0, len(hashes))
			for id := range hashes {
				ids = append(ids, id)
			}
			found, err := batch.ItemIDsByTMDB(ctx, kind, ids)
			if err != nil {
				s.log.Warn("library items", "err", err)
				continue
			}
			for tmdb, id := range found {
				for _, ih := range hashes[tmdb] {
					out[ih] = id
				}
			}
			continue
		}
		for tmdb, ihs := range hashes {
			if it, ok := s.libraryItem(ctx, acquisition{Kind: kind, TMDBID: int(tmdb)}); ok {
				for _, ih := range ihs {
					out[ih] = it.ID
				}
			}
		}
	}
	return out
}

// TitleProgress is the aggregate download progress of one title, what the
// download button's ring needs: numbers only, no names, hashes or trackers.
type TitleProgress struct {
	Status string `json:"status"` // "active" | "paused" | "done"
	Done   int64  `json:"done"`
	Size   int64  `json:"size"`
	// Stalled (active only): torrent.StalledNoPeers or StalledNoMetadata
	// when every active torrent of the title is stalled.
	Stalled string `json:"stalled,omitempty"`
	// Liveness, TrackerError and TrackerErrorKind (active only): as in
	// torrent.Status, when every active torrent of the title is stalled
	// or unseen (stallSum).
	Liveness         string `json:"liveness,omitempty"`
	TrackerError     string `json:"trackerError,omitempty"`
	TrackerErrorKind string `json:"trackerErrorKind,omitempty"`
}

// TitleProgress sums the download-mode torrents of a movie or series.
// season 0 means the whole title (packs included); a season counts only
// that season's own downloads — what «Отменить загрузку» there cancels
// (CancelTitle), so the ring never shows a download its menu cannot stop. Active torrents win over paused ones; with neither, the
// title is done when its newest torrent is seeding or organized. false:
// nothing to draw (no download, a stream-only torrent, or a failed one).
func (s *Service) TitleProgress(ctx context.Context, kind string, tmdbID, season int) (TitleProgress, bool) {
	acqs, err := s.acquisitions(ctx)
	if err != nil {
		s.log.Warn("load acquisitions", "err", err)
		return TitleProgress{}, false
	}
	return s.titleProgress(acqs, s.o.Engine.List(), kind, tmdbID, season)
}

// TitleKey names a title of TitlesProgress (Season 0: the whole title).
type TitleKey struct {
	Kind   string
	TMDBID int
	Season int
}

// TitlesProgress is TitleProgress of each of keys, from one read of the
// acquisitions and the engine's list (a search's cards); the titles with
// nothing downloading are left out.
func (s *Service) TitlesProgress(ctx context.Context, keys []TitleKey) map[TitleKey]TitleProgress {
	out := map[TitleKey]TitleProgress{}
	acqs, err := s.acquisitions(ctx)
	if err != nil {
		s.log.Warn("load acquisitions", "err", err)
		return out
	}
	list := s.o.Engine.List()
	for _, k := range keys {
		if p, ok := s.titleProgress(acqs, list, k.Kind, k.TMDBID, k.Season); ok {
			out[k] = p
		}
	}
	return out
}

func (s *Service) titleProgress(acqs map[string]acquisition, list []torrent.Status, kind string, tmdbID, season int) (TitleProgress, bool) {
	var active, paused, done TitleProgress
	var stalls stallSum
	var newestAt time.Time
	var newestDone, found bool
	for _, st := range list {
		a, ok := acqs[st.InfoHash]
		if !ok || st.Mode != torrent.ModeDownload || a.Kind != kind || a.TMDBID != tmdbID {
			continue
		}
		if season != 0 && !a.coversSeason(season) {
			continue
		}
		if season != 0 && a.isPack() {
			st = s.seasonOf(st, a, season) // that season's files only
		}
		isDone := st.Status == torrent.StatusSeeding || a.OrganizedAt.Valid
		if !found || st.AddedAt.After(newestAt) {
			newestAt, newestDone, found = st.AddedAt, isDone, true
		}
		switch st.Status {
		case torrent.StatusQueued, torrent.StatusDownloading, torrent.StatusWaitingSpace:
			active.Done, active.Size = active.Done+st.Done, active.Size+st.Size
			active.Status = "active"
			if st.Status == torrent.StatusWaitingSpace {
				stalls.moving = true // waits for space, not for peers
			} else {
				stalls.add(st)
			}
		case torrent.StatusPaused:
			paused.Done, paused.Size = paused.Done+st.Done, paused.Size+st.Size
			paused.Status = "paused"
		default:
			if isDone {
				done.Done, done.Size = done.Done+st.Done, done.Size+st.Size
				done.Status = "done"
			}
		}
	}
	switch {
	case active.Status != "":
		active.Stalled = stalls.result()
		active.Liveness = stalls.liveness()
		if active.Stalled != "" {
			active.TrackerError, active.TrackerErrorKind = stalls.trackerError, stalls.trackerKind
		}
		return active, true
	case paused.Status != "":
		return paused, true
	case done.Status != "" && newestDone:
		return done, true
	}
	return TitleProgress{}, false
}

// CancelTitle removes, with their data, the unfinished download-mode
// torrents of a movie or series, except keep. A season other than 0
// removes that season's torrents only: unlike TitleProgress, not a
// whole-series pack (season 0), which another adult may have started for
// every season; season 0 removes all of the title's. A pack downloading
// several seasons asked for one by one (acquisitionMeta.Seasons) loses
// only that season's files (cancelSeason; a placed season stays), and is
// removed only with the last season asked for. A finished or organized
// download stays. It returns how many it removed or shrank.
func (s *Service) CancelTitle(ctx context.Context, kind string, tmdbID, season int, keep string) (int, error) {
	acqs, err := s.acquisitions(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, st := range s.o.Engine.List() {
		a, ok := acqs[st.InfoHash]
		if !ok || st.Mode != torrent.ModeDownload || a.Kind != kind || a.TMDBID != tmdbID || a.OrganizedAt.Valid {
			continue
		}
		if season != 0 && !a.coversSeason(season) {
			continue
		}
		if keep != "" && strings.EqualFold(st.InfoHash, keep) {
			continue
		}
		switch st.Status {
		case torrent.StatusQueued, torrent.StatusDownloading, torrent.StatusWaitingSpace, torrent.StatusPaused:
		default:
			continue
		}
		if season != 0 && a.isPack() && len(a.Meta.Seasons) > 1 && slices.Contains(a.Meta.Seasons, season) {
			// a pack with other seasons asked for: only this one leaves it
			done, err := s.cancelSeason(ctx, a, season)
			if err != nil {
				return n, err
			}
			if done {
				n++
			}
			continue
		}
		if err := s.Remove(ctx, st.InfoHash, true); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// LocalizeTorrents renders the Russian texts of the views (made in the
// background: engine errors, organize errors, notes) in the language of
// ctx; Torrents() itself keeps them Russian. It changes vs in place.
func LocalizeTorrents(ctx context.Context, vs []TorrentView) []TorrentView {
	for i := range vs {
		vs[i].Error = i18n.Localize(ctx, vs[i].Error)
		vs[i].OrganizeError = i18n.Localize(ctx, vs[i].OrganizeError)
		vs[i].Note = i18n.Localize(ctx, vs[i].Note)
	}
	return vs
}

// userNames maps profile uids to names (for «Добавил: …»).
func (s *Service) userNames(ctx context.Context) map[string]string {
	out := map[string]string{}
	for _, u := range s.profiles(ctx) {
		if u.UID != "" {
			out[u.UID] = u.Name
		}
	}
	return out
}

// profiles are the core's profiles (nil without Users or when the core
// did not answer: logged).
func (s *Service) profiles(ctx context.Context) []addonkit.User {
	if s.o.Users == nil {
		return nil
	}
	us, err := s.o.Users.Profiles(ctx)
	if err != nil {
		s.log.Warn("load profiles", "err", err)
		return nil
	}
	return us
}

// profileID is the id the profile of uid has now; 0 when it is gone, or
// the profiles cannot be read.
func (s *Service) profileID(ctx context.Context, uid string) int64 {
	if uid == "" {
		return 0
	}
	for _, u := range s.profiles(ctx) {
		if u.UID == uid {
			return u.ID
		}
	}
	return 0
}

// asker is the profile a request's rows name: id and uid as given;
// without a uid, the uid the core lists for id now (a request the core
// or a session made just now, so the id is still that profile's);
// neither when the profile cannot be told.
func (s *Service) asker(ctx context.Context, id int64, uid string) (int64, string) {
	if id <= 0 {
		return 0, ""
	}
	if uid != "" {
		return id, uid
	}
	for _, u := range s.profiles(ctx) {
		if u.ID == id && u.UID != "" {
			return id, u.UID
		}
	}
	return 0, ""
}

func (s *Service) setForced(ih string) {
	s.mu.Lock()
	s.forced[strings.ToLower(ih)] = true
	s.mu.Unlock()
}

// growErr is the error of a selection that did not grow: the torrent's
// files being moved says so, anything else is the source's failure.
func growErr(err error) error {
	if errors.Is(err, torrent.ErrBusy) {
		return notFound(err)
	}
	return wrapErr(ErrSource, msgGrowFailed, err)
}

func notFound(err error) error {
	if errors.Is(err, torrent.ErrNotFound) {
		return uerr(ErrNotFound, "Раздача не найдена")
	}
	if errors.Is(err, torrent.ErrBusy) {
		return wrapErr(ErrInvalid, "Файлы раздачи сейчас переносятся. Попробуйте через минуту", err)
	}
	if errors.Is(err, torrent.ErrStorageOffline) {
		return wrapErr(ErrUnavailable, "Медиадиск недоступен: раздачи остановлены, пока его не подключат", err)
	}
	return err
}

// Pause pauses a torrent (admin).
func (s *Service) Pause(ctx context.Context, ih string) error {
	if IsDirect(ih) {
		return uerr(ErrInvalid, "Прямую загрузку нельзя приостановить")
	}
	return notFound(s.o.Engine.Pause(ctx, ih))
}

// Resume resumes a paused or waiting torrent (admin). A download resumed
// from waiting_space («Загрузить всё равно») is not held by the disk guard
// again.
func (s *Service) Resume(ctx context.Context, ih string) error {
	if IsDirect(ih) {
		return uerr(ErrInvalid, "Прямую загрузку нельзя возобновить")
	}
	h, ok := s.o.Engine.Get(ih)
	if ok && h.Status().Status == torrent.StatusWaitingSpace {
		defer s.setForced(h.InfoHash())
	}
	return notFound(s.o.Engine.Resume(ctx, ih))
}

// Organizing reports whether the torrent ih is being put into the library
// right now.
func (s *Service) Organizing(ih string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.organizing[ih] || s.organizing[strings.ToLower(ih)]
}

// Remove drops a torrent (admin). Library files created from it stay:
// they are separate links/copies. With deleteData the torrent's own data
// goes, never a file the library has: a torrent seeding the library's
// files in place (moved over from another client, or downloaded into a
// library folder) leaves those, and the folders holding them. Taking a
// title out of the library is the title page's delete (files.ItemDelete).
func (s *Service) Remove(ctx context.Context, ih string, deleteData bool) error {
	err := s.remove(ctx, ih, deleteData)
	if err == nil {
		s.removed(ih)
	}
	return err
}

func (s *Service) remove(ctx context.Context, ih string, deleteData bool) error {
	if IsDirect(ih) {
		return s.removeDirect(ih)
	}
	return s.removeWith(ctx, ih, func() error {
		if deleteData {
			if dk, root, keep, ok, err := s.libraryKeep(ctx, ih); err != nil {
				return err
			} else if ok {
				return dk.RemoveKeeping(ctx, ih, root, keep)
			}
		}
		return s.o.Engine.Remove(ctx, ih, deleteData)
	})
}

// removeWith is Remove with drop removing the torrent from the engine.
func (s *Service) removeWith(ctx context.Context, ih string, drop func() error) error {
	key := strings.ToLower(ih)
	s.mu.Lock()
	if s.organizing[key] {
		s.mu.Unlock()
		return uerr(ErrInvalid, "Раздача сейчас добавляется в медиатеку. Попробуйте через минуту")
	}
	// No organize may start while the torrent (and maybe its data) goes.
	s.removing[key] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.removing, key)
		delete(s.forced, key)
		s.forgetTracks(key)
		s.mu.Unlock()
	}()
	if err := drop(); err != nil {
		return notFound(err)
	}
	if _, err := s.o.DB.ExecContext(ctx, `DELETE FROM acquisitions WHERE infohash=?`, strings.ToLower(ih)); err != nil {
		s.log.Warn("delete acquisition", "infohash", ih, "err", err)
	}
	return nil
}

// --- organize ---

// completed is the engine's OnComplete hook. It fires for every mode;
// only downloads are organized.
func (s *Service) completed(st torrent.Status) {
	if st.Mode != ModeDownload {
		return
	}
	s.startOrganize(st.InfoHash, false)
}

// reconcile organizes complete downloads that were not organized yet (a
// restart in the middle, or a completion while organize was failing).
func (s *Service) reconcile(ctx context.Context) {
	acqs, err := s.acquisitions(ctx)
	if err != nil {
		s.log.Warn("load acquisitions", "err", err)
		return
	}
	list := s.o.Engine.List()
	live := make(map[string]bool, len(list))
	for _, st := range list {
		live[st.InfoHash] = true
		a, ok := acqs[st.InfoHash]
		// persisted completion, not the live status: a paused or still
		// re-verifying torrent that finished before is organized too
		if !ok || a.OrganizedAt.Valid || a.Attempts >= maxOrganizeAttempts || st.Mode != ModeDownload || st.CompletedAt == nil {
			continue
		}
		s.startOrganize(st.InfoHash, false)
	}
	// Torrents pruned from the stream cache or removed elsewhere.
	for ih := range acqs {
		if !live[ih] {
			if _, err := s.o.DB.ExecContext(ctx, `DELETE FROM acquisitions WHERE infohash=?`, ih); err != nil {
				s.log.Warn("delete acquisition", "infohash", ih, "err", err)
			}
		}
	}
}

// RetryOrganize organizes a finished download again (admin, after fixing
// e.g. folder permissions).
func (s *Service) RetryOrganize(ctx context.Context, ih string) error {
	return s.retryOrganize(ctx, ih, false)
}

// ForceOrganize organizes a finished download without the other-album
// check (admin: «Разложить всё равно» after «Скачан другой альбом»).
func (s *Service) ForceOrganize(ctx context.Context, ih string) error {
	return s.retryOrganize(ctx, ih, true)
}

func (s *Service) retryOrganize(ctx context.Context, ih string, force bool) error {
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return uerr(ErrNotFound, "Раздача не найдена")
	}
	st := h.Status()
	if st.Mode != ModeDownload || st.CompletedAt == nil {
		return uerr(ErrInvalid, "Раздача ещё не скачана")
	}
	if _, err := s.acquisition(ctx, st.InfoHash); err != nil {
		return uerr(ErrNotFound, "Эта раздача добавлена не через поиск — Antresol не знает, что в ней")
	}
	s.startOrganizeJob(st.InfoHash, true, force)
	return nil
}

func (s *Service) startOrganize(ih string, manual bool) { s.startOrganizeJob(ih, manual, false) }

// startOrganizeJob organizes in the background; force skips the
// other-album check (OrganizeJob.Force).
func (s *Service) startOrganizeJob(ih string, manual, force bool) {
	s.mu.Lock()
	if s.organizing[ih] || s.removing[ih] {
		s.mu.Unlock()
		return
	}
	select {
	case <-s.closing:
		s.mu.Unlock()
		return
	default:
	}
	s.organizing[ih] = true
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.organizing, ih)
			s.mu.Unlock()
		}()
		s.organize(s.root, ih, manual, force)
	}()
}

func (s *Service) organize(ctx context.Context, ih string, manual, force bool) {
	a, err := s.acquisition(ctx, ih)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			s.log.Warn("load acquisition", "infohash", ih, "err", err)
		}
		return // not added through acquire (e.g. imported): nothing to do
	}
	if a.OrganizedAt.Valid || (!manual && a.Attempts >= maxOrganizeAttempts) {
		return
	}
	h, ok := s.o.Engine.Get(ih)
	if !ok {
		return
	}
	// only the selected files: the others are not downloaded (or hold a
	// boundary piece only)
	var files []torrent.FileInfo
	for _, f := range h.Files() {
		if f.Selected {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		// Not ready (data being moved after a promote, or no metadata yet):
		// not an attempt; the post-move hook or the next reconcile runs it.
		return
	}
	if a.Kind == KindVoiceOver {
		s.organizeVoiceOver(ctx, a, files)
		return
	}
	// a pack of several seasons: only the seasons asked for and not placed
	// yet (season_packs.go)
	var placing []int
	if a.Kind == KindSeries && len(a.Meta.Seasons) > 0 {
		files, placing = packToPlace(files, a.Meta)
		if len(files) == 0 {
			s.packPlaced(ctx, a)
			return
		}
	}
	job := OrganizeJob{
		Kind: a.Kind, Title: a.Title, Year: a.Year, TMDBID: a.TMDBID, Season: a.Season, Episode: a.Episode,
		Files: filesOf(files), MoviesDir: s.o.MoviesDir, SeriesDir: s.o.SeriesDir,
		MusicDir: s.o.MusicDir, EbooksDir: s.o.EbooksDir, AudiobooksDir: s.o.AudiobooksDir, ComicsDir: s.o.ComicsDir,
		Artist: a.Meta.Artist, Author: a.Meta.Author, Release: a.Meta.Release, Aliases: a.Meta.Aliases, HandPicked: a.Meta.HandPicked, Force: force,
		Picked: a.Meta.Picked, RequestedRoot: a.Meta.RequestedRoot, AlbumDir: a.Meta.AlbumDir, AlbumDirs: a.Meta.AlbumDirs,
	}
	if s.o.FFprobe != "" && (a.Kind == KindMovie || a.Kind == KindSeries) {
		job.Probe = organize.MediaProber(s.o.FFprobe)
	}
	if sf := s.o.Library; sf != nil && a.Kind == KindSeries {
		if dir, err := sf.SeriesFolder(ctx, s.o.SeriesDir, int64(a.TMDBID)); err == nil {
			job.SeriesFolder = dir
		}
	}
	if he := s.o.Library; he != nil && a.Kind == KindSeries && a.TMDBID > 0 {
		have, herr := he.HaveEpisodes(ctx, int64(a.TMDBID))
		if herr != nil {
			s.log.Warn("episodes in the library", "infohash", ih, "err", herr) // organize all of them
		}
		job.Have = have
	}
	paths, err := organize.Organize(ctx, job)
	if err == nil && s.o.Library != nil {
		err = s.importPlaced(ctx, ih, a.Kind, paths)
	}
	if err != nil && ctx.Err() != nil {
		return // shutting down: not an attempt, redone after the restart
	}
	if err != nil {
		s.log.Error("organize torrent", "infohash", ih, "err", err)
		if _, dberr := s.o.DB.ExecContext(ctx, `UPDATE acquisitions SET organize_error=?, organize_attempts=organize_attempts+1 WHERE infohash=?
			AND `+selRevIs, err.Error(), ih, a.Meta.SelRev); dberr != nil {
			s.log.Error("save organize error", "infohash", ih, "err", dberr)
		}
		s.finished(Finished{Key: ih, Kind: a.Kind, Title: a.Title, Year: a.Year, Failed: true, Error: err.Error()})
		return
	}
	if to := s.o.Library; to != nil && a.AddedByUID != "" {
		// the titles are the asker's too («Моя медиатека»), by uid: a
		// profile given the asker's id since has no claim on them; a
		// failure costs only that filter, not the organize
		if owner := s.profileID(ctx, a.AddedByUID); owner != 0 {
			if cerr := to.ClaimRef(ctx, ih, owner); cerr != nil {
				s.log.Warn("record the title's owner", "infohash", ih, "err", cerr)
			}
		}
	}
	if paths == nil {
		paths = []string{} // "[]": nothing placed (see acquisition.NoFiles)
	}
	raw, _ := json.Marshal(paths)
	now := time.Now().UTC().Format(time.RFC3339)
	q := `UPDATE acquisitions SET organized_at=?, organize_error='', organize_attempts=organize_attempts+1, paths_json=? WHERE infohash=? AND ` + selRevIs
	args := []any{now, string(raw), ih, a.Meta.SelRev}
	if placing != nil {
		// a pack: the seasons placed now join those placed before
		b, _ := json.Marshal(union(a.Meta.Organized, placing))
		q = `UPDATE acquisitions SET organized_at=?, organize_error='', organize_attempts=organize_attempts+1, paths_json=?,
			meta_json=json_set(COALESCE(NULLIF(meta_json,''),'{}'), '$.organized', json(?)) WHERE infohash=? AND ` + selRevIs
		args = []any{now, string(raw), string(b), ih, a.Meta.SelRev}
	}
	res, dberr := s.o.DB.ExecContext(ctx, q, args...)
	if dberr != nil {
		s.log.Error("save organize result", "infohash", ih, "err", dberr)
	} else if n, _ := res.RowsAffected(); n == 0 {
		// the selection grew meanwhile: the larger one is imported once it
		// completes (its hook, or reconcile)
		s.log.Info("organized an older file selection; the grown one is imported when it completes", "infohash", ih)
	}
	s.log.Info("organized torrent into the library", "infohash", ih, "files", len(paths))
	if len(paths) == 0 {
		s.log.Info("every episode of the torrent is in the library already: nothing placed", "infohash", ih)
	}
	if s.o.Changed != nil {
		s.o.Changed()
	}
	s.publish(events.LibraryUpdated, map[string]any{"infoHash": ih, "kind": a.Kind, "tmdbId": a.TMDBID, "extId": a.ExtID, "title": a.Title})
	s.finished(Finished{Key: ih, Kind: a.Kind, Title: a.Title, Year: a.Year})
}

// importPlaced puts what organize placed into the library in one import
// (the core scans, then binds the files to the download ih): the
// Bindable paths, and for comics the folders of pages (a volume's file is
// its folder). A placed file the library does not have afterwards fails
// the organize, as does one another addon has; a folder of pages that is
// no volume (a cover next to the archives) is passed over.
func (s *Service) importPlaced(ctx context.Context, ih, kind string, paths []string) error {
	var files, folders []string
	for _, p := range paths {
		if organize.Bindable(kind, p) {
			files = append(files, p) // not a .cue, .log or picture: the scanner keeps no file row for those
		}
	}
	if kind == KindComic {
		folders = organize.PageFolders(paths)
	}
	if len(files)+len(folders) == 0 {
		return nil
	}
	_, missing, taken, ierr := s.o.Library.Import(ctx, append(slices.Clone(files), folders...), ih, 0)
	if ierr != nil {
		s.log.Warn("import into the library", "infohash", ih, "err", ierr)
		if len(files) == 0 {
			return i18n.Errorf("Не удалось связать папку %s с раздачей", filepath.Base(folders[0]))
		}
		return i18n.Errorf("Не удалось связать файл %s с раздачей", filepath.Base(files[0]))
	}
	isFolder := make(map[string]bool, len(folders))
	for _, d := range folders {
		isFolder[d] = true
	}
	for _, p := range missing {
		if isFolder[p] {
			continue // pictures that are no volume
		}
		s.log.Warn("placed file not in the library after the import", "infohash", ih, "path", p)
		return i18n.Errorf("Файл %s не появился в медиатеке после сканирования", filepath.Base(p))
	}
	for _, p := range taken {
		s.log.Warn("library file belongs to another addon", "infohash", ih, "path", p)
		return i18n.Errorf("Файл %s уже принадлежит другому расширению", filepath.Base(p))
	}
	return nil
}
