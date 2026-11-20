package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

// fakeProwlarrAPI is «Переезд»'s kept Prowlarr connection.
type fakeProwlarrAPI struct {
	kept  bool
	err   error
	calls int
}

func (f *fakeProwlarrAPI) ImportProwlarrAPI(context.Context) ([]indexer.Imported, bool, error) {
	f.calls++
	if !f.kept {
		return nil, false, nil
	}
	return []indexer.Imported{{Name: "RuTor", Reason: ""}, {Name: "NZB", Reason: "Тип «Newznab» не поддерживается: Antresol подключает только торрент-трекеры"}}, true, f.err
}

func (e *env) withProwlarrAPI(p ProwlarrAPI) {
	e.h = Routes(Deps{Acquire: e.acq, Sources: e.src, Settings: e.st, NoSeed: e.ns, Upload: e.eng,
		StreamCacheDefaultGB: 50, Outside: func() int { return e.out }, Languages: e.langs, ProwlarrAPI: p,
		ProwlarrDB: filepath.Join(e.dir, "no", "prowlarr.db")})
}

type importAnswer struct {
	Imported []indexer.Imported `json:"imported"`
	Via      string             `json:"via"`
}

// «Импортировать из Prowlarr»: a kept API connection is used when no file
// is asked for; skipped ones come back with their (localized) reason; a
// file asked for (via "file" or a path) never asks the API; via "api"
// without a connection says where to connect it.
func TestImportProwlarr_UsesKeptAPIConnection(t *testing.T) {
	e := newEnv(t, false, false)
	p := &fakeProwlarrAPI{kept: true}
	e.withProwlarrAPI(p)

	w := e.do("POST", "/api/admin/sources/import-prowlarr", `{}`, &admin)
	var a importAnswer
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &a) != nil || a.Via != "api" || len(a.Imported) != 2 {
		t.Fatalf("api: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(a.Imported[1].Reason, "не поддерживается") {
		t.Fatalf("skipped reason %+v", a.Imported[1])
	}

	// the file asked for: the API is not asked (no file there: the importer's error)
	p.calls = 0
	w = e.do("POST", "/api/admin/sources/import-prowlarr", `{"via":"file"}`, &admin)
	if p.calls != 0 || w.Code != http.StatusBadRequest {
		t.Fatalf("file: calls %d, %d %s", p.calls, w.Code, w.Body)
	}

	// no connection kept: via "api" is refused with where to connect it; no via falls back to the file
	p.kept = false
	w = e.do("POST", "/api/admin/sources/import-prowlarr", `{"via":"api"}`, &admin)
	if w.Code != http.StatusBadRequest || !strings.Contains(errorOf(w), "«Переезд»") {
		t.Fatalf("api without a connection: %d %s", w.Code, w.Body)
	}
	w = e.do("POST", "/api/admin/sources/import-prowlarr", `{}`, &admin)
	if w.Code != http.StatusBadRequest || strings.Contains(errorOf(w), "«Переезд»") {
		t.Fatalf("fallback to the file: %d %s", w.Code, w.Body)
	}

	// a connection problem is the importer's message, a 400
	p.kept, p.err = true, &indexer.ImportError{Msg: "Prowlarr не принял API-ключ"}
	w = e.do("POST", "/api/admin/sources/import-prowlarr", `{}`, &admin)
	if w.Code != http.StatusBadRequest || !strings.Contains(errorOf(w), "Prowlarr не принял API-ключ") {
		t.Fatalf("refused key: %d %s", w.Code, w.Body)
	}
}
