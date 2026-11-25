// Package indexer manages torrent sources (Cardigann definitions plus user
// settings), searches them in parallel and scores the releases.
package indexer

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// Embedded definitions: definitions/*.yml come from Prowlarr/Indexers
// (scripts/update-definitions.sh), definitions/extra/*.yml are maintained by
// Vault and win over them.
//
//go:embed definitions/*.yml definitions/extra/*.yml
var embeddedFS embed.FS

// CatalogEntry describes a tracker definition for the "add source" screen.
type CatalogEntry struct {
	ID          string
	Name        string
	Description string
	Language    string
	Type        string // public | semi-private | private
	Links       []string
	Settings    []SettingSpec
	Custom      bool // from DataDir/definitions
}

// Localized is e with its Russian texts (name, description, field labels,
// hints and option labels: catalog keys) in lang; the definitions' own
// English texts stay as they are.
func (e CatalogEntry) Localized(lang string) CatalogEntry {
	if lang == i18n.RU {
		return e
	}
	e.Name = i18n.LocalizeL(lang, e.Name)
	e.Description = i18n.LocalizeL(lang, e.Description)
	settings := make([]SettingSpec, len(e.Settings))
	for i, s := range e.Settings {
		s.Label = i18n.LocalizeL(lang, s.Label)
		s.Help = i18n.LocalizeL(lang, s.Help)
		if s.Options != nil {
			opts := make([]Option, len(s.Options))
			for j, o := range s.Options {
				opts[j] = Option{Value: o.Value, Label: i18n.LocalizeL(lang, o.Label)}
			}
			s.Options = opts
		}
		settings[i] = s
	}
	if e.Settings != nil {
		e.Settings = settings
	}
	return e
}

// SettingSpec is one form field. Required fields make the simple form;
// Advanced ones are hidden behind "more settings".
type SettingSpec struct {
	Name, Type, Label, Default string
	Help                       string
	Required                   bool
	Advanced                   bool
	Secret                     bool
	Options                    []Option // select
}

// Option of a select setting.
type Option struct{ Value, Label string }

type catalogDef struct {
	def   *cardigann.Definition
	entry CatalogEntry
}

// Catalog holds embedded definitions and the user's ones from a directory
// (they override embedded definitions with the same id). Safe for
// concurrent use.
//
// An embedded definition is parsed when someone asks for it (a source of it
// exists, or the «add source» list is built), not all 580 at the first
// search, and an unchanged user file keeps its *Definition when the
// directory is read again, so the runner of its source keeps its session.
type Catalog struct {
	userDir string
	once    sync.Once // the first use loads (load)
	mu      sync.RWMutex
	user    map[string]*catalogDef // by id
	files   map[string]userFile    // by path: what Reload read of it last
	list    []CatalogEntry         // all entries, built by List; nil = not yet
	gen     int                    // counts Reloads: a list built before one is stale
}

// userFile is a user definition file as Reload last read it.
type userFile struct {
	mod  time.Time
	size int64
	def  *catalogDef // nil: it did not parse
}

// The embedded definitions by id (the files are not all named by their
// id), parsed on demand and kept.
var embedded struct {
	once  sync.Once
	files map[string]string // id -> file
	mu    sync.Mutex
	defs  map[string]*catalogDef
	list  []CatalogEntry // every entry, built once by the first List
}

var idLine = regexp.MustCompile(`(?m)^id:\s*["']?([^\s"'#]+)`)

func embeddedFiles() map[string]string {
	embedded.once.Do(func() {
		embedded.files, embedded.defs = map[string]string{}, map[string]*catalogDef{}
		for _, dir := range []string{"definitions", "definitions/extra"} { // extra wins
			files, _ := fs.Glob(embeddedFS, dir+"/*.yml")
			for _, f := range files {
				data, _ := embeddedFS.ReadFile(f)
				if m := idLine.FindSubmatch(data); m != nil {
					embedded.files[string(m[1])] = f
				}
			}
		}
	})
	return embedded.files
}

func parseEmbedded(file string) (*catalogDef, bool) {
	data, _ := embeddedFS.ReadFile(file)
	d, err := cardigann.Parse(data)
	if err != nil {
		slog.Warn("indexer: skip embedded definition", "file", file, "err", err)
		return nil, false
	}
	return &catalogDef{def: d, entry: makeEntry(d, false)}, true
}

// embeddedDef is the embedded definition of an id, parsed the first time.
func embeddedDef(id string) (*catalogDef, bool) {
	file, ok := embeddedFiles()[id]
	if !ok {
		return nil, false
	}
	embedded.mu.Lock()
	defer embedded.mu.Unlock()
	if d, ok := embedded.defs[id]; ok {
		return d, d != nil
	}
	d, ok := parseEmbedded(file)
	embedded.defs[id] = d // nil: it does not parse, not tried again
	return d, ok
}

// embeddedEntries is the entry of every embedded definition. The first call
// parses them all (the «add source» screen is the only one that needs
// that) and keeps the entries only.
func embeddedEntries() []CatalogEntry {
	files := embeddedFiles()
	embedded.mu.Lock()
	defer embedded.mu.Unlock()
	if embedded.list == nil {
		embedded.list = make([]CatalogEntry, 0, len(files))
		for id, file := range files {
			d, ok := embedded.defs[id]
			if !ok {
				d, _ = parseEmbedded(file)
			}
			if d != nil {
				embedded.list = append(embedded.list, d.entry)
			}
		}
	}
	return embedded.list
}

// NewCatalog is the catalog of the embedded definitions and *.yml from
// userDir (may be "" or missing), read on first use. Broken user files are
// logged and skipped.
func NewCatalog(userDir string) *Catalog {
	return &Catalog{userDir: userDir}
}

// load reads the user definitions the first time the catalog is used.
func (c *Catalog) load() { c.once.Do(c.Reload) }

// Reload re-reads the user directory. A file that did not change since it
// was read keeps its parsed definition.
func (c *Catalog) Reload() {
	c.mu.RLock()
	prev := c.files
	c.mu.RUnlock()
	user, files := map[string]*catalogDef{}, map[string]userFile{}
	if c.userDir != "" {
		yml, _ := filepath.Glob(filepath.Join(c.userDir, "*.yml"))
		yaml2, _ := filepath.Glob(filepath.Join(c.userDir, "*.yaml"))
		for _, f := range append(yml, yaml2...) {
			st, err := os.Stat(f)
			if err != nil {
				slog.Warn("indexer: read user definition", "file", f, "err", err)
				continue
			}
			uf, ok := prev[f]
			if !ok || !uf.mod.Equal(st.ModTime()) || uf.size != st.Size() {
				uf = userFile{mod: st.ModTime(), size: st.Size()}
				data, err := os.ReadFile(f)
				if err != nil {
					slog.Warn("indexer: read user definition", "file", f, "err", err)
					continue
				}
				if d, err := cardigann.Parse(data); err != nil {
					slog.Warn("indexer: skip user definition", "file", f, "err", err)
				} else {
					uf.def = &catalogDef{def: d, entry: makeEntry(d, true)}
				}
			}
			files[f] = uf
			if uf.def != nil {
				user[uf.def.def.ID] = uf.def
			}
		}
	}
	c.mu.Lock()
	c.user, c.files, c.list = user, files, nil
	c.gen++
	c.mu.Unlock()
}

func isRussian(lang string) bool { return strings.HasPrefix(strings.ToLower(lang), "ru") }

// entries is every entry, Russian-language trackers first. The user's
// definitions replace the embedded ones of the same id, the synthetic OPDS
// entry is last of its language.
func (c *Catalog) entries() []CatalogEntry {
	c.load()
	c.mu.RLock()
	list, user, gen := c.list, c.user, c.gen
	c.mu.RUnlock()
	if list != nil {
		return list
	}
	list = make([]CatalogEntry, 0, len(embeddedEntries())+len(user)+1)
	for _, e := range embeddedEntries() {
		if _, over := user[e.ID]; !over && e.ID != OPDSDefinitionID {
			list = append(list, e)
		}
	}
	for id, d := range user {
		if id != OPDSDefinitionID {
			list = append(list, d.entry)
		}
	}
	list = append(list, opdsEntry) // synthetic: OPDS catalogs are not trackers
	sort.Slice(list, func(i, j int) bool {
		ri, rj := isRussian(list[i].Language), isRussian(list[j].Language)
		if ri != rj {
			return ri
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
	c.mu.Lock()
	if c.gen == gen {
		c.list = list
	}
	c.mu.Unlock()
	return list
}

// List returns entries matching q (name, id, description or site address;
// empty q = all). Russian-language trackers come first.
func (c *Catalog) List(q string) []CatalogEntry {
	list := c.entries()
	nq := release.Normalize(q)
	if nq == "" {
		return append([]CatalogEntry(nil), list...)
	}
	var byName, other []CatalogEntry
	for _, e := range list {
		switch {
		case strings.Contains(release.Normalize(e.Name), nq) || strings.Contains(release.Normalize(e.ID), nq):
			byName = append(byName, e)
		case strings.Contains(release.Normalize(e.Description), nq) || strings.Contains(release.Normalize(strings.Join(e.Links, " ")), nq):
			other = append(other, e)
		}
	}
	return append(byName, other...)
}

// def is the catalog's definition of an id: the user's, else the embedded.
func (c *Catalog) def(id string) (*catalogDef, bool) {
	c.load()
	c.mu.RLock()
	d, ok := c.user[id]
	c.mu.RUnlock()
	if ok {
		return d, true
	}
	return embeddedDef(id)
}

// Get returns one entry.
func (c *Catalog) Get(id string) (CatalogEntry, bool) {
	if id == OPDSDefinitionID {
		return opdsEntry, true
	}
	d, ok := c.def(id)
	if !ok {
		return CatalogEntry{}, false
	}
	return d.entry, true
}

// ErrNoDefinition: the source's tracker definition is not in the catalog.
var ErrNoDefinition = errors.New("описание этого трекера не найдено")

// Definition returns the parsed definition. Callers must not modify it.
func (c *Catalog) Definition(id string) (*cardigann.Definition, error) {
	d, ok := c.def(id)
	if !ok {
		return nil, fmt.Errorf("%w (%s)", ErrNoDefinition, id)
	}
	return d.def, nil
}

var safeFile = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// SaveUserDefinition validates data and writes it into the user directory as
// <id>.yml, then reloads. Returns the definition id.
func (c *Catalog) SaveUserDefinition(data []byte) (string, error) {
	d, err := cardigann.Parse(data)
	if err != nil {
		return "", i18n.Errorf("файл трекера не читается: %w", err)
	}
	if c.userDir == "" {
		return "", errors.New("indexer: catalog has no user definitions directory")
	}
	if err := os.MkdirAll(c.userDir, 0o755); err != nil {
		return "", err
	}
	name := safeFile.ReplaceAllString(d.ID, "_") + ".yml"
	tmp := filepath.Join(c.userDir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(c.userDir, path.Base(name))); err != nil {
		os.Remove(tmp)
		return "", err
	}
	c.Reload()
	return d.ID, nil
}

func makeEntry(d *cardigann.Definition, custom bool) CatalogEntry {
	e := CatalogEntry{
		ID: d.ID, Name: d.Name, Description: d.Description, Language: d.Language,
		Type: d.Type, Links: append([]string(nil), d.Links...), Custom: custom,
	}
	if e.Name == "" {
		e.Name = d.ID
	}
	if e.Type == "" {
		e.Type = "public"
	}
	for _, s := range d.Settings {
		if spec, ok := settingSpec(d, s); ok {
			e.Settings = append(e.Settings, spec)
		}
	}
	e.Settings = append(e.Settings, siteLinkSpec(d.Links))
	if d.Download != nil && d.Download.Direct {
		e.Settings = append(e.Settings, allowLocalSpec) // its client refuses the home network
	}
	return e
}
