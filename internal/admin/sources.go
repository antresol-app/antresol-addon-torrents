package admin

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/httpx"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol/addonkit"
)

// DefaultProwlarrDB is where the Prowlarr config is mounted in the container.
const DefaultProwlarrDB = "/prowlarr/prowlarr.db"

// sources: «Источники» (the catalog of definitions and the added sources).
type sources struct{ Deps }

// catalogField is one form field of a definition: Required fields make the
// simple form, Advanced ones hide behind «Ещё настройки».
type catalogField struct {
	Name     string          `json:"name"`
	Type     string          `json:"type"`
	Label    string          `json:"label"`
	Default  string          `json:"default,omitempty"`
	Help     string          `json:"help,omitempty"`
	Required bool            `json:"required"`
	Advanced bool            `json:"advanced"`
	Secret   bool            `json:"secret"`
	Options  []catalogOption `json:"options,omitempty"`
}

type catalogOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type catalogEntry struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Language    string         `json:"language"`
	Type        string         `json:"type"` // public | semi-private | private
	Links       []string       `json:"links"`
	Custom      bool           `json:"custom"`
	Fields      []catalogField `json:"fields,omitempty"`
}

func toCatalogEntry(ctx context.Context, e indexer.CatalogEntry, withFields bool) catalogEntry {
	e = e.Localized(i18n.FromContext(ctx))
	out := catalogEntry{ID: e.ID, Name: e.Name, Description: e.Description, Language: e.Language, Type: e.Type,
		Links: e.Links, Custom: e.Custom}
	if out.Links == nil {
		out.Links = []string{}
	}
	if withFields {
		out.Fields = []catalogField{}
		for _, s := range e.Settings {
			f := catalogField{Name: s.Name, Type: s.Type, Label: s.Label, Default: s.Default, Help: s.Help,
				Required: s.Required, Advanced: s.Advanced, Secret: s.Secret}
			for _, o := range s.Options {
				f.Options = append(f.Options, catalogOption{o.Value, o.Label})
			}
			out.Fields = append(out.Fields, f)
		}
	}
	return out
}

// sourceErr maps Manager errors; their Russian messages go in the
// request's language.
func sourceErr(w http.ResponseWriter, r *http.Request, what string, err error) {
	if r.Context().Err() != nil {
		return
	}
	var ve *indexer.ValidationError
	switch {
	case errors.As(err, &ve):
		addonkit.WriteJSON(w, http.StatusBadRequest, map[string]string{"error": i18n.Message(r.Context(), ve), "field": ve.Field})
	case errors.Is(err, indexer.ErrNotFound):
		httpx.WriteErr(w, r, http.StatusNotFound, "Источник не найден")
	case errors.Is(err, indexer.ErrNoDefinition):
		httpx.WriteErr(w, r, http.StatusBadRequest, "Описание этого трекера не найдено")
	default:
		httpx.InternalErr(w, r, "admin", what, err)
	}
}

// GET /api/admin/sources/catalog?q= — definitions without their fields
// (hundreds of entries); /catalog/{id} has the form.
func (s *sources) catalog(w http.ResponseWriter, r *http.Request) {
	list := s.Sources.Catalog().List(r.URL.Query().Get("q"))
	out := make([]catalogEntry, 0, len(list))
	for _, e := range list {
		out = append(out, toCatalogEntry(r.Context(), e, false))
	}
	addonkit.WriteJSON(w, http.StatusOK, out)
}

func (s *sources) catalogEntry(w http.ResponseWriter, r *http.Request) {
	e, ok := s.Sources.Catalog().Get(r.PathValue("id"))
	if !ok {
		httpx.WriteErr(w, r, http.StatusNotFound, "Описание этого трекера не найдено")
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, toCatalogEntry(r.Context(), e, true))
}

// view is a source as the client gets it: secrets redacted, the stored
// Russian test error in the request's language.
func (s *sources) view(r *http.Request, src indexer.Source) indexer.Source {
	src = s.Sources.Redact(src)
	src.LastError = i18n.Localize(r.Context(), src.LastError)
	return src
}

func (s *sources) list(w http.ResponseWriter, r *http.Request) {
	all, err := s.Sources.Sources(r.Context())
	if err != nil {
		httpx.InternalErr(w, r, "admin", "sources", err)
		return
	}
	out := make([]indexer.Source, 0, len(all))
	for _, src := range all {
		out = append(out, s.view(r, src))
	}
	addonkit.WriteJSON(w, http.StatusOK, out)
}

// POST /api/admin/sources {definitionId, settings}: saved and tested at
// once; the test's outcome is in status/lastError of the answer.
func (s *sources) add(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DefinitionID string            `json:"definitionId"`
		Settings     map[string]string `json:"settings"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	src, err := s.Sources.Add(r.Context(), strings.TrimSpace(in.DefinitionID), in.Settings)
	if err != nil {
		sourceErr(w, r, "add source", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusCreated, s.view(r, src))
}

func (s *sources) update(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.IDParam(w, r, "id")
	if !ok {
		return
	}
	var p indexer.SourcePatch
	if !httpx.Decode(w, r, &p) {
		return
	}
	src, err := s.Sources.Update(r.Context(), id, p)
	if err != nil {
		sourceErr(w, r, "update source", err)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, s.view(r, src))
}

// POST /api/admin/sources/{id}/test → the source with its new status; a
// test that ran and failed is not an HTTP error (lastError says it).
func (s *sources) test(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.IDParam(w, r, "id")
	if !ok {
		return
	}
	terr := s.Sources.Test(r.Context(), id)
	if terr != nil && (errors.Is(terr, indexer.ErrNotFound) || r.Context().Err() != nil) {
		sourceErr(w, r, "test source", terr)
		return
	}
	src, err := s.Sources.Get(r.Context(), id)
	if err != nil {
		sourceErr(w, r, "test source", err)
		return
	}
	// a test that ran and failed is stored as status error + lastError;
	// any other failure (the database) is ours
	if terr != nil && (src.Status != indexer.StatusError || src.LastError == "") {
		httpx.InternalErr(w, r, "admin", "test source", terr)
		return
	}
	addonkit.WriteJSON(w, http.StatusOK, s.view(r, src))
}

func (s *sources) remove(w http.ResponseWriter, r *http.Request) {
	id, ok := httpx.IDParam(w, r, "id")
	if !ok {
		return
	}
	if err := s.Sources.Remove(r.Context(), id); err != nil {
		sourceErr(w, r, "remove source", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/admin/sources/import-prowlarr {via, path}: via "api" reads
// Prowlarr over the connection kept on «Переезд» (Deps.ProwlarrAPI), via
// "file" (or a path) its prowlarr.db (default: Deps.ProwlarrDB, else the
// mounted Prowlarr config); neither: the connection when one is kept, else
// the file. The answer says which was used ("via"). Errors are the
// importer's Russian messages.
func (s *sources) importProwlarr(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Via  string `json:"via"`
		Path string `json:"path"`
	}
	if r.ContentLength != 0 && !httpx.Decode(w, r, &in) {
		return
	}
	p := strings.TrimSpace(in.Path)
	// the importer snapshots the file it is given: only Prowlarr's own
	// database name, never another SQLite file of this host
	if p != "" && filepath.Base(filepath.Clean(p)) != "prowlarr.db" {
		httpx.WriteErr(w, r, http.StatusBadRequest, "Укажите путь к файлу prowlarr.db")
		return
	}
	var (
		res []indexer.Imported
		err error
		via = "file"
	)
	tried := false
	if in.Via == "api" || (in.Via == "" && p == "") {
		if s.ProwlarrAPI != nil {
			res, tried, err = s.ProwlarrAPI.ImportProwlarrAPI(r.Context())
		}
		if tried {
			via = "api"
		} else if in.Via == "api" {
			addonkit.WriteError(w, http.StatusBadRequest, i18n.T(r.Context(), "Подключение к Prowlarr не сохранено: подключите его на странице «Переезд»"))
			return
		}
	}
	if !tried {
		if p == "" {
			p = s.ProwlarrDB
		}
		if p == "" {
			p = DefaultProwlarrDB
		}
		res, err = s.Sources.ImportProwlarr(r.Context(), p)
	}
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		var ie *indexer.ImportError
		if !errors.As(err, &ie) {
			httpx.InternalErr(w, r, "admin", "import prowlarr", err)
			return
		}
		addonkit.WriteError(w, http.StatusBadRequest, httpx.Message(r, ie.Msg, ie))
		return
	}
	if res == nil {
		res = []indexer.Imported{}
	}
	for i := range res {
		if res[i].Source != nil {
			red := s.view(r, *res[i].Source)
			res[i].Source = &red
		}
		res[i].Reason = i18n.Localize(r.Context(), res[i].Reason)
	}
	addonkit.WriteJSON(w, http.StatusOK, map[string]any{"imported": res, "via": via})
}
