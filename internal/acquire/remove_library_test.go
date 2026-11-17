package acquire

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
	"github.com/antresol-app/antresol-addon-torrents/internal/organize"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"golang.org/x/text/unicode/norm"
)

// realMusic is a real local-only torrent engine, the library (the core's,
// in memory: scan stands in for its scanner), and the service, for a music
// download's whole life.
type realMusic struct {
	svc      *Service
	eng      *torrent.Engine
	repo     *coretest.Library
	root     string
	musicDir string
}

func newRealMusic(t *testing.T) *realMusic {
	t.Helper()
	root := t.TempDir()
	d, err := db.Open(filepath.Join(root, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	eng, err := torrent.New(torrent.EngineConfig{DataDir: filepath.Join(root, "data"), StreamDir: filepath.Join(root, "stream"), LocalOnly: true}, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	m := &realMusic{eng: eng, repo: &coretest.Library{}, root: root, musicDir: filepath.Join(root, "media/music")}
	if err := os.MkdirAll(filepath.Join(root, "media/movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(m.musicDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m.svc = New(Options{DB: d, Engine: eng, Indexer: &fakeIndexer{}, Metadata: fakeMeta{}, Library: m.repo,
		TorrentsDir: filepath.Join(root, "torrents"), MusicDir: m.musicDir, Disk: &Disk{Root: root}})
	t.Cleanup(m.svc.Close)
	return m
}

// scan stands in for the core's scan: the library's files are the music
// and video files under its roots.
func (m *realMusic) scan(t *testing.T) {
	t.Helper()
	var files []library.File
	for _, root := range []string{m.musicDir, filepath.Join(m.root, "media/movies")} {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && (library.IsMusicFile(p) || organize.IsVideo(p)) {
				files = append(files, library.File{ID: int64(len(files) + 1), Path: p})
			}
			return nil
		})
	}
	m.repo.Files = files
}

// libraryFiles is the library's files under dir.
func (m *realMusic) libraryFiles(dir string) []string {
	var out []string
	for _, f := range m.repo.Files {
		if strings.HasPrefix(f.Path, dir+string(filepath.Separator)) {
			out = append(out, f.Path)
		}
	}
	return out
}

// setPath renames the library's file at old (as a scan stored it).
func (m *realMusic) setPath(old, new string) {
	for i := range m.repo.Files {
		if m.repo.Files[i].Path == old {
			m.repo.Files[i].Path = new
		}
	}
}

// album writes an album of n tracks as a torrent's data under
// TorrentsDir/music and returns its metafile and infohash.
func (m *realMusic) album(t *testing.T, name string, n int) ([]byte, string) {
	t.Helper()
	dir := filepath.Join(m.root, "torrents", "music", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		b := make([]byte, 40<<10)
		rand.Read(b)
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d. Песня %d.mp3", i, i)), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: ib}
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), mi.HashInfoBytes().HexString()
}

// organized waits for the acquisition of ih to be put into the library.
func (m *realMusic) organized(t *testing.T, ih string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var at, errText string
		m.svc.o.DB.QueryRow(`SELECT COALESCE(organized_at,''), organize_error FROM acquisitions WHERE infohash=?`, ih).Scan(&at, &errText)
		if errText != "" {
			t.Fatalf("organize: %s", errText)
		}
		if at != "" && !m.svc.Organizing(ih) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("not organized")
}

func (m *realMusic) tracksOnDisk(t *testing.T) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(m.musicDir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".mp3" {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// «Загрузки» → delete with «Удалить файлы с диска» of a music download of
// which one track of 28 was picked: the torrent's own data goes, the
// library's copy of that track stays (and so the album with it).
func TestRemoveDownloadWithDataKeepsLibraryTrack(t *testing.T) {
	ctx := context.Background()
	m := newRealMusic(t)
	meta, ih := m.album(t, "Артист - Альбом (2001)", 28)
	if _, err := m.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, year, added_by, meta_json)
		VALUES (?, 'music', 0, 'Альбом', 2001, 1, '{"artist":"Артист","picked":true}')`, ih); err != nil {
		t.Fatal(err)
	}
	if _, err := m.eng.AddTorrentFile(ctx, meta, torrent.AddOptions{Mode: torrent.ModeDownload,
		SaveDir: filepath.Join(m.root, "torrents", "music"), AddedBy: 1, Files: []int{4}}); err != nil {
		t.Fatal(err)
	}
	m.organized(t, ih)
	placed := m.tracksOnDisk(t)
	if len(placed) != 1 {
		t.Fatalf("library files after organize: %v", placed)
	}
	m.scan(t)
	if albums := m.libraryFiles(m.musicDir); len(albums) != 1 {
		t.Fatalf("albums %v", albums)
	}

	if err := m.svc.Remove(ctx, ih, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.root, "torrents", "music", "Артист - Альбом (2001)")); !os.IsNotExist(err) {
		t.Fatalf("the torrent's data is still there: %v", err)
	}
	if got := m.tracksOnDisk(t); len(got) != 1 {
		t.Fatalf("library files after the download was deleted with its data: %v", got)
	}
	m.scan(t)
	if albums := m.libraryFiles(m.musicDir); len(albums) != 1 {
		t.Fatalf("albums after the delete: %v", albums)
	}
}

// A torrent whose data is the library's own files (seeded where they are,
// as a moved-over client's torrents or a download into the library's
// folder): deleting it «с файлами» takes the torrent's other files, never
// a file the library has — before, the whole folder went and the album was
// left pointing at a missing file.
func TestRemoveDownloadWithDataKeepsInPlaceLibraryFiles(t *testing.T) {
	ctx := context.Background()
	m := newRealMusic(t)
	const name = "Артист - Альбом (2001)"
	dir := filepath.Join(m.musicDir, name)
	if err := os.MkdirAll(filepath.Join(dir, "Scans"), 0o755); err != nil {
		t.Fatal(err)
	}
	var files []string
	for i := 1; i <= 28; i++ {
		files = append(files, fmt.Sprintf("%02d. Песня %d.mp3", i, i))
	}
	files = append(files, "Scans/cover.jpg", "album.log")
	for _, f := range files {
		b := make([]byte, 32<<10) // whole pieces: no piece spans two files
		rand.Read(b)
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	ib, _ := bencode.Marshal(info)
	mi := metainfo.MetaInfo{InfoBytes: ib}
	var buf bytes.Buffer
	mi.Write(&buf)
	ih := mi.HashInfoBytes().HexString()
	// the library has one song of it (the others never were its: the
	// download picked one, the rest is the torrent's)
	for _, f := range files {
		if f != "05. Песня 5.mp3" {
			os.Remove(filepath.Join(dir, f))
		}
	}
	os.WriteFile(filepath.Join(dir, "Scans", "cover.jpg"), []byte("torrent's picture"), 0o644)
	if _, err := m.eng.AddTorrentFile(ctx, buf.Bytes(), torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: m.musicDir}); err != nil {
		t.Fatal(err)
	}
	m.scan(t)
	if albums := m.libraryFiles(m.musicDir); len(albums) != 1 {
		t.Fatalf("albums %v", albums)
	}
	if err := m.svc.Remove(ctx, ih, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "05. Песня 5.mp3")); err != nil {
		t.Fatalf("the library's song went with the torrent's data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Scans")); !os.IsNotExist(err) {
		t.Fatalf("the torrent's own files are still there: %v", err)
	}
	if _, ok := m.eng.Get(ih); ok {
		t.Fatal("the torrent is still in the engine")
	}
	m.scan(t)
	if albums := m.libraryFiles(m.musicDir); len(albums) != 1 {
		t.Fatalf("albums after the delete: %v", albums)
	}
}

// inPlace makes a torrent of an album folder under the library's music
// root of which the library has one song, seeded in place from saveDir
// (the music root, or another spelling of it), scans the library and
// returns the infohash and the song's path.
func (m *realMusic) inPlace(t *testing.T, saveDir string) (string, string) {
	t.Helper()
	ctx := context.Background()
	const name = "Артист - Ёлки (2001)"
	dir := filepath.Join(m.musicDir, name)
	if err := os.MkdirAll(filepath.Join(dir, "Scans"), 0o755); err != nil {
		t.Fatal(err)
	}
	var files []string
	for i := 1; i <= 6; i++ {
		files = append(files, fmt.Sprintf("%02d. Пой %d.mp3", i, i))
	}
	files = append(files, "Scans/cover.jpg")
	for _, f := range files {
		b := make([]byte, 32<<10)
		rand.Read(b)
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	ib, _ := bencode.Marshal(info)
	mi := metainfo.MetaInfo{InfoBytes: ib}
	var buf bytes.Buffer
	mi.Write(&buf)
	song := filepath.Join(dir, "05. Пой 5.mp3")
	for _, f := range files {
		if filepath.Join(dir, f) != song {
			os.Remove(filepath.Join(dir, f))
		}
	}
	os.WriteFile(filepath.Join(dir, "Scans", "cover.jpg"), []byte("torrent's picture"), 0o644)
	if _, err := m.eng.AddTorrentFile(ctx, buf.Bytes(), torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: saveDir}); err != nil {
		t.Fatal(err)
	}
	m.scan(t)
	if fs := m.libraryFiles(dir); len(fs) != 1 {
		t.Fatalf("library files %v", fs)
	}
	return mi.HashInfoBytes().HexString(), song
}

func (m *realMusic) removedKeeping(t *testing.T, ih, song string) {
	t.Helper()
	if err := m.svc.Remove(context.Background(), ih, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(song); err != nil {
		t.Fatalf("the library's song went with the torrent's data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(song), "Scans")); !os.IsNotExist(err) {
		t.Fatalf("the torrent's own files are still there: %v", err)
	}
}

// A film seeded in place from the library's folder, with what the library
// put beside it later (a voice-over of «Найти озвучку…», subtitles, the
// .nfo): «удалить с файлами» takes the torrent's own other files only; the
// video and every sidecar stay.
func TestRemoveDownloadWithDataKeepsSidecars(t *testing.T) {
	ctx := context.Background()
	m := newRealMusic(t)
	movies := filepath.Join(m.root, "media/movies")
	dir := filepath.Join(movies, "Брат (1997)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	video := filepath.Join(dir, "Брат (1997).mkv")
	own := filepath.Join(dir, "release.txt") // the torrent's, not the library's
	for _, p := range []string{video, own} {
		b := make([]byte, 32<<10)
		rand.Read(b)
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(dir); err != nil {
		t.Fatal(err)
	}
	ib, _ := bencode.Marshal(info)
	mi := metainfo.MetaInfo{InfoBytes: ib}
	var buf bytes.Buffer
	mi.Write(&buf)
	ih := mi.HashInfoBytes().HexString()
	if _, err := m.eng.AddTorrentFile(ctx, buf.Bytes(), torrent.AddOptions{Mode: torrent.ModeSeed, SaveDir: movies}); err != nil {
		t.Fatal(err)
	}
	sidecars := []string{filepath.Join(dir, "Брат (1997).rus.mka"), filepath.Join(dir, "Брат (1997).ru.srt"), filepath.Join(dir, "Брат (1997).nfo")}
	for _, p := range sidecars {
		if err := os.WriteFile(p, []byte("the library's"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m.scan(t)
	if fs := m.libraryFiles(dir); !slices.Equal(fs, []string{video}) {
		t.Fatalf("the video is no library file: %v", fs)
	}
	if err := m.svc.Remove(ctx, ih, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range append([]string{video}, sidecars...) {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s went with the torrent's data: %v", filepath.Base(p), err)
		}
	}
	if _, err := os.Stat(own); !os.IsNotExist(err) {
		t.Fatalf("the torrent's own file is still there: %v", err)
	}
}

// The torrent reaches the library's folder by another spelling (through a
// symlink): its file is still the library's song.
func TestRemoveDownloadWithDataKeepsLibraryFileOtherSpelling(t *testing.T) {
	m := newRealMusic(t)
	link := filepath.Join(m.root, "music-link")
	if err := os.Symlink(m.musicDir, link); err != nil {
		t.Skip(err)
	}
	ih, song := m.inPlace(t, link)
	m.removedKeeping(t, ih, song)
}

// The library stored the song's path in another Unicode form (NFD, as a
// tree made on macOS has it): the same song, kept.
func TestRemoveDownloadWithDataKeepsLibraryFileOtherNormalisation(t *testing.T) {
	m := newRealMusic(t)
	ih, song := m.inPlace(t, m.musicDir)
	m.setPath(song, norm.NFD.String(song))
	m.removedKeeping(t, ih, song)
}

// The music import's way: one song of 28 joins an album the library has
// (its folder, OrganizeJob.AlbumDir).
func TestRemoveImportDownloadWithDataKeepsLibraryTrack(t *testing.T) {
	ctx := context.Background()
	m := newRealMusic(t)
	albumDir := filepath.Join(m.musicDir, "Артист", "Альбом (2001)")
	if err := os.MkdirAll(albumDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		os.WriteFile(filepath.Join(albumDir, fmt.Sprintf("%02d. Песня %d.mp3", i, i)), []byte(fmt.Sprint("song", i)), 0o644)
	}
	m.scan(t)
	meta, ih := m.album(t, "Артист - Альбом (2001) [FLAC]", 28)
	if _, err := m.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, year, added_by, meta_json)
		VALUES (?, 'music', 0, 'Альбом', 2001, 1, json_object('artist','Артист','albumDir',?))`, ih, albumDir); err != nil {
		t.Fatal(err)
	}
	if _, err := m.eng.AddTorrentFile(ctx, meta, torrent.AddOptions{Mode: torrent.ModeDownload,
		SaveDir: filepath.Join(m.root, "torrents", "music"), AddedBy: 1, Files: []int{6}}); err != nil {
		t.Fatal(err)
	}
	m.organized(t, ih)
	if got := m.tracksOnDisk(t); len(got) != 3 {
		t.Fatalf("library files after organize: %v", got)
	}
	if err := m.svc.Remove(ctx, ih, true); err != nil {
		t.Fatal(err)
	}
	if got := m.tracksOnDisk(t); len(got) != 3 {
		t.Fatalf("library files after the download was deleted with its data: %v", got)
	}
}
