package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	mediaimport "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/mediaimport/runkit"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
)

// SourcePaths are where the old services' data is (read only); Jellyfin,
// Kavita and Audiobookshelf are the core import's (mediaimport).
type SourcePaths struct {
	Prowlarr, Sonarr, Radarr string
	QBBackup                 string // qBittorrent's BT_backup folder
}

// DefaultPaths lays the old services out under importDir (/import in the
// container, every folder mounted read-only); Prowlarr's database has its
// own setting.
func DefaultPaths(importDir, prowlarrDB string) (SourcePaths, LeftoverPaths) {
	at := func(p ...string) string { return filepath.Join(append([]string{importDir}, p...)...) }
	return SourcePaths{
		Prowlarr: prowlarrDB,
		Sonarr:   at("sonarr", "sonarr.db"),
		Radarr:   at("radarr", "radarr.db"),
		QBBackup: at("qbittorrent", "BT_backup"),
	}, LeftoverPaths{
		LibrarrDB:  at("librarr", "librarr.db"),
		SeerrDB:    at("seerr", "db", "db.sqlite3"),
		QBRSSRules: at("qbittorrent", "rss", "download_rules.json"),
		QBWatched:  at("qbittorrent", "watched_folders.json"),
	}
}

// Errors of the full build's part of the service; the API maps them with
// errors.Is (the core's are mediaimport's).
var (
	ErrNothingToRollback = errors.New("migrate: the report moved no torrents")
	ErrRolledBack        = errors.New("migrate: the report was given back already")
	ErrArrNotStopped     = errors.New("migrate: Sonarr and Radarr are not stopped")
	ErrCountChanged      = errors.New("migrate: the number of downloads changed")
	ErrNoFollow          = errors.New("migrate: following needs a TMDB key")
	ErrSwitchBlocked     = errors.New("migrate: downloads are blocked now")
)

// SwitchPlan is what «Перейти на Vault» would start downloading:
// Episodes (a season pack counts its episodes) and Movies; Errors: follows
// TMDB could not answer for (not counted, switched on all the same).
// Blocked: why nothing can be counted now (a full disk, a library title not
// yet recognised) — the downloads would start later, unconfirmed, so the
// switch waits until it clears.
type SwitchPlan struct {
	Follows  int    `json:"follows"`
	Episodes int    `json:"episodes"`
	Movies   int    `json:"movies"`
	Errors   int    `json:"errors"`
	Blocked  string `json:"blocked"`
}

// BlockedError is ErrSwitchBlocked with the reason to show.
type BlockedError struct{ Reason string }

func (e *BlockedError) Error() string        { return "migrate: switch blocked: " + e.Reason }
func (e *BlockedError) Is(target error) bool { return target == ErrSwitchBlocked }

// MigrationState: SwitchedAt is when «Перейти на Vault» was pressed.
type MigrationState struct {
	SwitchedAt *time.Time `json:"switchedAt"`
}

// ReportSummary is a report in the history: no lines; Torrents counts the
// qBittorrent torrents it moved («Вернуть раздачи в qBittorrent»).
type ReportSummary struct {
	*Report
	Torrents int `json:"torrents"`
}

// Service is the addon's «Переезд»: its own run (the embedded Runner)
// with Prowlarr, Sonarr, Radarr and qBittorrent registered, the leftovers
// of the old services, «Перейти на Antresol» and giving the torrents back
// to qBittorrent. Create it with New.
type Service struct {
	*Runner

	DB      *sql.DB
	Engine  TorrentEngine    // the torrent engine (qBittorrent import, rollback)
	Indexer ProwlarrImporter // *indexer.Manager
	TMDB    TMDBFinder       // nil without a key (never a typed nil)
	Follow  *follow.Service  // nil without a TMDB key: no «Перейти на Vault»
	// Library is the core's library (coreadapt.Library): the qBittorrent
	// import matches and binds the library files of each torrent.
	Library LibraryFiles

	Paths    SourcePaths
	Leftover LeftoverPaths
	// QB holds the qBittorrent settings (WebURL, login, Paths, LinkDir,
	// StateDir, LibraryRoots); BTBackupDir, DryRun, AddedBy and Checkpoint
	// are set per run.
	QB              QBOptions
	FlareSolverrSet bool
	// HoldsChanged (optional; the holds pusher's Changed, set by main) is
	// called after a real qBittorrent import and after a rollback: the
	// engine's downloads changed, so the core's file holds go out again.
	// It is read at each call.
	HoldsChanged func()
}

// New registers the addon's sources and leftovers on r and returns the
// service around it.
func New(r *Runner, s Service) *Service {
	s.Runner = r
	svc := &s
	r.Register(&arrSource{s: svc, id: SourceProwlarr, name: "Prowlarr", order: 10, path: s.Paths.Prowlarr})
	r.Register(&arrSource{s: svc, id: SourceSonarr, name: "Sonarr", order: 20, path: s.Paths.Sonarr})
	r.Register(&arrSource{s: svc, id: SourceRadarr, name: "Radarr", order: 30, path: s.Paths.Radarr})
	r.Register(&qbSource{s: svc})
	lp := s.Leftover
	r.Leftovers = func(ctx context.Context, tmp string) []Line { return Leftovers(ctx, lp, tmp) }
	return svc
}

func (s *Service) holdsChanged() {
	if s.HoldsChanged != nil {
		s.HoldsChanged()
	}
}

// --- the sources ---

// arrSource is Prowlarr, Sonarr or Radarr.
type arrSource struct {
	s     *Service
	id    string
	name  string
	order int
	path  string
}

func (a *arrSource) ID() string                     { return a.id }
func (a *arrSource) Name() string                   { return a.name }
func (a *arrSource) Order() int                     { return a.order }
func (a *arrSource) Path() string                   { return a.path }
func (a *arrSource) IsDir() bool                    { return false }
func (a *arrSource) Problem(context.Context) string { return "" }

// counts runs the counts on one snapshot of the database at path.
func counts(ctx context.Context, tmp, path string, cs ...mediaimport.TableCount) ([]int, error) {
	out := make([]int, len(cs))
	for i, c := range cs {
		n, err := mediaimport.CountIn(ctx, tmp, path, c)
		if err != nil {
			return nil, err
		}
		out[i] = n
	}
	return out, nil
}

func (a *arrSource) Detail(ctx context.Context, tmp string) (string, error) {
	switch a.id {
	case SourceProwlarr:
		n, err := mediaimport.CountIn(ctx, tmp, a.path, mediaimport.TableCount{Table: "Indexers"})
		return i18n.Plural(ctx, n, "индексатор", "индексатора", "индексаторов"), err
	case SourceSonarr:
		c, err := counts(ctx, tmp, a.path, mediaimport.TableCount{Table: "Series"}, mediaimport.TableCount{Table: "Series", Where: "Monitored=1"})
		if err != nil {
			return "", err
		}
		return i18n.T(ctx, "%s, %d отслеживается", i18n.Plural(ctx, c[0], "сериал", "сериала", "сериалов"), c[1]), nil
	}
	c, err := counts(ctx, tmp, a.path, mediaimport.TableCount{Table: "Movies"}, mediaimport.TableCount{Table: "Movies", Where: "Monitored=1"})
	if err != nil {
		return "", err
	}
	return i18n.T(ctx, "%s, %d отслеживается", i18n.Plural(ctx, c[0], "фильм", "фильма", "фильмов"), c[1]), nil
}

func (a *arrSource) Import(ctx context.Context, r mediaimport.Run) error {
	s, rep := a.s, r.Report
	arr := ArrOptions{ProwlarrDB: s.Paths.Prowlarr, SonarrDB: s.Paths.Sonarr, RadarrDB: s.Paths.Radarr, UserID: r.AdminID,
		DryRun: r.DryRun, FlareSolverrSet: s.FlareSolverrSet, Tmp: r.Tmp, Now: s.Now}
	if r.Report != nil {
		// the admin's uid from X-Antresol-User (StartAs): the follows are theirs
		arr.UserUID = s.AdminUID(r.Report.ID)
	}
	switch a.id {
	case SourceProwlarr:
		if s.Indexer == nil {
			rep.Add(Line{Source: a.id, Kind: "source", Name: "Prowlarr", Status: StatusFailed, Reason: "Источники Antresol недоступны"})
			return nil
		}
		return ImportProwlarr(ctx, arr, s.Indexer, rep)
	case SourceSonarr:
		return ImportSonarr(ctx, arr, s.DB, s.TMDB, rep)
	}
	return ImportRadarr(ctx, arr, s.DB, s.TMDB, rep)
}

// qbSource is qBittorrent: a folder of .torrent files and its Web UI.
type qbSource struct{ s *Service }

func (q *qbSource) ID() string   { return SourceQBittorrent }
func (q *qbSource) Name() string { return "qBittorrent" }
func (q *qbSource) Order() int   { return 90 } // last: Vault checks its torrents from disk one by one (hours)
func (q *qbSource) Path() string { return q.s.Paths.QBBackup }
func (q *qbSource) IsDir() bool  { return true }

func (q *qbSource) Detail(ctx context.Context, _ string) (string, error) {
	ents, err := os.ReadDir(q.Path())
	if err != nil {
		return "", &mediaimport.UserError{Msg: i18n.T(ctx, "Не удалось прочитать папку BT_backup"), Err: err}
	}
	n := 0
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".torrent") {
			n++
		}
	}
	return i18n.Plural(ctx, n, "раздача", "раздачи", "раздач"), nil
}

func (q *qbSource) Problem(ctx context.Context) string {
	if q.s.QB.WebURL == "" {
		return i18n.T(ctx, "Не задан адрес qBittorrent (ANTRESOL_TORRENTS_QBITTORRENT_URL) — раздачи не перенести")
	}
	return ""
}

func (q *qbSource) Import(ctx context.Context, r mediaimport.Run) error {
	s := q.s
	if s.Engine == nil {
		r.Report.Add(Line{Source: SourceQBittorrent, Kind: "torrent", Name: "qBittorrent", Status: StatusFailed, Reason: "Торренты Antresol недоступны — раздачи не перенесены"})
		return nil
	}
	o := s.QB
	o.BTBackupDir, o.DryRun, o.AddedBy = s.Paths.QBBackup, r.DryRun, r.AdminID
	if r.Report != nil {
		// the admin's uid from X-Antresol-User (StartAs): the torrents are theirs
		o.AddedByUID = s.AdminUID(r.Report.ID)
	}
	o.Checkpoint = r.Checkpoint
	err := ImportQBittorrent(ctx, o, s.Engine, s.Library, r.Report)
	if !r.DryRun {
		s.holdsChanged()
	}
	return err
}

// --- reports ---

// Torrents counts the qBittorrent torrents a real run's report moved
// («Вернуть раздачи в qBittorrent»); 0 for a report that cannot be loaded.
func (s *Service) Torrents(id string) int {
	full, err := LoadReport(s.ReportDir, id)
	if err != nil {
		return 0
	}
	return len(movedTorrents(full))
}

// movedTorrents: the qBittorrent lines a rollback gives back.
func movedTorrents(r *Report) []Line {
	return r.LinesWhere(func(l Line) bool {
		return l.Source == SourceQBittorrent && l.Status == StatusImported && l.Ref != ""
	})
}

// Rollback gives the torrents a real run moved back to qBittorrent
// (RollbackQB), in the background, into a new report.
func (s *Service) Rollback(ctx context.Context, reportID string) (string, error) {
	now := s.Clock()
	repID := NewReportID(now)
	end, err := s.Begin(repID)
	if err != nil {
		return "", err
	}
	started := false
	defer func() {
		if !started {
			end()
		}
	}()
	from, err := s.Report(reportID)
	if err != nil {
		return "", err
	}
	if from.DryRun || from.RollbackOf != "" || len(movedTorrents(from)) == 0 {
		return "", ErrNothingToRollback
	}
	if done, err := s.rolledBack(from.ID); err != nil {
		return "", err
	} else if done {
		return "", ErrRolledBack
	}
	rep := &Report{ID: repID, Sources: []string{SourceQBittorrent}, RollbackOf: from.ID, StartedAt: now, Lines: []Line{}}
	if err := SaveReport(s.ReportDir, rep); err != nil {
		return "", err
	}
	started = true
	s.Go(func(ctx context.Context) {
		defer end()
		if s.Engine == nil {
			rep.Add(Line{Source: SourceQBittorrent, Kind: "torrent", Name: "qBittorrent", Status: StatusFailed, Reason: "Торренты Antresol недоступны — ничего не возвращено"})
		} else {
			o := s.QB
			o.BTBackupDir, o.DryRun = s.Paths.QBBackup, false
			o.Checkpoint = func() { s.Save(rep) }
			if err := RollbackQB(ctx, o, s.Engine, from, rep); err != nil && ctx.Err() == nil {
				s.Logger().Error("rollback", "report", from.ID, "err", err)
			}
		}
		if ctx.Err() == nil {
			rep.FinishedAt = s.Clock()
		}
		s.Save(rep)
		s.holdsChanged()
		s.Publish(ctx, rep, SourceQBittorrent, 1, 1)
	})
	return rep.ID, nil
}

// rolledBack: the report fromID was given back already — a rollback of it
// finished, or one was cut short (a restart) and a real qBittorrent run
// started after it, which may have moved the same torrents again; giving
// them back now would silently undo that run. A rollback cut short with no
// run since may be finished.
func (s *Service) rolledBack(fromID string) (bool, error) {
	list, err := ListReports(s.ReportDir)
	if err != nil {
		return false, err
	}
	var cut []time.Time
	for _, r := range list {
		if r.RollbackOf != fromID {
			continue
		}
		if !r.FinishedAt.IsZero() {
			return true, nil
		}
		cut = append(cut, r.StartedAt)
	}
	for _, r := range list {
		if r.DryRun || r.RollbackOf != "" || !slices.Contains(r.Sources, SourceQBittorrent) {
			continue
		}
		for _, c := range cut {
			if r.StartedAt.After(c) {
				return true, nil
			}
		}
	}
	return false, nil
}

// --- «Перейти на Vault» ---

const migrationKey = "migration"

type pendingFollow struct {
	id, tmdb int64
}

// pending are the follows taken over from Sonarr and Radarr that
// «Перейти на Vault» switches on (inactive, never checked: one switched off
// after the switch stays off).
func (s *Service) pending(ctx context.Context) ([]pendingFollow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, tmdb_id FROM follows
		WHERE active=0 AND source IN ('sonarr','radarr') AND last_checked IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingFollow
	for rows.Next() {
		var p pendingFollow
		if err := rows.Scan(&p.id, &p.tmdb); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PendingFollows counts the follows «Перейти на Vault» switches on.
func (s *Service) PendingFollows(ctx context.Context) (int, error) {
	p, err := s.pending(ctx)
	return len(p), err
}

var (
	episodeLabel = regexp.MustCompile(`^S\d+E\d+$`)
	seasonLabel  = regexp.MustCompile(`^S(\d+)$`)
)

// SwitchPreview is what switching on the imported follows would download
// now (follow.Preview of each; nothing is queued).
func (s *Service) SwitchPreview(ctx context.Context) (SwitchPlan, error) {
	if s.Follow == nil {
		return SwitchPlan{}, ErrNoFollow
	}
	list, err := s.pending(ctx)
	if err != nil {
		return SwitchPlan{}, err
	}
	plan := SwitchPlan{Follows: len(list)}
	for _, p := range list {
		eps, movies, blocked, err := s.previewOne(ctx, p)
		if blocked != "" && plan.Blocked == "" {
			plan.Blocked = blocked
		}
		if err != nil {
			if ctx.Err() != nil {
				return SwitchPlan{}, ctx.Err()
			}
			s.Logger().Warn("switch preview", "follow", p.id, "err", err)
			plan.Errors++
			continue
		}
		plan.Episodes += eps
		plan.Movies += movies
	}
	return plan, nil
}

func (s *Service) previewOne(ctx context.Context, p pendingFollow) (episodes, movies int, blocked string, err error) {
	res, err := s.Follow.Preview(ctx, p.id)
	if err != nil {
		return 0, 0, "", err
	}
	blocked = res.Blocked
	for _, l := range res.Queued {
		switch {
		case l == "фильм":
			movies++
		case episodeLabel.MatchString(l):
			episodes++
		case seasonLabel.MatchString(l):
			n, _ := strconv.Atoi(seasonLabel.FindStringSubmatch(l)[1])
			sd, err := s.Follow.TMDB.Season(ctx, int(p.tmdb), n)
			if err != nil {
				return 0, 0, blocked, err
			}
			for _, e := range sd.Episodes {
				if e.Number > 0 {
					episodes++
				}
			}
		default:
			return 0, 0, blocked, errors.New("unknown follow target " + strconv.Quote(l))
		}
	}
	return episodes, movies, blocked, nil
}

// Switch is «Перейти на Vault»: once the admin says Sonarr and Radarr are
// stopped (both would download the same) and confirms the number of
// downloads the dialog showed (confirm = episodes + movies, recounted
// here), the imported follows are switched on. Pressing it again switches
// nothing (0).
func (s *Service) Switch(ctx context.Context, confirm int, arrStopped bool) (int, error) {
	end, err := s.Begin("")
	if err != nil {
		return 0, err
	}
	defer end()

	if s.Follow == nil {
		return 0, ErrNoFollow
	}
	if !arrStopped {
		return 0, ErrArrNotStopped
	}
	n, err := s.PendingFollows(ctx)
	if err != nil || n == 0 {
		return 0, err
	}
	plan, err := s.SwitchPreview(ctx)
	if err != nil {
		return 0, err
	}
	if plan.Blocked != "" {
		return 0, &BlockedError{Reason: plan.Blocked}
	}
	if plan.Episodes+plan.Movies != confirm {
		return 0, ErrCountChanged
	}
	activated, err := s.Follow.ActivateImported(ctx)
	if err != nil {
		return activated, err
	}
	st, err := s.State(ctx)
	if err == nil && st.SwitchedAt == nil {
		raw, _ := json.Marshal(map[string]string{"switchedAt": s.Clock().Format(time.RFC3339)})
		_, err = s.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, migrationKey, string(raw))
	}
	return activated, err
}

// State says whether «Перейти на Vault» was done.
func (s *Service) State(ctx context.Context) (MigrationState, error) {
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, migrationKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return MigrationState{}, nil
	}
	if err != nil {
		return MigrationState{}, err
	}
	var v struct {
		SwitchedAt string `json:"switchedAt"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return MigrationState{}, err
	}
	t, err := time.Parse(time.RFC3339, v.SwitchedAt)
	if err != nil {
		return MigrationState{}, nil
	}
	return MigrationState{SwitchedAt: &t}, nil
}
