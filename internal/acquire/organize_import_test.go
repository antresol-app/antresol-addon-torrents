package acquire

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

// importEnv is an env whose library is the core's (coretest), with a
// season of three episodes and an .nfo downloaded, ready to organize.
func importEnv(t *testing.T) (*env, *coretest.Library, string, []string) {
	t.Helper()
	e := newEnv(t)
	core := &coretest.Library{}
	e.svc.o.Library = core
	dir := filepath.Join(e.root, "torrents", "series")
	rels := []string{"Shogun.S01/Shogun.S01E01.mkv", "Shogun.S01/Shogun.S01E02.mkv", "Shogun.S01/Shogun.S01E03.mkv", "Shogun.S01/Shogun.S01.nfo"}
	e.eng.nextFiles = torrentFiles(t, dir, rels...)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Сёгун S01 1080p", "https://t/1", 1, 5)}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	season := filepath.Join(e.root, "media/series", "Сёгун", "Season 01")
	var eps []string
	for _, n := range []string{"01", "02", "03"} {
		eps = append(eps, filepath.Join(season, "Сёгун - S01E"+n+".mkv"))
	}
	return e, core, res.InfoHash, eps
}

// A finished season goes into the library in one import: its episodes
// (no .nfo), bound to the download's infohash.
func TestOrganize_OneImportPerJob(t *testing.T) {
	e, core, ih, eps := importEnv(t)
	e.svc.organize(context.Background(), ih, false, false)
	if len(core.Imports) != 1 || !slices.Equal(core.Imports[0], eps) {
		t.Fatalf("imports %q, want [%q]", core.Imports, eps)
	}
	if got := core.Bound(ih); !slices.Equal(got, eps) {
		t.Fatalf("bound to %s: %q", ih, got)
	}
	a, err := e.svc.acquisition(context.Background(), ih)
	if err != nil || !a.OrganizedAt.Valid || a.Error != "" {
		t.Fatalf("acquisition %+v %v", a, err)
	}
}

// An episode the core reports missing after its scan fails the organize
// with today's message, as one attempt.
func TestOrganize_MissingAfterImport(t *testing.T) {
	e, core, ih, eps := importEnv(t)
	core.Missing = map[string]bool{eps[1]: true}
	e.svc.organize(context.Background(), ih, false, false)
	a, err := e.svc.acquisition(context.Background(), ih)
	if err != nil {
		t.Fatal(err)
	}
	want := "Файл " + filepath.Base(eps[1]) + " не появился в медиатеке после сканирования"
	if a.OrganizedAt.Valid || a.Error != want || a.Attempts != 1 {
		t.Fatalf("acquisition error %q attempts %d organized %v, want %q", a.Error, a.Attempts, a.OrganizedAt.Valid, want)
	}
}

// A file another addon has is no «не появился»: it is named as taken.
func TestOrganize_TakenAfterImport(t *testing.T) {
	e, core, ih, eps := importEnv(t)
	core.Taken = map[string]bool{eps[0]: true}
	e.svc.organize(context.Background(), ih, false, false)
	a, err := e.svc.acquisition(context.Background(), ih)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Файл " + filepath.Base(eps[0]) + " уже принадлежит другому расширению"; a.Error != want || a.Attempts != 1 {
		t.Fatalf("acquisition error %q attempts %d, want %q", a.Error, a.Attempts, want)
	}
}

// failingImport is a core whose import fails.
type failingImport struct{ *coretest.Library }

func (failingImport) Import(context.Context, []string, string, int64) ([]string, []string, []string, error) {
	return nil, nil, nil, errors.New("core down")
}

// An import that fails is «Не удалось связать файл …».
func TestOrganize_ImportError(t *testing.T) {
	e, core, ih, eps := importEnv(t)
	e.svc.o.Library = failingImport{core}
	e.svc.organize(context.Background(), ih, false, false)
	a, err := e.svc.acquisition(context.Background(), ih)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Не удалось связать файл " + filepath.Base(eps[0]) + " с раздачей"; a.Error != want {
		t.Fatalf("acquisition error %q, want %q", a.Error, want)
	}
}

// The download list goes out as the event coreadapt.Forward forwards.
func TestEventProgressForwarded(t *testing.T) {
	if EventProgress != coreadapt.EventProgress {
		t.Fatalf("acquire's %q != coreadapt's %q", EventProgress, coreadapt.EventProgress)
	}
}
