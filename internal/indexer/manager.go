package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// Source statuses.
const (
	StatusOK      = "ok"
	StatusError   = "error"
	StatusUnknown = "unknown"
)

// RedactedValue replaces secrets in Source.Redacted; sending it back in an
// update keeps the stored secret.
const RedactedValue = "••••••"

// Source is a configured tracker.
type Source struct {
	ID           int64             `json:"id"`
	DefinitionID string            `json:"definitionId"`
	Name         string            `json:"name"`
	Enabled      bool              `json:"enabled"`
	Settings     map[string]string `json:"settings"`
	Status       string            `json:"status"` // ok | error | unknown
	LastError    string            `json:"lastError,omitempty"`
	LastChecked  time.Time         `json:"lastChecked,omitzero"`
	Priority     int               `json:"priority"` // lower = preferred
	// FallbackFor: this source is the backup way into that one (RuTracker
	// by cookie for RuTracker by login): searched only when it fails.
	FallbackFor int64 `json:"fallbackFor,omitempty"`
	// Origin: OriginProwlarr for a source a Prowlarr indexer list brought
	// in, "" for one added by hand; SyncedAt: the last import that listed
	// it. Searches never go through Prowlarr: the source is asked itself.
	Origin   string    `json:"origin,omitempty"`
	SyncedAt time.Time `json:"syncedAt,omitzero"`
}

// OriginProwlarr is Source.Origin of a source from Prowlarr's list.
const OriginProwlarr = "prowlarr"

// SourcePatch changes a source; nil fields stay. Settings are merged; an
// empty value deletes a setting, RedactedValue keeps it.
type SourcePatch struct {
	Name     *string           `json:"name,omitempty"`
	Enabled  *bool             `json:"enabled,omitempty"`
	Settings map[string]string `json:"settings,omitempty"`
	Priority *int              `json:"priority,omitempty"`
}

// Options configure a Manager.
type Options struct {
	HTTP          *http.Client  // shared transport; every source gets its own cookie jar
	FlareSolverr  string        // FlareSolverr base URL, "" = none
	SearchTimeout time.Duration // per source, default 20 s
	TestTimeout   time.Duration // login + test search, default 60 s (FlareSolverr is slow)
	Logger        *slog.Logger
	// OnStatus is called after a source status was stored (for the events
	// bus); it must not block.
	OnStatus func(id int64, status, message string)
	// Studios (optional) are the core's favourite studios
	// (studios.Core.List): a search scores with them ahead of the
	// stored prefs' own, without duplicates; the stored prefs keep theirs.
	Studios func(ctx context.Context) []string
}

// Manager stores sources, runs searches and downloads.
type Manager struct {
	db            *sql.DB
	cat           *Catalog
	http          *http.Client
	flareSolverr  string
	searchTimeout time.Duration
	testTimeout   time.Duration
	log           *slog.Logger
	onStatus      func(id int64, status, message string)
	studios       func(ctx context.Context) []string
	now           func() time.Time

	mu      sync.Mutex
	runners map[int64]*cachedRunner
	// resting: sources whose sign-in a tracker refused, left out of
	// searches until then (see rest)
	resting map[int64]time.Time
	// failing: when a source's last search failed (a timeout, an
	// unreachable site), until one of its searches answers (see
	// markFailed); flakyBudget is what a quick search gives such a source
	failing     map[int64]time.Time
	flakyBudget time.Duration
	// paused: sources FlareSolverr failed for, left out of searches for a
	// while (see solverOutcome); gate lets one request at a time through to
	// FlareSolverr for all of them
	paused map[int64]solverPause
	gate   *cardigann.SolverGate

	// importMu serialises Prowlarr imports: their «already added» check and
	// the insert must be one step, or two requests add a tracker twice.
	importMu sync.Mutex

	// directHTTP is the client of a direct-download source: opdsHTTP (the
	// home-network guard unless allow_local). A field for tests.
	directHTTP func(Source) *http.Client

	// topics: what release pages said of their voice-overs (topic.go)
	topics topicCache
}

// A runner keeps the login session of a source between searches.
type cachedRunner struct {
	key      string
	def      *cardigann.Definition
	runner   *cardigann.Runner
	settings map[string]string // as stored when the runner was built or last saved
}

// NewManager creates a Manager over the sources table.
func NewManager(db *sql.DB, cat *Catalog, o Options) *Manager {
	m := &Manager{
		db: db, cat: cat, http: o.HTTP, flareSolverr: o.FlareSolverr,
		searchTimeout: o.SearchTimeout, testTimeout: o.TestTimeout, log: o.Logger, onStatus: o.OnStatus,
		studios: o.Studios, flakyBudget: FlakyBudget, gate: cardigann.NewSolverGate(),
		now: time.Now, runners: map[int64]*cachedRunner{}, directHTTP: opdsHTTP,
	}
	if m.searchTimeout <= 0 {
		m.searchTimeout = 20 * time.Second
	}
	if m.testTimeout <= 0 {
		m.testTimeout = 60 * time.Second
	}
	if m.log == nil {
		m.log = slog.Default()
	}
	if m.http == nil {
		m.http = &http.Client{Timeout: 90 * time.Second}
	}
	return m
}

// Catalog returns the definitions catalog.
func (m *Manager) Catalog() *Catalog { return m.cat }

// Redacted returns a copy with secret settings replaced by RedactedValue.
func (s Source) Redacted() Source {
	return s.redacted(nil)
}

func (s Source) redacted(def *cardigann.Definition) Source {
	out := s
	out.Settings = make(map[string]string, len(s.Settings))
	for k, v := range s.Settings {
		if v != "" && isSecret(def, k) {
			v = RedactedValue
		}
		out.Settings[k] = v
	}
	return out
}

// MarshalJSON hides secrets (known secret fields such as password and
// cookie), so a Source never reaches the browser with them by accident.
// Manager.Redact additionally knows password-type fields of the definition.
func (s Source) MarshalJSON() ([]byte, error) {
	type plain Source
	return json.Marshal(plain(s.redacted(nil)))
}

// Redact hides secrets of a source using its definition (password-type
// settings of unusual names included). For API responses.
func (m *Manager) Redact(s Source) Source {
	def, _ := m.cat.Definition(s.DefinitionID)
	return s.redacted(def)
}

const sourceCols = `id, definition_id, name, enabled, settings_json, status, last_error, last_checked, priority, fallback_for, origin, synced_at`

func scanSource(row interface{ Scan(...any) error }) (Source, error) {
	var s Source
	var settings string
	var checked sql.NullString
	var fallbackFor sql.NullInt64
	var origin, synced sql.NullString
	if err := row.Scan(&s.ID, &s.DefinitionID, &s.Name, &s.Enabled, &settings, &s.Status, &s.LastError, &checked, &s.Priority, &fallbackFor, &origin, &synced); err != nil {
		return s, err
	}
	s.FallbackFor, s.Origin = fallbackFor.Int64, origin.String
	if synced.Valid {
		s.SyncedAt, _ = time.Parse(time.RFC3339, synced.String)
	}
	s.Settings = map[string]string{}
	if err := json.Unmarshal([]byte(settings), &s.Settings); err != nil {
		return s, fmt.Errorf("indexer: source %d settings: %w", s.ID, err)
	}
	if checked.Valid {
		s.LastChecked, _ = time.Parse(time.RFC3339, checked.String)
	}
	return s, nil
}

// Sources lists all sources by priority, then name.
func (m *Manager) Sources(ctx context.Context) ([]Source, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT `+sourceCols+` FROM sources ORDER BY priority, name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// HasEnabled reports whether at least one source is enabled (the apps'
// «Найти и добавить» appears only then).
func (m *Manager) HasEnabled(ctx context.Context) (bool, error) {
	var n int
	err := m.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sources WHERE enabled<>0)`).Scan(&n)
	return n != 0, err
}

// ErrNotFound: no such source.
var ErrNotFound = errors.New("источник не найден")

// Get returns one source.
func (m *Manager) Get(ctx context.Context, id int64) (Source, error) {
	s, err := scanSource(m.db.QueryRowContext(ctx, `SELECT `+sourceCols+` FROM sources WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, err
}

// ValidationError names a missing required field (Russian message).
type ValidationError struct{ Field, Label string }

func (e *ValidationError) Error() string {
	return fmt.Sprintf("Заполните поле «%s»", e.Label)
}

func (m *Manager) validate(defID string, settings map[string]string) (CatalogEntry, error) {
	entry, ok := m.cat.Get(defID)
	if !ok {
		return entry, fmt.Errorf("%w (%s)", ErrNoDefinition, defID)
	}
	for _, s := range entry.Settings {
		if s.Required && strings.TrimSpace(settings[s.Name]) == "" {
			return entry, &ValidationError{Field: s.Name, Label: s.Label}
		}
	}
	return entry, nil
}

func cleanSettings(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if v = strings.TrimSpace(v); v != "" && k != "" {
			out[k] = v
		}
	}
	return out
}

// Add creates a source and tests it right away. A failed test still saves
// the source (status error, human message in LastError); only missing
// required fields or an unknown definition are returned as errors.
func (m *Manager) Add(ctx context.Context, defID string, settings map[string]string) (Source, error) {
	settings = cleanSettings(settings)
	entry, err := m.validate(defID, settings)
	if err != nil {
		return Source{}, err
	}
	s, err := m.insert(ctx, defID, entry.Name, settings, true, 25)
	if err != nil {
		return Source{}, err
	}
	if err := m.Test(ctx, s.ID); err != nil && ctx.Err() != nil {
		// saved with status unknown; the caller went away
		saved, _ := m.Get(context.WithoutCancel(ctx), s.ID)
		return saved, ctx.Err()
	}
	return m.Get(ctx, s.ID)
}

func (m *Manager) insert(ctx context.Context, defID, name string, settings map[string]string, enabled bool, priority int) (Source, error) {
	raw, err := json.Marshal(settings)
	if err != nil {
		return Source{}, err
	}
	res, err := m.db.ExecContext(ctx, `INSERT INTO sources (definition_id, name, enabled, settings_json, priority) VALUES (?, ?, ?, ?, ?)`,
		defID, name, enabled, string(raw), priority)
	if err != nil {
		return Source{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Source{}, err
	}
	if err := m.linkFallbacks(ctx); err != nil {
		return Source{}, err
	}
	return m.Get(ctx, id)
}

// Update changes a source. When settings change the source is re-tested.
func (m *Manager) Update(ctx context.Context, id int64, p SourcePatch) (Source, error) {
	s, err := m.Get(ctx, id)
	if err != nil {
		return s, err
	}
	changed := false
	if p.Settings != nil {
		next := maps.Clone(s.Settings)
		for k, v := range p.Settings {
			switch v = strings.TrimSpace(v); v {
			case RedactedValue:
			case "":
				delete(next, k)
			default:
				next[k] = v
			}
		}
		if _, err := m.validate(s.DefinitionID, next); err != nil {
			return s, err
		}
		changed = !maps.Equal(next, s.Settings)
		s.Settings = next
	}
	if p.Name != nil && strings.TrimSpace(*p.Name) != "" {
		s.Name = strings.TrimSpace(*p.Name)
	}
	if p.Enabled != nil {
		s.Enabled = *p.Enabled
	}
	if p.Priority != nil {
		s.Priority = *p.Priority
	}
	raw, err := json.Marshal(s.Settings)
	if err != nil {
		return s, err
	}
	if _, err := m.db.ExecContext(ctx, `UPDATE sources SET name=?, enabled=?, settings_json=?, priority=? WHERE id=?`,
		s.Name, s.Enabled, string(raw), s.Priority, id); err != nil {
		return s, err
	}
	m.wake(id) // someone looked at it: searches ask it again
	if changed {
		m.dropRunner(id)
		if s.Enabled {
			m.Test(ctx, id)
		} else {
			m.setStatus(ctx, id, StatusUnknown, "")
		}
	}
	return m.Get(ctx, id)
}

// Remove deletes a source.
func (m *Manager) Remove(ctx context.Context, id int64) error {
	backups, err := m.backupsOf(ctx, id)
	if err != nil {
		return err
	}
	res, err := m.db.ExecContext(ctx, `DELETE FROM sources WHERE id=? OR fallback_for=?`, id, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	m.dropRunner(id)
	for _, b := range backups {
		m.dropRunner(b) // a backup goes with its source
	}
	return nil
}

func (m *Manager) backupsOf(ctx context.Context, id int64) ([]int64, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT id FROM sources WHERE fallback_for=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var b int64
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// fallbackDefs: a definition that is a backup way into another tracker's
// definition (the primary).
var fallbackDefs = map[string]string{"rutracker-org-cookie": "rutracker"}

// linkFallbacks makes a backup-way source (RuTracker by cookie) the
// fallback of the tracker's primary source when both exist and neither is
// linked yet; one backup per primary.
func (m *Manager) linkFallbacks(ctx context.Context) error {
	for fb, primary := range fallbackDefs {
		_, err := m.db.ExecContext(ctx, `UPDATE sources SET fallback_for = (SELECT p.id FROM sources p WHERE p.definition_id = ? ORDER BY p.id LIMIT 1)
			WHERE id = (SELECT MIN(id) FROM sources WHERE definition_id = ? AND fallback_for IS NULL)
			  AND EXISTS (SELECT 1 FROM sources p WHERE p.definition_id = ?)
			  AND NOT EXISTS (SELECT 1 FROM sources b WHERE b.fallback_for = (SELECT p.id FROM sources p WHERE p.definition_id = ? ORDER BY p.id LIMIT 1))`,
			primary, fb, primary, primary)
		if err != nil {
			return err
		}
	}
	return nil
}

// FlareSolverr is the FlareSolverr address the searches use ("" = none).
func (m *Manager) FlareSolverr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flareSolverr
}

// SetFlareSolverr changes the FlareSolverr address ("" = none): the next
// request of each source goes through it (its runner is built anew), and
// the sources paused for the old one are asked again.
func (m *Manager) SetFlareSolverr(url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if url != m.flareSolverr {
		clear(m.paused)
	}
	m.flareSolverr = url
}

// RecheckFailed tests, one after another, the enabled sources whose last
// check failed (after the FlareSolverr address changed); their new status
// reaches the page through OnStatus. It returns when all are done.
func (m *Manager) RecheckFailed(ctx context.Context) {
	list, err := m.Sources(ctx)
	if err != nil {
		m.log.Info("indexer: recheck: sources", "err", err)
		return
	}
	for _, s := range list {
		if s.Enabled && s.Status == StatusError && ctx.Err() == nil {
			_ = m.Test(ctx, s.ID)
		}
	}
}

// Test logs in and runs an empty search; the outcome is stored as the
// source status. The returned error carries a human message.
func (m *Manager) Test(ctx context.Context, id int64) error {
	s, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if isOPDS(s) {
		return m.testOPDS(ctx, s)
	}
	r, err := m.runner(s)
	if err == nil {
		tctx, cancel := context.WithTimeout(ctx, m.testTimeout)
		err = r.Test(tctx)
		if err != nil && tctx.Err() != nil && ctx.Err() == nil {
			err = fmt.Errorf("%w: %w", errSourceTimeout, err)
		}
		cancel()
	}
	if err != nil && ctx.Err() != nil {
		// cancelled by the caller: nothing is known about the site
		m.setStatus(ctx, id, StatusUnknown, "")
		return ctx.Err()
	}
	if err != nil {
		m.log.Info("indexer: source test failed", "source", s.Name, "err", cardigann.RedactText(err.Error()))
		err = m.pausedFailure(id, err) // the test may have paused the source: say until when
		m.setStatus(ctx, id, StatusError, humanError(err))
		return humanize("", err)
	}
	m.wake(id)
	m.setStatus(ctx, id, StatusOK, "")
	return nil
}

// restAfterRefusedSignIn: how long a source whose sign-in was refused (an
// expired cookie, a wrong password, a captcha) is left out of searches.
// Every search would be one more refused login the tracker sees, and the
// sign-in does not mend by itself: new settings or a passing test wake it.
const restAfterRefusedSignIn = 6 * time.Hour

// rest leaves source id out of searches for restAfterRefusedSignIn.
func (m *Manager) rest(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.resting == nil {
		m.resting = map[int64]time.Time{}
	}
	m.resting[id] = m.now().Add(restAfterRefusedSignIn)
}

// wake ends the rest of source id, and what is held against it for a
// failed search, a FlareSolverr that failed on it or a failed sign-in.
func (m *Manager) wake(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.resting, id)
	delete(m.failing, id)
	delete(m.paused, id)
	if c, ok := m.runners[id]; ok {
		c.runner.ResetSignIn()
	}
}

// FlakyBudget is how long a quick search (WithSettle) waits for a source
// that failed its last search, instead of the search timeout; flakyFor is
// how long after that failure the source is still held to it.
const (
	FlakyBudget = 4 * time.Second
	flakyFor    = 15 * time.Minute
)

// markFailed notes that a search of source id failed on its own (not
// cancelled): a timeout, an unreachable or broken site. Every quick search
// that fails it again keeps the note; an answer, or flakyFor without one,
// ends it. (A refused sign-in is rest's: that source is not asked at all.)
func (m *Manager) markFailed(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failing == nil {
		m.failing = map[int64]time.Time{}
	}
	m.failing[id] = m.now()
}

// markAnswered ends the note of markFailed: the source answered.
func (m *Manager) markAnswered(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.failing, id)
}

// isFlaky: source id failed its last search lately (see markFailed).
func (m *Manager) isFlaky(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flakyLocked(id)
}

// flakyLocked is isFlaky under m.mu: a note older than flakyFor is dropped.
func (m *Manager) flakyLocked(id int64) bool {
	at, ok := m.failing[id]
	if ok && !m.now().Before(at.Add(flakyFor)) {
		delete(m.failing, id)
		return false
	}
	return ok
}

// sourceBudget is how long a search of source id waits for it: the search
// timeout, or in a quick search (WithSettle: a start that must not wait
// for a dead site) a short one for a source that failed its last search —
// it is still asked, so one that is back answers at once and is trusted
// again, but a dead one holds the start up for FlakyBudget, not for the
// whole timeout. A search for a list that wants every source (no
// WithSettle) keeps the timeout.
func (m *Manager) sourceBudget(ctx context.Context, id int64) time.Duration {
	if !Settling(ctx) {
		return m.searchTimeout
	}
	if !m.isFlaky(id) {
		return m.searchTimeout
	}
	return min(m.searchTimeout, m.flakyBudget)
}

// isResting: source id is left out of searches now.
func (m *Manager) isResting(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	until, ok := m.resting[id]
	if ok && !m.now().Before(until) {
		delete(m.resting, id)
		return false
	}
	return ok
}

func (m *Manager) setStatus(ctx context.Context, id int64, status, msg string) {
	// the caller's context may be cancelled already; the status must stick
	ctx = context.WithoutCancel(ctx)
	if _, err := m.db.ExecContext(ctx, `UPDATE sources SET status=?, last_error=?, last_checked=? WHERE id=?`,
		status, msg, m.now().UTC().Format(time.RFC3339), id); err != nil {
		m.log.Warn("indexer: save source status", "id", id, "err", err)
		return
	}
	if m.onStatus != nil {
		m.onStatus(id, status, msg)
	}
}

// runner returns the cached runner of a source (keeps its login session) or
// builds a new one when the settings or the definition changed.
func (m *Manager) runner(s Source) (*cardigann.Runner, error) {
	def, err := m.cat.Definition(s.DefinitionID)
	if err != nil {
		return nil, err
	}
	key := runnerKey(s.DefinitionID, s.Settings)
	m.mu.Lock()
	defer m.mu.Unlock()
	// a changed FlareSolverr address builds the runner anew (SetFlareSolverr)
	if c, ok := m.runners[s.ID]; ok && c.key == key && c.def == def && c.runner.FlareSolverr == m.flareSolverr {
		return c.runner, nil
	}
	r := &cardigann.Runner{
		Def: def, Settings: maps.Clone(s.Settings), HTTP: m.http,
		FlareSolverr: m.flareSolverr, Logger: m.log, SolverGate: m.gate,
	}
	r.Now = func() time.Time { return m.now() }
	id := s.ID
	r.OnSolve = func(err error) { m.solverOutcome(id, r, err) }
	if def.Download != nil && def.Download.Direct {
		// the site's pages choose the next requests and the response is
		// saved into the library: no home network (unless the admin allows
		// it), no client timeout over a long file (contexts bound it),
		// stricter redirects; FlareSolverr is reached as usual
		r.HTTP, r.SolverHTTP, r.Strict = m.directHTTP(s), m.http, true
	}
	if def.Login != nil && def.Login.Method == "cookie" {
		r.OnCookies = func(cookie string) { m.saveSessionCookie(id, r, cookie) }
	}
	m.runners[s.ID] = &cachedRunner{key: key, def: def, runner: r, settings: maps.Clone(s.Settings)}
	return r, nil
}

func runnerKey(defID string, settings map[string]string) string {
	raw, _ := json.Marshal(settings) // map keys are sorted
	return defID + "\x00" + string(raw)
}

// saveSessionCookie stores the cookies a cookie-login site refreshed
// (cardigann.Runner.OnCookies) as the source's cookie setting, so the
// session survives a restart. Only the cookie changes, and only while it is
// still the one the runner started from: a cookie the user entered
// meanwhile wins. The cached runner keeps its session (its key follows).
func (m *Manager) saveSessionCookie(id int64, r *cardigann.Runner, cookie string) {
	m.mu.Lock()
	c, ok := m.runners[id]
	if !ok || c.runner != r || c.settings["cookie"] == cookie {
		m.mu.Unlock()
		return
	}
	prev, had := c.settings["cookie"]
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	q := `UPDATE sources SET settings_json=json_set(settings_json, '$.cookie', ?) WHERE id=? AND json_extract(settings_json, '$.cookie') IS NULL`
	args := []any{cookie, id}
	if had {
		q = `UPDATE sources SET settings_json=json_set(settings_json, '$.cookie', ?) WHERE id=? AND json_extract(settings_json, '$.cookie') = ?`
		args = append(args, prev)
	}
	res, err := m.db.ExecContext(ctx, q, args...)
	if err != nil {
		m.log.Warn("indexer: save refreshed cookie", "id", id, "err", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // removed, or its cookie changed by the user meanwhile
	}
	m.log.Debug("indexer: source cookie refreshed by the site", "id", id)
	m.mu.Lock()
	if c, ok := m.runners[id]; ok && c.runner == r {
		next := maps.Clone(c.settings)
		next["cookie"] = cookie
		c.settings, c.key = next, runnerKey(c.def.ID, next)
	}
	m.mu.Unlock()
}

func (m *Manager) dropRunner(id int64) {
	m.mu.Lock()
	delete(m.runners, id)
	m.mu.Unlock()
}

// Download fetches a release link of a source: a magnet or a .torrent file.
func (m *Manager) Download(ctx context.Context, sourceID int64, link string) (cardigann.DownloadResult, error) {
	s, err := m.Get(ctx, sourceID)
	if err != nil {
		return cardigann.DownloadResult{}, err
	}
	if m.isDirect(s) {
		return cardigann.DownloadResult{}, i18n.Errorf("%s: это прямая загрузка, не торрент", s.Name)
	}
	r, err := m.runner(s)
	if err != nil {
		return cardigann.DownloadResult{}, err
	}
	dctx, cancel := context.WithTimeout(ctx, m.testTimeout)
	defer cancel()
	res, err := r.Download(dctx, link)
	if err != nil {
		return res, humanize(s.Name, err)
	}
	return res, nil
}

// errForeignRelease: a release link that is not of the chosen source.
var errForeignRelease = errors.New("Эта раздача не из выбранного источника")

// CheckReleaseLink tells whether a release link sent by a client belongs to
// the source: a magnet, or an absolute http(s) link on a host of the source
// (its definition's links, the mirror sitelink, the api host) or on a
// subdomain of it. Download would fetch any other link with the source's
// session, so a link from a profile is checked first. A direct source passes
// as Download refuses it and Fetch checks its links itself.
func (m *Manager) CheckReleaseLink(ctx context.Context, sourceID int64, link string) error {
	link = strings.TrimSpace(link)
	if strings.HasPrefix(strings.ToLower(link), "magnet:") {
		return nil
	}
	s, err := m.Get(ctx, sourceID)
	if err != nil {
		return err
	}
	if m.isDirect(s) {
		// Download refuses a direct source; Fetch and fetchCardigann check
		// the host by their own rules (an OPDS catalog may live on a mirror)
		return nil
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return errForeignRelease
	}
	def, err := m.cat.Definition(s.DefinitionID)
	if err != nil {
		return err
	}
	sites := append([]string{s.Settings["sitelink"], s.Settings["apiurl"]}, def.Links...)
	sites = append(sites, def.LegacyLinks...)
	for _, st := range def.Settings {
		if st.Name == "apiurl" {
			sites = append(sites, st.Default) // an API definition's own host
		}
	}
	if !hostOnSite(sites, u.Hostname()) {
		return errForeignRelease
	}
	return nil
}

// hostOnSite: host is the host of one of the site urls (a bare host such as
// an api host is one too), or a subdomain of it.
func hostOnSite(sites []string, host string) bool {
	host = strings.ToLower(host)
	for _, l := range sites {
		l = strings.TrimSpace(l)
		if !strings.Contains(l, "//") {
			l = "//" + l
		}
		u, err := url.Parse(l)
		if err != nil || u.Hostname() == "" {
			continue
		}
		h := strings.ToLower(u.Hostname())
		// an IP has no subdomains
		if host == h || net.ParseIP(h) == nil && strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// directDef: the source's tracker definition serves files, not torrents
// (download.direct, a Vault extension).
func (m *Manager) directDef(s Source) bool {
	if isOPDS(s) {
		return false
	}
	def, err := m.cat.Definition(s.DefinitionID)
	return err == nil && def.Download != nil && def.Download.Direct
}

// isDirect: the source's releases are direct downloads (an OPDS catalog or
// a direct-download definition).
func (m *Manager) isDirect(s Source) bool { return isOPDS(s) || m.directDef(s) }

// sortSources orders by priority, then id.
func sortSources(s []Source) {
	sort.SliceStable(s, func(i, j int) bool {
		if s[i].Priority != s[j].Priority {
			return s[i].Priority < s[j].Priority
		}
		return s[i].ID < s[j].ID
	})
}
