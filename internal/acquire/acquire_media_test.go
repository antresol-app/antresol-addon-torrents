package acquire

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/musicbrainz"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/openlibrary"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/wikidata"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

type fakeMB struct {
	groups []musicbrainz.ReleaseGroup
	err    error
	qs     []string
}

func (f *fakeMB) SearchFree(_ context.Context, q string) ([]musicbrainz.ReleaseGroup, error) {
	f.qs = append(f.qs, q)
	return f.groups, f.err
}
func (f *fakeMB) CoverURL(id string) string { return "https://caa/" + id }

type fakeOL struct {
	docs []openlibrary.Doc
	err  error
	// byTitle: Search answers (cover lookups), keyed by "title|author"
	byTitle map[string][]openlibrary.Doc
	lookups []string
}

func (f *fakeOL) SearchQ(context.Context, string) ([]openlibrary.Doc, error) { return f.docs, f.err }
func (f *fakeOL) Search(_ context.Context, title, author string) ([]openlibrary.Doc, error) {
	f.lookups = append(f.lookups, title+"|"+author)
	return f.byTitle[title+"|"+author], nil
}
func (f *fakeOL) CoverURL(id int) string {
	if id == 0 {
		return ""
	}
	return "https://covers/" + itoa(int64(id))
}

func (l *fakeLib) FindAlbum(_ context.Context, artist, album string) (library.Item, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	it, ok := l.albums[artist+"|"+album]
	return it, ok, nil
}

// FindTitle is library.Repo.FindTitle over the fake's lookups.
func (l *fakeLib) FindTitle(ctx context.Context, kind string, tmdbID int64, title, creator string) (library.Item, bool, error) {
	switch kind {
	case KindMovie, KindSeries:
		it, err := l.ItemByTMDB(ctx, kind, tmdbID)
		return it, err == nil, nil
	case KindMusic:
		return l.FindAlbum(ctx, creator, title)
	case KindBook, KindAudiobook:
		return l.FindBook(ctx, kind, creator, title)
	}
	return library.Item{}, false, nil
}

func (l *fakeLib) FindBook(_ context.Context, kind, author, title string) (library.Item, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	it, ok := l.books[kind+"|"+author+"|"+title]
	return it, ok, nil
}

func TestMediaSearchRequest(t *testing.T) {
	sr := mediaSearchRequest(KindMusic, "Группа крови", "Кино")
	if sr.Kind != "music" || sr.Text != "Кино Группа крови" || !slices.Equal(sr.AltTitles, []string{"Группа крови"}) || sr.Artist != "Кино" || sr.Work != "Группа крови" {
		t.Fatalf("request %+v", sr)
	}
	if c := mediaSearchRequest(KindComic, "Берсерк", ""); c.Text != "Берсерк" || c.Kind != "comic" || c.Work != "Берсерк" {
		t.Fatalf("comic %+v", c)
	}
	if a := mediaSearchRequest(KindMusic, "", "Ария"); a.Text != "Ария" || len(a.AltTitles) != 0 || a.Artist != "Ария" || a.Work != "" {
		t.Fatalf("artist only %+v", a)
	}
	// the title filter Search applies (indexer.Score for media kinds)
	want := indexer.Wanted{Kind: sr.Kind, Titles: append([]string{sr.Text}, sr.AltTitles...), Artist: sr.Artist, Work: sr.Work}
	for title, ok := range map[string]bool{
		"Кино - Группа крови (1988) FLAC": true,
		"Алиса - Группа крови":            false,
	} {
		s, why := indexer.Score(indexer.Result{Title: title, Seeders: 3}, release.Parse(title), want, indexer.DefaultPrefs())
		if (s != indexer.Excluded) != ok {
			t.Errorf("%q: %d %v", title, s, why)
		}
	}
}

func TestVideoPickUnchanged(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Releases(context.Background(), ReleaseRequest{TMDBID: 693134, Kind: KindMovie}); err != nil {
		t.Fatal(err)
	}
	d, _ := fakeMeta{}.Movie(context.Background(), 693134)
	if got, want := e.idx.requests[0], searchRequest(KindMovie, d, 0, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("request %+v, want %+v", got, want)
	}
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Сёгун S01", "https://t/s1", 3, 9)}}
	if _, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Mode: ModeDownload}); err != nil {
		t.Fatal(err)
	}
	tv, _ := fakeMeta{}.TV(context.Background(), 126308)
	if got, want := e.idx.requests[1], searchRequest(KindSeries, tv, 1, 0); !reflect.DeepEqual(got, want) {
		t.Fatalf("series request %+v, want %+v", got, want)
	}
	if e.eng.added[0].SaveDir != filepath.Join(e.root, "torrents", "series") {
		t.Fatalf("save dir %s", e.eng.added[0].SaveDir)
	}
}

func TestAcquireMusicDownloadOnly(t *testing.T) {
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Кино - Группа крови (1988) FLAC", "https://t/1", 1, 50)}}
	for _, kind := range []string{KindMusic, KindBook, KindAudiobook, KindComic} {
		_, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: kind, Title: "Группа крови", Artist: "Кино", Mode: ModeStream})
		msg, _ := UserMessage(err)
		if !errors.Is(err, ErrInvalid) || msg != "Для музыки и книг доступна только загрузка на сервер" {
			t.Fatalf("%s stream: %v", kind, err)
		}
	}
	if len(e.eng.added) != 0 || len(e.idx.requests) != 0 {
		t.Fatal("a stream of music was started")
	}
	_, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindMusic, Title: "Группа крови", Mode: ModeDownload})
	if msg, _ := UserMessage(err); !errors.Is(err, ErrInvalid) || msg != "Укажите исполнителя" {
		t.Fatalf("no artist: %v", err)
	}
	if _, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Artist: "Булгаков", Mode: ModeDownload}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no book title: %v", err)
	}
	// the whole artist: a discography is welcome
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindMusic, Artist: "Кино", Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	if q := e.idx.requests[0]; q.Text != "Кино" || q.Work != "" || q.Artist != "Кино" {
		t.Fatalf("artist request %+v", q)
	}
	for _, tv := range e.svc.Torrents() {
		if tv.InfoHash == res.InfoHash && tv.Title != "Кино" {
			t.Fatalf("view %+v", tv)
		}
	}
}

func TestAcquisitionExtID(t *testing.T) {
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Кино - Группа крови (1988) FLAC", "https://t/1", 1, 50)}}
	tor := filepath.Join(e.root, "torrents", "music")
	e.eng.nextFiles = torrentFiles(t, tor, "Кино - Группа крови (1988) FLAC/01. Группа крови.flac", "Кино - Группа крови (1988) FLAC/folder.jpg")
	const mbid = "1b022e01-4da6-387b-8658-8678046e4cef"
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindMusic, ExtID: mbid, Title: "Группа крови", Artist: "Кино", Year: 1988, Mode: ModeDownload, UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if q := e.idx.requests[0]; q.Kind != "music" || q.Text != "Кино Группа крови" {
		t.Fatalf("search %+v", q)
	}
	if got := e.eng.added[0].SaveDir; got != tor {
		t.Fatalf("save dir %s", got)
	}
	var kind, ext, meta, title string
	var tmdbID, year int
	if err := e.svc.o.DB.QueryRow(`SELECT kind, tmdb_id, ext_id, meta_json, title, year FROM acquisitions WHERE infohash=?`, res.InfoHash).
		Scan(&kind, &tmdbID, &ext, &meta, &title, &year); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal([]byte(meta), &m)
	if kind != "music" || tmdbID != 0 || ext != mbid || m["artist"] != "Кино" || title != "Группа крови" || year != 1988 {
		t.Fatalf("row %s %d %s %s %s %d", kind, tmdbID, ext, meta, title, year)
	}

	e.eng.complete(res.InfoHash)
	e.svc.wait()
	track := filepath.Join(e.root, "media/music", "Кино", "Группа крови (1988)", "01. Группа крови.flac")
	if _, err := os.Stat(track); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(track), "folder.jpg")); err != nil {
		t.Fatal(err)
	}
	if e.lib.bound[track] != res.InfoHash || len(e.lib.bound) != 1 {
		t.Fatalf("bound %v", e.lib.bound)
	}
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); !a.OrganizedAt.Valid || a.Error != "" {
		t.Fatalf("acquisition %+v", a)
	}
}

func TestOrganizeBindsOnlyMediaFiles(t *testing.T) {
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Pink Floyd - The Wall (1979) FLAC", "https://t/1", 1, 50)}}
	tor := filepath.Join(e.root, "torrents", "music")
	e.eng.nextFiles = torrentFiles(t, tor, "The Wall/The Wall.flac", "The Wall/The Wall.cue", "The Wall/rip.log", "The Wall/Scans/front.jpg")
	album := filepath.Join(e.root, "media/music", "Pink Floyd", "The Wall (1979)")
	// the scan indexed only the audio
	e.lib.only = map[string]bool{filepath.Join(album, "The Wall.flac"): true}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindMusic, Title: "The Wall", Artist: "Pink Floyd", Year: 1979, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	a, _ := e.svc.acquisition(context.Background(), res.InfoHash)
	if !a.OrganizedAt.Valid || a.Error != "" || e.scans != 1 {
		t.Fatalf("acquisition %+v scans %d", a, e.scans)
	}
	if len(e.lib.bound) != 1 || e.lib.bound[filepath.Join(album, "The Wall.flac")] != res.InfoHash {
		t.Fatalf("bound %v", e.lib.bound)
	}
	var raw string
	var paths []string
	if err := e.svc.o.DB.QueryRow(`SELECT paths_json FROM acquisitions WHERE infohash=?`, res.InfoHash).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal([]byte(raw), &paths); len(paths) != 4 {
		t.Fatalf("paths %v", paths)
	}
}

func TestOrganizeMediaFailureNotLooping(t *testing.T) {
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Мастер и Маргарита DJVU", "https://t/1", 1, 5)}}
	e.eng.nextFiles = torrentFiles(t, filepath.Join(e.root, "torrents", "books"), "Мастер/master.djvu")
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindBook, Title: "Мастер и Маргарита", Artist: "Булгаков", Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	for range maxOrganizeAttempts + 2 {
		e.eng.complete(res.InfoHash)
		e.svc.wait()
		e.svc.reconcile(context.Background())
		e.svc.wait()
	}
	if got := e.svc.attempts(res.InfoHash); got != maxOrganizeAttempts {
		t.Fatalf("attempts %d", got)
	}
	if e.scans != 0 {
		t.Fatalf("scans %d", e.scans)
	}
}

func TestAcquireMediaWaitsWhenFull(t *testing.T) {
	e := newEnv(t)
	*e.disk = *diskAt(96)
	e.disk.Engine = e.eng
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Берсерк [RUS] CBZ", "https://t/1", 1, 5)}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindComic, Title: "Берсерк", Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != torrent.StatusWaitingSpace || e.eng.added[0].SaveDir != filepath.Join(e.root, "torrents", "comics") {
		t.Fatalf("result %+v options %+v", res, e.eng.added[0])
	}
}

func TestMediaReleases(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Releases(context.Background(), ReleaseRequest{Kind: KindAudiobook, Title: "Солярис", Artist: "Лем"}); err != nil {
		t.Fatal(err)
	}
	if q := e.idx.requests[0]; q.Kind != "audiobook" || q.Text != "Лем Солярис" || !slices.Equal(q.AltTitles, []string{"Солярис"}) {
		t.Fatalf("request %+v", q)
	}
	if _, err := e.svc.Releases(context.Background(), ReleaseRequest{Kind: KindMusic}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("no title: %v", err)
	}
}

func TestDiscoverMusicFromMusicBrainz(t *testing.T) {
	e := newEnv(t)
	e.mb.groups = []musicbrainz.ReleaseGroup{
		{ID: "mb-1", Title: "Группа крови", Artist: "Кино", Year: 1988},
		{ID: "mb-2", Title: "Звезда по имени Солнце", Artist: "Кино", Year: 1989, MatchedTrack: "Кино"},
	}
	e.lib.albums = map[string]library.Item{"Кино|Группа крови": {ID: 77, Kind: library.KindAlbum}}
	cs, err := e.svc.DiscoverMedia(context.Background(), KindMusic, "кино")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || e.mb.qs[0] != "кино" {
		t.Fatalf("candidates %+v", cs)
	}
	c := cs[0]
	if c.Kind != "music" || c.Title != "Группа крови" || c.Subtitle != "Кино" || c.Year != 1988 || c.ExtID != "mb-1" ||
		c.Poster != "https://caa/mb-1" || !c.InLibrary || c.LibraryItemID == nil || *c.LibraryItemID != 77 || c.TMDBID != 0 {
		t.Fatalf("first %+v", c)
	}
	if cs[1].InLibrary || cs[1].MatchedTrack != "Кино" || c.MatchedTrack != "" {
		t.Fatalf("second %+v", cs[1])
	}

	e.mb.err = errors.New("dial tcp: no route")
	_, err = e.svc.DiscoverMedia(context.Background(), KindMusic, "кино")
	if msg, _ := UserMessage(err); !errors.Is(err, ErrUnavailable) || msg != "Каталог недоступен — проверьте интернет" {
		t.Fatalf("offline: %v", err)
	}
	e.svc.o.MusicBrainz = nil
	if _, err := e.svc.DiscoverMedia(context.Background(), KindMusic, "кино"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("off: %v", err)
	}
	if _, err := e.svc.DiscoverMedia(context.Background(), "games", "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown kind: %v", err)
	}
}

// The albums of the artist the query names say so (byArtist) and keep the
// usual 20 other matches after them; one already on the server is marked.
func TestDiscoverMusicArtistDiscography(t *testing.T) {
	e := newEnv(t)
	for i := range 25 {
		e.mb.groups = append(e.mb.groups, musicbrainz.ReleaseGroup{ID: "d" + itoa(int64(i)), Title: "Album " + itoa(int64(i)), Artist: "Metallica", ByArtist: "Metallica"})
	}
	for i := range 30 {
		e.mb.groups = append(e.mb.groups, musicbrainz.ReleaseGroup{ID: "o" + itoa(int64(i)), Title: "Metallica", Artist: "Other " + itoa(int64(i))})
	}
	e.lib.albums = map[string]library.Item{"Metallica|Album 3": {ID: 5, Kind: library.KindAlbum}}
	cs, err := e.svc.DiscoverMedia(context.Background(), KindMusic, "metallica")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 25+discoverLimit {
		t.Fatalf("%d candidates", len(cs))
	}
	for i, c := range cs {
		if want := i < 25; want != (c.ByArtist == "Metallica") || (!want && c.ByArtist != "") {
			t.Fatalf("#%d %+v", i, c)
		}
	}
	if !cs[3].InLibrary || cs[3].LibraryItemID == nil || *cs[3].LibraryItemID != 5 || cs[4].InLibrary {
		t.Fatalf("in library %+v %+v", cs[3], cs[4])
	}
}

func TestDiscoverBooksFromOpenLibrary(t *testing.T) {
	e := newEnv(t)
	e.ol.docs = []openlibrary.Doc{{Key: "/works/OL1W", Title: "Мастер и Маргарита", Authors: []string{"Михаил Булгаков", "X"}, Year: 1967, CoverID: 5}}
	e.lib.books = map[string]library.Item{"audiobook|Михаил Булгаков|Мастер и Маргарита": {ID: 9, Kind: library.KindAudiobook}}
	for kind, inLib := range map[string]bool{KindBook: false, KindAudiobook: true} {
		cs, err := e.svc.DiscoverMedia(context.Background(), kind, "мастер")
		if err != nil {
			t.Fatal(err)
		}
		c := cs[0]
		if len(cs) != 1 || c.Kind != kind || c.Subtitle != "Михаил Булгаков" || c.ExtID != "/works/OL1W" || c.Poster != "https://covers/5" || c.Year != 1967 || c.InLibrary != inLib {
			t.Fatalf("%s: %+v", kind, cs)
		}
	}
	e.svc.o.OpenLibrary = nil
	if _, err := e.svc.DiscoverMedia(context.Background(), KindBook, "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("off: %v", err)
	}
}

type fakeWD struct {
	works []wikidata.Work
	err   error
	langs []string
}

func (f *fakeWD) SearchBooks(_ context.Context, q, lang string) ([]wikidata.Work, error) {
	f.langs = append(f.langs, lang)
	return f.works, f.err
}

// shantaram is what Wikidata answers for «Шантарам» and «Shantaram»
// (internal/metadata/wikidata/testdata), plus the TV series' novel-less
// namesake left out by the client.
var shantaram = []wikidata.Work{
	{ID: "Q2373810", TitleRU: "Шантарам", TitleEN: "Shantaram", Original: "Shantaram", AuthorsRU: []string{"Грегори Дэвид Робертс"},
		AuthorsEN: []string{"Gregory David Roberts"}, Year: 2003, OpenLibrary: "/works/OL19061442W", Sitelinks: 30},
	{ID: "Q1", TitleRU: "Шантарам: пленник", AuthorsRU: []string{"Кто-то"}, AuthorsEN: []string{"Somebody"}, Sitelinks: 50},
}

func TestDiscoverBooksRussianTitleFromWikidata(t *testing.T) {
	e := newEnv(t)
	wd := &fakeWD{works: shantaram}
	e.svc.o.Wikidata = wd
	// Open Library has no Russian edition: nothing for «Шантарам»
	e.ol.byTitle = map[string][]openlibrary.Doc{"Shantaram|Gregory David Roberts": {
		{Key: "/works/OL5730814W", Title: "Shantaram", CoverID: 1},
		{Key: "/works/OL19061442W", Title: "Shantaram", CoverID: 7, Year: 2003},
	}}
	cs, err := e.svc.DiscoverMedia(context.Background(), KindBook, "Шантарам")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("%+v", cs)
	}
	c := cs[0] // the exact title first, before a more known namesake
	if c.Title != "Шантарам" || c.Subtitle != "Грегори Дэвид Робертс" || c.ExtID != "/works/OL19061442W" || c.Year != 2003 ||
		c.Poster != "https://covers/7" || !slices.Equal(c.Aliases, []string{"Shantaram"}) {
		t.Fatalf("%+v", c)
	}
	if cs[1].ExtID != "wikidata:Q1" || wd.langs[0] != "ru" {
		t.Fatalf("%+v %v", cs[1], wd.langs)
	}
}

func TestDiscoverBooksEnglishTitleGetsTheRussianAlias(t *testing.T) {
	e := newEnv(t)
	e.svc.o.Wikidata = &fakeWD{works: shantaram[:1]}
	e.ol.docs = []openlibrary.Doc{{Key: "/works/OL19061442W", Title: "Shantaram", Authors: []string{"Gregory David Roberts"}, CoverID: 7, Year: 2003},
		{Key: "/works/OL5730814W", Title: "Shantaram", Authors: []string{"Gregory David Roberts"}}}
	cs, err := e.svc.DiscoverMedia(context.Background(), KindAudiobook, "Shantaram")
	if err != nil {
		t.Fatal(err)
	}
	// the second Open Library work of the same book is one candidate
	if len(cs) != 1 || cs[0].Title != "Shantaram" || !slices.Equal(cs[0].Aliases, []string{"Шантарам"}) || cs[0].Poster != "https://covers/7" {
		t.Fatalf("%+v", cs)
	}
}

func TestDiscoverBooksOneCatalogDown(t *testing.T) {
	e := newEnv(t)
	e.svc.o.Wikidata = &fakeWD{err: errors.New("down")}
	e.ol.docs = []openlibrary.Doc{{Key: "/works/OL1W", Title: "Dune", Authors: []string{"Frank Herbert"}}}
	if cs, err := e.svc.DiscoverMedia(context.Background(), KindBook, "dune"); err != nil || len(cs) != 1 {
		t.Fatalf("%+v %v", cs, err)
	}
	e.ol.err = errors.New("down")
	if _, err := e.svc.DiscoverMedia(context.Background(), KindBook, "dune"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("both down: %v", err)
	}
	e.ol.err, e.svc.o.Wikidata = errors.New("down"), &fakeWD{works: shantaram[:1]}
	if cs, err := e.svc.DiscoverMedia(context.Background(), KindBook, "Шантарам"); err != nil || len(cs) != 1 {
		t.Fatalf("Wikidata alone: %+v %v", cs, err)
	}
}

func TestBookReleasesSearchBothTitles(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Releases(context.Background(), ReleaseRequest{Kind: KindBook, Title: "Шантарам", Artist: "Грегори Дэвид Робертс", Aliases: []string{"Shantaram"}}); err != nil {
		t.Fatal(err)
	}
	sr := e.idx.requests[len(e.idx.requests)-1]
	if !slices.Contains(sr.AltTitles, "Shantaram") || !slices.Contains(sr.AltTitles, "Шантарам") {
		t.Fatalf("%+v", sr)
	}
	want := indexer.Wanted{Kind: sr.Kind, Titles: append([]string{sr.Text}, sr.AltTitles...), Artist: sr.Artist, Work: sr.Work, Aliases: sr.Aliases}
	for title, ok := range map[string]bool{
		"Робертс Грегори Дэвид - Шантарам [2010, FB2, RUS]":  true,
		"Gregory David Roberts - Shantaram (2003) [EPUB]":    true,
		"Roberts G.D. - Shantaram: A Novel [ENG, EPUB]":      true,
		"Шантарам (сериал) Робертс":                          true,
		"Иванов - Шантарам [FB2]":                            false,
		"Gregory David Roberts - The Mountain Shadow [EPUB]": false,
	} {
		s, why := indexer.Score(indexer.Result{Title: title, Seeders: 3}, release.Parse(title), want, indexer.DefaultPrefs())
		if (s != indexer.Excluded) != ok {
			t.Errorf("%q: %d %v", title, s, why)
		}
	}
}

func TestDiscoverComicPlainQuery(t *testing.T) {
	e := newEnv(t)
	e.svc.o.MusicBrainz, e.svc.o.OpenLibrary = nil, nil // no catalog needed
	cs, err := e.svc.DiscoverMedia(context.Background(), KindComic, "  Берсерк ")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Kind != "comic" || cs[0].Title != "Берсерк" || cs[0].InLibrary {
		t.Fatalf("candidates %+v", cs)
	}
	if cs, err := e.svc.DiscoverMedia(context.Background(), KindComic, " "); err != nil || len(cs) != 0 {
		t.Fatalf("empty: %+v %v", cs, err)
	}
}

func TestMediaKindsDownloadDirs(t *testing.T) {
	s := &Service{o: Options{TorrentsDir: "/t"}}
	for kind, dir := range map[string]string{KindMusic: "music", KindBook: "books", KindAudiobook: "audiobooks", KindComic: "comics", KindMovie: "movies", KindSeries: "series"} {
		if got := s.downloadDir(kind); got != filepath.Join("/t", dir) {
			t.Errorf("downloadDir(%s) = %s", kind, got)
		}
	}
}

func TestOrganizeBindsComicPageFolders(t *testing.T) {
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Akira [RUS] (JPG)", "https://t/1", 1, 5)}}
	e.eng.nextFiles = torrentFiles(t, filepath.Join(e.root, "torrents", "comics"),
		"Akira/Vol 1/001.jpg", "Akira/Vol 1/002.jpg", "Akira/Vol 2/001.jpg", "Akira/v03.cbz", "Akira/cover.jpg")
	series := filepath.Join(e.root, "media/books/manga", "Akira")
	// the scan made rows for the volumes: two folders and an archive
	e.lib.only = map[string]bool{filepath.Join(series, "Vol 1"): true, filepath.Join(series, "Vol 2"): true, filepath.Join(series, "v03.cbz"): true}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{Kind: KindComic, Title: "Akira", Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	if a, _ := e.svc.acquisition(context.Background(), res.InfoHash); !a.OrganizedAt.Valid || a.Error != "" {
		t.Fatalf("acquisition %+v", a)
	}
	if len(e.lib.bound) != 3 {
		t.Fatalf("bound %v", e.lib.bound)
	}
}

func TestBrokenMetaJSONDoesNotHideTorrents(t *testing.T) {
	e := newEnv(t)
	e.eng.put(torrent.Status{InfoHash: "dddddddddddddddddddddddddddddddddddddddd", Mode: ModeDownload})
	if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, meta_json) VALUES ('dddddddddddddddddddddddddddddddddddddddd','music',0,'X','{broken')`); err != nil {
		t.Fatal(err)
	}
	acqs, err := e.svc.acquisitions(context.Background())
	if err != nil || len(acqs) != 1 {
		t.Fatalf("%v %v", acqs, err)
	}
}
