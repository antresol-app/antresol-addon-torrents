package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	mediaimport "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/mediaimport/runkit"
)

// The run's types and errors (mediaimport is the core's runkit).
type (
	Source       = mediaimport.Source
	Run          = mediaimport.Run
	RunRequest   = mediaimport.RunRequest
	SourceStatus = mediaimport.SourceStatus
	Progress     = mediaimport.Progress
)

var (
	ErrNeedsDryRun   = mediaimport.ErrNeedsDryRun
	ErrNotReady      = mediaimport.ErrNotReady
	ErrBusy          = mediaimport.ErrBusy
	ErrUnknownSource = mediaimport.ErrUnknownSource
	ErrNoSources     = mediaimport.ErrNoSources
	ErrNotFound      = mediaimport.ErrNotFound
)

// EventProgress is the event a run's progress goes out as (Client.Event):
// the app's addon.torrents.admin.migrate.progress, for admins only; the
// web part declares the short name migrate.progress.
const EventProgress = "admin.migrate.progress"

// Eventer publishes an event to the app (*addonkit.Client).
type Eventer interface {
	Event(ctx context.Context, name string, data any) error
}

// PublishTo is a Runner.Progress that sends each step as EventProgress
// through ev; a send that fails is logged (the page polls the state too).
func PublishTo(ev Eventer, log *slog.Logger) func(ctx context.Context, p Progress) {
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, p Progress) {
		if err := ev.Event(ctx, EventProgress, p); err != nil && ctx.Err() == nil {
			log.Warn("torrents: migrate progress not sent", "report", p.ReportID, "err", err)
		}
	}
}

// planTTL: how long a dry run allows the real run.
const planTTL = time.Hour

// Runner is the addon's own «Переезд» run, written from the core's
// mediaimport.Service over runkit: the registered sources, the dry run and
// its plan hash, one run at a time, the reports in ReportDir and the
// database snapshots in TmpDir. Its lock is its own: the core's
// media-server run may go on at the same time, each writing only its own
// reports. Create it with NewRunner.
type Runner struct {
	// ReportDir holds the reports (data/migrate/reports); TmpDir the run
	// folders with database snapshots (data/tmp/migrate), removed after
	// each run and on start.
	ReportDir, TmpDir string
	// BaseCtx is the addon's: a run survives the browser leaving and ends
	// with the addon.
	BaseCtx context.Context
	Now     func() time.Time
	// Progress (optional) is told after each source of a run and at the
	// end of a rollback (the admin.migrate.progress event).
	Progress func(ctx context.Context, p Progress)
	// LibraryReady: false (with the reason) until the core's library was
	// scanned once — matching by path needs it; nil = ready.
	LibraryReady func(ctx context.Context) (bool, string)
	// Leftovers (optional) are the lines of what does not move, put first
	// into every run's report; tmp is the run's snapshot folder.
	Leftovers func(ctx context.Context, tmp string) []Line
	Log       *slog.Logger

	mu        sync.Mutex
	running   string // report id of the run in progress
	runUID    string // its admin's uid (StartAs; "" = none: its rows name nobody)
	switching bool
	runs      sync.WaitGroup

	srcMu   sync.RWMutex
	sources []Source // by Order

	scans atomic.Int64 // reads of the old databases for the screen
	cache screenCache
}

// NewRunner removes the run folders an earlier process left (database
// snapshots hold tracker passwords and cookies) and returns r.
func NewRunner(r *Runner) *Runner {
	if r.TmpDir != "" {
		old, _ := filepath.Glob(filepath.Join(r.TmpDir, "run-*"))
		for _, p := range old {
			if err := os.RemoveAll(p); err != nil {
				r.Logger().Warn("remove old run folder", "dir", p, "err", err)
			}
		}
	}
	return r
}

// Register adds a source (at start-up, before the runner serves); a
// source with the same ID replaces the earlier one.
func (r *Runner) Register(src Source) {
	r.srcMu.Lock()
	defer r.srcMu.Unlock()
	r.sources = slices.DeleteFunc(r.sources, func(o Source) bool { return o.ID() == src.ID() })
	r.sources = append(r.sources, src)
	sort.SliceStable(r.sources, func(i, j int) bool { return r.sources[i].Order() < r.sources[j].Order() })
}

func (r *Runner) list() []Source {
	r.srcMu.RLock()
	defer r.srcMu.RUnlock()
	return slices.Clone(r.sources)
}

func (r *Runner) source(id string) Source {
	for _, src := range r.list() {
		if src.ID() == id {
			return src
		}
	}
	return nil
}

// Scans counts how many times the screen's status (Discover) read the old
// services.
func (r *Runner) Scans() int64 { return r.scans.Load() }

// Clock is the runner's time (UTC).
func (r *Runner) Clock() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// Logger is the runner's logger.
func (r *Runner) Logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default().With("component", "migrate")
}

func (r *Runner) baseCtx() context.Context {
	if r.BaseCtx != nil {
		return r.BaseCtx
	}
	return context.Background()
}

// Ready says whether a run may start (the library was scanned).
func (r *Runner) Ready(ctx context.Context) (bool, string) {
	if r.LibraryReady == nil {
		return true, ""
	}
	return r.LibraryReady(ctx)
}

// Wait returns once no run is left (BaseCtx ended: they stop at the next
// step, their report saved).
func (r *Runner) Wait() { r.runs.Wait() }

// Running is the id of the report being written, "" when idle.
func (r *Runner) Running() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Begin takes the runner for something that is not a run of Start (a
// rollback, id = its report; «Перейти на Antresol», id ""): ErrBusy while
// a run, rollback or switch holds it. Call end when done.
func (r *Runner) Begin(id string) (end func(), err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != "" || r.switching {
		return nil, ErrBusy
	}
	if id == "" {
		r.switching = true
		return func() {
			r.mu.Lock()
			r.switching = false
			r.mu.Unlock()
		}, nil
	}
	r.running = id
	return func() { r.idle(id) }, nil
}

// Go runs f in the background on BaseCtx; Wait waits for it.
func (r *Runner) Go(f func(ctx context.Context)) {
	r.runs.Add(1)
	go func() {
		defer r.runs.Done()
		f(r.baseCtx())
	}()
}

func (r *Runner) idle(id string) {
	r.mu.Lock()
	if r.running == id {
		r.running = ""
	}
	r.mu.Unlock()
}

// Save writes the report; a failure is logged.
func (r *Runner) Save(rep *Report) {
	if err := SaveReport(r.ReportDir, rep); err != nil {
		r.Logger().Error("save report", "id", rep.ID, "err", err)
	}
}

// Publish tells Progress where rep is.
func (r *Runner) Publish(ctx context.Context, rep *Report, source string, done, total int) {
	if r.Progress == nil {
		return
	}
	r.Progress(ctx, Progress{ReportID: rep.ID, Source: source, Done: done, Total: total,
		Counts: rep.Tally(), Finished: !rep.FinishedAt.IsZero()})
}

// RunTmp makes a private folder for snapshots.
func (r *Runner) RunTmp() (string, error) {
	base := r.TmpDir
	if base == "" {
		base = os.TempDir()
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(base, "run-*")
}

// --- what is there ---

// Discover looks at each old service: found or not, and what it has.
// While none of their files changed, the last look is reused.
func (r *Runner) Discover(ctx context.Context) []SourceStatus {
	srcs := r.list()
	paths := make([]string, 0, len(srcs))
	for _, src := range srcs {
		paths = append(paths, src.Path())
	}
	key := fingerprint(paths...)
	for _, src := range srcs {
		key += src.ID() + "\n"
	}
	key += i18n.FromContext(ctx)
	if v, ok := r.cache.get("discover", key); ok {
		return slices.Clone(v.([]SourceStatus))
	}
	r.scans.Add(1)
	out := r.discover(ctx, srcs)
	if ctx.Err() == nil {
		r.cache.put("discover", key, slices.Clone(out))
	}
	return out
}

func (r *Runner) discover(ctx context.Context, srcs []Source) []SourceStatus {
	tmp, err := r.RunTmp()
	if err != nil {
		r.Logger().Warn("discover: temporary folder", "err", err)
	} else {
		defer os.RemoveAll(tmp)
	}
	out := make([]SourceStatus, 0, len(srcs))
	for _, src := range srcs {
		st := SourceStatus{ID: src.ID(), Name: src.Name(), Path: src.Path()}
		if !present(st.Path, src.IsDir()) {
			st.Detail = i18n.T(ctx, "Не найден — смонтируйте %s", st.Path)
			out = append(out, st)
			continue
		}
		st.Found = true
		detail, err := src.Detail(ctx, tmp)
		if err != nil {
			if ctx.Err() != nil {
				return out
			}
			r.Logger().Warn("discover", "source", src.ID(), "err", err)
			var ue *mediaimport.UserError
			if errors.As(err, &ue) {
				st.Error = ue.Msg
			} else {
				st.Error = i18n.T(ctx, "Не удалось прочитать базу %s", st.Name)
			}
		}
		st.Detail = detail
		if st.Error == "" {
			st.Error = src.Problem(ctx)
		}
		out = append(out, st)
	}
	return out
}

func present(p string, dir bool) bool {
	if p == "" {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.IsDir() == dir
}

// --- runs ---

// PlanHash identifies what a run does (runkit): the addon's sources map no
// profiles, so it is the sources alone.
var PlanHash = mediaimport.PlanHash

// checkSources checks the chosen sources and puts them in run order.
func (r *Runner) checkSources(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, ErrNoSources
	}
	want := map[string]bool{}
	for _, id := range in {
		if r.source(id) == nil {
			return nil, ErrUnknownSource
		}
		want[id] = true
	}
	var out []string
	for _, src := range r.list() {
		if want[src.ID()] {
			out = append(out, src.ID())
		}
	}
	return out, nil
}

// Start checks req and runs it in the background (BaseCtx: a browser that
// leaves does not stop it); the report id comes back at once. A real run
// needs a finished dry run of the same plan from the last hour; no run
// starts before the library was scanned once. req.UserMap is not used:
// the addon's sources map no profiles. req.AdminID (the admin who starts
// it, from X-Antresol-User) is the run's AddedBy.
func (r *Runner) Start(ctx context.Context, req RunRequest) (string, error) {
	return r.StartAs(ctx, req, "")
}

// StartAs is Start with the uid of req.AdminID (X-Antresol-User
// uid): the follows and torrents the run makes are that profile's ("" =
// nobody's). Sources read it with AdminUID.
func (r *Runner) StartAs(ctx context.Context, req RunRequest, adminUID string) (string, error) {
	sources, err := r.checkSources(req.Sources)
	if err != nil {
		return "", err
	}
	if ok, _ := r.Ready(ctx); !ok {
		return "", ErrNotReady
	}
	hash := PlanHash(sources, nil)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running != "" || r.switching {
		return "", ErrBusy
	}
	if !req.DryRun {
		if err := r.checkPlan(req.PlanID, hash); err != nil {
			return "", err
		}
	}
	now := r.Clock()
	rep := &Report{ID: NewReportID(now), Sources: sources, DryRun: req.DryRun, PlanHash: hash, StartedAt: now, Lines: []Line{}}
	if !req.DryRun {
		rep.PlanID = req.PlanID
	}
	if err := SaveReport(r.ReportDir, rep); err != nil {
		return "", err
	}
	r.running, r.runUID = rep.ID, adminUID
	r.cache.forget() // the screen looks afresh after a run
	r.runs.Add(1)
	go r.run(rep, req.AdminID)
	return rep.ID, nil
}

// checkPlan: id is a finished dry run of the same plan, at most an hour old.
func (r *Runner) checkPlan(id, hash string) error {
	if id == "" {
		return ErrNeedsDryRun
	}
	plan, err := LoadReport(r.ReportDir, id)
	if err != nil || !plan.DryRun || plan.RollbackOf != "" || plan.FinishedAt.IsZero() || plan.PlanHash != hash {
		return ErrNeedsDryRun
	}
	if age := r.Clock().Sub(plan.FinishedAt); age > planTTL || age < -time.Minute {
		return ErrNeedsDryRun
	}
	return nil
}

func (r *Runner) run(rep *Report, adminID int64) {
	defer r.runs.Done()
	defer r.idle(rep.ID)
	ctx := r.baseCtx()
	log := r.Logger().With("report", rep.ID)
	tmp, err := r.RunTmp()
	if err != nil {
		log.Error("run folder", "err", err)
		rep.Add(Line{Kind: "leftover", Name: "Antresol", Status: StatusFailed, Reason: "Не удалось создать временную папку переноса — ничего не перенесено"})
		rep.FinishedAt = r.Clock()
		r.Save(rep)
		r.Publish(ctx, rep, "", 0, len(rep.Sources))
		return
	}
	defer os.RemoveAll(tmp)

	// what does not move comes first
	if r.Leftovers != nil {
		for _, l := range r.Leftovers(ctx, tmp) {
			rep.Add(l)
		}
	}
	r.Save(rep)

	for i, id := range rep.Sources {
		if ctx.Err() != nil {
			break
		}
		if err := r.importOne(ctx, id, rep, tmp, adminID); err != nil && ctx.Err() == nil {
			name := id
			if src := r.source(id); src != nil {
				name = src.Name()
			}
			log.Error("import", "source", id, "err", err)
			rep.Add(Line{Source: id, Name: name, Status: StatusFailed,
				Reason: fmt.Sprintf("Перенос из %s прервался: что-то пошло не так (подробности в журнале Antresol)", name)})
		}
		if i == len(rep.Sources)-1 && ctx.Err() == nil {
			rep.FinishedAt = r.Clock()
		}
		r.Save(rep)
		r.Publish(ctx, rep, id, i+1, len(rep.Sources))
	}
	if ctx.Err() != nil {
		log.Warn("run interrupted", "err", ctx.Err()) // FinishedAt stays empty: «Прерван»
	}
}

func (r *Runner) importOne(ctx context.Context, id string, rep *Report, tmp string, adminID int64) error {
	src := r.source(id)
	if src == nil {
		return ErrUnknownSource
	}
	return src.Import(ctx, Run{Report: rep, Tmp: tmp, DryRun: rep.DryRun, AdminID: adminID,
		Checkpoint: func() { r.Save(rep) }})
}

// AdminUID is the uid of the admin running the run of report id
// (StartAs); "" for another report or none.
func (r *Runner) AdminUID(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if id == "" || id != r.running {
		return ""
	}
	return r.runUID
}

// --- reports ---

// Report loads a saved report.
func (r *Runner) Report(id string) (*Report, error) {
	rep, err := LoadReport(r.ReportDir, id)
	if errors.Is(err, mediaimport.ErrBadReportID) || errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return rep, err
}

// Reports is the history, newest first, without the lines (the old
// build's reports, copied at import, included).
func (r *Runner) Reports() ([]*Report, error) { return ListReports(r.ReportDir) }

// --- the screen's cache ---

// screenCache keeps the screen's last look at the old services (each look
// copies their databases): reused while every file it read is unchanged
// (path, size, mtime) and dropped when a run starts («Проверить» looks
// afresh).
type screenCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	key string
	val any
}

func (c *screenCache) get(name, key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[name]
	if !ok || e.key != key {
		return nil, false
	}
	return e.val, true
}

func (c *screenCache) put(name, key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]cacheEntry{}
	}
	c.entries[name] = cacheEntry{key: key, val: v}
}

func (c *screenCache) forget() {
	c.mu.Lock()
	c.entries = nil
	c.mu.Unlock()
}

// fingerprint names the state of the files at paths: size and mtime of
// each, of a SQLite database's -wal file (its recent writes) and of a
// folder itself (entries added or removed).
func fingerprint(paths ...string) string {
	var b strings.Builder
	for _, p := range paths {
		for _, f := range []string{p, p + "-wal"} {
			b.WriteString(f)
			if st, err := os.Stat(f); err == nil {
				b.WriteString("|" + strconv.FormatInt(st.Size(), 10) + "|" + strconv.FormatInt(st.ModTime().UnixNano(), 10))
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}
