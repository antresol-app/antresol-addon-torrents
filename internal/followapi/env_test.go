package followapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	"github.com/antresol-app/antresol-addon-torrents/internal/follow"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/pinned"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings"
	"github.com/antresol-app/antresol-addon-torrents/internal/studios"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

func TestMain(m *testing.M) {
	studios.Register()
	os.Exit(m.Run())
}

// The profiles the core sends (X-Antresol-User).
const (
	adminU = `{"id":1,"uid":"uid-1","name":"Папа","isAdmin":true,"isKid":false,"lang":"ru"}`
	mamaU  = `{"id":2,"uid":"uid-2","name":"Мама","isAdmin":false,"isKid":false,"lang":"ru"}`
	sonU   = `{"id":3,"uid":"uid-3","name":"Сын","isAdmin":false,"isKid":false,"lang":"ru"}`
	kidU   = `{"id":4,"uid":"uid-4","name":"Малыш","isAdmin":false,"isKid":true,"lang":"ru"}`
)

// enU is u in English.
func enU(u string) string { return strings.Replace(u, `"lang":"ru"`, `"lang":"en"`, 1) }

// flAcq records the follow checks' acquisitions.
type flAcq struct {
	mu    sync.Mutex
	calls []acquire.AcquireRequest
}

func (f *flAcq) Acquire(_ context.Context, r acquire.AcquireRequest) (acquire.AcquireResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r)
	return acquire.AcquireResult{InfoHash: "ih"}, nil
}

func (f *flAcq) Pending(context.Context) ([]acquire.PendingAcquisition, error) { return nil, nil }

func (f *flAcq) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// flTMDB: series 42 has one fully aired season of two episodes; movie 7
// is out.
type flTMDB struct{}

func (flTMDB) TV(_ context.Context, id int) (tmdb.Details, error) {
	return tmdb.Details{ID: id, Title: "Сёгун", Seasons: []tmdb.SeasonCount{{Number: 1, Episodes: 2}}}, nil
}
func (flTMDB) Season(_ context.Context, _, n int) (tmdb.SeasonDetails, error) {
	return tmdb.SeasonDetails{Number: n, Episodes: []tmdb.Episode{{Number: 1, AirDate: "2024-01-01"}, {Number: 2, AirDate: "2024-01-08"}}}, nil
}
func (flTMDB) Movie(_ context.Context, id int) (tmdb.Details, error) {
	return tmdb.Details{ID: id, Title: "Дюна", Year: 2024, ReleaseDate: "2024-02-27"}, nil
}
func (flTMDB) ImageURL(_, p string) string { return p }

// metafile builds a v1 metafile named name holding paths of size bytes
// each (pieces enough for size bytes in total when pad > 0).
func metafile(t *testing.T, name string, pad int, paths ...string) []byte {
	t.Helper()
	info := metainfo.Info{Name: name, PieceLength: 16384, Pieces: make([]byte, 20+pad)}
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

// pnIndexer: source 3 is on tracker.test; its links download as given.
type pnIndexer struct{ downloads map[string][]byte }

func (pnIndexer) Sources(context.Context) ([]indexer.Source, error) {
	return []indexer.Source{{ID: 3, Name: "Tracker", Enabled: true}}, nil
}
func (pnIndexer) CheckReleaseLink(_ context.Context, id int64, link string) error {
	if strings.HasPrefix(link, "magnet:") || (id == 3 && strings.Contains(link, "//tracker.test/")) {
		return nil
	}
	return errors.New("Эта раздача не из выбранного источника") // the Manager's text
}
func (f pnIndexer) Download(_ context.Context, _ int64, link string) (cardigann.DownloadResult, error) {
	mf, ok := f.downloads[link]
	if !ok {
		return cardigann.DownloadResult{}, errors.New("no such link")
	}
	return cardigann.DownloadResult{Torrent: mf}, nil
}

// pnEngine has no torrents; magnets resolve from its map.
type pnEngine struct{ magnets map[string][]byte }

func (pnEngine) Get(string) (torrent.Handle, bool) { return nil, false }
func (f pnEngine) FetchInfo(_ context.Context, magnet string) ([]byte, error) {
	if mf, ok := f.magnets[magnet]; ok {
		return mf, nil
	}
	return nil, torrent.ErrNoInfo
}
func (pnEngine) Replace(context.Context, string, []byte, func(string) bool) (torrent.Handle, error) {
	return nil, errors.New("no replace here")
}

// player is the pin and voice-over routes' acquire: it records the
// streams, the pinned downloads and the voice-over calls.
type player struct {
	mu     sync.Mutex
	plays  []acquire.AcquireRequest
	dl     []pinDownload
	voices []acquire.VoiceOverRequest
}

type pinDownload struct {
	ih    string
	req   acquire.AcquireRequest
	files []int
}

func (p *player) Acquire(_ context.Context, r acquire.AcquireRequest) (acquire.AcquireResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.plays = append(p.plays, r)
	return acquire.AcquireResult{InfoHash: "ABCDEF0123456789ABCDEF0123456789ABCDEF01", FileIndex: 1, Status: "streaming"}, nil
}

func (p *player) AcquirePinnedFiles(_ context.Context, ih string, r acquire.AcquireRequest, files []int) (acquire.AcquireResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dl = append(p.dl, pinDownload{ih, r, files})
	return acquire.AcquireResult{InfoHash: ih, Status: "downloading"}, nil
}

func (p *player) FindVoiceOvers(context.Context, int64) (acquire.VoiceOvers, error) {
	return acquire.VoiceOvers{Candidates: []acquire.VoiceOverCandidate{}, VideoReleases: []acquire.VoiceOverCandidate{}}, nil
}

func (p *player) DownloadVoiceOver(_ context.Context, r acquire.VoiceOverRequest) (acquire.VoiceOverStarted, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.voices = append(p.voices, r)
	return acquire.VoiceOverStarted{InfoHash: "ih"}, nil
}

func (p *player) PreviewVoiceOver(context.Context, int64, string) (acquire.VoiceOverPreview, error) {
	return acquire.VoiceOverPreview{}, nil
}

func (p *player) VoiceOverDownloads(context.Context, int64) ([]acquire.VoiceOverDownload, error) {
	return nil, nil
}

// watched is one Watch call.
type watched struct {
	user int64
	ih   string
	idx  int
}

type env struct {
	t       *testing.T
	h       http.Handler
	follows *follow.Service
	users   *coretest.Users
	acq     *flAcq
	pins    *pinned.Service
	track   *coretest.Tracking
	lib     *coretest.Library
	player  *player
	watches []watched
	added   int
	magnet  string
	mf      []byte
	dl      map[string][]byte
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e := &env{t: t, acq: &flAcq{}, track: &coretest.Tracking{}, lib: &coretest.Library{}, player: &player{}}
	e.mf = metafile(t, "Show.S01.1080p.WEB-DL", 0, "Show.S01E01.1080p.mkv", "Show.S01E02.1080p.mkv")
	ih, _, _, err := torrent.ParseFiles(e.mf)
	if err != nil {
		t.Fatal(err)
	}
	e.magnet = "magnet:?xt=urn:btih:" + ih
	e.dl = map[string][]byte{"https://tracker.test/dl/1": e.mf}
	users := &coretest.Users{Names: map[int64]string{1: "Папа", 2: "Мама", 3: "Сын"}}
	e.users = users
	e.follows = &follow.Service{DB: db, Acquire: e.acq, TMDB: flTMDB{}, Library: e.lib, Users: users.Adapter(), Titles: e.lib,
		Pause: -1, Now: func() time.Time { return time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC) }}
	e.pins = &pinned.Service{DB: db, Tracking: e.track.Adapter(), Indexer: pnIndexer{downloads: e.dl},
		Engine: pnEngine{magnets: map[string][]byte{e.magnet: e.mf}}}
	mux := http.NewServeMux()
	Register(mux, Deps{Follows: e.follows, Pins: e.pins, Acquire: e.player, Library: e.lib,
		Watch: func(_ context.Context, u addonkit.User, ih string, idx int) (string, error) {
			e.watches = append(e.watches, watched{u.ID, ih, idx})
			return "in-" + strconv.Itoa(len(e.watches)), nil
		},
		Added: func() { e.added++ }})
	e.h = mux
	return e
}

func (e *env) do(method, path, user, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if user != "" {
		r.Header.Set(addonkit.UserHeader, user)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *env) follow(user, kind string, id int) follow.Follow {
	e.t.Helper()
	w := e.do("POST", "/api/follow", user, `{"kind":"`+kind+`","tmdbId":`+strconv.Itoa(id)+`}`)
	if w.Code != 200 {
		e.t.Fatalf("follow: %d %s", w.Code, w.Body)
	}
	var f follow.Follow
	if err := json.Unmarshal(w.Body.Bytes(), &f); err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *env) list(user string) []follow.Follow {
	e.t.Helper()
	w := e.do("GET", "/api/follow", user, "")
	var l struct{ Items []follow.Follow }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &l) != nil || l.Items == nil {
		e.t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	return l.Items
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// item puts a film into the fake library and returns its id.
func (e *env) item(kind, title string) int64 {
	id := int64(len(e.lib.Items) + 1)
	e.lib.Items = append(e.lib.Items, library.Item{ID: id, Kind: kind, Title: title, MetaJSON: "{}"})
	return id
}
