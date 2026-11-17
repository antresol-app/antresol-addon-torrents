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
	def    *cardigann.Definition
	entry  CatalogEntry
	custom bool
	raw    []byte
}

// Catalog holds embedded definitions and the user's ones from a directory
// (they override embedded definitions with the same id). Safe for
// concurrent use.
type Catalog struct {
	userDir string
	once    sync.Once // the first use loads (load)
	mu      sync.RWMutex
	defs    map[string]*catalogDef
	list    []CatalogEntry
}

var (
	embeddedOnce sync.Once
	embeddedDefs map[string]*catalogDef
)

// loadEmbedded parses the embedded set once per process (~0.3 s).
func loadEmbedded() map[string]*catalogDef {
	embeddedOnce.Do(func() {
		embeddedDefs = map[string]*catalogDef{}
		for _, dir := range []string{"definitions", "definitions/extra"} {
			files, _ := fs.Glob(embeddedFS, dir+"/*.yml")
			for _, f := range files {
				data, _ := embeddedFS.ReadFile(f)
				d, err := cardigann.Parse(data)
				if err != nil {
					slog.Warn("indexer: skip embedded definition", "file", f, "err", err)
					continue
				}
				embeddedDefs[d.ID] = &catalogDef{def: d, entry: makeEntry(d, false), raw: data}
			}
		}
	})
	return embeddedDefs
}

// NewCatalog is the catalog of the embedded definitions and *.yml from
// userDir (may be "" or missing), read on first use: parsing them takes a
// tenth of a second and some 100 MB of short-lived memory, which the
// server's start need not pay. Broken user files are logged and skipped.
func NewCatalog(userDir string) *Catalog {
	return &Catalog{userDir: userDir}
}

// load reads the catalog the first time it is used.
func (c *Catalog) load() { c.once.Do(c.Reload) }

// Reload re-reads the user directory.
func (c *Catalog) Reload() {
	defs := map[string]*catalogDef{}
	for id, d := range loadEmbedded() {
		defs[id] = d
	}
	if c.userDir != "" {
		files, _ := filepath.Glob(filepath.Join(c.userDir, "*.yml"))
		yaml2, _ := filepath.Glob(filepath.Join(c.userDir, "*.yaml"))
		for _, f := range append(files, yaml2...) {
			data, err := os.ReadFile(f)
			if err != nil {
				slog.Warn("indexer: read user definition", "file", f, "err", err)
				continue
			}
			d, err := cardigann.Parse(data)
			if err != nil {
				slog.Warn("indexer: skip user definition", "file", f, "err", err)
				continue
			}
			defs[d.ID] = &catalogDef{def: d, entry: makeEntry(d, true), custom: true, raw: data}
		}
	}
	list := make([]CatalogEntry, 0, len(defs)+1)
	for id, d := range defs {
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
	c.defs, c.list = defs, list
	c.mu.Unlock()
}

func isRussian(lang string) bool { return strings.HasPrefix(strings.ToLower(lang), "ru") }

// List returns entries matching q (name, id, description or site address;
// empty q = all). Russian-language trackers come first.
func (c *Catalog) List(q string) []CatalogEntry {
	c.load()
	c.mu.RLock()
	defer c.mu.RUnlock()
	nq := release.Normalize(q)
	if nq == "" {
		return append([]CatalogEntry(nil), c.list...)
	}
	var byName, other []CatalogEntry
	for _, e := range c.list {
		switch {
		case strings.Contains(release.Normalize(e.Name), nq) || strings.Contains(release.Normalize(e.ID), nq):
			byName = append(byName, e)
		case strings.Contains(release.Normalize(e.Description), nq) || strings.Contains(release.Normalize(strings.Join(e.Links, " ")), nq):
			other = append(other, e)
		}
	}
	return append(byName, other...)
}

// Get returns one entry.
func (c *Catalog) Get(id string) (CatalogEntry, bool) {
	if id == OPDSDefinitionID {
		return opdsEntry, true
	}
	c.load()
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.defs[id]
	if !ok {
		return CatalogEntry{}, false
	}
	return d.entry, true
}

// ErrNoDefinition: the source's tracker definition is not in the catalog.
var ErrNoDefinition = errors.New("описание этого трекера не найдено")

// Definition returns the parsed definition. Callers must not modify it.
func (c *Catalog) Definition(id string) (*cardigann.Definition, error) {
	c.load()
	c.mu.RLock()
	defer c.mu.RUnlock()
	d, ok := c.defs[id]
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
