package libraryhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/acquire"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

type fakeAcq struct {
	dls      []acquire.LibraryDownload
	found    []acquire.Candidate
	searched string
	titles   map[string]acquire.RequestTitleInfo
}

func (f *fakeAcq) LibraryDownloads(context.Context) []acquire.LibraryDownload { return f.dls }

func (f *fakeAcq) DiscoverMedia(_ context.Context, kind, q string) ([]acquire.Candidate, error) {
	f.searched = kind + "|" + q
	return f.found, nil
}

func (f *fakeAcq) RequestTitle(_ context.Context, kind, extID string) (acquire.RequestTitleInfo, bool) {
	t, ok := f.titles[kind+"|"+extID]
	return t, ok
}

type fakeSources struct {
	on  bool
	err error
}

func (s fakeSources) HasEnabled(context.Context) (bool, error) { return s.on, s.err }

func call(t *testing.T, d Deps, hook, user, body string) (int, []byte) {
	t.Helper()
	m := &addonkit.HookMux{}
	Register(m, d)
	r := httptest.NewRequest("POST", "/hooks/"+hook, strings.NewReader(body))
	if user != "" {
		r.Header.Set(addonkit.UserHeader, user)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w.Code, w.Body.Bytes()
}

func cards(t *testing.T, d Deps, user, body string) []addonkit.PendingCard {
	t.Helper()
	code, b := call(t, d, "library.pending", user, body)
	var a addonkit.PendingAnswer
	if code != 200 || json.Unmarshal(b, &a) != nil {
		t.Fatalf("%d %s", code, b)
	}
	return a.Cards
}

func downloads() []acquire.LibraryDownload {
	return []acquire.LibraryDownload{
		{Key: "movie:603", Kind: acquire.KindMovie, TMDBID: 603, Title: "Матрица", Year: 1999, Status: acquire.DownloadActive,
			Done: 1, Size: 2, By: []string{"uid-1"}, Poster: "abc.jpg", Stalled: torrent.StalledNoPeers},
		{Key: "music:mb-1", Kind: acquire.KindMusic, Title: "Группа крови", Creator: "Кино", Status: acquire.DownloadError,
			By: []string{"uid-2"}, Poster: "abc.jpg", ItemID: 9},
	}
}

func TestPendingHook_ScopeMine(t *testing.T) {
	d := Deps{Acquire: &fakeAcq{dls: downloads()}}
	if cs := cards(t, d, `{"id":1,"uid":"uid-1"}`, `{"scope":"mine"}`); len(cs) != 1 || cs[0].Pending.Key != "movie:603" {
		t.Fatalf("mine %+v", cs)
	}
	// «mine» goes by uid: a new profile given id 1 has none of these,
	// nor has a header without a uid
	for _, u := range []string{`{"id":1,"uid":"B"}`, `{"id":1}`} {
		if cs := cards(t, d, u, `{"scope":"mine"}`); len(cs) != 0 {
			t.Fatalf("%s: mine %+v", u, cs)
		}
	}
	if cs := cards(t, d, `{"id":1}`, `{"scope":"all"}`); len(cs) != 2 {
		t.Fatalf("all %+v", cs)
	}
}

// The card: library kinds, the movie's title page, the cached poster name
// as it is; an album opens the downloads page for an admin only.
func TestPendingHook_Cards(t *testing.T) {
	d := Deps{Acquire: &fakeAcq{dls: downloads()}}
	cs := cards(t, d, `{"id":1,"isAdmin":true}`, `{"scope":"all"}`)
	m, a := cs[0], cs[1]
	if m.Card != (addonkit.PendingCardInfo{Kind: "movie", Title: "Матрица", Year: 1999, Poster: "abc.jpg", TmdbKind: "movie", TmdbID: 603}) ||
		m.Pending.Href != "/title/movie/603" || m.Pending.Status != "active" || m.Pending.Done != 1 || m.Pending.Size != 2 {
		t.Fatalf("movie %+v", m)
	}
	if a.Card.Kind != "album" || a.Card.Subtitle != "Кино" || a.Card.ID != 9 || a.Card.TmdbID != 0 || a.Pending.Href != "/addons/torrents/downloads" {
		t.Fatalf("album %+v", a)
	}
	if cs := cards(t, d, `{"id":2}`, `{"scope":"all"}`); cs[1].Pending.Href != "" || cs[0].Pending.Href != "/title/movie/603" {
		t.Fatalf("not an admin %+v", cs)
	}
}

func TestPendingHook_HintInLanguage(t *testing.T) {
	d := Deps{Acquire: &fakeAcq{dls: downloads()}}
	if cs := cards(t, d, `{"id":1,"lang":"en"}`, `{"scope":"all"}`); cs[0].Pending.Hint != "No peers for this release yet — waiting…" || cs[1].Pending.Hint != "" {
		t.Fatalf("en %+v", cs)
	}
	if cs := cards(t, d, `{"id":1,"lang":"ru"}`, `{}`); cs[0].Pending.Hint != "Нет участников раздачи — ждём…" {
		t.Fatalf("ru %+v", cs)
	}
}

// An error card is sent as such: the core shows it to admins only.
// A movie or series without a TMDB id has no title page: no link, or the
// downloads page for an admin.
func TestPendingHook_NoTMDBID(t *testing.T) {
	d := Deps{Acquire: &fakeAcq{dls: []acquire.LibraryDownload{{Key: "movie:x", Kind: acquire.KindMovie, Title: "X", Status: acquire.DownloadActive}}}}
	if cs := cards(t, d, `{"id":2}`, `{}`); cs[0].Pending.Href != "" || cs[0].Card.TmdbKind != "" {
		t.Fatalf("adult %+v", cs[0])
	}
	if cs := cards(t, d, `{"id":1,"isAdmin":true}`, `{}`); cs[0].Pending.Href != "/addons/torrents/downloads" {
		t.Fatalf("admin %+v", cs[0])
	}
}

func TestPendingHook_ErrorStatus(t *testing.T) {
	d := Deps{Acquire: &fakeAcq{dls: downloads()}}
	if cs := cards(t, d, `{"id":5}`, `{"scope":"all"}`); len(cs) != 2 || cs[1].Pending.Status != "error" {
		t.Fatalf("%+v", cs)
	}
	if code, b := call(t, d, "library.pending", `{"id":5}`, `not json`); code == 200 {
		t.Fatalf("bad body: %d %s", code, b)
	}
}

func TestDiskHook(t *testing.T) {
	dl, sc := t.TempDir(), filepath.Join(t.TempDir(), "stream")
	d := Deps{Acquire: &fakeAcq{}, DownloadDir: dl, StreamDir: sc}
	segs := func() []addonkit.DiskSegment {
		code, b := call(t, d, "disk.usage", "", `{}`)
		var u addonkit.DiskUsage
		if code != 200 || json.Unmarshal(b, &u) != nil {
			t.Fatalf("%d %s", code, b)
		}
		return u.Segments
	}
	if s := segs(); len(s) != 1 || s[0].ID != "downloads" || s[0].Dir != dl || s[0].Label.EN != "Downloads" {
		t.Fatalf("stream dir missing: %+v", s)
	}
	os.MkdirAll(sc, 0o755)
	if s := segs(); len(s) != 2 || s[1].ID != "stream-cache" || s[1].Dir != sc || s[1].Label.RU != "Кеш просмотра" {
		t.Fatalf("both: %+v", s)
	}
}

// A segment folder the core would drop (outside the media root and the
// file roots) is logged at the start.
func TestCheckSegmentDirs(t *testing.T) {
	media, other := t.TempDir(), t.TempDir()
	in := filepath.Join(media, "torrents")
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	info := addonkit.Info{MediaRoot: media, FileRoots: []string{media}}
	bad := CheckSegmentDirs(log, info, in, other, "")
	if len(bad) != 1 || bad[0] != other || !strings.Contains(buf.String(), other) || strings.Contains(buf.String(), in) {
		t.Fatalf("bad %v log %s", bad, buf.String())
	}
	// a file root outside the media root counts too
	if bad := CheckSegmentDirs(slog.New(slog.NewTextHandler(io.Discard, nil)), addonkit.Info{MediaRoot: media, FileRoots: []string{other}}, other); len(bad) != 0 {
		t.Fatalf("a file root: %v", bad)
	}
	// the core's other drops: the media root itself, a library folder or
	// one around it, a folder inside an earlier one
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	movies := filepath.Join(media, "movies")
	os.MkdirAll(movies, 0o755)
	li := addonkit.Info{MediaRoot: media, FileRoots: []string{media}, LibraryRoots: addonkit.LibraryRoots{Movies: movies}}
	got := CheckSegmentDirs(quiet, li, in, media, movies, filepath.Join(movies, "x"), filepath.Join(in, "cache"), filepath.Join(media, "cache"))
	want := []string{media, movies, filepath.Join(movies, "x"), filepath.Join(in, "cache")}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestRequestsOpen_NoSource(t *testing.T) {
	for _, c := range []struct {
		src  fakeSources
		want bool
	}{{fakeSources{on: false}, false}, {fakeSources{on: true}, true}, {fakeSources{err: errors.New("db")}, false}} {
		code, b := call(t, Deps{Sources: c.src}, "requests.open", "", `{}`)
		var a addonkit.RequestsOpenAnswer
		if code != 200 || json.Unmarshal(b, &a) != nil || a.Open != c.want {
			t.Errorf("%+v: %d %s", c.src, code, b)
		}
	}
}

func TestRequestsSearch_Music(t *testing.T) {
	f := &fakeAcq{found: []acquire.Candidate{
		{Kind: "music", ExtID: "mb-1", Title: "Группа крови", Subtitle: "Кино", Year: 1988, Poster: "https://caa.test/x", Aliases: []string{"Blood Type"}},
		{Kind: "music", ExtID: "mb-2", Title: "X", Explicit: true},
	}}
	code, b := call(t, Deps{Acquire: f}, "requests.search", "", `{"kind":"music","q":"кино"}`)
	var a addonkit.RequestSearchAnswer
	if code != 200 || json.Unmarshal(b, &a) != nil || len(a.Candidates) != 2 || f.searched != "music|кино" {
		t.Fatalf("%d %s", code, b)
	}
	c := a.Candidates[0]
	if c.ExtID != "mb-1" || c.Subtitle != "Кино" || c.Year != 1988 || c.Poster != "https://caa.test/x" || c.Aliases[0] != "Blood Type" || c.Explicit || !a.Candidates[1].Explicit {
		t.Fatalf("%+v", a.Candidates)
	}
	// video is the core's; an empty query asks nothing
	for _, body := range []string{`{"kind":"movie","q":"x"}`, `{"kind":"book","q":""}`} {
		f.searched = ""
		code, b := call(t, Deps{Acquire: f}, "requests.search", "", body)
		if code != 200 || !strings.Contains(string(b), `"candidates":[]`) || f.searched != "" {
			t.Errorf("%s: %d %s %q", body, code, b, f.searched)
		}
	}
}

func TestRequestsTitle_Unknown(t *testing.T) {
	f := &fakeAcq{titles: map[string]acquire.RequestTitleInfo{"book|ol-1": {Title: "Мастер и Маргарита", Creator: "Булгаков", Year: 1967}}}
	if code, _ := call(t, Deps{Acquire: f}, "requests.title", "", `{"kind":"book","extId":"ol-2"}`); code != http.StatusNoContent {
		t.Fatalf("unknown: %d", code)
	}
	if code, _ := call(t, Deps{Acquire: f}, "requests.title", "", `{"kind":"movie","extId":"x"}`); code != http.StatusNoContent {
		t.Fatalf("a movie: %d", code)
	}
	code, b := call(t, Deps{Acquire: f}, "requests.title", "", `{"kind":"book","extId":"ol-1"}`)
	var a addonkit.RequestTitleAnswer
	if code != 200 || json.Unmarshal(b, &a) != nil || a != (addonkit.RequestTitleAnswer{Title: "Мастер и Маргарита", Subtitle: "Булгаков", Year: 1967}) {
		t.Fatalf("%d %s", code, b)
	}
}
