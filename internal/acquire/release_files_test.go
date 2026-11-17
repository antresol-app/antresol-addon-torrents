package acquire

import (
	"context"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// testMetafile builds a v1 metafile named name holding paths (relative to
// the torrent's folder), each 1000 bytes.
func testMetafile(t *testing.T, name string, paths ...string) []byte {
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

func metafileIH(t *testing.T, mf []byte) string {
	t.Helper()
	ih, _, _, err := torrent.ParseFiles(mf)
	if err != nil {
		t.Fatal(err)
	}
	return ih
}

var groupaKrovi = ReleaseFilesRequest{SourceID: 3, ReleaseLink: "https://t/1", Title: "Группа крови", Artist: "Кино", Year: 1988}

func previewFiles(t *testing.T, e *env, req ReleaseFilesRequest) ReleaseFiles {
	t.Helper()
	res, err := e.svc.ReleaseFiles(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func fileByPath(t *testing.T, res ReleaseFiles, p string) ReleaseFile {
	t.Helper()
	for _, f := range res.Files {
		if f.Path == p {
			return f
		}
	}
	t.Fatalf("no file %q in %+v", p, res.Files)
	return ReleaseFile{}
}

func TestReleaseFilesTorrentFile(t *testing.T) {
	e := newEnv(t)
	mf := testMetafile(t, "Кино - Группа крови (1988)",
		"01 Группа крови.flac", "02 Закрой за мной дверь.flac", "03 Война.flac",
		"cover.jpg", "album.cue", "album.log", "._01.flac")
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	res := previewFiles(t, e, groupaKrovi)
	if res.Added || res.Message != "" || res.InfoHash != metafileIH(t, mf) || res.Name != "Кино - Группа крови (1988)" {
		t.Fatalf("%+v", res)
	}
	if len(res.Files) != 6 {
		t.Fatalf("files %+v", res.Files)
	}
	for i, f := range res.Files {
		if i > 0 && res.Files[i-1].Path >= f.Path {
			t.Errorf("not sorted by path: %q, %q", res.Files[i-1].Path, f.Path)
		}
		want := "extra"
		if strings.HasSuffix(f.Path, ".flac") {
			want = "track"
		}
		if f.Kind != want || !f.Checked || f.Album != "Группа крови (1988)" || f.Size != 1000 {
			t.Errorf("file %+v", f)
		}
		if strings.Contains(f.Path, "._") {
			t.Errorf("junk listed: %+v", f)
		}
	}
	// indexes are the engine's (junk at index 6 left out, not renumbered)
	if f := fileByPath(t, res, "Кино - Группа крови (1988)/03 Война.flac"); f.Index != 2 {
		t.Errorf("index %+v", f)
	}
	if f := fileByPath(t, res, "Кино - Группа крови (1988)/album.log"); f.Index != 5 {
		t.Errorf("index %+v", f)
	}
}

func TestReleaseFilesSkipDirAudioIsExtra(t *testing.T) {
	e := newEnv(t)
	mf := testMetafile(t, "Кино - Группа крови (1988)", "01 Группа крови.flac", "Scans/interview.flac")
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	res := previewFiles(t, e, groupaKrovi)
	if f := fileByPath(t, res, "Кино - Группа крови (1988)/Scans/interview.flac"); f.Kind != "extra" || !f.Checked || f.Album != "Группа крови (1988)" {
		t.Fatalf("%+v", f)
	}
	if f := fileByPath(t, res, "Кино - Группа крови (1988)/01 Группа крови.flac"); f.Kind != "track" {
		t.Fatalf("%+v", f)
	}
}

func TestReleaseFilesDiscography(t *testing.T) {
	e := newEnv(t)
	mf := testMetafile(t, "Кино - Дискография",
		"1988 - Группа крови/CD1/01 Группа крови.flac",
		"1988 - Группа крови/CD2/01 Бонус.flac",
		"1988 - Группа крови/cover.jpg",
		"1989 - Звезда по имени Солнце/01 Звезда по имени Солнце.flac",
		"1989 - Звезда по имени Солнце/folder.jpg",
		"info.txt")
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	res := previewFiles(t, e, groupaKrovi)
	if len(res.Files) != 6 {
		t.Fatalf("%+v", res.Files)
	}
	for _, f := range res.Files {
		requested := strings.Contains(f.Path, "Группа крови") || strings.HasSuffix(f.Path, "info.txt")
		want := "Звезда по имени Солнце (1989)"
		if requested {
			want = "Группа крови (1988)"
		}
		if f.Checked != requested || f.Album != want {
			t.Errorf("file %+v", f)
		}
	}
}

func TestReleaseFilesReissueFolderIsRequested(t *testing.T) {
	files := []torrent.FileEntry{
		{Index: 0, Path: "Кино/1988 - Группа крови (Remastered 2012)/01.flac"},
		{Index: 1, Path: "Кино/1989 - Звезда по имени Солнце/01.flac"},
		{Index: 2, Path: "Кино/Группа крови [Deluxe Edition]/01.flac"},
	}
	if got := requestedRoot(files[:2], "Группа крови", nil); got != "Кино/1988 - Группа крови (Remastered 2012)" {
		t.Fatalf("requested root %q", got)
	}
	if got := requestedRoot(files[1:], "Группа крови", nil); got != "Кино/Группа крови [Deluxe Edition]" {
		t.Fatalf("requested root %q", got)
	}
	if got := requestedRoot(files[1:2], "Blood Type", []string{"Звезда по имени Солнце"}); got != "Кино/1989 - Звезда по имени Солнце" {
		t.Fatalf("alias: %q", got)
	}
	if got := requestedRoot(files, "Чёрный альбом", nil); got != "" {
		t.Fatalf("no match: %q", got)
	}

	e := newEnv(t)
	mf := testMetafile(t, "Кино", "1988 - Группа крови (Remastered 2012)/01.flac", "1989 - Звезда по имени Солнце/01.flac")
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	res := previewFiles(t, e, groupaKrovi)
	if f := fileByPath(t, res, "Кино/1988 - Группа крови (Remastered 2012)/01.flac"); !f.Checked || f.Album != "Группа крови (1988)" {
		t.Fatalf("%+v", f)
	}
	if f := fileByPath(t, res, "Кино/1989 - Звезда по имени Солнце/01.flac"); f.Checked {
		t.Fatalf("%+v", f)
	}
}

func TestReleaseFilesNoRootMatchesAllChecked(t *testing.T) {
	e := newEnv(t)
	mf := testMetafile(t, "Кино", "1986 - Ночь/01.flac", "1989 - Звезда по имени Солнце/01.flac")
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	res := previewFiles(t, e, groupaKrovi)
	for _, f := range res.Files {
		if !f.Checked {
			t.Errorf("%+v", f)
		}
	}
	if f := fileByPath(t, res, "Кино/1986 - Ночь/01.flac"); f.Album != "Ночь (1986)" {
		t.Errorf("%+v", f)
	}
}

func TestReleaseFileKind(t *testing.T) {
	for _, c := range []struct {
		f    torrent.FileEntry
		want string
	}{
		{torrent.FileEntry{Path: "A/01.flac"}, "track"},
		{torrent.FileEntry{Path: "A/Scans/01.flac"}, "extra"},
		{torrent.FileEntry{Path: "A/cover.jpg"}, "extra"},
		{torrent.FileEntry{Path: "A/._01.flac"}, ""},
		{torrent.FileEntry{Path: "A/__MACOSX/01.flac"}, ""},
		{torrent.FileEntry{Path: "A/.DS_Store"}, ""},
		{torrent.FileEntry{Path: "A/.pad/0", Padding: true}, ""},
	} {
		if got := releaseFileKind(c.f); got != c.want {
			t.Errorf("%q: %q, want %q", c.f.Path, got, c.want)
		}
	}
}

func TestReleaseFilesMagnetTimeout(t *testing.T) {
	e := newEnv(t)
	e.eng.fetchInfo = func(context.Context, string) ([]byte, error) { return nil, torrent.ErrNoInfo }
	req := groupaKrovi
	req.ReleaseLink = "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)
	_, err := e.svc.ReleaseFiles(context.Background(), req)
	if !errors.Is(err, ErrNoFiles) {
		t.Fatalf("%v", err)
	}
	// a timeout says what happened, in plain words
	if msg, _ := UserMessage(err); msg != msgFileListTimeout {
		t.Fatalf("%q", msg)
	}
	if e.eng.fetches != 1 || len(e.idx.downloads) != 0 {
		t.Fatalf("fetches %d downloads %v", e.eng.fetches, e.idx.downloads)
	}
}

func TestReleaseFilesMagnetDeadline(t *testing.T) {
	e := newEnv(t)
	var hasDeadline bool
	e.eng.fetchInfo = func(ctx context.Context, _ string) ([]byte, error) {
		_, hasDeadline = ctx.Deadline()
		return nil, torrent.ErrNoInfo
	}
	req := groupaKrovi
	req.ReleaseLink = "magnet:?xt=urn:btih:" + strings.Repeat("ab", 20)
	e.svc.ReleaseFiles(context.Background(), req)
	if !hasDeadline {
		t.Fatal("FetchInfo without a deadline")
	}
	if releaseFilesTimeout.Seconds() != 40 {
		t.Fatalf("timeout %v", releaseFilesTimeout)
	}
}

func TestReleaseFilesMagnet(t *testing.T) {
	e := newEnv(t)
	mf := testMetafile(t, "Кино - Группа крови (1988)", "01.flac", "cover.jpg")
	ih := metafileIH(t, mf)
	var got string
	e.eng.fetchInfo = func(_ context.Context, m string) ([]byte, error) { got = m; return mf, nil }
	// the indexer answers with a magnet
	e.idx.dl = &cardigann.DownloadResult{Magnet: "magnet:?xt=urn:btih:" + ih}
	res := previewFiles(t, e, groupaKrovi)
	if got != "magnet:?xt=urn:btih:"+ih || res.InfoHash != ih || len(res.Files) != 2 || res.Added {
		t.Fatalf("%q %+v", got, res)
	}
}

func TestReleaseFilesAlreadyAddedNoFetch(t *testing.T) {
	e := newEnv(t)
	raw := make([]byte, 20)
	for i := range raw {
		raw[i] = byte(i * 7)
	}
	ih := hex.EncodeToString(raw)
	e.eng.put(torrent.Status{InfoHash: ih, Name: "Кино - Группа крови", Status: torrent.StatusDownloading})
	e.eng.fetchInfo = func(context.Context, string) ([]byte, error) {
		t.Error("FetchInfo called")
		return nil, torrent.ErrNoInfo
	}
	const msg = "Эта раздача уже добавлена. Чтобы выбрать другие песни, удалите её в «Загрузках» и добавьте снова."
	for _, m := range []string{
		"magnet:?xt=urn:btih:" + ih,
		"magnet:?xt=urn:btih:" + strings.ToUpper(ih),
		"magnet:?xt=urn:btih:" + base32.StdEncoding.EncodeToString(raw),
	} {
		req := groupaKrovi
		req.ReleaseLink = m
		res := previewFiles(t, e, req)
		if !res.Added || res.Message != msg || res.InfoHash != ih || res.Files == nil || len(res.Files) != 0 || res.Name != "Кино - Группа крови" {
			t.Errorf("%s: %+v", m, res)
		}
	}
	// a .torrent of a torrent the engine has: no parse needed either
	mf := testMetafile(t, "Кино", "01.flac")
	e.eng.put(torrent.Status{InfoHash: metafileIH(t, mf), Name: "Кино"})
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	if res := previewFiles(t, e, groupaKrovi); !res.Added || res.Message != msg || len(res.Files) != 0 {
		t.Errorf("metafile: %+v", res)
	}
	// FetchInfo racing an add answers ErrKnown: the same answer
	e2 := newEnv(t)
	e2.eng.fetchInfo = func(context.Context, string) ([]byte, error) { return nil, torrent.ErrKnown }
	req := groupaKrovi
	req.ReleaseLink = "magnet:?xt=urn:btih:" + ih
	if res := previewFiles(t, e2, req); !res.Added || res.Message != msg || res.InfoHash != ih {
		t.Errorf("ErrKnown: %+v", res)
	}
}

func TestReleaseFilesDirect(t *testing.T) {
	e := newEnv(t)
	req := groupaKrovi
	req.Direct = true
	_, err := e.svc.ReleaseFiles(context.Background(), req)
	if msg, _ := UserMessage(err); !errors.Is(err, ErrInvalid) || msg != "У этой раздачи нет списка файлов" {
		t.Fatalf("%v", err)
	}
	if len(e.idx.downloads) != 0 {
		t.Fatalf("downloads %v", e.idx.downloads)
	}
}

func TestReleaseFilesNoMetafile(t *testing.T) {
	for name, dl := range map[string]cardigann.DownloadResult{
		"nothing": {},
		"garbage": {Torrent: []byte("not a torrent")},
	} {
		e := newEnv(t)
		e.idx.dl = &dl
		_, err := e.svc.ReleaseFiles(context.Background(), groupaKrovi)
		if msg, _ := UserMessage(err); !errors.Is(err, ErrInvalid) || msg != "У этой раздачи нет списка файлов" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestReleaseFilesCached(t *testing.T) {
	e := newEnv(t)
	mf := testMetafile(t, "Кино - Группа крови (1988)", "01.flac")
	e.eng.fetchInfo = func(context.Context, string) ([]byte, error) { return mf, nil }
	e.idx.dl = &cardigann.DownloadResult{Magnet: "magnet:?xt=urn:btih:" + metafileIH(t, mf)}
	a := previewFiles(t, e, groupaKrovi)
	b := previewFiles(t, e, groupaKrovi)
	if len(e.idx.downloads) != 1 || e.eng.fetches != 1 {
		t.Fatalf("downloads %v fetches %d", e.idx.downloads, e.eng.fetches)
	}
	if a.InfoHash != b.InfoHash || len(b.Files) != 1 {
		t.Fatalf("%+v %+v", a, b)
	}
	// another source of the same link is another release
	other := groupaKrovi
	other.SourceID = 4
	previewFiles(t, e, other)
	if len(e.idx.downloads) != 2 {
		t.Fatalf("downloads %v", e.idx.downloads)
	}
}

func TestReleaseFilesCacheExpiresAndBounded(t *testing.T) {
	var c metafileCache
	now := time.Now()
	for i := range releaseFilesCacheMax + 5 {
		c.put(cacheKey(int64(i), "l"), cachedMetafile{infoHash: "x"}, now)
	}
	if len(c.m) > releaseFilesCacheMax {
		t.Fatalf("%d entries", len(c.m))
	}
	c.put("k", cachedMetafile{infoHash: "y"}, now)
	if _, ok := c.get("k", now.Add(releaseFilesTTL-1)); !ok {
		t.Fatal("expired early")
	}
	if _, ok := c.get("k", now.Add(releaseFilesTTL+1)); ok {
		t.Fatal("not expired")
	}
}

func TestReleaseFilesSongMarked(t *testing.T) {
	e := newEnv(t)
	mf := testMetafile(t, "Кино - Группа крови (1988)",
		"01 Группа крови.flac", "02 Закрой за мной дверь.flac", "1-03 Кино - Война.flac", "cover.jpg")
	e.idx.dl = &cardigann.DownloadResult{Torrent: mf}
	for _, song := range []string{"Война", "война", "Кино Война", "Кино - Война", "Война (Remastered 2012)"} {
		req := groupaKrovi
		req.Song = song
		res := previewFiles(t, e, req)
		for _, f := range res.Files {
			want := strings.HasSuffix(f.Path, "Война.flac")
			if f.Matched != want {
				t.Errorf("song %q: %+v", song, f)
			}
		}
	}
	res := previewFiles(t, e, groupaKrovi) // no song: none marked
	for _, f := range res.Files {
		if f.Matched {
			t.Errorf("no song: %+v", f)
		}
	}
	req := groupaKrovi
	req.Song = "Звезда по имени Солнце"
	for _, f := range previewFiles(t, e, req).Files {
		if f.Matched {
			t.Errorf("another song: %+v", f)
		}
	}
	// track numbers (and discs) from the file names; none for extras
	if f := fileByPath(t, res, "Кино - Группа крови (1988)/02 Закрой за мной дверь.flac"); f.No != 2 || f.Disc != 0 {
		t.Errorf("%+v", f)
	}
	if f := fileByPath(t, res, "Кино - Группа крови (1988)/1-03 Кино - Война.flac"); f.No != 3 || f.Disc != 1 {
		t.Errorf("%+v", f)
	}
	if f := fileByPath(t, res, "Кино - Группа крови (1988)/cover.jpg"); f.No != 0 {
		t.Errorf("%+v", f)
	}
}
