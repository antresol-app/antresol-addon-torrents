package migrate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// reopened is a «Переезд» service over the same database, as after a
// restart or an addon update.
func reopened(t *testing.T, e *svcEnv, idx ProwlarrImporter) *Service {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := NewRunner(&Runner{ReportDir: t.TempDir(), TmpDir: t.TempDir(), BaseCtx: ctx})
	src, left := DefaultPaths(e.dir, filepath.Join(e.dir, "prowlarr", "prowlarr.db"))
	return New(r, Service{DB: e.db, Indexer: idx, Paths: src, Leftover: left})
}

func sealedRow(t *testing.T, e *svcEnv) (string, bool) {
	t.Helper()
	var v string
	err := e.db.QueryRow(`SELECT value FROM settings WHERE key=?`, remotesSetting).Scan(&v)
	if err != nil {
		return "", false
	}
	return v, true
}

// A connection typed on «Переезд» survives a restart (sealed in the
// settings, never the key in the clear); «Забыть» removes it for good.
func TestRemotesSurviveRestart(t *testing.T) {
	e := newSvcEnv(t, nil)
	ctx := context.Background()
	api := sonarrAPI(t, []sonarrRow{{ID: 1, Title: "Сериал 1", Year: 2001, TmdbID: 1001, Monitored: true}}, nil, false)
	if _, err := e.s.Connect(ctx, SourceSonarr, Remote{URL: api.URL, APIKey: fakeAPIKey}); err != nil {
		t.Fatal(err)
	}
	row, ok := sealedRow(t, e)
	if !ok || strings.Contains(row, fakeAPIKey) || strings.Contains(row, api.URL) {
		t.Fatalf("sealed row %q %v", row, ok)
	}

	again := reopened(t, e, nil)
	v, ok := again.Remote(SourceSonarr)
	if !ok || !v.Connected || v.URL != api.URL || !v.HasSecret {
		t.Fatalf("after a restart: %+v %v", v, ok)
	}
	if r, ok := again.remoteFor(SourceSonarr); !ok || r.APIKey != fakeAPIKey {
		t.Fatalf("the key was not kept: %+v", r)
	}

	if err := again.Forget(SourceSonarr); err != nil {
		t.Fatal(err)
	}
	if _, ok := sealedRow(t, e); ok {
		t.Fatal("the row stays after the last connection was forgotten")
	}
	if v, _ := reopened(t, e, nil).Remote(SourceSonarr); v.Connected {
		t.Fatalf("a forgotten connection came back: %+v", v)
	}
}

// A row that does not open (another key: a copied database) is dropped:
// nothing is connected, nothing breaks.
func TestRemotesDamagedRowDropped(t *testing.T) {
	e := newSvcEnv(t, nil)
	ctx := context.Background()
	api := sonarrAPI(t, nil, nil, false)
	if _, err := e.s.Connect(ctx, SourceSonarr, Remote{URL: api.URL, APIKey: fakeAPIKey}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Exec(`UPDATE settings SET value=? WHERE key=?`, strings.Repeat("0", 64), remotesKeySetting); err != nil {
		t.Fatal(err)
	}
	if v, _ := reopened(t, e, nil).Remote(SourceSonarr); v.Connected {
		t.Fatalf("a row under another key opened: %+v", v)
	}
	if _, ok := sealedRow(t, e); ok {
		t.Fatal("the damaged row was kept")
	}
}

// «Источники» → «Импортировать из Prowlarr» uses the Prowlarr connection
// kept on «Переезд»: the indexers become sources; skipped ones say why.
// Without a connection: not used (ok false). A refused key: an
// ImportError with the reason, never the key.
func TestImportProwlarrAPIUsesKeptConnection(t *testing.T) {
	const indexers = `[{"id":1,"name":"RuTor","implementation":"Cardigann","enable":true,"priority":25,"tags":[],"fields":[{"name":"definitionFile","value":"rutor"}]},
	  {"id":2,"name":"Usenet Thing","implementation":"Newznab","enable":true,"priority":25,"tags":[],"fields":[]}]`
	api := newFakeArr(t, "Prowlarr", map[string]any{"indexer": indexers, "tag": `[]`})
	e := newSvcEnv(t, nil)
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := indexer.NewManager(d, indexer.NewCatalog(filepath.Join(dir, "definitions")), indexer.Options{})
	s := reopened(t, e, m)
	ctx := context.Background()
	if _, ok, err := s.ImportProwlarrAPI(ctx); ok || err != nil {
		t.Fatalf("no connection: ok %v err %v", ok, err)
	}
	if _, err := s.Connect(ctx, SourceProwlarr, *api.remote()); err != nil {
		t.Fatal(err)
	}
	res, ok, err := s.ImportProwlarrAPI(ctx)
	if !ok || err != nil {
		t.Fatalf("import: %v %v", ok, err)
	}
	var added, skipped []string
	for _, im := range res {
		if im.Source != nil {
			added = append(added, im.Name)
		} else {
			skipped = append(skipped, im.Name+": "+im.Reason)
		}
	}
	if len(added) != 1 || added[0] != "RuTor" || len(skipped) != 1 || !strings.Contains(skipped[0], "не поддерживается") {
		t.Fatalf("added %v skipped %v", added, skipped)
	}

	s.remotes.set(SourceProwlarr, Remote{URL: api.URL, APIKey: "bad-key-123"})
	_, ok, err = s.ImportProwlarrAPI(ctx)
	var ie *indexer.ImportError
	if !ok || !errors.As(err, &ie) || !strings.Contains(ie.Msg, "Prowlarr не принял API-ключ") || strings.Contains(ie.Msg, "bad-key-123") {
		t.Fatalf("refused key: %v %v", ok, err)
	}
}
