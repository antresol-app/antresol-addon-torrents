// Package torrent is Vault's BitTorrent engine on top of anacrolix/torrent.
//
// Persistence: every torrent is a row in `torrents` plus its metafile at
// DataDir/torrents/<infohash>.torrent (magnets keep the URI until metadata
// arrives). Data lives in plain files under the torrent's save dir; piece
// completion is kept in the `torrent_pieces` table of the Vault database, so
// a restart resumes exactly where it stopped. Pieces with no recorded state
// are hash-checked, which is how existing data (e.g. from qBittorrent) is
// imported without re-downloading.
//
// Seeding never stops on its own: completed torrents stay in the client
// until an admin removes or pauses them.
package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	g "github.com/anacrolix/generics"
	atorrent "github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/netguard"
	"golang.org/x/time/rate"
)

// Modes.
const (
	ModeStream   = "stream"   // «Смотреть сейчас»: StreamDir/<infohash>, only what readers request
	ModeDownload = "download" // full download into SaveDir
	ModeSeed     = "seed"     // existing data (import); verified, then seeded
)

// Statuses.
const (
	StatusQueued       = "queued" // waiting for metadata
	StatusDownloading  = "downloading"
	StatusSeeding      = "seeding"
	StatusPaused       = "paused"
	StatusWaitingSpace = "waiting_space"
	StatusError        = "error"
)

var (
	ErrNotFound  = errors.New("torrent: not found")
	ErrClosed    = errors.New("torrent: engine closed")
	ErrBadMode   = errors.New("torrent: unknown mode")
	ErrBusy      = errors.New("torrent: data is being moved")
	ErrPathInUse = errors.New("torrent: data path belongs to another torrent")
	// ErrStorageOffline: the media disk (or one of its folders) is missing;
	// no torrent runs until it is back (see EngineConfig.StorageCheck).
	ErrStorageOffline = errors.New("torrent: media disk is not available")
	// ErrOff: the engine was made with EngineConfig.Off.
	ErrOff = errors.New("torrent: the torrent engine is off on this server")
)

// PathInUseError is ErrPathInUse: the same files (another infohash of the
// same data, or a seed of data another torrent has) are in the engine
// already, at Path, as torrent Owner.
type PathInUseError struct {
	Path  string
	Owner string // its infohash
}

func (p *PathInUseError) Error() string { return p.Unwrap().Error() }

// Unwrap is ErrPathInUse with the Russian message (i18n).
func (p *PathInUseError) Unwrap() error {
	return i18n.Errorf("%w: %s уже используется торрентом %s", ErrPathInUse, p.Path, p.Owner)
}

func pathInUse(path, owner string) error { return &PathInUseError{Path: path, Owner: owner} }

// EngineConfig configures the engine.
type EngineConfig struct {
	DataDir   string // metafiles live in DataDir/torrents
	StreamDir string // stream torrents live in StreamDir/<infohash>
	// DownloadDir is where SetMode(stream→download) moves data when no
	// explicit directory is given; "" keeps the data where it is.
	DownloadDir                string
	ListenPort                 int
	UploadLimit, DownloadLimit int64 // bytes/s, 0 = unlimited
	// ReadTimeout bounds how long one Read of a stream reader waits for data
	// (default 60s). On expiry the Read fails with ErrReadTimeout.
	ReadTimeout time.Duration
	// LocalOnly disables DHT, trackers, PEX, UPnP, IPv6, uTP and binds to
	// 127.0.0.1. For tests.
	LocalOnly bool
	// NoUPnP: never ask the router for ListenPort by UPnP
	// (VAULT_TORRENT_UPNP=off; test and dev servers, whose random ports
	// would stay mapped on the router). LocalOnly implies it.
	NoUPnP bool

	// StallTimeout: a peer connection with outstanding requests and no
	// request resolved (data, reject or cancel) for this long is closed and
	// its address re-added (default 60s). Works around requests that
	// anacrolix v1.61 rarely leaves hanging forever.
	StallTimeout time.Duration

	// StalledAfter: a running download with no peer (or a magnet with no
	// metadata) this long gets Status.Stalled (default 2 min).
	StalledAfter time.Duration
	// IncomingCheckAfter: torrents running this long without one incoming
	// peer connection make Connectivity().IncomingBlocked true (default
	// 15 min).
	IncomingCheckAfter time.Duration

	// StorageCheck reports whether the media disk is there (nil = yes).
	// While it fails, torrents are kept in the database untouched (piece
	// state included) but none is attached, nothing is added and nothing
	// is written: an unmounted USB disk leaves an empty folder on the root
	// disk, and downloading into it would fill the root disk. It is
	// re-checked every StorageRecheck (default 30 s); when it passes, the
	// torrents start as usual. nil = no check.
	StorageCheck   func() error
	StorageRecheck time.Duration

	// Off: no torrent client at all (VAULT_LIBRARY_ONLY). No listening
	// port, no folders created, nothing restored or monitored; the stored
	// torrents stay in the database untouched. Adding and FetchInfo fail
	// with ErrOff, List is empty, Close does nothing. Offline still runs
	// StorageCheck (on every call): scans must not prune an unmounted disk.
	Off bool
	// NoSeeding: «Не раздавать», as it stands at start
	// (SetNoSeeding changes it): see noseed.go.
	NoSeeding bool
	// NoUpload: the client itself never sends file data (anacrolix's
	// NoUpload), whatever the per-torrent calls do. The server sets it when
	// no-seeding is forced for the whole process life; a torrent attached
	// by some future path that forgets DisallowDataUpload still gives
	// nothing. Downloading is not touched.
	NoUpload bool

	tick time.Duration // stats sampling period (tests shorten it)
	// persistEvery: how often the monitor saves the transfer counters
	// (default 30 s; tests shorten it).
	persistEvery time.Duration
	// noAggressiveUpload sets anacrolix DisableAggressiveUpload, which makes
	// Torrent.Seeding() call needData (tests drive its consistency check).
	noAggressiveUpload bool
	tempDir            string // parent of FetchInfo's temp storage ("" = os.TempDir)
}

// AddOptions describe a new torrent.
type AddOptions struct {
	Mode    string
	SaveDir string // ignored for stream (always StreamDir/<infohash>)
	ItemID  *int64
	AddedBy int64 // 0 = unknown
	// AddedByUID is AddedBy's uid; without it the torrent names
	// nobody (AddedBy is not stored either).
	AddedByUID string
	Release    json.RawMessage
	// SourceURL is the tracker page the release came from.
	SourceURL string
	// WaitingSpace adds the torrent without downloading (disk full); Resume
	// starts it.
	WaitingSpace bool
	Paused       bool
	// Files selects the files to download: indexes in Info.UpvertedFiles
	// order (= FileInfo.Index). nil = the whole torrent. Needs the metafile
	// (AddTorrentFile); empty or out-of-range selections are refused, and
	// one naming every file is the same as nil. Fixed once added.
	Files []int
	// history (Replace): the added row takes AddedAt and the transfer
	// counters from it, and CompletedAt too with keepCompleted (a
	// rollback re-adding the very torrent: its OnComplete hooks ran).
	history       *record
	keepCompleted bool
}

// FileInfo describes one file of a torrent.
type FileInfo struct {
	Index    int
	Path     string // inside the torrent, "/"-separated, includes the torrent dir for multi-file torrents
	FullPath string // on disk
	Size     int64
	Progress float64
	// Selected: the file is part of the download (every file when the
	// torrent has no selection).
	Selected bool
}

// Status is a snapshot of a torrent.
type Status struct {
	InfoHash    string     `json:"infoHash"`
	Name        string     `json:"name"`
	Mode        string     `json:"mode"`
	Status      string     `json:"status"`
	Size        int64      `json:"size"`
	Done        int64      `json:"done"`
	DownRate    int64      `json:"downRate"` // bytes/s
	UpRate      int64      `json:"upRate"`
	Uploaded    int64      `json:"uploaded"` // all-time
	Downloaded  int64      `json:"downloaded"`
	Peers       int        `json:"peers"`
	Seeds       int        `json:"seeds"`
	Ratio       float64    `json:"ratio"`
	ItemID      *int64     `json:"itemId"`
	AddedBy     *int64     `json:"addedBy,omitempty"` // profile id, nil = unknown
	AddedByUID  string     `json:"-"`                 // its uid ("" = unknown)
	AddedAt     time.Time  `json:"addedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	SaveDir     string     `json:"saveDir"`
	Error       string     `json:"error,omitempty"`
	// DataMissing: all the files were gone at start; the torrent waits for
	// them to come back or for an admin to resume it (download again).
	DataMissing bool `json:"dataMissing,omitempty"`
	// Parked: finished and out of the swarm, seeding being off (noseed.go).
	Parked bool `json:"parked,omitempty"`
	// SelectedFiles of TotalFiles are downloaded (equal without a
	// selection; both 0 before the metadata). Size and Done cover only the
	// selected files.
	SelectedFiles int `json:"selectedFiles"`
	TotalFiles    int `json:"totalFiles"`
	// Stalled says why a running download makes no headway: StalledNoPeers
	// or StalledNoMetadata ("" = nothing to say). Only a hint: the torrent
	// keeps trying.
	Stalled string `json:"stalled,omitempty"`
	// Liveness: what the probe of the swarm found when the torrent was
	// picked (CheckLiveness): LivenessChecking, LivenessAlive or
	// LivenessUnseen; "" = never probed. Advisory only. A torrent with a
	// peer or a byte is LivenessAlive whatever the probe said.
	Liveness string `json:"liveness,omitempty"`
	// TrackerError: the latest refusal of one of its trackers, redacted
	// (no passkeys), as the tracker put it; TrackerErrorKind classifies it
	// (TrackerBadPasskey, TrackerNotRegistered, TrackerRejected). Set only
	// while it should be downloading (queued or downloading).
	TrackerError     string `json:"trackerError,omitempty"`
	TrackerErrorKind string `json:"trackerErrorKind,omitempty"`
	// HadPeers: a peer has been connected since the torrent was attached
	// (this run of the server). Not part of the API.
	HadPeers bool `json:"-"`
}

// Handle is a live torrent in the engine.
type Handle interface {
	InfoHash() string
	WaitInfo(ctx context.Context) error
	Files() []FileInfo
	NewReader(ctx context.Context, fileIndex int) (io.ReadSeekCloser, error)
	Status() Status
}

// Engine manages all torrents.
type Engine struct {
	cfg    EngineConfig
	db     *sql.DB
	st     *store
	pc     *pieceDB
	ps     *persister // the rows, written off e.mu (nil: Off)
	client *atorrent.Client
	log    *slog.Logger

	mu      sync.Mutex
	entries map[string]*entry
	// fetching holds the metadata fetches in flight (FetchInfo), by hex
	// infohash; add waits for one before attaching the same infohash.
	fetching map[string]*fetch
	hooks    []func(Status)

	// offline is the Russian reason the media disk is unavailable ("" =
	// available); guarded by mu.
	offline     string
	lastStorage time.Time // last recheckStorage run (guarded by mu)
	reqs        *reqTracker
	noSeed      bool          // «Не раздавать», guarded by mu
	upLimit     *rate.Limiter // own instance: SetUploadLimit changes it live
	seq         uint64        // guarded by mu
	stallKicks  atomic.Int64  // stuck connections closed by the watchdog
	idleNudges  atomic.Int64  // request re-plans of idle torrents (checkIdle)
	idleKicks   atomic.Int64  // reconnects of idle torrents' peers (checkIdle)
	incoming    atomic.Int64  // incoming peer handshakes (installIncoming)
	// activeSince: since when torrents have been running without a break
	// (checkActive; guarded by mu).
	activeSince time.Time
	// replan re-plans an idle torrent's requests (checkIdle; guarded by
	// mu). Always the package's replan; tests swap it.
	replan func(t *atorrent.Torrent, files []int)
	// probe runs a liveness probe (prober.probe; tests swap it). self:
	// the IPs peers see us at (prober's self exclusion).
	probe func(ctx context.Context, tg ProbeTarget) SwarmProbe
	self  *selfAddrs
	// peered: torrents a peer completed a handshake with (installPeerSeen).
	peered *peerSeen
	// started, reprobeGrace, reprobeJitter: re-probes wait reprobeGrace
	// after start, and each is put off by up to reprobeJitter (per
	// infohash), so a restart never probes every stalled torrent at once.
	started                     time.Time
	reprobeGrace, reprobeJitter time.Duration
	closing                     chan struct{}
	closeOnce                   sync.Once
	wg                          sync.WaitGroup
}

type entry struct {
	e   *Engine
	ih  string
	rec record // guarded by e.mu
	t   *atorrent.Torrent
	// hold is "", StatusPaused or StatusWaitingSpace.
	hold   string
	moving bool
	// counters: totals = base + stats of the current anacrolix torrent
	baseUp, baseDown int64
	lastUp, lastDown int64
	// saved: the counters the monitor last wrote (savedOK: it has, this
	// run); unchanged counters are not written again.
	savedUp, savedDown int64
	savedOK            bool
	// savedRow: the persister's lastWrite when they were (a full row
	// written since may carry older counters: they are written again)
	savedRow         uint64
	lastAt           time.Time
	upRate, downRate int64
	removed          bool
	seq              uint64 // add order; lower = senior for path conflicts
	nameKnown        bool   // rec.Name comes from the info, not a magnet dn
	dupOf            string // blocked: its files belong to this senior torrent
	// content identifies the data (contentKey; "" = no info yet): two
	// torrents of one name and content are duplicates, of one name and
	// other content namesakes (each gets a path of its own).
	content string
	// relocating: a namesake whose info has just arrived is being moved to
	// a path of its own (reattach); nothing is wanted meanwhile.
	relocating bool
	// dataMissing: all its files were gone at start; nothing is downloaded
	// until Resume.
	dataMissing bool
	reads       readPositions
	// parked: finished and dropped from the client while seeding is off
	// (noseed.go); parkedInfo is its info meanwhile.
	parked     bool
	parkedInfo *metainfo.Info
	// uploadOff: DisallowDataUpload set for seeding being off: a torrent
	// still downloading, or a finished one an open reader keeps attached.
	uploadOff bool
	// readers: open live readers and those being opened.
	readers atomic.Int64
	// checkIdle state (guarded by e.mu): bytes completed when last seen
	// changing, since when, and whether this idle spell was logged.
	idleDone   int64
	idleSince  time.Time
	idleLogged bool
	idleTries  int // actions in this idle spell
	// checkStalled state (guarded by e.mu): since when it has been running,
	// the last tick with a peer, and the hint (Status.Stalled).
	runSince, peersAt time.Time
	stalled           string
	// applied is the anacrolix torrent whose info gotInfo has handled
	// (ownership and priorities); readers wait for it (guarded by e.mu).
	applied *atorrent.Torrent
	held    atomic.Bool // blocked(), for readers without e.mu (syncHeld)
	// holdWake is closed when a hold begins (then dropped; holdWaker makes
	// the next), waking stream Reads blocked waiting for data.
	holdWake atomic.Pointer[chan struct{}]
	// Liveness probes (guarded by e.mu): the torrent's trackers (attach),
	// Status.Liveness, the latest tracker refusal, whether a probe runs
	// and when the last one ended; everPeer: a peer was connected since
	// attach.
	trackers                   []string
	liveness                   string
	trackerErr, trackerErrKind string
	probing                    bool
	probedAt                   time.Time
	lastAlive                  bool // what the last probe found
	private                    bool // the metafile says private (attach)
	everPeer                   bool
}

// New starts the engine and restores every torrent from the database.
func New(cfg EngineConfig, db *sql.DB) (*Engine, error) {
	if cfg.DataDir == "" || cfg.StreamDir == "" {
		return nil, errors.New("torrent: DataDir and StreamDir are required")
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 60 * time.Second
	}
	if cfg.tick <= 0 {
		cfg.tick = time.Second
	}
	if cfg.persistEvery <= 0 {
		cfg.persistEvery = 30 * time.Second
	}
	if cfg.StallTimeout <= 0 {
		cfg.StallTimeout = 60 * time.Second
	}
	if cfg.StorageRecheck <= 0 {
		cfg.StorageRecheck = 30 * time.Second
	}
	if cfg.StalledAfter <= 0 {
		cfg.StalledAfter = defaultStalledAfter
	}
	if cfg.IncomingCheckAfter <= 0 {
		cfg.IncomingCheckAfter = defaultIncomingCheckAfter
	}
	if cfg.Off {
		e := &Engine{cfg: cfg, db: db, log: slog.Default().With("component", "torrent"),
			entries: map[string]*entry{}, fetching: map[string]*fetch{}, closing: make(chan struct{}),
			reqs: newReqTracker(), upLimit: rate.NewLimiter(rate.Inf, 0), replan: replan, self: &selfAddrs{}, peered: &peerSeen{}}
		e.probe = func(context.Context, ProbeTarget) SwarmProbe { return SwarmProbe{} }
		return e, nil
	}
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "torrents"), 0o755); err != nil {
		return nil, err
	}
	offline := storageProblem(cfg.StorageCheck)
	if offline == "" {
		// StreamDir is on the media disk: never create it on the root disk.
		if err := os.MkdirAll(cfg.StreamDir, 0o755); err != nil {
			return nil, err
		}
	}
	e := &Engine{
		cfg:      cfg,
		db:       db,
		st:       &store{db},
		pc:       newPieceDB(db),
		log:      slog.Default().With("component", "torrent"),
		entries:  map[string]*entry{},
		fetching: map[string]*fetch{},
		closing:  make(chan struct{}),
		reqs:     newReqTracker(),
		offline:  offline,
		replan:   replan,
		self:     &selfAddrs{},
		peered:   &peerSeen{},
		started:  time.Now(),
		// a restart: the client announces everything anyway; re-probes
		// wait, then spread out
		reprobeGrace:  5 * time.Minute,
		reprobeJitter: 5 * time.Minute,
	}
	if offline != "" {
		e.log.Error("media disk is not available: torrents are held until it is back", "reason", offline)
	}

	cl, err := newClient(e.clientConfig())
	if err != nil {
		return nil, fmt.Errorf("torrent client: %w", err)
	}
	e.client = cl
	e.initProbe(cl)
	e.ps = newPersister(e.st, e.log)

	if err := e.restore(); err != nil {
		cl.Close()
		e.ps.close(5 * time.Second)
		return nil, err
	}
	e.wg.Add(1)
	go e.monitor()
	return e, nil
}

func (e *Engine) fileStorage(dir string) storage.ClientImpl {
	return newSafeStorage(storage.NewFileOpts(storage.NewFileClientOpts{
		ClientBaseDir:   dir,
		PieceCompletion: e.pc,
		// Final file names from the start: imported data matches and
		// hard-linking/organizing never sees ".part" files.
		UsePartFiles: g.Some(false),
	}))
}

func (e *Engine) metafilePath(ih string) string {
	return filepath.Join(e.cfg.DataDir, "torrents", ih+".torrent")
}

func (e *Engine) streamDir(ih string) string { return filepath.Join(e.cfg.StreamDir, ih) }

// restore loads every stored torrent and, when the media disk is there,
// attaches them.
func (e *Engine) restore() error {
	recs, err := e.st.all(context.Background())
	if err != nil {
		return fmt.Errorf("load torrents: %w", err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.noSeed = e.cfg.NoSeeding
	for _, r := range recs {
		e.seq++
		en := &entry{e: e, ih: r.InfoHash, rec: r, seq: e.seq}
		if r.Status == StatusPaused || r.Status == StatusWaitingSpace {
			en.hold = r.Status
		}
		e.entries[r.InfoHash] = en
	}
	if e.offline == "" {
		e.attachRestored()
	}
	return nil
}

// attachRestored attaches every restored torrent that is not attached yet,
// oldest first. e.mu must be held.
func (e *Engine) attachRestored() {
	type restored struct {
		en   *entry
		spec *atorrent.TorrentSpec
	}
	var todo []restored
	for _, en := range e.bySeq() {
		if en.t != nil || en.removed || en.moving || en.dataMissing || en.parked {
			continue
		}
		spec, err := e.specFor(en.rec)
		if err != nil {
			en.rec.Error = err.Error()
			e.log.Error("restore torrent", "infohash", en.ih, "err", err)
			continue
		}
		if spec.InfoBytes != nil {
			en.nameKnown = true
			en.content = contentKey(spec.InfoBytes)
		}
		todo = append(todo, restored{en, spec})
	}
	waiting := map[*entry]bool{}
	for _, r := range todo {
		waiting[r.en] = true
	}
	for _, r := range todo {
		e.separateNamesake(r.en, waiting)
	}
	for _, r := range todo {
		en, spec := r.en, r.spec
		if spec.InfoBytes != nil {
			e.reconcileData(en, spec)
		}
		if en.dataMissing {
			continue // detached until the files are back or an admin resumes it
		}
		if e.parkRestored(en, spec) {
			continue
		}
		en.rec.Error = ""
		if err := e.attach(en, spec); err != nil {
			en.rec.Error = err.Error()
			e.log.Error("restore torrent", "infohash", en.ih, "err", err)
		}
	}
}

// separateNamesake moves a restored torrent (not attached yet, as every
// torrent of waiting) that shares its path with a namesake (other files)
// to a path of its own, when it may (movesAside) and the namesake may not
// or is senior: e.g. one an earlier version blocked as a duplicate.
// e.mu must be held.
func (e *Engine) separateNamesake(en *entry, waiting map[*entry]bool) {
	if !en.nameKnown || en.content == "" {
		return
	}
	for _, o := range e.bySeq() {
		if o == en || o.removed || !o.nameKnown || o.content == "" || o.content == en.content || !o.usesPath(en.rec.SaveDir, en.rec.Name) {
			continue
		}
		if !e.movesAside(en, o.ih) {
			return
		}
		if waiting[o] && o.seq > en.seq && e.movesAside(o, en.ih) {
			continue // the junior moves when its turn comes
		}
		to := namesakeDir(en.rec.SaveDir, en.ih)
		e.log.Info("restored torrent shares its name with another one: it gets a folder of its own", "infohash", en.ih, "other", o.ih, "dir", to)
		en.rec.SaveDir, en.rec.Error = to, ""
		e.save(en.rec)
		return
	}
}

func dataMissingError(p string) string {
	return fmt.Sprintf("Файлы раздачи не найдены: %s. Верните их на место или нажмите «Продолжить», чтобы скачать заново", p)
}

// bySeq returns the entries in add order. e.mu must be held.
func (e *Engine) bySeq() []*entry {
	out := make([]*entry, 0, len(e.entries))
	for _, en := range e.entries {
		out = append(out, en)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].seq < out[j].seq })
	return out
}

// storageProblem runs check and returns the Russian reason it failed, "" when
// the disk is there.
func storageProblem(check func() error) string {
	if check == nil {
		return ""
	}
	if err := check(); err != nil {
		return fmt.Sprintf("Медиадиск недоступен: %s. Раздачи остановлены, пока диск не подключат", err.Error())
	}
	return ""
}

// recheckStorage (monitor, every StorageRecheck) brings the torrents up
// once the media disk is back, and starts torrents held for missing data
// whose files are back.
func (e *Engine) recheckStorage(now time.Time) {
	e.mu.Lock()
	if now.Sub(e.lastStorage) < e.cfg.StorageRecheck {
		e.mu.Unlock()
		return
	}
	e.lastStorage = now
	wasOffline := e.offline != ""
	type missing struct {
		en   *entry
		save string
	}
	var gone []missing
	if !wasOffline {
		for _, en := range e.entries {
			if en.dataMissing && en.t == nil && !en.removed {
				gone = append(gone, missing{en: en, save: en.rec.SaveDir})
			}
		}
	}
	e.mu.Unlock()
	if !wasOffline && len(gone) == 0 {
		return
	}
	// File system calls without the lock.
	problem := ""
	back := map[*entry]bool{}
	if wasOffline {
		problem = storageProblem(e.cfg.StorageCheck)
		if problem == "" {
			if err := os.MkdirAll(e.cfg.StreamDir, 0o755); err != nil {
				problem = fmt.Sprintf("Медиадиск недоступен: %s", err.Error())
			}
		}
	}
	for _, g := range gone {
		if b, err := os.ReadFile(e.metafilePath(g.en.ih)); err == nil {
			if mi, err := metainfo.Load(bytes.NewReader(b)); err == nil && anyDataExists(g.save, mi.InfoBytes) {
				back[g.en] = true
			}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.closing:
		return
	default:
	}
	if wasOffline {
		if problem != "" {
			e.offline = problem
			return
		}
		e.log.Info("media disk is available again: starting torrents")
		e.offline = ""
	}
	for en := range back {
		if en.dataMissing && en.t == nil {
			e.log.Info("torrent data is back: starting it", "infohash", en.ih)
			en.dataMissing = false
		}
	}
	e.attachRestored()
}

// Offline returns why torrents are held because the media disk is missing
// ("" = the disk is there).
func (e *Engine) Offline() string {
	if e.cfg.Off {
		return storageProblem(e.cfg.StorageCheck)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.offline
}

func (e *Engine) specFor(r record) (*atorrent.TorrentSpec, error) {
	b, err := os.ReadFile(e.metafilePath(r.InfoHash))
	if err == nil {
		mi, err := metainfo.Load(bytes.NewReader(b))
		if err != nil {
			return nil, i18n.Errorf("метафайл повреждён: %w", err)
		}
		return atorrent.TorrentSpecFromMetaInfoErr(mi)
	}
	if r.Magnet != "" {
		return atorrent.TorrentSpecFromMagnetUri(r.Magnet)
	}
	return nil, i18n.Errorf("метафайл не найден: %w", err)
}

// reconcileData finishes an interrupted move and drops piece completion when
// data files have disappeared, so anacrolix re-verifies instead of trusting
// stale records.
func (e *Engine) reconcileData(en *entry, spec *atorrent.TorrentSpec) {
	e.reconcile(en, spec, true)
}

// reconcile is reconcileData; holdMissing=false (an admin's Resume) never
// holds and only forgets the pieces of the missing files.
func (e *Engine) reconcile(en *entry, spec *atorrent.TorrentSpec, holdMissing bool) {
	mi := metainfo.MetaInfo{InfoBytes: spec.InfoBytes}
	inf, err := mi.UnmarshalInfo()
	if err != nil {
		return
	}
	name := inf.BestName()
	r := &en.rec
	if r.TargetDir != "" && !exists(filepath.Join(r.SaveDir, name)) && exists(filepath.Join(r.TargetDir, name)) {
		r.SaveDir, r.TargetDir = r.TargetDir, ""
		e.save(*r)
	}
	// All of the data is gone (moved or deleted outside Vault, or a disk
	// that is not mounted): keep the piece state and the torrent detached
	// (anacrolix would mark the pieces incomplete as soon as it looks at
	// them) with an error, instead of downloading everything again. Resume
	// (admin) downloads it again on purpose.
	if holdMissing && r.Mode != ModeStream && !anyDataExists(r.SaveDir, spec.InfoBytes) && e.pc.anyComplete(en.ih) {
		en.dataMissing = true
		r.Error = dataMissingError(filepath.Join(r.SaveDir, name))
		e.log.Error("torrent data is missing: held", "infohash", en.ih, "path", filepath.Join(r.SaveDir, name))
		return
	}
	// Forget only the pieces of files that are gone; they get re-verified.
	var off int64
	for _, fi := range inf.UpvertedFiles() {
		begin, end := off, off+fi.Length
		off = end
		if fi.Length == 0 {
			continue
		}
		p := filepath.Join(append([]string{r.SaveDir, name}, fi.BestPath()...)...)
		if !exists(p) {
			first, last := int(begin/inf.PieceLength), int((end-1)/inf.PieceLength)
			if err := e.pc.forgetPieces(context.Background(), en.ih, first, last+1); err != nil {
				e.log.Error("forget piece state", "infohash", en.ih, "err", err)
			}
		}
	}
}

// save persists rec, logging failures (callers run in goroutines with no one
// to return the error to). e.mu may or may not be held.
// The write itself is the persister's, after e.mu is released.
func (e *Engine) save(r record) {
	if e.ps != nil {
		e.ps.put(r)
		return
	}
	if err := e.st.update(context.Background(), r); err != nil {
		e.log.Error("persist torrent", "infohash", r.InfoHash, "err", err)
	}
}

// anyDataExists reports whether any file of the torrent is already on disk.
// Unknown info (magnet) counts as no data.
func anyDataExists(saveDir string, infoBytes []byte) bool {
	if infoBytes == nil {
		return false
	}
	inf, err := (&metainfo.MetaInfo{InfoBytes: infoBytes}).UnmarshalInfo()
	if err != nil {
		return false
	}
	for _, fi := range inf.UpvertedFiles() {
		if exists(filepath.Join(append([]string{saveDir, inf.BestName()}, fi.BestPath()...)...)) {
			return true
		}
	}
	return false
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// DirsPresent is a StorageCheck: every dir must exist and be a folder.
func DirsPresent(dirs ...string) func() error {
	return func() error {
		for _, d := range dirs {
			st, err := os.Stat(d)
			if err != nil {
				return i18n.Errorf("нет папки %s", d)
			}
			if !st.IsDir() {
				return i18n.Errorf("%s — не папка", d)
			}
		}
		return nil
	}
}

// attach adds the entry's torrent to the anacrolix client. e.mu must be held.
func (e *Engine) attach(en *entry, spec *atorrent.TorrentSpec) error {
	spec.Storage = e.fileStorage(en.rec.SaveDir)
	if e.cfg.LocalOnly {
		spec.Trackers, spec.Sources = nil, nil // no network: not even a magnet's xs=
	}
	en.trackers = flatTrackers(spec.Trackers)
	en.private = privateInfo(spec.InfoBytes)
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return err
	}
	// TorrentSpec.DisallowDataDownload/Upload are never read by anacrolix
	// v1.61, so a hold is applied here: no upload while paused, and no
	// priorities (applyWanted, once the info is there) while held — never
	// DisallowDataDownload (see holds.go).
	// With seeding off nothing is uploaded from the first moment, not only
	// from the next sweepNoSeeding.
	en.uploadOff = false // decided anew by every attach, never inherited
	if en.hold == StatusPaused {
		t.DisallowDataUpload()
	} else if e.noSeed {
		t.DisallowDataUpload()
		en.uploadOff = true
	}
	en.t = t
	en.baseUp, en.baseDown = en.rec.Uploaded, en.rec.Downloaded
	en.lastUp, en.lastDown, en.lastAt = en.rec.Uploaded, en.rec.Downloaded, time.Now()
	t.SetOnWriteChunkError(func(err error) { go e.writeFailed(en, t, err) })
	e.wg.Add(1)
	go e.watch(en, t)
	return nil
}

func (e *Engine) writeFailed(en *entry, t *atorrent.Torrent, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if en.t != t || strings.HasPrefix(en.rec.Error, writeErrorPrefix) {
		return // stale, or already recorded (failures come in bursts)
	}
	en.rec.Error = fmt.Sprintf("ошибка записи: %s", err.Error()) // starts with writeErrorPrefix
	e.applyWanted(en, t)                                         // blocked now: nothing more is requested
	e.save(en.rec)
}

// watch reacts to metadata arrival and completion of one anacrolix torrent.
func (e *Engine) watch(en *entry, t *atorrent.Torrent) {
	defer e.wg.Done()
	select {
	case <-t.GotInfo():
	case <-t.Closed():
		return
	case <-e.closing:
		return
	}
	files, ok := e.gotInfo(en, t)
	if !ok {
		return
	}
	if files != nil {
		// anacrolix never reports a partial torrent complete.
		e.watchSelection(en, t)
		return
	}
	select {
	case <-t.Complete().On():
	case <-t.Closed():
		return
	case <-e.closing:
		return
	}
	e.completed(en, t)
}

// gotInfo handles the metadata of t and returns the entry's selection;
// ok=false when t is no longer the entry's torrent.
func (e *Engine) gotInfo(en *entry, t *atorrent.Torrent) (files []int, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if en.t != t {
		return nil, false
	}
	en.rec.Name = t.Name()
	en.content = contentKey(t.Metainfo().InfoBytes)
	// A name learned only now (a magnet resolving) is junior to every torrent
	// whose name was already known, whatever the add order: those may
	// already be writing the files.
	newest := !en.nameKnown
	en.nameKnown = true
	if !exists(e.metafilePath(en.ih)) {
		mi := t.Metainfo()
		var buf bytes.Buffer
		if err := mi.Write(&buf); err == nil {
			if err := writeFileAtomic(e.metafilePath(en.ih), buf.Bytes()); err != nil {
				e.log.Error("save metafile", "infohash", en.ih, "err", err)
			} else {
				en.rec.Magnet = ""
			}
		}
	} else {
		en.rec.Magnet = ""
	}
	if e.pc.isFresh(en.ih) {
		if err := e.pc.markAllIncomplete(context.Background(), en.ih, t.NumPieces()); err != nil {
			e.log.Error("store piece state", "infohash", en.ih, "err", err)
		}
	}
	if e.applyOwnership(en, t, newest) {
		e.save(en.rec)
		return nil, false // the watcher of the re-attached torrent goes on
	}
	en.applied = t
	en.rec.Status = e.statusOf(en).Status
	e.save(en.rec)
	return en.rec.Files, true
}

// applyOwnership blocks en when a senior torrent owns its files, otherwise
// starts it per its mode. newest treats en as junior to every known torrent.
// A namesake of the owner (other files) that has downloaded nothing is
// moved to a path of its own instead: true, t is being dropped and en
// re-attached there (reattach). e.mu must be held.
func (e *Engine) applyOwnership(en *entry, t *atorrent.Torrent, newest bool) (relocated bool) {
	owner := en
	if newest {
		owner = nil
	}
	if other := e.pathOwner(owner, en.rec.SaveDir, en.rec.Name, en); other != "" {
		if e.movesAside(en, other) {
			to := namesakeDir(en.rec.SaveDir, en.ih)
			e.log.Info("torrent shares its name with another one: it gets a folder of its own", "infohash", en.ih, "other", other, "dir", to)
			en.rec.SaveDir = to
			en.dupOf, en.rec.Error = "", ""
			en.relocating = true
			clearWanted(t)
			en.syncHeld()
			e.wg.Add(1)
			go e.reattach(en, t)
			return true
		}
		// Never let two torrents write one path; only the junior is blocked.
		en.dupOf = other
		en.rec.Error = fmt.Sprintf("файлы этого торрента совпадают с торрентом %s", other)
		e.applyWanted(en, t)
		return
	}
	if en.dupOf != "" {
		en.dupOf, en.rec.Error = "", ""
	}
	e.applyWanted(en, t)
	return false
}

// reattach drops t, a namesake's torrent attached at the shared path, and
// attaches en again at its own path (applyOwnership).
func (e *Engine) reattach(en *entry, t *atorrent.Torrent) {
	defer e.wg.Done()
	t.Drop()
	e.mu.Lock()
	defer e.mu.Unlock()
	en.relocating = false
	if en.t != t || en.removed || en.moving { // moving: Finish attaches it
		return
	}
	e.foldCounters(en)
	en.t, en.uploadOff = nil, false
	select {
	case <-e.closing: // the next New attaches it from the DB
		return
	default:
	}
	spec, err := e.specFor(en.rec)
	if err == nil {
		err = e.attach(en, spec)
	}
	if err != nil {
		en.rec.Error = err.Error()
		e.log.Error("re-attach torrent at its own path", "infohash", en.ih, "err", err)
	}
	en.rec.Status = e.statusOf(en).Status
	e.save(en.rec)
}

// namesakeDir is the folder of its own a namesake gets in saveDir.
func namesakeDir(saveDir, ih string) string { return filepath.Join(saveDir, ih) }

// contentKey identifies a torrent's files: their paths and sizes, padding
// files left out. Two torrents of the same files have one key whatever
// their piece size, piece hashes, private or source field (a re-upload, a
// v1+v2 hybrid): duplicates, not namesakes. The rare same paths and sizes
// with other bytes are blocked too, which is safer than a second copy.
// "" when the info is unreadable.
func contentKey(infoBytes []byte) string {
	if infoBytes == nil {
		return ""
	}
	inf, err := (&metainfo.MetaInfo{InfoBytes: infoBytes}).UnmarshalInfo()
	if err != nil {
		return ""
	}
	h := sha1.New()
	for _, f := range inf.UpvertedFiles() {
		if isPadding(f) {
			continue
		}
		fmt.Fprintf(h, "%q %d\n", strings.Join(f.BestPath(), "/"), f.Length)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sameContent reports whether torrent ih holds the data content; an
// unknown content on either side counts as the same (blocked, as before
// namesakes were told apart). e.mu must be held.
func (e *Engine) sameContent(ih, content string) bool {
	o := e.entries[ih]
	return o == nil || o.content == "" || content == "" || o.content == content
}

// movesAside reports whether en, whose path other owns, is a namesake that
// may take a path of its own: a download with other files than other's and
// nothing of its own verified yet (its data, if any, is where it is).
// e.mu must be held.
func (e *Engine) movesAside(en *entry, other string) bool {
	return en.rec.Mode == ModeDownload && en.rec.TargetDir == "" && !e.sameContent(other, en.content) && !e.pc.anyComplete(en.ih)
}

// unblockDuplicates re-checks torrents that were blocked by owner (which
// has just been removed). e.mu must be held.
func (e *Engine) unblockDuplicates(owner string) {
	// Senior first: the first one unblocked owns the files, any later one
	// with the same path stays blocked by it.
	for _, d := range e.bySeq() {
		if d.dupOf != owner || d.t == nil || d.t.Info() == nil {
			continue
		}
		e.applyOwnership(d, d.t, false) // clears dupOf/Error when no owner is left
		d.rec.Status = e.statusOf(d).Status
		e.save(d.rec)
	}
}

func (e *Engine) needsMove(en *entry) bool {
	r := en.rec
	return r.Mode != ModeStream && r.TargetDir != "" && r.TargetDir != r.SaveDir
}

// completed records a complete torrent and runs the OnComplete hooks the
// first time. false: its selection grew since it was seen complete
// (AddFiles), nothing recorded — wait again.
func (e *Engine) completed(en *entry, t *atorrent.Torrent) bool {
	e.mu.Lock()
	if en.t != t {
		e.mu.Unlock()
		return true
	}
	if en.rec.Files != nil && !selectionComplete(t, en.rec.Files) {
		e.mu.Unlock()
		return false
	}
	if e.needsMove(en) {
		e.mu.Unlock()
		if e.move(en) != moveRefused {
			// Re-attached (the new watcher reports completion) or skipped
			// (busy, removed, shutting down).
			return true
		}
		// Move refused before touching anything: complete in place.
		e.mu.Lock()
		if en.t != t || en.removed {
			e.mu.Unlock()
			return true
		}
	}
	first := en.rec.CompletedAt == nil
	if first {
		now := time.Now().UTC().Truncate(time.Second)
		en.rec.CompletedAt = &now
	}
	e.foldCounters(en)
	en.rec.Status = e.statusOf(en).Status
	st := e.statusOf(en)
	hooks := append([]func(Status){}, e.hooks...)
	// the completion is stored before the hooks hear of it (off e.mu)
	if err := e.persistUnlock(context.Background(), en.rec); errors.Is(err, ErrNotFound) {
		// its row was dropped by a delete: gone, nothing to organize —
		// unless the delete failed (the torrent stays, its row re-queued);
		// e.mu is held across the delete, so this sees how it ended
		e.mu.Lock()
		removed := en.removed
		e.mu.Unlock()
		if removed {
			return true
		}
	} else if err != nil {
		e.log.Error("persist torrent", "infohash", en.ih, "err", err)
	}
	if first {
		for _, h := range hooks {
			h(st)
		}
	}
	return true
}

// move drops the torrent, moves its data to TargetDir and re-attaches it.
// Piece completion is kept, so nothing is re-downloaded or re-hashed.
// The result says whether the torrent was re-attached (its new watcher then
// handles completion), refused (destination problem; nothing touched, the
// torrent completes in place) or skipped (busy, gone or shutting down).
func (e *Engine) move(en *entry) moveResult {
	e.mu.Lock()
	if en.moving || en.t == nil || en.removed || !e.needsMove(en) {
		e.mu.Unlock()
		return moveSkipped
	}
	select {
	case <-e.closing:
		e.mu.Unlock()
		return moveSkipped
	default:
	}
	t := en.t
	from, to, name := en.rec.SaveDir, en.rec.TargetDir, ""
	if info := t.Info(); info != nil {
		name = info.BestName()
	}
	// Check before dropping the torrent: a failed move leaves it attached
	// in place, seeding, with the error shown and no further move attempts.
	if err := checkMove(from, to, name); err != nil {
		e.moveFailed(en, from, to, err)
		e.mu.Unlock()
		return moveRefused
	}
	en.moving = true
	e.foldCounters(en)
	e.mu.Unlock()

	t.Drop()
	err := moveData(e.log, from, to, name)
	if err == nil && from == e.streamDir(en.ih) {
		if rerr := os.RemoveAll(from); rerr != nil {
			e.log.Warn("remove stream dir after move", "dir", from, "err", rerr)
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	en.moving = false
	if en.removed {
		return moveReattached
	}
	if err != nil {
		e.moveFailed(en, from, to, err)
	} else {
		en.rec.SaveDir, en.rec.TargetDir = to, ""
		e.save(en.rec)
	}
	select {
	case <-e.closing: // client is shutting down; the next New re-attaches from the DB
		en.t, en.uploadOff = nil, false
		return moveReattached
	default:
	}
	spec, serr := e.specFor(en.rec)
	if serr == nil {
		serr = e.attach(en, spec)
	}
	if serr != nil {
		en.t, en.uploadOff = nil, false
		en.rec.Error = serr.Error()
		e.save(en.rec)
	}
	return moveReattached
}

type moveResult int

const (
	moveSkipped moveResult = iota
	moveRefused
	moveReattached
)

// moveFailed records a move error and cancels the pending move, so neither
// the watcher nor a restart retries it in a loop. e.mu must be held.
func (e *Engine) moveFailed(en *entry, from, to string, err error) {
	e.log.Error("move torrent data", "infohash", en.ih, "from", from, "to", to, "err", err)
	en.rec.Error = fmt.Sprintf("не удалось перенести файлы: %s", err.Error())
	en.rec.TargetDir = ""
	e.save(en.rec)
}

func validName(name string) bool {
	return name != "" && name == filepath.Base(name) && name != "." && name != ".."
}

// checkMove validates a move of from/name to to/name without touching data.
func checkMove(from, to, name string) error {
	if !validName(name) {
		return fmt.Errorf("bad torrent name %q", name)
	}
	if !exists(filepath.Join(from, name)) {
		return i18n.Errorf("%s не найден", filepath.Join(from, name))
	}
	if dst := filepath.Join(to, name); exists(dst) {
		return i18n.Errorf("%s уже существует", dst)
	}
	return nil
}

// moveData moves from/name to to/name: rename when possible, otherwise copy
// into a temporary name, rename it into place and delete the source. Once
// the copy is in place the move counts as done; a failure to delete the
// source is only logged.
func moveData(log *slog.Logger, from, to, name string) error {
	if !validName(name) {
		return fmt.Errorf("bad torrent name %q", name)
	}
	src, dst := filepath.Join(from, name), filepath.Join(to, name)
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	if exists(dst) {
		return i18n.Errorf("%s уже существует", dst)
	}
	err := os.Rename(src, dst)
	if err == nil || !errors.Is(err, syscall.EXDEV) {
		return err
	}
	tmp := dst + ".vault-partial"
	os.RemoveAll(tmp)
	if err := copyTree(src, tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	if err := os.RemoveAll(src); err != nil {
		log.Warn("moved torrent data, but could not remove the source", "src", src, "err", err)
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if fi.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, in); err != nil {
			f.Close()
			return err
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
}

func writeFileAtomic(p string, b []byte) error {
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// OnComplete registers a callback fired once when a torrent finishes
// downloading (after any pending move), and again when an already complete
// stream torrent is switched to download/seed. Called without engine locks.
func (e *Engine) OnComplete(f func(Status)) {
	e.mu.Lock()
	e.hooks = append(e.hooks, f)
	e.mu.Unlock()
}

// AddTorrentFile adds a torrent from metafile bytes. Adding a torrent that is
// already present returns the existing handle.
func (e *Engine) AddTorrentFile(ctx context.Context, data []byte, opt AddOptions) (Handle, error) {
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("torrent file: %w", err)
	}
	spec, err := atorrent.TorrentSpecFromMetaInfoErr(mi)
	if err != nil {
		return nil, fmt.Errorf("torrent file: %w", err)
	}
	if spec.InfoHash.IsZero() {
		return nil, errors.New("torrent: v2-only torrents are not supported")
	}
	return e.add(ctx, spec, data, "", opt)
}

// AddMagnet adds a torrent by magnet URI; metadata is fetched from peers.
func (e *Engine) AddMagnet(ctx context.Context, uri string, opt AddOptions) (Handle, error) {
	spec, err := atorrent.TorrentSpecFromMagnetUri(uri)
	if err != nil {
		return nil, fmt.Errorf("magnet: %w", err)
	}
	if spec.InfoHash.IsZero() {
		return nil, errors.New("torrent: v2-only magnets are not supported")
	}
	return e.add(ctx, spec, nil, uri, opt)
}

func (e *Engine) add(ctx context.Context, spec *atorrent.TorrentSpec, metafile []byte, magnet string, opt AddOptions) (Handle, error) {
	if e.cfg.Off {
		return nil, ErrOff
	}
	switch opt.Mode {
	case ModeStream, ModeDownload, ModeSeed:
	default:
		return nil, fmt.Errorf("%w: %q", ErrBadMode, opt.Mode)
	}
	files, err := normaliseSelection(opt.Files, spec.InfoBytes)
	if err != nil {
		return nil, err
	}
	ih := spec.InfoHash.HexString()
	if err := e.lockAfterFetch(ctx, ih); err != nil {
		return nil, err
	}
	defer e.mu.Unlock()
	select {
	case <-e.closing:
		return nil, ErrClosed
	default:
	}
	if e.offline != "" {
		return nil, fmt.Errorf("%w: %s", ErrStorageOffline, e.offline)
	}
	if en, ok := e.entries[ih]; ok {
		return en, nil
	}
	saveDir := opt.SaveDir
	if opt.Mode == ModeStream {
		saveDir = e.streamDir(ih)
	}
	if saveDir == "" {
		saveDir = e.cfg.DownloadDir
	}
	if saveDir == "" {
		return nil, errors.New("torrent: SaveDir is required")
	}
	name := spec.DisplayName
	nameKnown := false
	if spec.InfoBytes != nil {
		if inf, err := (&metainfo.MetaInfo{InfoBytes: spec.InfoBytes}).UnmarshalInfo(); err == nil {
			name, nameKnown = inf.BestName(), true
		}
		if other := e.pathOwner(nil, saveDir, name); other != "" {
			// A namesake (another release whose top folder has the same
			// name) downloads into a folder of its own; the same files
			// again, or a seed of data that is there, are refused.
			if opt.Mode != ModeDownload || e.sameContent(other, contentKey(spec.InfoBytes)) {
				return nil, pathInUse(filepath.Join(saveDir, name), other)
			}
			saveDir = namesakeDir(saveDir, ih)
			if other := e.pathOwner(nil, saveDir, name); other != "" {
				return nil, pathInUse(filepath.Join(saveDir, name), other)
			}
		}
	}
	if metafile != nil {
		if err := writeFileAtomic(e.metafilePath(ih), metafile); err != nil {
			return nil, err
		}
	}
	rec := record{
		InfoHash: ih, Name: name, Mode: opt.Mode, SaveDir: saveDir,
		ItemID: opt.ItemID, AddedAt: time.Now().UTC().Truncate(time.Second),
		Status: StatusQueued, SourceURL: opt.SourceURL, Magnet: magnet, Release: opt.Release,
		Files: files,
	}
	if opt.AddedBy != 0 && opt.AddedByUID != "" {
		by := opt.AddedBy
		rec.AddedBy, rec.AddedByUID = &by, opt.AddedByUID
	}
	if h := opt.history; h != nil {
		rec.AddedAt, rec.Uploaded, rec.Downloaded = h.AddedAt, h.Uploaded, h.Downloaded
		if opt.keepCompleted {
			rec.CompletedAt = h.CompletedAt
		}
	}
	e.seq++
	en := &entry{e: e, ih: ih, rec: rec, seq: e.seq, nameKnown: nameKnown}
	if spec.InfoBytes != nil {
		en.content = contentKey(spec.InfoBytes)
	}
	switch {
	case opt.Paused:
		en.hold = StatusPaused
	case opt.WaitingSpace:
		en.hold = StatusWaitingSpace
	}
	if en.hold != "" {
		en.rec.Status = en.hold
	}
	// Stale completion from an earlier, removed copy must not be trusted.
	if err := e.pc.forget(ctx, ih); err != nil {
		return nil, err
	}
	// Nothing on disk yet: skip anacrolix's initial hash pass. Seed mode and
	// files already present are verified piece by piece instead.
	if opt.Mode != ModeSeed && !anyDataExists(saveDir, spec.InfoBytes) {
		e.pc.setFresh(ih, true)
	}
	if e.ps != nil {
		e.ps.drop(ih) // a late row of an earlier torrent of this infohash
		e.ps.settle(ih)
	}
	if err := e.st.insert(ctx, en.rec); err != nil {
		os.Remove(e.metafilePath(ih))
		return nil, err
	}
	if err := e.attach(en, spec); err != nil {
		if e.ps != nil {
			e.ps.drop(ih)
		}
		if derr := e.st.delete(ctx, ih); derr != nil {
			e.log.Error("delete torrent row", "infohash", ih, "err", derr)
		}
		os.Remove(e.metafilePath(ih))
		return nil, err
	}
	e.entries[ih] = en
	return en, nil
}

// lockAfterFetch takes e.mu once no metadata fetch of ih is in flight, so
// add never attaches to a fetch's temporary torrent. The wait is bounded by
// the fetch's callers (each FetchInfo ctx) and by ctx.
func (e *Engine) lockAfterFetch(ctx context.Context, ih string) error {
	for {
		e.mu.Lock()
		f := e.fetching[ih]
		if f == nil {
			return nil
		}
		e.mu.Unlock()
		select {
		case <-f.done:
		case <-ctx.Done():
			return ctx.Err()
		case <-e.closing:
			return ErrClosed
		}
	}
}

// Get returns a torrent by hex infohash.
func (e *Engine) Get(infohash string) (Handle, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	en, ok := e.entries[strings.ToLower(infohash)]
	if !ok {
		return nil, false
	}
	return en, true
}

// List returns all torrents, oldest first.
func (e *Engine) List() []Status {
	e.mu.Lock()
	out := make([]Status, 0, len(e.entries))
	for _, en := range e.entries {
		out = append(out, e.statusOf(en))
	}
	e.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].AddedAt.Equal(out[j].AddedAt) {
			return out[i].AddedAt.Before(out[j].AddedAt)
		}
		return out[i].InfoHash < out[j].InfoHash
	})
	return out
}

// usesPath reports whether o's data is (or, with a pending move, will be)
// dir/name.
func (o *entry) usesPath(dir, name string) bool {
	if name == "" || o.removed || o.rec.Name != name {
		return false
	}
	dir = filepath.Clean(dir)
	return filepath.Clean(o.rec.SaveDir) == dir || (o.rec.TargetDir != "" && filepath.Clean(o.rec.TargetDir) == dir)
}

// pathOwner returns the infohash of an older torrent that owns saveDir/name,
// or "". Owners must have a name from real metadata (an unresolved magnet's
// display name blocks nothing) and must not be blocked duplicates
// themselves. en == nil means a torrent newer than all (being added, or a
// magnet that just resolved; then self is that entry, skipped).
// e.mu must be held.
func (e *Engine) pathOwner(en *entry, saveDir, name string, self ...*entry) string {
	for oih, o := range e.entries {
		if o == en || (len(self) > 0 && o == self[0]) || !o.nameKnown || o.dupOf != "" || !o.usesPath(saveDir, name) {
			continue
		}
		if en != nil && o.seq > en.seq {
			continue
		}
		return oih
	}
	return ""
}

// sharesPath reports whether any other torrent uses dir/name at all.
// e.mu must be held.
func (e *Engine) sharesPath(ih, dir, name string) bool {
	for oih, o := range e.entries {
		if oih != ih && o.usesPath(dir, name) {
			return true
		}
	}
	return false
}

func (e *Engine) entry(ih string) (*entry, error) {
	en, ok := e.entries[strings.ToLower(ih)]
	if !ok {
		return nil, ErrNotFound
	}
	return en, nil
}

// Remove drops a torrent. With deleteData its files are deleted too (only
// the torrent's own top-level file/dir inside SaveDir, never SaveDir itself).
func (e *Engine) Remove(ctx context.Context, infohash string, deleteData bool) error {
	return e.remove(ctx, infohash, "", deleteData)
}

// RemoveKeeping is Remove with its data deleted, except the files keep
// reports (the library's own files a torrent seeds in place, which the
// library would otherwise lose without knowing): they and the folders
// holding them stay. root is the DataRoot keep was made for: a torrent
// whose data moved meanwhile is not removed (ErrBusy).
func (e *Engine) RemoveKeeping(ctx context.Context, infohash, root string, keep func(path string) bool) error {
	return e.removeIf(ctx, infohash, "", true, func(en *entry) error {
		if r, ok := e.dataRootOf(en); !ok || r != root {
			return ErrBusy
		}
		return nil
	}, keep)
}

// DataRoot is the torrent's own top-level file or folder, the one a
// Remove with its data deletes; false for a stream torrent, one being
// moved or one with no name yet.
func (e *Engine) DataRoot(infohash string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	en, err := e.entry(infohash)
	if err != nil {
		return "", false
	}
	return e.dataRootOf(en)
}

// dataRootOf is DataRoot (e.mu held), named as removeIf names it.
func (e *Engine) dataRootOf(en *entry) (string, bool) {
	if en.rec.Mode == ModeStream || en.moving || en.rec.SaveDir == "" {
		return "", false
	}
	name := en.rec.Name
	if en.t != nil {
		if info := en.t.Info(); info != nil {
			name = info.BestName()
		}
	}
	if !validName(name) {
		return "", false
	}
	return filepath.Join(en.rec.SaveDir, name), true
}

// removeOwnFiles deletes the torrent's own files (paths, under root) but
// those keep reports, and then the folders on their way that end up empty.
// Nothing else in the folder is touched: what the library put beside a
// video later (a voice-over, subtitles, an .nfo) is no file of the torrent.
func removeOwnFiles(root string, paths []string, keep func(string) bool) error {
	var firstErr error
	dirs := map[string]bool{}
	for _, p := range paths {
		if p != root && !within(root, p) {
			continue // a hostile name: never outside the data
		}
		fi, err := os.Lstat(p)
		if err != nil || fi.IsDir() || keep(p) {
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for d := filepath.Dir(p); p != root && within(root, d); d = filepath.Dir(d) {
			dirs[d] = true
			if d == root {
				break
			}
		}
	}
	list := make([]string, 0, len(dirs))
	for d := range dirs {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) }) // deepest first
	for _, d := range list {
		os.Remove(d) // only an empty one goes
	}
	return firstErr
}

// ownPaths are the files of info under root (root itself for a single-file
// torrent); a name that is no plain path component is left out.
func ownPaths(root string, info *metainfo.Info) []string {
	if info == nil {
		return nil
	}
	if !info.IsDir() {
		return []string{root}
	}
	var out []string
	for _, f := range info.UpvertedFiles() {
		parts := f.BestPath()
		ok := len(parts) > 0
		for _, c := range parts {
			if c == "" || c == "." || c == ".." || strings.ContainsAny(c, `/\`) {
				ok = false
			}
		}
		if ok {
			out = append(out, filepath.Join(append([]string{root}, parts...)...))
		}
	}
	return out
}

// ErrModeChanged: RemoveIfMode found the torrent in another mode.
var ErrModeChanged = errors.New("torrent: mode changed")

// RemoveIfMode removes the torrent only if it is in mode at the moment of
// removal (checked under the engine lock), else ErrModeChanged. The stream
// cache uses it so a torrent promoted meanwhile is never deleted.
func (e *Engine) RemoveIfMode(ctx context.Context, infohash, mode string, deleteData bool) error {
	if mode == "" {
		return ErrBadMode
	}
	return e.remove(ctx, infohash, mode, deleteData)
}

func (e *Engine) remove(ctx context.Context, infohash, mode string, deleteData bool) error {
	return e.removeIf(ctx, infohash, mode, deleteData, nil, nil)
}

// removeIf is remove; a non-nil keep (checked under e.mu, after the mode)
// returns the error that keeps the torrent instead; a non-nil keepFile
// (with deleteData) spares the data files it reports (RemoveKeeping).
func (e *Engine) removeIf(ctx context.Context, infohash, mode string, deleteData bool, keep func(*entry) error, keepFile func(string) bool) error {
	e.mu.Lock()
	en, err := e.entry(infohash)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if mode != "" && en.rec.Mode != mode {
		e.mu.Unlock()
		return ErrModeChanged
	}
	if keep != nil {
		if err := keep(en); err != nil {
			e.mu.Unlock()
			return err
		}
	}
	if en.moving {
		e.mu.Unlock()
		return ErrBusy
	}
	// Row first: if that fails the torrent stays fully in place. A row
	// queued for writing never lands after the delete.
	if e.ps != nil {
		e.ps.drop(en.ih)
	}
	if err := e.st.delete(ctx, en.ih); err != nil {
		if e.ps != nil {
			e.ps.put(en.rec) // it stays: its latest row is not lost
		}
		e.mu.Unlock()
		return err
	}
	t, rec := en.t, en.rec
	name := rec.Name
	var info *metainfo.Info
	if t != nil {
		if info = t.Info(); info != nil {
			name = info.BestName()
		}
	}
	if info == nil && keepFile != nil { // the file list from the metafile
		if mi, err := metainfo.LoadFromFile(e.metafilePath(en.ih)); err == nil {
			if i, err := mi.UnmarshalInfo(); err == nil {
				info = &i
			}
		}
	}
	// Never delete files another torrent uses (e.g. a blocked duplicate).
	shared := deleteData && e.sharesPath(en.ih, rec.SaveDir, name)
	en.removed = true
	delete(e.entries, en.ih)
	e.peered.forget(en.ih)
	en.t = nil
	e.mu.Unlock()

	if t != nil {
		t.Drop()
	}
	// Only now (nothing of the old owner runs any more) may a blocked
	// duplicate take the files over.
	e.mu.Lock()
	e.unblockDuplicates(en.ih)
	e.mu.Unlock()
	os.Remove(e.metafilePath(en.ih))
	if !deleteData {
		return nil
	}
	if shared {
		e.log.Warn("torrent removed, data kept: used by another torrent", "infohash", en.ih, "path", filepath.Join(rec.SaveDir, name))
		return nil
	}
	if rec.SaveDir == e.streamDir(en.ih) {
		return os.RemoveAll(rec.SaveDir)
	}
	if !validName(name) {
		return fmt.Errorf("torrent: refusing to delete data for name %q", name)
	}
	if keepFile != nil {
		// the torrent's own files only (none known: none deleted)
		root := filepath.Join(rec.SaveDir, name)
		if err := removeOwnFiles(root, ownPaths(root, info), keepFile); err != nil {
			return err
		}
	} else if err := os.RemoveAll(filepath.Join(rec.SaveDir, name)); err != nil {
		return err
	}
	if filepath.Base(rec.SaveDir) == en.ih {
		os.Remove(rec.SaveDir) // a namesake's own folder, when empty
	}
	return nil
}

// SetModeTo changes the mode. stream→download/seed downloads the rest and,
// once complete (immediately if already complete), moves the data from
// StreamDir/<infohash> to saveDir (or EngineConfig.DownloadDir; when both are
// empty the data stays put). Nothing is re-downloaded. OnComplete fires
// after the move. Switching download/seed back to stream is refused (stream
// torrents are subject to cache pruning). download↔seed only relabels.
func (e *Engine) SetModeTo(ctx context.Context, infohash, mode, saveDir string) error {
	switch mode {
	case ModeStream, ModeDownload, ModeSeed:
	default:
		return fmt.Errorf("%w: %q", ErrBadMode, mode)
	}
	e.mu.Lock()
	en, err := e.entry(infohash)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if e.offline != "" {
		e.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrStorageOffline, e.offline)
	}
	old := en.rec.Mode
	if old == mode {
		e.mu.Unlock()
		return nil
	}
	if mode == ModeStream {
		e.mu.Unlock()
		return errors.New("torrent: cannot switch back to stream mode")
	}
	if old == ModeStream && en.moving {
		e.mu.Unlock()
		return ErrBusy // it would be attached at the place it is leaving
	}
	en.rec.Mode = mode
	notify := false
	if old == ModeStream {
		if saveDir == "" {
			saveDir = e.cfg.DownloadDir
		}
		if saveDir != "" && saveDir != en.rec.SaveDir {
			en.rec.TargetDir = saveDir
		}
		if en.parked {
			e.unpark(en) // the move below needs it attached
		}
		if en.t != nil && en.t.Info() != nil {
			e.applyWanted(en, en.t)
		}
		// Completed already (the watcher has finished): move/notify here.
		notify = en.rec.CompletedAt != nil
	}
	if err := e.persistUnlock(ctx, en.rec); err != nil {
		return err
	}
	if notify {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.move(en)
			e.mu.Lock()
			if en.removed || en.rec.Error != "" {
				e.mu.Unlock()
				return
			}
			st := e.statusOf(en)
			hooks := append([]func(Status){}, e.hooks...)
			e.mu.Unlock()
			for _, h := range hooks {
				h(st)
			}
		}()
	}
	return nil
}

// Pause stops downloading and uploading (admin action).
func (e *Engine) Pause(ctx context.Context, infohash string) error {
	return e.setHold(ctx, infohash, StatusPaused)
}

// HoldForSpace puts a torrent on waiting_space: nothing more is
// downloaded until Resume (it keeps seeding what it has).
func (e *Engine) HoldForSpace(ctx context.Context, infohash string) error {
	return e.setHold(ctx, infohash, StatusWaitingSpace)
}

// Resume clears paused/waiting_space.
func (e *Engine) Resume(ctx context.Context, infohash string) error {
	return e.setHold(ctx, infohash, "")
}

func (e *Engine) setHold(ctx context.Context, infohash, hold string) error {
	e.mu.Lock()
	en, err := e.entry(infohash)
	if err != nil {
		e.mu.Unlock()
		return err
	}
	if e.offline != "" {
		e.mu.Unlock()
		// the status saved now would be "error" and lose the hold
		return fmt.Errorf("%w: %s", ErrStorageOffline, e.offline)
	}
	if hold == "" && en.dataMissing && en.moving {
		e.mu.Unlock()
		return ErrBusy // the resume below attaches it at the place it is leaving
	}
	en.hold = hold
	// a pause or resume starts the stall window afresh (checkStalled)
	en.runSince, en.peersAt, en.stalled = time.Time{}, time.Time{}, ""
	if hold == "" && strings.HasPrefix(en.rec.Error, writeErrorPrefix) {
		en.rec.Error = "" // e.g. disk was full; retry
	}
	if t := en.t; t != nil {
		switch hold {
		case "":
			if e.noSeed {
				t.DisallowDataUpload()
				en.uploadOff = true
			} else {
				t.AllowDataUpload()
				en.uploadOff = false
			}
		case StatusPaused:
			t.DisallowDataUpload()
		}
		if t.Info() != nil {
			e.applyWanted(en, t)
		}
	}
	if hold == "" && en.dataMissing && en.t == nil {
		// The admin asked for it: whatever is back is verified, the rest
		// is downloaded again.
		spec, err := e.specFor(en.rec)
		if err != nil {
			e.mu.Unlock()
			return err
		}
		e.reconcile(en, spec, false)
		en.dataMissing, en.rec.Error = false, ""
		if err := e.attach(en, spec); err != nil {
			en.rec.Error = err.Error()
		}
	}
	en.syncHeld()
	en.rec.Status = e.statusOf(en).Status
	return e.persistUnlock(ctx, en.rec)
}

// persistUnlock queues r with e.mu held, releases e.mu and waits for the
// write without it (persister), returning the write's error.
func (e *Engine) persistUnlock(ctx context.Context, r record) error {
	if e.ps == nil {
		defer e.mu.Unlock()
		return e.st.update(ctx, r)
	}
	v := e.ps.put(r)
	e.mu.Unlock()
	err := e.ps.wait(ctx, r.InfoHash, v)
	if errors.Is(err, errRowDropped) {
		return ErrNotFound // deleted meanwhile
	}
	return err
}

// totals returns all-time up/down bytes. e.mu must be held.
func (e *Engine) totals(en *entry) (up, down int64) {
	up, down = en.baseUp, en.baseDown
	if en.t != nil && !en.moving {
		s := en.t.Stats()
		up += s.BytesWrittenData.Int64()
		down += s.BytesReadUsefulData.Int64()
	}
	return
}

// foldCounters stores current totals into rec. e.mu must be held.
func (e *Engine) foldCounters(en *entry) {
	en.rec.Uploaded, en.rec.Downloaded = e.totals(en)
}

// statusOf builds a Status. e.mu must be held.
func (e *Engine) statusOf(en *entry) Status {
	r := en.rec
	s := Status{
		InfoHash: en.ih, Name: r.Name, Mode: r.Mode, ItemID: r.ItemID, AddedBy: r.AddedBy, AddedByUID: r.AddedByUID, AddedAt: r.AddedAt,
		CompletedAt: r.CompletedAt, SaveDir: r.SaveDir, Error: r.Error,
		UpRate: en.upRate, DownRate: en.downRate,
	}
	s.Uploaded, s.Downloaded = e.totals(en)
	t := en.t
	complete := false
	if t != nil && !en.moving {
		if t.Info() != nil {
			s.TotalFiles = len(t.Files())
			if r.Files == nil {
				s.Size = t.Length()
				s.Done = t.BytesCompleted()
				s.SelectedFiles = s.TotalFiles
				complete = t.Complete().Bool()
			} else {
				s.Size, s.Done, s.SelectedFiles = selectionProgress(t, r.Files)
				complete = selectionComplete(t, r.Files)
			}
		}
		st := t.Stats()
		s.Peers, s.Seeds = st.ActivePeers, st.ConnectedSeeders
	}
	if en.parked {
		en.parkedStatus(&s)
	}
	if e.offline != "" && t == nil {
		s.Error = e.offline
	}
	s.DataMissing = en.dataMissing && t == nil
	switch {
	case s.Error != "":
		s.Status = StatusError
	case en.hold != "":
		s.Status = en.hold
	case en.moving, en.parked:
		s.Status = StatusSeeding
	case t == nil || t.Info() == nil:
		s.Status = StatusQueued
	case complete:
		s.Status = StatusSeeding
	default:
		s.Status = StatusDownloading
	}
	if t != nil && (s.Status == StatusQueued || s.Status == StatusDownloading) {
		s.Stalled = en.stalled // the monitor's last tick: a hold or completion since clears it
		s.TrackerError, s.TrackerErrorKind = en.trackerErr, en.trackerErrKind
	}
	s.HadPeers = en.everPeer || s.Peers > 0 || e.peered.has(en.ih)
	if s.Liveness = en.liveness; s.Liveness != "" && (s.HadPeers || s.Done > 0 || s.Downloaded > 0) {
		s.Liveness = LivenessAlive
	}
	if s.Downloaded > 0 {
		s.Ratio = float64(s.Uploaded) / float64(s.Downloaded)
	} else if s.Size > 0 {
		s.Ratio = float64(s.Uploaded) / float64(s.Size)
	}
	return s
}

// monitor samples rates every tick and persists counters every 30s.
func (e *Engine) monitor() {
	defer e.wg.Done()
	tk := time.NewTicker(e.cfg.tick)
	defer tk.Stop()
	lastPersist := time.Now()
	for {
		select {
		case <-e.closing:
			return
		case now := <-tk.C:
			e.recheckStorage(now)
			e.sweepNoSeeding()
			persist := now.Sub(lastPersist) >= e.cfg.persistEvery
			var recs []counters
			e.mu.Lock()
			e.checkActive(now)
			for _, en := range e.entries {
				e.checkStalled(en, now)
				e.maybeReprobe(en, now)
				if en.t == nil || en.moving {
					en.upRate, en.downRate = 0, 0
					continue
				}
				up, down := e.totals(en)
				if dt := now.Sub(en.lastAt).Seconds(); dt > 0 {
					en.upRate = int64(float64(up-en.lastUp) / dt)
					en.downRate = int64(float64(down-en.lastDown) / dt)
				}
				en.lastUp, en.lastDown, en.lastAt = up, down, now
				e.checkIdle(en, now)
				if persist {
					en.rec.Uploaded, en.rec.Downloaded = up, down
					row := e.ps.lastWrite(en.ih)
					if !en.savedOK || up != en.savedUp || down != en.savedDown || row != en.savedRow {
						recs = append(recs, counters{en: en, ih: en.ih, up: up, down: down, row: row})
					}
				}
			}
			e.mu.Unlock()
			for _, p := range e.reqs.stuck(now, e.cfg.StallTimeout) {
				e.stallKicks.Add(1)
				e.reconnectPeer(p, "connection stuck")
			}
			if persist {
				lastPersist = now
				// Only the counters: the copy is not under e.mu any more,
				// and a promote, hold or move saved meanwhile must not be
				// overwritten by it. Those that moved, in one transaction.
				if len(recs) > 0 {
					if err := e.st.updateCounters(context.Background(), recs); err != nil {
						e.log.Error("persist torrent counters", "torrents", len(recs), "err", err)
					} else {
						e.mu.Lock()
						for _, c := range recs {
							c.en.savedUp, c.en.savedDown, c.en.savedOK, c.en.savedRow = c.up, c.down, true, c.row
						}
						e.mu.Unlock()
					}
				}
			}
		}
	}
}

// Close persists counters and stops the client. Torrents are restored by the
// next New on the same database and DataDir.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		if e.cfg.Off {
			close(e.closing)
			return
		}
		// Piece completion first, inside Docker's 30 s grace: a lost "not
		// complete" matters more than the counters below.
		e.pc.flushFor(10 * time.Second)
		e.mu.Lock()
		close(e.closing)
		var recs []record
		for _, en := range e.entries {
			if en.t == nil && (e.offline != "" || en.dataMissing) {
				continue // never attached: the stored row (and its hold) is still right
			}
			e.foldCounters(en)
			en.rec.Status = e.statusOf(en).Status
			recs = append(recs, en.rec)
		}
		e.mu.Unlock()
		// The final rows go through the queue too: each replaces its
		// torrent's queued row, so no older row lands after it; no write
		// here races the queue's.
		for _, r := range recs {
			e.ps.put(r)
		}
		if !e.ps.flush(10 * time.Second) {
			e.log.Error("persist torrents at shutdown: the database is not answering, rows still queued")
		}
		e.client.Close()
		e.wg.Wait()
		e.ps.close(5 * time.Second) // what the watchers saved while closing
		e.pc.stop(5 * time.Second)  // what anacrolix set while closing
	})
	return nil
}

// --- Handle ---

func (en *entry) InfoHash() string { return en.ih }

func (en *entry) Status() Status {
	en.e.mu.Lock()
	defer en.e.mu.Unlock()
	return en.e.statusOf(en)
}

// current returns the live anacrolix torrent (nil while moving or failed).
func (en *entry) current() (*atorrent.Torrent, error) {
	en.e.mu.Lock()
	defer en.e.mu.Unlock()
	select {
	case <-en.e.closing:
		return nil, ErrClosed
	default:
	}
	if en.removed {
		return nil, ErrNotFound
	}
	if en.t == nil && !en.moving {
		return nil, errors.New("torrent: " + en.rec.Error)
	}
	if en.moving {
		return nil, nil
	}
	return en.t, nil
}

func (en *entry) WaitInfo(ctx context.Context) error {
	for {
		en.e.mu.Lock()
		parked := en.parked
		en.e.mu.Unlock()
		if parked {
			return nil
		}
		t, err := en.current()
		if err != nil {
			return err
		}
		if t == nil { // moving
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			continue
		}
		select {
		case <-t.GotInfo():
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-en.e.closing:
			return ErrClosed
		case <-t.Closed():
			// dropped for a move or removal; re-check
		}
	}
}

func (en *entry) Files() []FileInfo {
	if fs := en.parkedFiles(); fs != nil {
		return fs
	}
	t, err := en.current()
	if err != nil || t == nil || t.Info() == nil {
		return nil
	}
	en.e.mu.Lock()
	dir, sel := en.rec.SaveDir, en.rec.Files
	en.e.mu.Unlock()
	fs := t.Files()
	out := make([]FileInfo, len(fs))
	for i, f := range fs {
		fi := FileInfo{Index: i, Path: f.Path(), FullPath: filepath.Join(dir, filepath.FromSlash(f.Path())), Size: f.Length(),
			Selected: isSelected(sel, i)}
		if f.Length() > 0 {
			fi.Progress = float64(f.BytesCompleted()) / float64(f.Length())
		} else {
			fi.Progress = 1
		}
		out[i] = fi
	}
	return out
}

// NewReader opens a streaming reader over one file. It waits (bounded by
// ctx) for metadata. ctx also bounds the reader's lifetime.
func (en *entry) NewReader(ctx context.Context, fileIndex int) (io.ReadSeekCloser, error) {
	// counted before the torrent is looked at: a sweep either parked it
	// already (parkedRead sees that) or sees this reader and leaves it
	en.readers.Add(1)
	live := false
	defer func() {
		if !live {
			en.readers.Add(-1)
		}
	}()
	if r, handled, err := en.parkedRead(ctx, fileIndex); handled {
		return r, err
	}
	if err := en.WaitInfo(ctx); err != nil {
		return nil, err
	}
	t, err := en.current()
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, ErrBusy
	}
	if err := en.waitApplied(ctx, t); err != nil {
		return nil, err
	}
	if err := en.readBlocked(); err != nil {
		return nil, err
	}
	fs := t.Files()
	if fileIndex < 0 || fileIndex >= len(fs) {
		return nil, fmt.Errorf("torrent: no file %d", fileIndex)
	}
	fr := newFileReader(ctx, fs[fileIndex], en.e.cfg.ReadTimeout)
	fr.onRead = func(off int64) { en.reads.set(fileIndex, off) }
	fr.blocked = en.readBlocked
	fr.wake = en.holdWaker
	fr.onClose = func() { en.readers.Add(-1) }
	live = true
	return fr, nil
}

// clientConfig is the anacrolix client's configuration for e.cfg: what the
// engine's callbacks, limits and storage are, and which of its network
// switches are off. It opens nothing (a test reads the guard from it).
func (e *Engine) clientConfig() *atorrent.ClientConfig {
	cc := atorrent.NewDefaultClientConfig()
	cc.DataDir = e.cfg.StreamDir
	cc.DefaultStorage = e.fileStorage(e.cfg.StreamDir)
	cc.ListenPort = e.cfg.ListenPort
	cc.Seed = true
	cc.NoUpload = e.cfg.NoUpload
	cc.DisableAggressiveUpload = e.cfg.noAggressiveUpload
	// a middling server: the library's defaults (50 connections per torrent,
	// 100 half-open) are made for a desktop
	cc.EstablishedConnsPerTorrent = 30
	cc.TotalHalfOpenConns = 40
	cc.Slogger = slog.New(quietHandler{slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})}).With("component", "anacrolix")
	e.reqs.install(&cc.Callbacks)
	e.installIncoming(&cc.Callbacks)
	e.installPeerSeen(&cc.Callbacks)
	e.installYourIP(&cc.Callbacks)
	// Always our own limiter (anacrolix shares one "unlimited" instance
	// between clients): the settings change it at runtime.
	e.upLimit = rate.NewLimiter(rate.Inf, 0)
	cc.UploadRateLimiter = e.upLimit
	e.SetUploadLimit(e.cfg.UploadLimit)
	if e.cfg.DownloadLimit > 0 {
		cc.DownloadRateLimiter = rate.NewLimiter(rate.Limit(e.cfg.DownloadLimit), 0)
	}
	// Outside LocalOnly anacrolix asks the router for ListenPort (TCP and
	// UDP) by UPnP at start (NoDefaultPortForwarding stays false). The
	// discovery is multicast on the local network: behind Docker bridge
	// networking it never reaches the router (deploy/README.md).
	cc.NoDefaultPortForwarding = e.cfg.NoUPnP
	if e.cfg.LocalOnly {
		cc.NoDHT = true
		cc.DisableTrackers = true
		cc.DisablePEX = true
		cc.NoDefaultPortForwarding = true
		cc.DisableIPv6 = true
		cc.DisableUTP = true
		cc.DisableWebtorrent = true
		cc.DisableWebseeds = true
		cc.ListenHost = func(string) string { return "127.0.0.1" }
	}
	if !e.cfg.LocalOnly {
		guardNetwork(cc)
	}
	return cc
}

// guardNetwork makes every HTTP fetch of the client (webseeds, magnet
// xs=/as= sources, HTTP and websocket trackers) and every UDP tracker write
// refuse local, private and metadata addresses: those URLs come from
// torrents anyone can publish. The check is on the dialled IP, so redirects
// and DNS tricks are covered too.
func guardNetwork(cc *atorrent.ClientConfig) {
	cc.HTTPProxy = nil // a proxy would connect for us and dodge the check
	cc.HTTPDialContext = netguard.DialContext
	// webseeds and xs=/as= sources use this transport (HTTP/2 stays on, as
	// with anacrolix's own; no env proxy, 10 connections per host as its
	// default)
	web := netguard.Transport(nil)
	web.Proxy, web.MaxConnsPerHost = nil, 10
	cc.WebTransport = web
	cc.TrackerDialContext = netguard.DialContext
	cc.TrackerListenPacket = netguard.ListenPacket
}

// newClient starts the anacrolix client. On a host without IPv6 (a kernel
// built without it, ipv6.disable=1, some containers) the tcp6/udp6 sockets
// fail with EAFNOSUPPORT: the client then starts again on IPv4 alone
// instead of the server refusing to start.
func newClient(cc *atorrent.ClientConfig) (*atorrent.Client, error) {
	cl, err := atorrent.NewClient(cc)
	if err != nil && !cc.DisableIPv6 && errors.Is(err, syscall.EAFNOSUPPORT) {
		slog.Warn("no IPv6 on this host: torrents listen on IPv4 only", "component", "torrent", "err", err)
		cc.DisableIPv6 = true
		cl, err = atorrent.NewClient(cc)
	}
	return cl, err
}
