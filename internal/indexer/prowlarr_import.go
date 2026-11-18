package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/sqlitesnap"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// Imported is the outcome for one Prowlarr indexer: Source is the created
// source, or nil with Reason (Russian) when it was skipped.
type Imported struct {
	Name         string  `json:"name"`
	DefinitionID string  `json:"definitionId,omitempty"`
	Source       *Source `json:"source,omitempty"`
	Reason       string  `json:"reason,omitempty"`
	// NeedsFlareSolverr: the indexer has Prowlarr's flaresolverr tag (its
	// site sits behind a Cloudflare challenge).
	NeedsFlareSolverr bool `json:"needsFlareSolverr,omitempty"`
	// Hidden: the settings Prowlarr's API did not give (passwords); the
	// source is added without them and needs them typed in.
	Hidden []string `json:"hidden,omitempty"`
}

// Reasons of PreviewProwlarr and ImportProwlarr the migration report sorts
// by.
const (
	ReasonWillAdd = "Будет добавлен"
	ReasonAlready = "Уже добавлен"
	// a source imported before with a cookie Prowlarr has refreshed since:
	// its cookie is replaced by Prowlarr's live session (Source is set)
	ReasonCookieUpdated    = "Уже добавлен: cookie обновлён из сеанса Prowlarr"
	ReasonWillUpdateCookie = "Уже добавлен: cookie будет обновлён из сеанса Prowlarr"
)

// ImportError is an import failure the user can act on (its message is
// Russian); other errors from ImportProwlarr are internal (I/O, database).
type ImportError struct{ Msg string }

func (e *ImportError) Error() string { return e.Msg }

// errProwlarrDB is the user-facing message for an unreadable database.
var errProwlarrDB = &ImportError{Msg: "Не удалось прочитать базу Prowlarr. Проверьте, что указан файл prowlarr.db из папки настроек Prowlarr"}

type indexerRow struct {
	id                   int64
	name, impl, settings string
	enable               bool
	priority             int
	flaresolverr         bool
	cookies              map[string]string // Prowlarr's live session (IndexerStatus)
	hidden               []string          // settings Prowlarr's API masked (ProwlarrIndexer.Hidden)
}

// readProwlarrIndexers reads the Indexers table from a snapshot of the
// database: Prowlarr's config is often read-only, and a WAL database cannot
// be opened in place there (SQLite needs to create -shm). The copy also
// keeps a running Prowlarr undisturbed.
// The snapshot goes into a fresh folder under tmpDir ("" = the system's),
// removed before returning.
func (m *Manager) readProwlarrIndexers(ctx context.Context, dbPath, tmpDir string) ([]indexerRow, error) {
	tmp, err := os.MkdirTemp(tmpDir, "prowlarr-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	copyPath, err := sqlitesnap.Snapshot(dbPath, tmp)
	if err != nil {
		m.log.Warn("indexer: copy prowlarr db", "file", dbPath, "err", err)
		return nil, errProwlarrDB
	}
	pdb, err := sqlitesnap.OpenRO(copyPath)
	if err != nil {
		m.log.Warn("indexer: read prowlarr db", "file", dbPath, "err", err)
		return nil, errProwlarrDB
	}
	defer pdb.Close()
	fail := func(err error) ([]indexerRow, error) {
		m.log.Warn("indexer: read prowlarr db", "file", dbPath, "err", err)
		return nil, errProwlarrDB
	}
	flare, err := flareSolverrTags(ctx, pdb)
	if err != nil {
		return fail(err)
	}
	cols, err := sqlitesnap.Columns(ctx, pdb, "Indexers")
	if err != nil {
		return fail(err)
	}
	tagsCol := "'[]'"
	if cols["tags"] {
		tagsCol = "COALESCE(Tags, '[]')"
	}
	rows, err := pdb.QueryContext(ctx, `SELECT Id, Name, Implementation, COALESCE(Settings, '{}'), COALESCE(Enable, 1), COALESCE(Priority, 25), `+tagsCol+` FROM Indexers ORDER BY Id`)
	if err != nil {
		return fail(err)
	}
	defer rows.Close()
	var list []indexerRow
	for rows.Next() {
		var r indexerRow
		var tags string
		if err := rows.Scan(&r.id, &r.name, &r.impl, &r.settings, &r.enable, &r.priority, &tags); err != nil {
			return fail(err)
		}
		var ids []int
		if err := json.Unmarshal([]byte(tags), &ids); err != nil {
			m.log.Debug("indexer: prowlarr indexer tags unreadable (no FlareSolverr detected)", "indexer", r.name, "tags", tags, "err", err)
		}
		r.flaresolverr = slices.ContainsFunc(ids, func(id int) bool { return flare[id] })
		list = append(list, r)
	}
	if err := rows.Err(); err != nil {
		return fail(err)
	}
	live, err := m.sessionCookies(ctx, pdb)
	if err != nil {
		return fail(err)
	}
	for i := range list {
		list[i].cookies = live[list[i].id]
	}
	return list, nil
}

// sessionCookies are the live cookies Prowlarr keeps per indexer
// (IndexerStatus.Cookies, refreshed from the site's Set-Cookie answers) by
// Indexers.Id; cookies past CookiesExpirationDate are left out. A database
// without them has none.
func (m *Manager) sessionCookies(ctx context.Context, pdb *sql.DB) (map[int64]map[string]string, error) {
	out := map[int64]map[string]string{}
	if ok, err := sqlitesnap.HasTable(ctx, pdb, "IndexerStatus"); err != nil || !ok {
		return out, err
	}
	cols, err := sqlitesnap.Columns(ctx, pdb, "IndexerStatus")
	if err != nil {
		return nil, err
	}
	if !cols["providerid"] || !cols["cookies"] {
		return out, nil
	}
	expires := "NULL"
	if cols["cookiesexpirationdate"] {
		expires = "CookiesExpirationDate"
	}
	rows, err := pdb.QueryContext(ctx, `SELECT ProviderId, Cookies, `+expires+` FROM IndexerStatus WHERE Cookies IS NOT NULL AND Cookies <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := m.now()
	for rows.Next() {
		var id int64
		var raw string
		var exp sql.NullString
		if err := rows.Scan(&id, &raw, &exp); err != nil {
			return nil, err
		}
		if exp.Valid && strings.TrimSpace(exp.String) != "" {
			if t, ok := parseProwlarrTime(exp.String); ok && !t.After(now) {
				continue // expired: Prowlarr would sign in again
			}
		}
		var cookies map[string]string
		if err := json.Unmarshal([]byte(raw), &cookies); err != nil {
			m.log.Debug("indexer: prowlarr session cookies unreadable", "indexer", id, "err", err)
			continue
		}
		clean := map[string]string{}
		for k, v := range cookies {
			if k = strings.TrimSpace(k); k != "" {
				clean[k] = v
			}
		}
		if len(clean) > 0 {
			out[id] = clean
		}
	}
	return out, rows.Err()
}

// parseProwlarrTime reads a datetime as Prowlarr stores it in SQLite
// ("2026-11-01 12:00:00Z", with or without a fraction, "T" or zone; a time
// without a zone is UTC).
func parseProwlarrTime(v string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// mergeCookies lays the live cookies over a "a=b; c=d" cookie string: known
// names get the live value in place, new ones follow by name.
func mergeCookies(configured string, live map[string]string) string {
	var parts []string
	seen := map[string]bool{}
	for _, p := range strings.Split(configured, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		k = cardigann.CookieName(k)
		if !ok || k == "" || seen[k] {
			continue
		}
		p = k + "=" + strings.TrimSpace(v)
		if lv, ok := live[k]; ok {
			p = k + "=" + lv
		}
		seen[k] = true
		parts = append(parts, p)
	}
	for _, k := range slices.Sorted(maps.Keys(live)) {
		if !seen[k] {
			parts = append(parts, k+"="+live[k])
		}
	}
	return strings.Join(parts, "; ")
}

// cookieLogin: the definition signs in with the cookie setting.
func cookieLogin(def *cardigann.Definition) bool {
	if def == nil || def.Login == nil || def.Login.Method != "cookie" {
		return false
	}
	return slices.ContainsFunc(def.Settings, func(s cardigann.Setting) bool { return s.Name == "cookie" })
}

// flareSolverrTags returns the ids of Prowlarr's tags named flaresolverr
// (any case); a database without a Tags table has none.
func flareSolverrTags(ctx context.Context, pdb *sql.DB) (map[int]bool, error) {
	out := map[int]bool{}
	if ok, err := sqlitesnap.HasTable(ctx, pdb, "Tags"); err != nil || !ok {
		return out, err
	}
	rows, err := pdb.QueryContext(ctx, `SELECT Id, Label FROM Tags`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(label), "flaresolverr") {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// snapshotDB copies the database and its -wal to dst (see
// sqlitesnap.CopyTo).
func snapshotDB(src, dst string) error { return sqlitesnap.CopyTo(src, dst) }

func stampOf(p string) sqlitesnap.Stamp { return sqlitesnap.StampOf(p) }

// Prowlarr's own C# indexers that have a Cardigann counterpart here.
var nativeIndexers = map[string]string{
	"rutracker": "rutracker",
}

type prowlarrSettings struct {
	DefinitionFile string         `json:"definitionFile"`
	ExtraFieldData map[string]any `json:"extraFieldData"`
	Fields         []struct {
		Name  string `json:"name"`
		Value any    `json:"value"`
	} `json:"fields"`
	BaseURL string `json:"baseUrl"`
}

// ImportProwlarr creates sources from a Prowlarr database (prowlarr.db):
// Cardigann indexers with their logins, passwords and cookies, plus
// Prowlarr's native RuTracker. Custom definitions from the
// Definitions/Custom directory next to the database are copied into the
// user definitions directory first; a definition missing from the catalog
// is then taken from Prowlarr's own downloaded Definitions/<id>.yml (copied
// the same way). A cookie-login indexer gets the live session cookies
// Prowlarr keeps (IndexerStatus) over its configured, possibly stale,
// cookie. Sources are not tested (status unknown); indexers already
// imported are skipped, or get Prowlarr's newer session cookie
// (ReasonCookieUpdated). Imports run one at a time.
func (m *Manager) ImportProwlarr(ctx context.Context, prowlarrDBPath string) ([]Imported, error) {
	return m.prowlarr(ctx, prowlarrDBPath, "", false)
}

// ImportProwlarrIn is ImportProwlarr with the database snapshot (it holds
// tracker passwords and cookies) taken under tmpDir, e.g. the migration
// run's folder, purged on start.
func (m *Manager) ImportProwlarrIn(ctx context.Context, prowlarrDBPath, tmpDir string) ([]Imported, error) {
	return m.prowlarr(ctx, prowlarrDBPath, tmpDir, false)
}

// PreviewProwlarr is what ImportProwlarr would do, writing nothing: no
// source is added and custom definitions are read in memory, not copied.
// Indexers that would be added have Reason ReasonWillAdd.
func (m *Manager) PreviewProwlarr(ctx context.Context, prowlarrDBPath string) ([]Imported, error) {
	return m.prowlarr(ctx, prowlarrDBPath, "", true)
}

// PreviewProwlarrIn is PreviewProwlarr with the snapshot under tmpDir.
func (m *Manager) PreviewProwlarrIn(ctx context.Context, prowlarrDBPath, tmpDir string) ([]Imported, error) {
	return m.prowlarr(ctx, prowlarrDBPath, tmpDir, true)
}

func (m *Manager) prowlarr(ctx context.Context, prowlarrDBPath, tmpDir string, dry bool) ([]Imported, error) {
	m.importMu.Lock()
	defer m.importMu.Unlock()
	if _, err := os.Stat(prowlarrDBPath); err != nil {
		return nil, &ImportError{Msg: fmt.Sprintf("Файл базы Prowlarr не найден: %s. Подключите папку настроек Prowlarr к Antresol (в compose: PROWLARR_CONFIG → /prowlarr, только чтение) или укажите путь к prowlarr.db", prowlarrDBPath)}
	}
	defsDir := filepath.Join(filepath.Dir(prowlarrDBPath), "Definitions")
	customDir := filepath.Join(defsDir, "Custom")
	if !dry {
		m.copyCustomDefinitions(customDir)
	}

	list, err := m.readProwlarrIndexers(ctx, prowlarrDBPath, tmpDir)
	if err != nil {
		return nil, err
	}
	return m.importRows(ctx, list, customDir, defsDir, dry)
}

// ProwlarrIndexer is one indexer as Prowlarr's API gives it
// (ParseProwlarrAPI), for ImportProwlarrList: what a row of the database
// holds, in the same shape. Hidden are the settings Prowlarr masks in its
// answers (passwords): they are not carried over.
type ProwlarrIndexer struct {
	ID             int64
	Name           string
	Implementation string
	// Settings is the JSON the Indexers.Settings column holds:
	// definitionFile, extraFieldData and baseUrl of a Cardigann indexer,
	// the flat fields of a native one.
	Settings     string
	Enable       bool
	Priority     int
	FlareSolverr bool
	Cookies      map[string]string
	Hidden       []string
}

// ImportProwlarrList is ImportProwlarr over indexers read from Prowlarr's
// API: there is no Definitions folder, so a definition Antresol does not
// ship is not found.
func (m *Manager) ImportProwlarrList(ctx context.Context, list []ProwlarrIndexer) ([]Imported, error) {
	return m.prowlarrList(ctx, list, false)
}

// PreviewProwlarrList is what ImportProwlarrList would do, writing nothing.
func (m *Manager) PreviewProwlarrList(ctx context.Context, list []ProwlarrIndexer) ([]Imported, error) {
	return m.prowlarrList(ctx, list, true)
}

func (m *Manager) prowlarrList(ctx context.Context, in []ProwlarrIndexer, dry bool) ([]Imported, error) {
	m.importMu.Lock()
	defer m.importMu.Unlock()
	rows := make([]indexerRow, len(in))
	for i, x := range in {
		rows[i] = indexerRow{id: x.ID, name: x.Name, impl: x.Implementation, settings: x.Settings, enable: x.Enable,
			priority: x.Priority, flaresolverr: x.FlareSolverr, cookies: x.Cookies, hidden: x.Hidden}
	}
	return m.importRows(ctx, rows, "", "", dry)
}

// importRows adds the indexers read from Prowlarr (its database or its
// API); customDir and defsDir are Prowlarr's definition folders ("" =
// none). The caller holds importMu.
func (m *Manager) importRows(ctx context.Context, list []indexerRow, customDir, defsDir string, dry bool) ([]Imported, error) {
	existing, err := m.Sources(ctx)
	if err != nil {
		return nil, err
	}
	have := map[string]int64{} // import key → source id (0: planned in a dry run)
	// cookie-login sources by their settings without the cookie (the
	// lowest id first): a re-import with a newer cookie is the same tracker
	byLoose := map[string]Source{}
	for _, s := range existing {
		if k := importKey(s.DefinitionID, s.Settings); have[k] == 0 {
			have[k] = s.ID
		}
		if def, err := m.cat.Definition(s.DefinitionID); err == nil && cookieLogin(def) {
			lk := looseKey(s.DefinitionID, s.Settings)
			if prev, ok := byLoose[lk]; !ok || s.ID < prev.ID {
				byLoose[lk] = s
			}
		}
	}
	matched := map[int64]bool{} // sources an indexer of this run already matched
	// an indexer whose passwords Prowlarr hid matches a source by its
	// other settings: the source has the password, the answer did not
	sameBut := func(defID string, settings map[string]string, hidden []string) (int64, bool) {
		for _, s := range existing {
			if s.DefinitionID != defID {
				continue
			}
			rest := maps.Clone(s.Settings)
			for _, h := range hidden {
				delete(rest, h)
			}
			if importKey(defID, rest) == importKey(defID, settings) {
				return s.ID, true
			}
		}
		return 0, false
	}
	prowlarrDefs := map[string]*cardigann.Definition{}

	var out []Imported
	for _, r := range list {
		im := Imported{Name: r.name, NeedsFlareSolverr: r.flaresolverr, Hidden: r.hidden}
		var ps prowlarrSettings
		if err := json.Unmarshal([]byte(r.settings), &ps); err != nil {
			im.Reason = "Настройки индексатора не читаются"
			out = append(out, im)
			continue
		}
		var values map[string]any
		switch impl := strings.ToLower(r.impl); {
		case impl == "cardigann":
			im.DefinitionID = ps.DefinitionFile
			values = ps.ExtraFieldData
			if values == nil {
				values = map[string]any{}
			}
			for _, f := range ps.Fields {
				if _, ok := values[f.Name]; !ok {
					values[f.Name] = f.Value
				}
			}
		case nativeIndexers[impl] != "":
			im.DefinitionID = nativeIndexers[impl]
			values = map[string]any{}
			json.Unmarshal([]byte(r.settings), &values)
		default:
			im.Reason = fmt.Sprintf("Тип «%s» не поддерживается: Antresol подключает только торрент-трекеры", r.impl)
			out = append(out, im)
			continue
		}
		// Prowlarr names a definition by its file (Custom first), whose
		// id may differ: Custom/rutracker-cookie.yml is rutracker-org-cookie
		def := m.prowlarrDefinition(customDir, im.DefinitionID, dry, prowlarrDefs)
		if def == nil {
			def, _ = m.cat.Definition(im.DefinitionID) // nil when not found
		}
		if def == nil {
			def = m.prowlarrDefinition(defsDir, im.DefinitionID, dry, prowlarrDefs)
		}
		if def == nil {
			im.Reason = fmt.Sprintf("Описание трекера «%s» не найдено", im.DefinitionID)
			out = append(out, im)
			continue
		}
		fresh := false
		if len(r.cookies) > 0 && cookieLogin(def) {
			configured, _ := values["cookie"].(string)
			if merged := mergeCookies(configured, r.cookies); merged != strings.TrimSpace(configured) {
				values["cookie"], fresh = merged, true
			}
		}
		settings := convertSettings(def, values)
		if link := siteLink(def, ps.BaseURL); link != "" {
			settings["sitelink"] = link
		}
		key := importKey(def.ID, settings)
		if id, ok := have[key]; ok {
			matched[id] = true
			im.Reason = ReasonAlready
			out = append(out, im)
			continue
		}
		if len(r.hidden) > 0 {
			if id, ok := sameBut(def.ID, settings, r.hidden); ok {
				matched[id], have[key] = true, id
				im.Reason = ReasonAlready
				out = append(out, im)
				continue
			}
		}
		if prev, ok := byLoose[looseKey(def.ID, settings)]; ok && cookieLogin(def) && !matched[prev.ID] {
			matched[prev.ID] = true
			have[key] = prev.ID
			if !fresh {
				// Prowlarr's configured cookie: the source's own may be newer
				im.Reason = ReasonAlready
				out = append(out, im)
				continue
			}
			if dry {
				im.Reason = ReasonWillUpdateCookie
				out = append(out, im)
				continue
			}
			s, err := m.replaceCookie(ctx, prev, settings["cookie"])
			if err != nil {
				return out, err
			}
			im.Source, im.Reason = &s, ReasonCookieUpdated
			out = append(out, im)
			continue
		}
		if dry {
			have[key] = 0
			im.Reason = ReasonWillAdd
			out = append(out, im)
			continue
		}
		s, err := m.insert(ctx, def.ID, r.name, settings, r.enable, r.priority)
		if err != nil {
			return out, err
		}
		have[key] = s.ID
		im.Source = &s
		out = append(out, im)
	}
	return out, nil
}

// looseKey is importKey without the cookie.
func looseKey(defID string, settings map[string]string) string {
	rest := maps.Clone(settings)
	delete(rest, "cookie")
	return importKey(defID, rest)
}

// replaceCookie gives an imported source Prowlarr's live cookie; it is to
// be tested again (status unknown).
func (m *Manager) replaceCookie(ctx context.Context, s Source, cookie string) (Source, error) {
	if _, err := m.db.ExecContext(ctx, `UPDATE sources SET settings_json=json_set(settings_json, '$.cookie', ?), status=?, last_error='' WHERE id=?`,
		cookie, StatusUnknown, s.ID); err != nil {
		return s, err
	}
	m.dropRunner(s.ID)
	m.wake(s.ID)
	return m.Get(ctx, s.ID)
}

// prowlarrDefinition is the definition in Prowlarr's file dir/<name>.yml
// (Definitions/Custom, or a definition Prowlarr fetched from its indexers
// repository), named as Prowlarr names it, by file; its id may differ.
// One Antresol ships under that id wins (ours is adapted to this engine);
// otherwise it is copied into the user definitions directory, or only read
// in a dry run. The cache keeps it per run. Nil when there is none or it
// does not parse.
func (m *Manager) prowlarrDefinition(dir, name string, dry bool, cache map[string]*cardigann.Definition) *cardigann.Definition {
	if dir == "" { // read over the API: no Prowlarr folder
		return nil
	}
	key := dir + "\x00" + name
	if d, ok := cache[key]; ok {
		return d
	}
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return nil
	}
	var d *cardigann.Definition
	for _, ext := range []string{".yml", ".yaml"} {
		f := filepath.Join(dir, name+ext)
		data, err := os.ReadFile(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			d, err = cardigann.Parse(data)
		}
		if err == nil {
			if e, ok := loadEmbedded()[d.ID]; ok {
				d = e.def
			} else if !dry {
				if _, err = m.cat.SaveUserDefinition(data); err == nil {
					d, err = m.cat.Definition(d.ID)
				}
			}
		}
		if err != nil {
			m.log.Warn("indexer: prowlarr definition", "file", f, "err", err)
			d = nil
		}
		break
	}
	cache[key] = d
	return d
}

func customDefinitionFiles(dir string) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.yml"))
	more, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	return append(files, more...)
}

// copyCustomDefinitions copies Prowlarr's custom definitions into the user
// definitions directory, except those Antresol ships (ours, adapted to this
// engine, would be overridden).
func (m *Manager) copyCustomDefinitions(dir string) {
	for _, f := range customDefinitionFiles(dir) {
		data, err := os.ReadFile(f)
		var d *cardigann.Definition
		if err == nil {
			d, err = cardigann.Parse(data)
		}
		if err == nil {
			if _, shipped := loadEmbedded()[d.ID]; shipped {
				continue
			}
			_, err = m.cat.SaveUserDefinition(data)
		}
		if err != nil {
			m.log.Warn("indexer: prowlarr custom definition", "file", f, "err", err)
		}
	}
}

func importKey(defID string, settings map[string]string) string {
	raw, _ := json.Marshal(settings)
	return defID + "\x00" + string(raw)
}

// convertSettings maps Prowlarr values onto definition settings: selects are
// stored by Prowlarr as an index into the sorted option keys.
func convertSettings(def *cardigann.Definition, values map[string]any) map[string]string {
	out := map[string]string{}
	for _, s := range def.Settings {
		v, ok := values[s.Name]
		if !ok || v == nil {
			continue
		}
		typ := strings.ToLower(s.Type)
		if strings.HasPrefix(typ, "info") {
			continue
		}
		switch x := v.(type) {
		case string:
			if x = strings.TrimSpace(x); x != "" {
				out[s.Name] = x
			}
		case bool:
			out[s.Name] = strconv.FormatBool(x)
		case float64:
			if typ == "select" {
				if k, ok := s.OptionKeyByIndex(int(x)); ok {
					out[s.Name] = k
				}
				continue
			}
			out[s.Name] = strconv.FormatFloat(x, 'f', -1, 64)
		}
	}
	return out
}

// siteLink returns baseUrl when it is not the definition's default address.
func siteLink(def *cardigann.Definition, base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	if u, err := url.Parse(base); err != nil || u.Host == "" {
		return ""
	}
	norm := func(s string) string { return strings.TrimRight(strings.ToLower(s), "/") }
	if len(def.Links) > 0 && norm(def.Links[0]) == norm(base) {
		return ""
	}
	if slices.ContainsFunc(def.LegacyLinks, func(l string) bool { return norm(l) == norm(base) }) {
		return ""
	}
	return base
}
