// Package follow is «Следить» for series (new episodes are downloaded as
// they air, like Sonarr) and «Хочу посмотреть» for movies (downloaded once
// released, replacing Seerr requests). Downloads go through «На сервер»
// (acquire), season by season or episode by episode.
package follow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/events"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol/addonkit"
)

// Kinds of follows.
const (
	KindSeries = "series"
	KindMovie  = "movie"
)

const (
	defaultInterval = 6 * time.Hour
	// defaultPause spaces tracker searches (every Acquire searches all
	// sources), across follows and checks.
	defaultPause = 10 * time.Second
	// retryAfter: a target without releases is searched again after this;
	// one that aired in the last freshDays after retryFresh (TMDB air dates
	// are the origin country's, often a day behind Europe).
	retryAfter = 24 * time.Hour
	retryFresh = 6 * time.Hour
	freshDays  = 3
	// goneFor: a target that was downloaded but did not show up in the
	// library (organize skipped it, the torrent was removed) waits this
	// long before it is searched again.
	goneFor = 7 * 24 * time.Hour
	// queuedFor: how long a queued label is remembered (to notice it
	// coming back as missing).
	queuedFor = 60 * 24 * time.Hour
	// maxPerCheck bounds the downloads one check starts; the rest come on
	// the next check.
	maxPerCheck = 10
	movieLabel  = "фильм"
	msgOffline  = "Нет связи с TMDB"
)

// startDelay: the first CheckAll after start (the server settles first).
var startDelay = time.Minute

// Errors of the service; the API maps them with errors.Is.
var (
	ErrInvalid     = errors.New("follow: invalid request")
	ErrNotFound    = errors.New("follow: not found")
	ErrUnavailable = errors.New("follow: TMDB unavailable")
	ErrInactive    = errors.New("follow: inactive")
)

// Acquirer is the part of *acquire.Service the service uses. When it also
// reports the disk (Disk), nothing is queued on a full or missing disk.
type Acquirer interface {
	Acquire(ctx context.Context, req acquire.AcquireRequest) (acquire.AcquireResult, error)
	Pending(ctx context.Context) ([]acquire.PendingAcquisition, error)
}

type diskReporter interface {
	Disk() (acquire.DiskUsage, error)
}

// TMDB is the part of *tmdb.Client the service uses.
type TMDB interface {
	TV(ctx context.Context, id int) (tmdb.Details, error)
	Season(ctx context.Context, tvID, n int) (tmdb.SeasonDetails, error)
	Movie(ctx context.Context, id int) (tmdb.Details, error)
	ImageURL(size, p string) string
}

// Library is the part of *library.Repo the service uses.
type Library interface {
	ItemByTMDB(ctx context.Context, kind string, tmdbID int64) (library.Item, error)
	// HaveEpisodes: episodes with a file, multi-episode files by range.
	HaveEpisodes(ctx context.Context, tmdbID int64) (map[[2]int]bool, error)
}

// Users is the server's profiles (coreadapt.Users): GET /users, each
// with its uid.
type Users interface {
	Profiles(ctx context.Context) ([]addonkit.User, error)
}

// Titles finds library items by title (coreadapt.Library).
type Titles interface {
	SearchTitles(ctx context.Context, kind, title string, limit int) ([]library.Item, error)
}

// msgDeletedProfile is AddedBy of a follow whose profile is gone: the
// follow stays (nobody deletes it but a person) and names no one.
const msgDeletedProfile = "Профиль удалён"

// Service checks follows. Create it with the fields set; Run checks all
// of them in the background and the follows Kick asks for.
type Service struct {
	DB       *sql.DB
	Acquire  Acquirer
	TMDB     TMDB
	Library  Library
	Interval time.Duration // between rounds; 0 = 6 h
	Now      func() time.Time
	Log      *slog.Logger
	Bus      *events.Bus // optional: follow.queued (admins), follow.checked
	// Users names the follows' authors, by uid; a follow whose profile
	// is gone has AddedBy «Профиль удалён» and UserID 0, for good once a
	// list was read without its uid (ForgetGone). Without it (or when it
	// fails: logged) the stored id and uid stay and AddedBy is "".
	Users Users
	// Titles (optional) finds the library's titles not yet matched to
	// TMDB (check.unmatched); without it none is.
	Titles Titles
	// Pause spaces tracker searches; 0 = 10 s, negative = none (tests).
	Pause time.Duration
	// Covered (optional) says which episodes of a series the title's pinned
	// release holds: those are the pin's to download (internal/pinned
	// grows the pin's download), the follow's own search leaves them out.
	// An error is logged and the follow goes on without it.
	Covered func(ctx context.Context, tmdbID int64) (map[[2]int]bool, error)
	// OnFollowed (optional) is told of a follow turned on (added, or made
	// active again): the pinned release's missing episodes are looked at
	// at once, not at the title's next refresh.
	OnFollowed func(ctx context.Context, f Follow)
	// FilmPinned (optional) says a film has a pinned release: the pin is
	// downloaded (internal/pinned), the follow's own search leaves it. An
	// error is logged and the follow searches as before.
	FilmPinned func(ctx context.Context, tmdbID int64) (bool, error)

	mu         sync.Mutex // one check at a time
	lastSearch time.Time

	// kicked: follows waiting for a check (a set: kicks coalesce);
	// kickSignal wakes Run (1 slot, never blocks).
	kickMu     sync.Mutex
	kicked     map[int64]bool
	kickOnce   sync.Once
	kickSignal chan struct{}
}

// Follow is one followed title.
type Follow struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	TMDBID int    `json:"tmdbId"`
	Title  string `json:"title"`
	Poster string `json:"poster"` // full TMDB URL (w342), "" = none
	Active bool   `json:"active"`
	// Source: vault, or sonarr|radarr for a follow taken over by the
	// migration (inactive until «Перейти на Vault»).
	Source      string     `json:"source"`
	LastChecked *time.Time `json:"lastChecked"`
	LastError   string     `json:"lastError"`
	// Missing: aired episodes (a released movie: 1) not in the library and
	// not downloading, as of the last check.
	Missing int `json:"missing"`
	// AddedBy: the profile that added it ("" when unknown or deleted); any
	// adult may switch a follow off, so the list says whose it is.
	AddedBy string `json:"addedBy"`
	// UserID: the profile that added it (0 = unknown or deleted), the id
	// the core is told; UserUID its stable key (users.uid, "" = nobody):
	// the API lets only that profile (by uid: an id may come back as
	// another profile's) and the admin change the follow.
	UserID  int64  `json:"-"`
	UserUID string `json:"-"`
	// Mine: the profile asking added it (by uid, never by name; set by
	// the API per request, false elsewhere).
	Mine bool `json:"mine"`
	// gone: the profile that added it was deleted (AddedBy says so).
	gone bool
	// Resolution: the quality its downloads prefer ("720p" | "1080p" |
	// "2160p"; "" = the household's release preferences).
	Resolution string `json:"resolution"`
}

// Resolutions a follow may prefer ("" = the household's release
// preferences).
var Resolutions = []string{"", "720p", "1080p", "2160p"}

// CheckResult is what one check queued ("S01E08", "S02", "фильм").
type CheckResult struct {
	Queued  []string `json:"queued"`
	Missing int      `json:"missing"`
	// Later: targets over the per-check cap, queued by the next checks.
	Later int `json:"later"`
	// Blocked: why nothing was queued (disk full or offline, a library
	// title not yet looked up in TMDB); "" = not blocked.
	Blocked string `json:"blocked,omitempty"`
}

// Queued is the data of the follow.queued event (admins).
type Queued struct {
	FollowID int64    `json:"followId"`
	Queued   []string `json:"queued"`
}

// Checked is the data of the follow.checked event (everyone): a check
// finished, the follow list may be reloaded.
type Checked struct {
	FollowID  int64    `json:"followId"`
	Queued    []string `json:"queued"`
	Missing   int      `json:"missing"`
	LastError string   `json:"lastError"`
	// UserID: the profile that added the follow (not sent); the event
	// stream gives the full payload only to it and to admins.
	UserID int64 `json:"-"`
}

// state is follows.state_json.
type state struct {
	Missing int `json:"missing"`
	// Tried: when a target last had no release (searched again after
	// retryAfter / retryFresh).
	Tried map[string]time.Time `json:"tried,omitempty"`
	// Queued: when a target was queued; Gone: when a queued target was
	// found missing again (deferred for goneFor).
	Queued map[string]time.Time `json:"queued,omitempty"`
	Gone   map[string]time.Time `json:"gone,omitempty"`
	// Import: Sonarr's choice of episodes, for a follow imported from it.
	Import *ImportState `json:"import,omitempty"`
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default().With("component", "follow")
}

// --- storage ---

const followCols = `id, kind, tmdb_id, title, poster, active, source, last_checked, last_error, state_json,
	COALESCE(user_id, 0), resolution, user_gone, COALESCE(user_uid, '')`

func scanFollow(sc interface{ Scan(...any) error }) (Follow, state, error) {
	var f Follow
	var checked sql.NullString
	var raw string
	var st state
	var gone bool
	if err := sc.Scan(&f.ID, &f.Kind, &f.TMDBID, &f.Title, &f.Poster, &f.Active, &f.Source, &checked, &f.LastError, &raw, &f.UserID, &f.Resolution, &gone, &f.UserUID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return f, st, err
	}
	if checked.Valid {
		if t, err := time.Parse(time.RFC3339, checked.String); err == nil {
			f.LastChecked = &t
		}
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			slog.Warn("follow: bad state_json", "id", f.ID, "err", err) // starts afresh
			st = state{}
		}
	}
	f.Missing = st.Missing
	if gone {
		f.AddedBy, f.UserID, f.UserUID, f.gone = msgDeletedProfile, 0, "", true
	}
	return f, st, nil
}

func (s *Service) load(ctx context.Context, id int64) (Follow, state, error) {
	f, st, err := scanFollow(s.DB.QueryRowContext(ctx, `SELECT `+followCols+` FROM follows WHERE id=?`, id))
	if err != nil {
		return f, st, err
	}
	return f.withAuthor(s.names(ctx)), st, nil
}

// names are the profiles by uid; nil when they cannot be told (no Users,
// the core did not answer, or a profile came without its uid: logged).
func (s *Service) names(ctx context.Context) map[string]addonkit.User {
	if s.Users == nil {
		return nil
	}
	us, err := s.Users.Profiles(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log().Warn("follow: profile names", "err", err)
		}
		return nil
	}
	m := make(map[string]addonkit.User, len(us))
	for _, u := range us {
		if u.UID == "" {
			s.log().Warn("follow: a profile without its uid", "id", u.ID)
			return nil
		}
		m[u.UID] = u
	}
	if err := ForgetGone(ctx, s.DB, us); err != nil && ctx.Err() == nil {
		s.log().Warn("follow: forget deleted profiles", "err", err)
	}
	return m
}

// ForgetGone makes the rows of profiles missing from profiles (the list
// the core gave) the deleted profile's for good, by uid: a follow's
// user_id and user_uid NULL, user_gone set; a pin's pinned_by and
// pinned_by_uid NULL. The core may give a deleted profile's id to a new
// one (users.id has no AUTOINCREMENT): the uid tells them apart, so
// without it a new profile would inherit the follow, its ownership and
// its letters. A row with an id but no uid (none is written
// so after MapUserUIDs) belongs to nobody known: it is forgotten too. An
// empty list, or one with a profile without its uid, forgets nobody (a
// server always has its admin; an older core sends no uid: such an
// answer is not trusted).
func ForgetGone(ctx context.Context, db *sql.DB, profiles []addonkit.User) error {
	if len(profiles) == 0 {
		return nil
	}
	uids := make([]string, 0, len(profiles))
	for _, u := range profiles {
		if u.UID == "" {
			return nil
		}
		uids = append(uids, u.UID)
	}
	b, err := json.Marshal(uids)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `UPDATE follows SET user_id = NULL, user_uid = NULL, user_gone = 1
		WHERE (user_id IS NOT NULL OR user_uid IS NOT NULL)
		  AND (user_uid IS NULL OR user_uid NOT IN (SELECT value FROM json_each(?)))`, string(b)); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE pinned_releases SET pinned_by = NULL, pinned_by_uid = NULL
		WHERE (pinned_by IS NOT NULL OR pinned_by_uid IS NOT NULL)
		  AND (pinned_by_uid IS NULL OR pinned_by_uid NOT IN (SELECT value FROM json_each(?)))`, string(b))
	return err
}

// withAuthor names f's author from profiles (by uid; nil: not known, f
// as it is); a profile no longer there makes f anonymous (UserID 0,
// AddedBy «Профиль удалён»), though another profile may have its id now.
func (f Follow) withAuthor(profiles map[string]addonkit.User) Follow {
	if (f.UserID == 0 && f.UserUID == "") || profiles == nil {
		return f
	}
	if u, ok := profiles[f.UserUID]; ok && f.UserUID != "" {
		f.AddedBy, f.UserID = u.Name, u.ID
		return f
	}
	f.AddedBy, f.UserID, f.UserUID, f.gone = msgDeletedProfile, 0, "", true
	return f
}

// Get returns one follow.
func (s *Service) Get(ctx context.Context, id int64) (Follow, error) {
	f, _, err := s.load(ctx, id)
	return f.localized(ctx), err
}

// ByTMDB returns the follow of a title; ErrNotFound when it is not followed.
func (s *Service) ByTMDB(ctx context.Context, kind string, tmdbID int) (Follow, error) {
	f, _, err := scanFollow(s.DB.QueryRowContext(ctx, `SELECT `+followCols+` FROM follows WHERE kind=? AND tmdb_id=?`, kind, tmdbID))
	if err != nil {
		return f, err
	}
	return f.withAuthor(s.names(ctx)).localized(ctx), nil
}

// List returns all follows, newest first.
func (s *Service) List(ctx context.Context) ([]Follow, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT `+followCols+` FROM follows ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Follow{}
	for rows.Next() {
		f, _, err := scanFollow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	names := s.names(ctx)
	for i := range out {
		out[i] = out[i].withAuthor(names).localized(ctx)
	}
	return out, nil
}

// Add follows a title; a title already followed returns its follow as is.
// The title and poster come from TMDB. A new follow is checked right away
// (Kick), not with the next round.
func (s *Service) Add(ctx context.Context, by addonkit.User, kind string, tmdbID int) (Follow, error) {
	return s.AddWithResolution(ctx, by, kind, tmdbID, "")
}

// AddWithResolution is Add with the quality a new follow's downloads
// prefer (one of Resolutions), written with the row: its first check and
// OnFollowed see it. A title already followed keeps its own. by is the
// profile adding it (X-Antresol-User); without its uid the follow is
// nobody's (never an id alone).
func (s *Service) AddWithResolution(ctx context.Context, by addonkit.User, kind string, tmdbID int, resolution string) (Follow, error) {
	if (kind != KindSeries && kind != KindMovie) || tmdbID <= 0 || !slices.Contains(Resolutions, resolution) {
		return Follow{}, ErrInvalid
	}
	if f, err := s.ByTMDB(ctx, kind, tmdbID); !errors.Is(err, ErrNotFound) {
		return f, err
	}
	d, err := s.details(ctx, kind, tmdbID)
	if err != nil {
		return Follow{}, err
	}
	title := strings.TrimSpace(d.Title)
	if title == "" {
		title = strings.TrimSpace(d.OriginalTitle)
	}
	poster := ""
	if d.Poster != "" {
		poster = s.TMDB.ImageURL("w342", d.Poster)
	}
	id, uid := owner(by)
	res, err := s.DB.ExecContext(ctx, `INSERT INTO follows (kind, tmdb_id, title, poster, user_id, user_uid, created_at, resolution) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(kind, tmdb_id) DO NOTHING`, kind, tmdbID, title, poster, id, uid, s.now().UTC().Format(time.RFC3339Nano), resolution)
	if err != nil {
		return Follow{}, err
	}
	f, err := s.ByTMDB(ctx, kind, tmdbID)
	if n, _ := res.RowsAffected(); err == nil && n == 1 {
		s.Kick(f.ID)
		s.followed(ctx, f)
	}
	return f, err
}

// owner is the user_id and user_uid columns of a row by (both NULL
// without a uid or an id).
func owner(by addonkit.User) (id, uid any) {
	if by.ID > 0 && by.UID != "" {
		return by.ID, by.UID
	}
	return nil, nil
}

// Remove stops following.
func (s *Service) Remove(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM follows WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetActive turns a follow on or off; inactive follows are not checked.
func (s *Service) SetActive(ctx context.Context, id int64, active bool) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE follows SET active=? WHERE id=?`, active, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if active {
		if f, _, err := s.load(ctx, id); err == nil {
			s.followed(ctx, f)
		}
	}
	return nil
}

// followed tells OnFollowed of a follow turned on.
func (s *Service) followed(ctx context.Context, f Follow) {
	if s.OnFollowed != nil {
		s.OnFollowed(ctx, f)
	}
}

// SetResolution sets the quality a follow's downloads prefer (one of
// Resolutions).
func (s *Service) SetResolution(ctx context.Context, id int64, resolution string) error {
	if !slices.Contains(Resolutions, resolution) {
		return ErrInvalid
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE follows SET resolution=? WHERE id=?`, resolution, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Following reports whether a title has an active follow («Докачивать
// новые»), and its author: its id now and uid (zero = unknown or
// deleted).
func (s *Service) Following(ctx context.Context, kind string, tmdbID int64) (by addonkit.User, active bool, err error) {
	var id sql.NullInt64
	var uid sql.NullString
	err = s.DB.QueryRowContext(ctx, `SELECT user_id, user_uid FROM follows WHERE kind=? AND tmdb_id=? AND active=1`, kind, tmdbID).Scan(&id, &uid)
	if errors.Is(err, sql.ErrNoRows) {
		return addonkit.User{}, false, nil
	}
	if err != nil {
		return addonkit.User{}, false, err
	}
	f := Follow{UserID: id.Int64, UserUID: uid.String}.withAuthor(s.names(ctx))
	return addonkit.User{ID: f.UserID, UID: f.UserUID}, true, nil
}

// --- checks ---

func (s *Service) signal() chan struct{} {
	s.kickOnce.Do(func() { s.kickSignal = make(chan struct{}, 1) })
	return s.kickSignal
}

// Kick asks Run to check one follow soon (outside any request; the result
// arrives as follow.checked). Never blocks and never drops a follow; kicks
// of a follow that is still waiting add nothing.
func (s *Service) Kick(id int64) {
	s.kickMu.Lock()
	if s.kicked == nil {
		s.kicked = map[int64]bool{}
	}
	s.kicked[id] = true
	s.kickMu.Unlock()
	select {
	case s.signal() <- struct{}{}:
	default: // Run is already woken
	}
}

// takeKicked empties the set of waiting follows, in id order.
func (s *Service) takeKicked() []int64 {
	s.kickMu.Lock()
	defer s.kickMu.Unlock()
	ids := make([]int64, 0, len(s.kicked))
	for id := range s.kicked {
		ids = append(ids, id)
	}
	clear(s.kicked)
	slices.Sort(ids)
	return ids
}

// runKicked checks the follows Kick asked for (a follow kicked again
// during its own check is checked once more).
func (s *Service) runKicked(ctx context.Context) {
	for _, id := range s.takeKicked() {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.Check(ctx, id); err != nil && ctx.Err() == nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrInactive) {
			s.log().Warn("check follow", "id", id, "err", err)
		}
	}
}

// Run checks every active follow a minute after the start, then every
// Interval, and the follows Kick asks for, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	wake := s.signal()
	t := time.NewTimer(startDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			s.runKicked(ctx)
		case <-t.C:
			if err := s.CheckAll(ctx); err != nil && ctx.Err() == nil {
				s.log().Warn("check follows", "err", err)
			}
			t.Reset(interval)
		}
	}
}

// CheckAll checks the active follows, the longest unchecked first. A
// follow's error does not stop the others; TMDB unreachable (or refusing
// the key) ends the round, as the rest would fail the same way.
func (s *Service) CheckAll(ctx context.Context) error {
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM follows WHERE active=1 ORDER BY last_checked IS NOT NULL, last_checked, id`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		_, err := s.Check(ctx, id)
		switch {
		case err == nil, errors.Is(err, ErrInactive), errors.Is(err, ErrNotFound):
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrUnavailable) && !titleFailure(err):
			s.log().Info("TMDB is unreachable: follows wait for the next round", "err", err)
			return nil
		default:
			s.log().Warn("check follow", "id", id, "err", err)
		}
	}
	return nil
}

// titleFailure: TMDB answered with an error status for this request
// (a 5xx or a rate limit), not for every request (401: the key).
func titleFailure(err error) bool {
	var se *tmdb.StatusError
	return errors.As(err, &se) && se.Code != 401
}

// Check looks for what a follow lacks and queues it: the aired episodes
// not in the library and not downloading yet (see missingEpisodes), or a
// released movie. A movie in the library completes its follow (inactive).
// Targets without releases wait 24 h (6 h when just aired); a target that
// was downloaded and is missing again waits 7 days. Nothing is queued
// while the disk is full or offline, or while the library holds the title
// not yet looked up in TMDB (it may already be there).
func (s *Service) Check(ctx context.Context, id int64) (CheckResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, st, err := s.load(ctx, id)
	if err != nil {
		return CheckResult{}, err
	}
	if !f.Active {
		return CheckResult{}, ErrInactive
	}
	now := s.now()
	c := &check{s: s, f: f, st: &st, now: now, res: CheckResult{Queued: []string{}, Missing: st.Missing}}
	err = c.run(ctx)
	if err != nil && ctx.Err() != nil {
		return CheckResult{}, ctx.Err()
	}
	c.res.Later, c.res.Blocked = c.later, c.blocked
	note := c.note()
	switch {
	case errors.Is(err, ErrUnavailable) && titleFailure(err):
		note = "TMDB ответил ошибкой, проверка повторится позже"
	case errors.Is(err, ErrUnavailable):
		note = msgOffline
	case errors.Is(err, ErrNotFound):
		note, err = "Этого названия больше нет в TMDB", nil
	case err != nil:
		note = fmt.Sprintf("Не удалось проверить: %s", err.Error())
	default:
		st.Missing = c.res.Missing
	}
	raw, jerr := json.Marshal(st)
	if jerr != nil {
		return CheckResult{}, jerr
	}
	active := !c.done
	if _, uerr := s.DB.ExecContext(context.WithoutCancel(ctx), `UPDATE follows SET last_checked=?, last_error=?, state_json=?, active=active AND ? WHERE id=?`,
		now.UTC().Format(time.RFC3339), note, string(raw), active, id); uerr != nil {
		return CheckResult{}, uerr
	}
	if err == nil && len(c.res.Queued) > 0 {
		s.publish(EventFollowQueued, Queued{FollowID: id, Queued: c.res.Queued})
	}
	s.publish(EventFollowChecked, Checked{FollowID: id, Queued: c.res.Queued, Missing: st.Missing, LastError: note, UserID: f.UserID})
	if err != nil {
		return CheckResult{}, err
	}
	return c.res.localized(ctx), nil
}

// Preview is the check Check would do now, inactive follows included:
// Queued is what it would queue. Nothing is downloaded and nothing of the
// follow is written (state, last check), so a later Check still sees the
// same; the backoffs of earlier checks are honoured.
func (s *Service) Preview(ctx context.Context, id int64) (CheckResult, error) {
	f, st, err := s.load(ctx, id)
	if err != nil {
		return CheckResult{}, err
	}
	c := &check{s: s, f: f, st: &st, now: s.now(), dry: true, res: CheckResult{Queued: []string{}, Missing: st.Missing}}
	if err := c.run(ctx); err != nil {
		if ctx.Err() != nil {
			return CheckResult{}, ctx.Err()
		}
		return CheckResult{}, err
	}
	c.res.Later, c.res.Blocked = c.later, c.blocked
	return c.res.localized(ctx), nil
}

func (c *check) run(ctx context.Context) error {
	if c.f.Kind == KindMovie {
		return c.movie(ctx)
	}
	return c.series(ctx)
}

func (s *Service) publish(typ string, data any) {
	if s.Bus != nil {
		s.Bus.Publish(events.Event{Type: typ, Data: data})
	}
}

// check is one run of Check.
type check struct {
	s    *Service
	f    Follow
	st   *state
	now  time.Time
	res  CheckResult
	done bool // a movie follow is fulfilled
	dry  bool // Preview: queue nothing, only say what would be queued

	blocked   string   // why nothing was queued
	notes     []string // worth knowing, nothing blocked
	noRelease []string // targets without releases
	gone      []string // downloaded, yet missing again
	later     int      // over the per-check cap
	failed    []string // "S01E08: why"
}

func (c *check) note() string {
	var parts []string
	if c.blocked != "" {
		parts = append(parts, c.blocked)
	}
	parts = append(parts, c.notes...)
	if len(c.noRelease) > 0 {
		parts = append(parts, fmt.Sprintf("Раздач пока нет: %s", strings.Join(c.noRelease, ", ")))
	}
	if len(c.gone) > 0 {
		parts = append(parts, fmt.Sprintf("Скачано, но не появилось в медиатеке (повтор через неделю): %s", strings.Join(c.gone, ", ")))
	}
	if c.later > 0 {
		parts = append(parts, fmt.Sprintf("Ещё %d — в следующей проверке", c.later))
	}
	parts = append(parts, c.failed...)
	return strings.Join(parts, ". ")
}

// localized is f with its stored note (made in Russian by a check) in the
// language of ctx.
func (f Follow) localized(ctx context.Context) Follow {
	f.LastError = LocalizeNote(ctx, f.LastError)
	if f.gone {
		f.AddedBy = i18n.Localize(ctx, msgDeletedProfile)
	}
	return f
}

// localized is r for the request's language: the labels ("фильм") and the
// reason nothing was queued. The events keep the Russian texts.
func (r CheckResult) localized(ctx context.Context) CheckResult {
	lang := i18n.FromContext(ctx)
	if lang == i18n.RU {
		return r
	}
	q := make([]string, len(r.Queued))
	for i, l := range r.Queued {
		q[i] = i18n.LocalizeL(lang, l)
	}
	r.Queued = q
	r.Blocked = LocalizeNoteL(lang, r.Blocked)
	return r
}

// LocalizeNote is LocalizeNoteL in the language of ctx.
func LocalizeNote(ctx context.Context, note string) string {
	return LocalizeNoteL(i18n.FromContext(ctx), note)
}

// LocalizeNoteL translates a check's note (Follow.LastError, the
// follow.checked event): the messages check.note joins with ". " are
// translated one by one. A message may hold ". " itself, so each run of
// pieces is tried shortest first and taken when it translates fully (no
// Russian left), else when it translates at all; "S01E08: why" (a failed
// target) is the label and the translated reason.
func LocalizeNoteL(lang, note string) string {
	if lang == i18n.RU || note == "" {
		return note
	}
	if tr := localizePiece(lang, note); !release.HasCyrillic(tr) {
		return tr
	}
	pieces := strings.Split(note, ". ")
	var out []string
	for i := 0; i < len(pieces); {
		best, bestEnd := "", -1
		for j := i; j < len(pieces); j++ {
			cand := strings.Join(pieces[i:j+1], ". ")
			tr := localizePiece(lang, cand)
			if tr == cand {
				continue
			}
			if !release.HasCyrillic(tr) {
				best, bestEnd = tr, j
				break
			}
			if bestEnd < 0 {
				best, bestEnd = tr, j
			}
		}
		if bestEnd < 0 {
			out = append(out, pieces[i])
			i++
			continue
		}
		out = append(out, best)
		i = bestEnd + 1
	}
	return strings.Join(out, ". ")
}

// failedRE is a failed target in a note: "S01E08: why", "фильм: why".
var failedRE = regexp.MustCompile(`^(S\d+(?:E\d+(?:-E\d+)?)?|` + movieLabel + `): (.+)$`)

func localizePiece(lang, s string) string {
	if tr := i18n.LocalizeL(lang, s); tr != s {
		return tr
	}
	if m := failedRE.FindStringSubmatch(s); m != nil {
		if why := i18n.LocalizeL(lang, m[2]); why != m[2] {
			return i18n.LocalizeL(lang, m[1]) + ": " + why
		}
	}
	return s
}

func (c *check) series(ctx context.Context) error {
	s := c.s
	d, err := s.details(ctx, KindSeries, c.f.TMDBID)
	if err != nil {
		return err
	}
	if blocked, err := c.unmatched(ctx, library.KindSeries, d); err != nil || blocked {
		return err
	}
	have, err := s.Library.HaveEpisodes(ctx, int64(c.f.TMDBID))
	if err != nil {
		return err
	}
	pending, err := s.Acquire.Pending(ctx)
	if err != nil {
		return err
	}
	// queued downloads count as had: what each release covers
	packs := map[int]bool{}
	for _, p := range pending {
		if p.Kind != acquire.KindSeries || p.TMDBID != c.f.TMDBID {
			continue
		}
		if p.Episode == 0 {
			packs[p.Season] = true
			continue
		}
		for e := p.Episode; e <= max(p.Episode, p.EpisodeEnd); e++ {
			have[[2]int{p.Season, e}] = true
		}
	}
	perSeason := map[int]int{}
	for k := range have {
		perSeason[k[0]]++
	}
	var seasons []tmdb.SeasonDetails
	for _, sc := range d.Seasons {
		if sc.Number <= 0 || sc.Episodes <= 0 || packs[sc.Number] || perSeason[sc.Number] >= sc.Episodes {
			continue // specials, empty, queued as a whole or all there
		}
		sd, err := s.TMDB.Season(ctx, c.f.TMDBID, sc.Number)
		if errors.Is(err, tmdb.ErrNotFound) {
			continue
		}
		if err != nil {
			return s.tmdbErr(ctx, err)
		}
		if sd.Number == 0 {
			sd.Number = sc.Number
		}
		seasons = append(seasons, sd)
	}
	if c.st.Import != nil {
		importedHave(seasons, have, *c.st.Import)
	}
	covered := s.covered(ctx, c.f.TMDBID)
	c.res.Missing = missingCount(seasons, have, c.now, covered)
	targets := missingEpisodes(seasons, have, c.now, covered)
	reqs := make([]acquire.AcquireRequest, 0, len(targets))
	labels := make([]string, 0, len(targets))
	fresh := map[string]bool{}
	since := c.now.AddDate(0, 0, -freshDays).Format("2006-01-02")
	for _, t := range targets {
		reqs = append(reqs, acquire.AcquireRequest{TMDBID: c.f.TMDBID, Kind: acquire.KindSeries, Season: t.Season, Episode: t.Episode, Background: true})
		labels = append(labels, t.label())
		if latestAir(seasons, t) >= since {
			fresh[t.label()] = true
		}
	}
	return c.queue(ctx, reqs, labels, fresh)
}

func (c *check) movie(ctx context.Context) error {
	s := c.s
	_, err := s.Library.ItemByTMDB(ctx, library.KindMovie, int64(c.f.TMDBID))
	if err == nil {
		c.done = true // in the library: the wish came true
		c.res.Missing = 0
		return nil
	}
	if !errors.Is(err, library.ErrNotFound) {
		return err
	}
	d, err := s.details(ctx, KindMovie, c.f.TMDBID)
	if err != nil {
		return err
	}
	if blocked, err := c.unmatched(ctx, library.KindMovie, d); err != nil || blocked {
		return err
	}
	day := c.now.Format("2006-01-02")
	released := d.Year > 0 && d.Year <= c.now.Year()
	if d.ReleaseDate != "" {
		released = d.ReleaseDate <= day
	}
	c.res.Missing = 0
	if !released {
		return nil
	}
	pending, err := s.Acquire.Pending(ctx)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if p.Kind == acquire.KindMovie && p.TMDBID == c.f.TMDBID {
			return nil // downloading
		}
	}
	c.res.Missing = 1
	if s.FilmPinned != nil {
		pinned, err := s.FilmPinned(ctx, int64(c.f.TMDBID))
		switch {
		case err != nil && ctx.Err() == nil:
			s.log().Warn("pinned release of a film", "tmdb", c.f.TMDBID, "err", err)
		case pinned:
			return nil // the pin's to download
		}
	}
	fresh := map[string]bool{movieLabel: d.ReleaseDate >= c.now.AddDate(0, 0, -freshDays).Format("2006-01-02")}
	return c.queue(ctx, []acquire.AcquireRequest{{TMDBID: c.f.TMDBID, Kind: acquire.KindMovie}}, []string{movieLabel}, fresh)
}

// keep returns the entries of m for the labels (the rest are dropped).
func keep(m map[string]time.Time, labels []string) map[string]time.Time {
	out := map[string]time.Time{}
	for _, l := range labels {
		if t, ok := m[l]; ok {
			out[l] = t
		}
	}
	return out
}

// queue downloads the targets («На сервер», added by the follow's
// author), skipping those in a backoff.
func (c *check) queue(ctx context.Context, reqs []acquire.AcquireRequest, labels []string, fresh map[string]bool) error {
	s := c.s
	// forget targets that are no longer wanted (Queued is kept while the
	// download runs: its label is no target then)
	c.st.Tried = keep(c.st.Tried, labels)
	c.st.Gone = keep(c.st.Gone, labels)
	for l, t := range c.st.Queued {
		if c.now.Sub(t) > queuedFor {
			delete(c.st.Queued, l)
		}
	}
	if c.st.Queued == nil {
		c.st.Queued = map[string]time.Time{}
	}
	if len(reqs) == 0 {
		return nil
	}
	if msg := s.diskBlocked(); msg != "" {
		c.blocked = msg
		return nil
	}
	by, err := s.author(ctx, c.f.ID)
	if err != nil {
		return err
	}
	started := 0
	for i, req := range reqs {
		l := labels[i]
		// queued before and missing again: downloaded, yet not in the
		// library (or the torrent was removed) — wait a week
		if g, ok := c.st.Gone[l]; ok {
			if c.now.Sub(g) < goneFor {
				c.gone = append(c.gone, l)
				continue
			}
			delete(c.st.Gone, l)
		} else if _, ok := c.st.Queued[l]; ok {
			c.st.Gone[l] = c.now
			delete(c.st.Queued, l)
			c.gone = append(c.gone, l)
			continue
		}
		wait := retryAfter
		if fresh[l] {
			wait = retryFresh
		}
		if t, ok := c.st.Tried[l]; ok && c.now.Sub(t) < wait {
			c.noRelease = append(c.noRelease, l)
			continue
		}
		if started >= maxPerCheck {
			c.later++
			continue
		}
		if c.dry {
			started++
			c.res.Queued = append(c.res.Queued, l)
			continue
		}
		if err := s.wait(ctx); err != nil {
			return err
		}
		started++
		req.Mode, req.UserID, req.UserUID, req.Resolution = acquire.ModeDownload, by.ID, by.UID, c.f.Resolution
		got, err := s.Acquire.Acquire(ctx, req)
		switch {
		case err == nil:
			delete(c.st.Tried, l)
			c.st.Queued[l] = c.now
			c.res.Queued = append(c.res.Queued, l)
			if got.Note != "" {
				c.notes = append(c.notes, l+": "+got.Note)
			}
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, acquire.ErrSourcesFailed):
			// some sources did not answer: no backoff, the next round retries
			c.failed = append(c.failed, l+": "+err.Error())
		case errors.Is(err, acquire.ErrNoRelease):
			c.st.Tried[l] = c.now
			c.noRelease = append(c.noRelease, l)
		default:
			// one failure does not stop the others
			c.failed = append(c.failed, l+": "+err.Error())
			s.log().Warn("follow download", "follow", c.f.ID, "target", l, "err", err)
		}
	}
	return nil
}

// unmatched: the library holds an item of this title (and year, ±1)
// without a TMDB id. While the enricher has not looked it up yet, nothing
// is queued (it may be this very title); once it has (and found nothing),
// the follow goes on with a note.
func (c *check) unmatched(ctx context.Context, kind string, d tmdb.Details) (bool, error) {
	if c.s.Titles == nil {
		return false, nil
	}
	// one search per normalised title or alias (the core's search
	// normalises, matches aliases and leaves missing items out); a match
	// not looked up yet wins over one TMDB found nothing for
	var found *library.Item
	foundTried := false
	seen := map[string]bool{}
	for _, t := range append([]string{d.Title, d.OriginalTitle}, d.AltTitles...) {
		n := release.Normalize(t)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		its, err := c.s.Titles.SearchTitles(ctx, kind, t, 0)
		if err != nil {
			return false, err
		}
		for i, it := range its {
			if it.TmdbID != 0 || it.ParentID != 0 || (it.Year != 0 && d.Year != 0 && abs(it.Year-d.Year) > 1) {
				continue
			}
			if tried := tmdbTried(it.MetaJSON); found == nil || (foundTried && !tried) {
				found, foundTried = &its[i], tried
			}
		}
		if found != nil && !foundTried {
			break
		}
	}
	if found == nil {
		return false, nil
	}
	title := found.Title
	if foundTried {
		c.notes = append(c.notes, fmt.Sprintf("В медиатеке есть «%s» без данных TMDB — если это то же самое, привяжите её", title))
		return false, nil
	}
	c.blocked = fmt.Sprintf("В медиатеке есть «%s» без данных TMDB: загрузки ждут, пока она распознается", title)
	return true, nil
}

// tmdbTried: the item's meta has a tmdb_at (the enricher looked it up).
func tmdbTried(meta string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(meta), &m) != nil {
		return false
	}
	v, ok := m["tmdb_at"]
	return ok && string(v) != "null"
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// covered is what Covered says of a series ({} without it, or on its
// error: logged, the follow goes on as if no pin held anything).
func (s *Service) covered(ctx context.Context, tmdbID int) map[[2]int]bool {
	if s.Covered == nil {
		return nil
	}
	m, err := s.Covered(ctx, int64(tmdbID))
	if err != nil {
		if ctx.Err() == nil {
			s.log().Warn("episodes of the pinned release", "tmdb", tmdbID, "err", err)
		}
		return nil
	}
	return m
}

// author is the profile that followed the title (the admin sees who
// added a download; «Моя медиатека» by its uid); zero when it was
// deleted.
func (s *Service) author(ctx context.Context, id int64) (addonkit.User, error) {
	f, _, err := s.load(ctx, id)
	return addonkit.User{ID: f.UserID, UID: f.UserUID}, err
}

// diskBlocked says why nothing may be queued now: the media disk is full
// (the «ждёт места» guard would only hold the downloads), offline or
// cannot be read.
func (s *Service) diskBlocked() string {
	dr, ok := s.Acquire.(diskReporter)
	if !ok {
		return ""
	}
	u, err := dr.Disk()
	switch {
	case err != nil:
		s.log().Warn("disk usage", "err", err)
		return "Не удалось проверить место на диске: новые загрузки подождут"
	case u.Offline != "":
		return "Медиадиск недоступен: новые загрузки подождут"
	case u.Level == acquire.LevelFull:
		return "Диск почти заполнен: новые загрузки подождут, пока освободится место"
	}
	return ""
}

// wait spaces tracker searches by Pause.
func (s *Service) wait(ctx context.Context) error {
	p := s.Pause
	if p == 0 {
		p = defaultPause
	}
	if p < 0 {
		return nil
	}
	if !s.lastSearch.IsZero() {
		if d := time.Until(s.lastSearch.Add(p)); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			}
		}
	}
	s.lastSearch = time.Now()
	return nil
}

// details fetches a title from TMDB: ErrNotFound when TMDB has no such
// title, ErrUnavailable when it cannot be reached or fails.
func (s *Service) details(ctx context.Context, kind string, id int) (tmdb.Details, error) {
	if s.TMDB == nil {
		return tmdb.Details{}, ErrUnavailable
	}
	var d tmdb.Details
	var err error
	if kind == KindMovie {
		d, err = s.TMDB.Movie(ctx, id)
	} else {
		d, err = s.TMDB.TV(ctx, id)
	}
	if err != nil {
		return d, s.tmdbErr(ctx, err)
	}
	return d, nil
}

func (s *Service) tmdbErr(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, tmdb.ErrNotFound):
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
