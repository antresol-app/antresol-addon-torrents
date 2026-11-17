package pinned

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// metafile builds a v1 metafile named name holding paths (relative to the
// torrent's folder), each 1000 bytes.
func metafile(t *testing.T, name string, paths ...string) []byte {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: 16384, Pieces: make([]byte, 20)}
	for _, p := range paths {
		info.Files = append(info.Files, metainfo.FileInfo{Length: 1000, Path: strings.Split(p, "/")})
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mf, err := bencode.Marshal(metainfo.MetaInfo{InfoBytes: ib})
	if err != nil {
		t.Fatal(err)
	}
	return mf
}

func ihOf(t *testing.T, mf []byte) string {
	t.Helper()
	ih, _, _, err := torrent.ParseFiles(mf)
	if err != nil {
		t.Fatal(err)
	}
	return ih
}

type fakeIndexer struct {
	sources   []indexer.Source
	hosts     map[int64]string // the host of each source's site
	downloads map[string]cardigann.DownloadResult
	calls     []string
}

func (f *fakeIndexer) Sources(context.Context) ([]indexer.Source, error) { return f.sources, nil }

func (f *fakeIndexer) CheckReleaseLink(_ context.Context, id int64, link string) error {
	if strings.HasPrefix(link, "magnet:") || strings.Contains(link, "//"+f.hosts[id]+"/") {
		return nil
	}
	return errors.New("foreign")
}

func (f *fakeIndexer) Download(_ context.Context, id int64, link string) (cardigann.DownloadResult, error) {
	f.calls = append(f.calls, fmt.Sprintf("%d %s", id, link))
	r, ok := f.downloads[link]
	if !ok {
		return r, errors.New("no such link")
	}
	return r, nil
}

type replaceCall struct {
	old       string
	newIH     string
	cancelled bool // the ctx could end (not a background one)
	keepsNew  bool
}

type fakeEngine struct {
	has      map[string]bool
	magnets  map[string][]byte
	replaced []replaceCall
	// modes: the mode of a torrent it has (none: Get gives no handle)
	modes map[string]string
	// data: a torrent's folder and files on disk (by infohash; with a
	// mode); Replace gives the new torrent the old one's folder
	data map[string]*fakeData
}

type fakeData struct {
	saveDir   string
	files     []torrent.FileInfo
	completed bool
	status    string
}

func (f *fakeEngine) Get(ih string) (torrent.Handle, bool) {
	ih = strings.ToLower(ih)
	if m := f.modes[ih]; m != "" && f.has[ih] {
		return modeHandle{ih: ih, mode: m, d: f.data[ih]}, true
	}
	return nil, f.has[ih]
}

// modeHandle is a torrent of the fake engine that tells only its mode.
type modeHandle struct {
	torrent.Handle
	ih, mode string
	d        *fakeData
}

func (h modeHandle) InfoHash() string { return h.ih }
func (h modeHandle) Status() torrent.Status {
	st := torrent.Status{InfoHash: h.ih, Mode: h.mode}
	if h.d != nil {
		st.SaveDir, st.Status = h.d.saveDir, h.d.status
		if h.d.completed {
			now := time.Now()
			st.CompletedAt = &now
		}
	}
	return st
}
func (h modeHandle) Files() []torrent.FileInfo {
	if h.d == nil {
		return nil
	}
	return h.d.files
}

func (f *fakeEngine) FetchInfo(_ context.Context, magnet string) ([]byte, error) {
	if mf, ok := f.magnets[magnet]; ok {
		return mf, nil
	}
	return nil, torrent.ErrNoInfo
}

func (f *fakeEngine) Replace(ctx context.Context, oldIH string, mf []byte, keep func(string) bool) (torrent.Handle, error) {
	ih, _, files, err := torrent.ParseFiles(mf)
	if err != nil {
		return nil, err
	}
	kn := false
	for _, fe := range files {
		if keep != nil && keep(fe.Path) {
			kn = true
		}
	}
	f.replaced = append(f.replaced, replaceCall{old: oldIH, newIH: ih, cancelled: ctx.Done() != nil, keepsNew: kn})
	delete(f.has, oldIH)
	f.has[ih] = true
	if m, ok := f.modes[oldIH]; ok {
		f.modes[ih] = m
		delete(f.modes, oldIH)
	}
	if d, ok := f.data[oldIH]; ok {
		nd := &fakeData{saveDir: d.saveDir}
		for _, fe := range files {
			nd.files = append(nd.files, torrent.FileInfo{Index: fe.Index, Path: fe.Path, FullPath: filepath.Join(d.saveDir, filepath.FromSlash(fe.Path)), Size: fe.Size, Selected: true})
		}
		f.data[ih] = nd
		delete(f.data, oldIH)
	}
	return nil, nil
}

type env struct {
	svc   *Service
	track *coretest.Tracking
	lib   *coretest.Library
	idx   *fakeIndexer
	eng   *fakeEngine
	title tracking.Title
	now   time.Time
	users []int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	st := &coretest.Tracking{}
	ti, err := st.EnsureTitle(context.Background(), "series", 100, "Show", "")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{
		idx: &fakeIndexer{sources: []indexer.Source{{ID: 3, Name: "Tracker", Enabled: true}, {ID: 4, Name: "Other", Enabled: true}},
			hosts: map[int64]string{3: "tracker.test", 4: "other.test"}, downloads: map[string]cardigann.DownloadResult{}},
		eng:   &fakeEngine{has: map[string]bool{}, magnets: map[string][]byte{}},
		title: ti,
		now:   time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
		track: st,
		lib:   &coretest.Library{},
		users: []int64{1, 2}, // profiles are the core's: ids only
	}
	e.svc = &Service{DB: d, Tracking: st.Adapter(), Library: e.lib, Indexer: e.idx, Engine: e.eng, Now: func() time.Time { return e.now }}
	return e
}

// production's adapter is a Tracking and a Library
var (
	_ Tracking = coreadapt.Tracking{}
	_ Library  = coreadapt.Library{}
)

const releaseName = "Show.S01.1080p.WEB-DL.HDRezka"

func twoEpisodes(t *testing.T) []byte {
	return metafile(t, releaseName, "Show.S01E01.1080p.mkv", "Show.S01E02.1080p.mkv")
}

func TestPinReleaseStoresFiles(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	mf := twoEpisodes(t)
	e.idx.downloads["https://tracker.test/dl/1"] = cardigann.DownloadResult{Torrent: mf}
	p, err := e.svc.PinRelease(ctx, e.title.ID, "3", "https://tracker.test/dl/1", "https://tracker.test/t/1", "Show S01 1080p", by(e.users[0]))
	if err != nil {
		t.Fatal(err)
	}
	if p.InfoHash != ihOf(t, mf) || len(p.Files) != 2 || p.Quality != "1080p" || p.SourceID != "3" || p.PinnedBy != e.users[0] || p.PinnedByUID != by(e.users[0]).UID {
		t.Fatalf("pin %+v", p)
	}
	got, ok, err := e.svc.ByTitle(ctx, e.title.ID)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got.ID != p.ID || got.InfoHash != p.InfoHash || len(got.Files) != 2 || got.Files[1].Path != releaseName+"/Show.S01E02.1080p.mkv" ||
		got.DetailsURL != "https://tracker.test/t/1" || got.ReleaseTitle != "Show S01 1080p" || !got.PinnedAt.Equal(e.now) || !got.LastChecked.Equal(e.now) {
		t.Fatalf("stored %+v", got)
	}
	if err := e.svc.Unpin(ctx, e.title.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := e.svc.ByTitle(ctx, e.title.ID); ok {
		t.Fatal("pin left after Unpin")
	}
}

func TestPinIsHouseholdWide(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	e.idx.downloads["https://tracker.test/dl/1"] = cardigann.DownloadResult{Torrent: twoEpisodes(t)}
	other := metafile(t, "Show.S01.720p", "Show.S01E01.720p.mkv")
	e.idx.downloads["https://other.test/dl/9"] = cardigann.DownloadResult{Torrent: other}
	if _, err := e.svc.PinRelease(ctx, e.title.ID, "3", "https://tracker.test/dl/1", "", "A", by(e.users[0])); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.PinRelease(ctx, e.title.ID, "4", "https://other.test/dl/9", "", "B", by(e.users[1])); err != nil {
		t.Fatal(err)
	}
	var n int
	e.svc.DB.QueryRow(`SELECT COUNT(*) FROM pinned_releases WHERE title_id=?`, e.title.ID).Scan(&n)
	p, _, _ := e.svc.ByTitle(ctx, e.title.ID)
	if n != 1 || p.InfoHash != ihOf(t, other) || p.PinnedBy != e.users[1] || p.SourceID != "4" || p.ReleaseTitle != "B" {
		t.Fatalf("%d rows, pin %+v", n, p)
	}
}

func TestPinOwnUnknownTrackerRefused(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	_, err := e.svc.PinOwn(ctx, e.title.ID, "https://elsewhere.test/viewtopic.php?t=5", nil, by(e.users[0]))
	if !errors.Is(err, ErrNoSource) || err.Error() != "Этот трекер не подключён в «Источниках»" {
		t.Fatalf("err %v", err)
	}
	if _, ok, _ := e.svc.ByTitle(ctx, e.title.ID); ok {
		t.Fatal("pinned anyway")
	}
	// a topic of a connected tracker: that source downloads it
	mf := twoEpisodes(t)
	e.idx.downloads["https://other.test/viewtopic.php?t=5"] = cardigann.DownloadResult{Torrent: mf}
	p, err := e.svc.PinOwn(ctx, e.title.ID, " https://other.test/viewtopic.php?t=5 ", nil, by(e.users[0]))
	if err != nil {
		t.Fatal(err)
	}
	if p.SourceID != "4" || p.DetailsURL != "https://other.test/viewtopic.php?t=5" || p.InfoHash != ihOf(t, mf) || p.ReleaseTitle != releaseName {
		t.Fatalf("pin %+v", p)
	}
	// a magnet: its metafile from the swarm, never fetched again
	mag := "magnet:?xt=urn:btih:" + ihOf(t, mf)
	e.eng.magnets[mag] = mf
	if p, err = e.svc.PinOwn(ctx, e.title.ID, mag, nil, by(e.users[0])); err != nil || p.SourceID != "" || p.Link != mag || len(p.Files) != 2 {
		t.Fatalf("magnet pin %+v %v", p, err)
	}
	// an own .torrent file as is
	one := metafile(t, "Show.S01", "Show.S01E01.mkv")
	if p, err = e.svc.PinOwn(ctx, e.title.ID, "", one, by(e.users[0])); err != nil || p.InfoHash != ihOf(t, one) || p.SourceID != "" || p.Link != "" {
		t.Fatalf("file pin %+v %v", p, err)
	}
	if _, err := e.svc.PinOwn(ctx, e.title.ID, "", []byte("<html>no</html>"), by(e.users[0])); err == nil {
		t.Fatal("a non-torrent file pinned")
	}
}

func TestEpisodesOfFindsExternalTracks(t *testing.T) {
	files := []torrent.FileEntry{
		{Index: 0, Path: "Show/[Studio] Show - 08 [1080p].mkv"},
		{Index: 1, Path: "Show/RUS Sound/[Studio] Show - 08.mka"},
		{Index: 2, Path: "Show/[Studio] Show - 09 [1080p].mkv"},
		{Index: 3, Path: "Show/Subs/[Studio] Show - 08.ass"},
		{Index: 4, Path: "Show/.pad/1000", Padding: true},
		{Index: 5, Path: "Show/readme.txt"},
		{Index: 6, Path: "Show/Season 2/Show.S02E01-E02.mkv"},
		{Index: 7, Path: "Show/Season 2/Show.S02E01.rus.srt"},
	}
	got := EpisodesOf(files, 1)
	want := map[[2]int][]int{{1, 8}: {0, 1, 3}, {1, 9}: {2}, {2, 1}: {6, 7}, {2, 2}: {6}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestScannerEmitsOneSourcePerEpisode(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	mf := metafile(t, releaseName, "Show.S01E01.1080p.mkv", "Show.S01E02.1080p.mkv", "Sound/Show.S01E01.mka")
	e.idx.downloads["https://tracker.test/dl/1"] = cardigann.DownloadResult{Torrent: mf}
	p, err := e.svc.PinRelease(ctx, e.title.ID, "3", "https://tracker.test/dl/1", "", releaseName, by(e.users[0]))
	if err != nil {
		t.Fatal(err)
	}
	sc := e.svc.Scanner()
	if sc.Kind() != "pinned" {
		t.Fatal(sc.Kind())
	}
	e.now = e.now.Add(time.Hour) // fresh: no refresh
	srcs, err := sc.Scan(ctx, e.title, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 2 || len(e.idx.calls) != 1 {
		t.Fatalf("sources %+v, downloads %v", srcs, e.idx.calls)
	}
	for i, s := range srcs {
		if s.Season != 1 || s.Episode != i+1 || s.Kind != "pinned" || s.Ref != fmt.Sprintf("pin:%d:%d", p.ID, i) ||
			s.Quality != "1080p" || s.Provider != "Tracker" || !contains(s.Labels, "HDRezka Studio") {
			t.Fatalf("source %d: %+v", i, s)
		}
	}
	// a pin checked over 6 hours ago is refreshed first
	e.now = e.now.Add(6 * time.Hour)
	three := metafile(t, releaseName, "Show.S01E01.1080p.mkv", "Show.S01E02.1080p.mkv", "Sound/Show.S01E01.mka", "Show.S01E03.1080p.mkv")
	e.idx.downloads["https://tracker.test/dl/1"] = cardigann.DownloadResult{Torrent: three}
	if srcs, err = sc.Scan(ctx, e.title, nil); err != nil || len(srcs) != 3 || len(e.idx.calls) != 2 {
		t.Fatalf("after refresh: %+v %v %v", srcs, err, e.idx.calls)
	}
	// nothing pinned: no sources
	e.svc.Unpin(ctx, e.title.ID)
	if srcs, err = sc.Scan(ctx, e.title, nil); err != nil || len(srcs) != 0 {
		t.Fatalf("unpinned: %+v %v", srcs, err)
	}
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

// by is profile id as X-Antresol-User carries it.
func by(id int64) addonkit.User {
	return addonkit.User{ID: id, UID: "uid-" + strconv.FormatInt(id, 10)}
}
