package main

// The releases mode: the fake site's releases for the e2e, with
// real payloads, so a download completes with nobody else around — the
// engine finds the data already in its download dir, verifies it and
// acquire organizes it.
//
//	go run ./tools/mkfixture -releases DIR -metafiles DIR
//
// writes each release's payload under the -releases dir (a single file
// or a folder named as the release), builds its metafile from that payload
// into the -metafiles dir as <infohash>.torrent and prints the manifest
// (JSON) to stdout. The payloads are made with ffmpeg in bit-exact mode
// and the book is a zip with fixed times, so the same ffmpeg gives the
// same bytes and the same infohashes on every run (the e2e caches them).

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// releasePieceLength is the metafiles' piece length (the payloads are a
// few hundred KiB each).
const releasePieceLength = 64 << 10

// Release is one release of the manifest.
type Release struct {
	Key      string        `json:"key"`  // movie | watch | series | voiceover | album | book | hold | broken
	Kind     string        `json:"kind"` // movie | series | album | book
	Name     string        `json:"name"` // the metafile's name: the file or folder under the download dir
	Title    string        `json:"title"`
	Year     int           `json:"year"`
	Infohash string        `json:"infohash"`
	Size     int64         `json:"size"`
	Files    []ReleaseFile `json:"files"` // relative to the release's folder (a single file: its own name)
	Metafile string        `json:"metafile"`
}

// ReleaseFile is one file of a release.
type ReleaseFile struct {
	Path   string `json:"path"`
	Length int64  `json:"length"`
}

// Manifest is what the e2e setup reads.
type Manifest struct {
	DownloadDir string    `json:"downloadDir"`
	Releases    []Release `json:"releases"`
}

// albumTracks are the album's songs, in order.
var albumTracks = []string{
	"Группа крови",
	"Закрой за мной дверь",
	"Война",
	"Спокойная ночь",
	"Мама, мы все тяжело больны",
	"Бошетунмай",
}

const (
	albumArtist = "Кино"
	albumTitle  = "Группа крови"
	albumYear   = 1988
	movieTitle  = "Test Movie"
	movieYear   = 2024
	// the «Смотреть сейчас» release: a title of its own, so the movie's
	// library copy never answers it (FAKE_MOVIE_2 of web/e2e/fakes.ts)
	watchTitle = "Second Test Movie"
	watchYear  = 2023
	// the series release: season 1 of FAKE_SERIES (web/e2e/fakes.ts), its
	// first two episodes (of three)
	seriesTitle = "Test Show"
	seriesYear  = 2023
	// seriesEpisodeSeconds: each episode's length; voiceOverDelayMs: how
	// late the voice-over release's tracks start against the episodes'
	// own audio (the fit finds offsetMs -1500). A track that long
	// (the episode and the delay) is within the 10 % a placed track may
	// differ by; it is not cut back to the episode's length, since the
	// fit's window covers the whole of so short a video and needs the
	// track to go on that much past it
	seriesEpisodeSeconds = 20
	voiceOverDelayMs     = 1500
	bookAuthor           = "Михаил Булгаков"
	bookTitle            = "Мастер и Маргарита"
	bookYear             = 1967
	// hold: a book the e2e never places (its download waits); broken: one
	// whose folder has no book in it (organize fails it)
	holdTitle   = "Собачье сердце"
	holdYear    = 1987
	brokenTitle = "Белая гвардия"
	brokenYear  = 1925
)

// AlbumFolder is the album release's folder name.
var albumFolder = fmt.Sprintf("%s - %s (%d) [FLAC]", albumArtist, albumTitle, albumYear)

// releaseSpec makes one release's payload under dir (the release's file
// or folder is dir/name).
type releaseSpec struct {
	key, kind, name, title string
	year                   int
	make                   func(ff ffmpegRunner, dir, name string) error
}

var releaseSpecs = []releaseSpec{
	{"movie", "movie", "Test.Movie.2024.1080p.WEB-DL.mkv", movieTitle, movieYear,
		func(ff ffmpegRunner, dir, name string) error {
			return ff.video(filepath.Join(dir, name), "testsrc", 440, 8)
		}},
	{"watch", "movie", "Second.Test.Movie.2023.720p.WEB-DL.mkv", watchTitle, watchYear,
		func(ff ffmpegRunner, dir, name string) error {
			return ff.video(filepath.Join(dir, name), "testsrc2", 660, 6)
		}},
	{"series", "series", "Test.Show.S01.720p.WEB-DL", seriesTitle, seriesYear,
		func(ff ffmpegRunner, dir, name string) error {
			for ep := 1; ep <= 2; ep++ {
				out := filepath.Join(dir, name, fmt.Sprintf("Test.Show.S01E%02d.720p.WEB-DL.mkv", ep))
				if err := ff.episode(out, ep); err != nil {
					return err
				}
			}
			return nil
		}},
	// the voice-over of the series release: each episode's own audio
	// starting voiceOverDelayMs later, as a
	// Russian .mka named for its episode («Найти озвучку» finds the folder
	// by its words, the download picks the tracks by their codes)
	{"voiceover", "series", "Test.Show.S01.RUS.Sound", seriesTitle, seriesYear,
		func(ff ffmpegRunner, dir, name string) error {
			for ep := 1; ep <= 2; ep++ {
				out := filepath.Join(dir, name, fmt.Sprintf("Test.Show.S01E%02d.rus.mka", ep))
				if err := ff.voiceOver(out, ep); err != nil {
					return err
				}
			}
			return nil
		}},
	{"album", "album", albumFolder, albumTitle, albumYear, makeAlbum},
	{"book", "book", fmt.Sprintf("%s - %s.epub", bookAuthor, bookTitle), bookTitle, bookYear,
		func(_ ffmpegRunner, dir, name string) error {
			b, err := epub(bookTitle, bookAuthor, bookYear)
			if err != nil {
				return err
			}
			return writeFile(filepath.Join(dir, name), b)
		}},
	{"hold", "book", fmt.Sprintf("%s - %s.epub", bookAuthor, holdTitle), holdTitle, holdYear,
		func(_ ffmpegRunner, dir, name string) error {
			b, err := epub(holdTitle, bookAuthor, holdYear)
			if err != nil {
				return err
			}
			return writeFile(filepath.Join(dir, name), b)
		}},
	{"broken", "book", fmt.Sprintf("%s - %s (%d)", bookAuthor, brokenTitle, brokenYear), brokenTitle, brokenYear,
		func(_ ffmpegRunner, dir, name string) error {
			return writeFile(filepath.Join(dir, name, "readme.txt"), []byte("Здесь нет книги.\n"))
		}},
}

// buildReleases writes every release's payload into downloadDir and its
// metafile into metaDir (both created when missing; a release already
// there is replaced).
func buildReleases(downloadDir, metaDir string) (Manifest, error) {
	var err error
	if downloadDir, err = filepath.Abs(downloadDir); err != nil {
		return Manifest{}, err
	}
	if metaDir, err = filepath.Abs(metaDir); err != nil {
		return Manifest{}, err
	}
	for _, d := range []string{downloadDir, metaDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return Manifest{}, err
		}
	}
	ff, err := newFFmpeg()
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{DownloadDir: downloadDir}
	for _, s := range releaseSpecs {
		root := filepath.Join(downloadDir, s.name)
		if err := os.RemoveAll(root); err != nil {
			return Manifest{}, err
		}
		if err := s.make(ff, downloadDir, s.name); err != nil {
			return Manifest{}, fmt.Errorf("%s: %w", s.key, err)
		}
		r, err := metafile(root, metaDir)
		if err != nil {
			return Manifest{}, fmt.Errorf("%s: %w", s.key, err)
		}
		r.Key, r.Kind, r.Title, r.Year = s.key, s.kind, s.title, s.year
		m.Releases = append(m.Releases, r)
	}
	return m, nil
}

// metafile builds root's metafile (root is a file or a folder) and writes
// it to metaDir/<infohash>.torrent.
func metafile(root, metaDir string) (Release, error) {
	info := metainfo.Info{PieceLength: releasePieceLength}
	if err := info.BuildFromFilePath(root); err != nil {
		return Release{}, err
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		return Release{}, err
	}
	mi := metainfo.MetaInfo{InfoBytes: ib}
	ih := mi.HashInfoBytes().HexString()
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		return Release{}, err
	}
	p := filepath.Join(metaDir, ih+".torrent")
	if err := writeFile(p, buf.Bytes()); err != nil {
		return Release{}, err
	}
	r := Release{Name: info.Name, Infohash: ih, Size: info.TotalLength(), Metafile: p}
	for _, f := range info.UpvertedFiles() {
		fp := strings.Join(f.BestPath(), "/")
		if len(info.Files) == 0 {
			fp = info.Name
		}
		r.Files = append(r.Files, ReleaseFile{Path: fp, Length: f.Length})
	}
	return r, nil
}

// makeAlbum writes the album's folder: six tagged FLAC songs (a tone each),
// a cover, a cue sheet and a rip log (the track picker's «Обложка и прочее»).
func makeAlbum(ff ffmpegRunner, dir, name string) error {
	root := filepath.Join(dir, name)
	for i, t := range albumTracks {
		n := i + 1
		out := filepath.Join(root, fmt.Sprintf("%02d - %s.flac", n, t))
		if err := ff.song(out, 330+55*n, 3, map[string]string{
			"artist":       albumArtist,
			"album_artist": albumArtist,
			"album":        albumTitle,
			"title":        t,
			"track":        fmt.Sprintf("%d/%d", n, len(albumTracks)),
			"date":         strconv.Itoa(albumYear),
		}); err != nil {
			return err
		}
	}
	if err := ff.image(filepath.Join(root, "cover.jpg")); err != nil {
		return err
	}
	var cue strings.Builder
	fmt.Fprintf(&cue, "PERFORMER \"%s\"\nTITLE \"%s\"\nREM DATE %d\n", albumArtist, albumTitle, albumYear)
	for i, t := range albumTracks {
		fmt.Fprintf(&cue, "FILE \"%02d - %s.flac\" WAVE\n  TRACK %02d AUDIO\n    TITLE \"%s\"\n    INDEX 01 00:00:00\n", i+1, t, i+1, t)
	}
	if err := writeFile(filepath.Join(root, fmt.Sprintf("%s - %s.cue", albumArtist, albumTitle)), []byte(cue.String())); err != nil {
		return err
	}
	return writeFile(filepath.Join(root, fmt.Sprintf("%s - %s.log", albumArtist, albumTitle)),
		[]byte("Fake rip log for the e2e: every track generated, not ripped.\n"))
}

// ffmpegRunner runs ffmpeg in bit-exact mode (no encoder or time tags,
// one thread), so the same ffmpeg writes the same bytes every time.
type ffmpegRunner struct{ bin string }

func newFFmpeg() (ffmpegRunner, error) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ffmpegRunner{}, errors.New("ffmpeg not found on PATH (the releases are made with it)")
	}
	return ffmpegRunner{bin: bin}, nil
}

func (f ffmpegRunner) run(out string, args ...string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	all := append([]string{"-hide_banner", "-loglevel", "error", "-y", "-threads", "1"}, args...)
	all = append(all, "-map_metadata", "-1", "-fflags", "+bitexact", "-flags:v", "+bitexact", "-flags:a", "+bitexact")
	return f.exec(out, all)
}

func (f ffmpegRunner) exec(out string, args []string) error {
	cmd := exec.Command(f.bin, append(args, out)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg %s: %v: %s", filepath.Base(out), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// video is an H.264/AAC Matroska file: a test pattern and a tone.
func (f ffmpegRunner) video(out, pattern string, tone, seconds int) error {
	return f.run(out,
		"-f", "lavfi", "-i", fmt.Sprintf("%s=size=320x240:rate=25:duration=%d", pattern, seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=48000:duration=%d", tone, seconds),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "50",
		"-c:a", "aac", "-b:a", "96k", "-shortest")
}

// soundtrack is episode ep's audio: a tone gated at random, frame by
// frame (a soundtrack's envelope: never periodic, so the voice-over fit
// can measure against it; a plain tone has nothing to measure), the
// same for the same ep on every run (ffmpeg's random() starts from its
// fixed seed).
func soundtrack(ep int) string {
	return fmt.Sprintf("sine=frequency=%d:sample_rate=48000:duration=%d,volume='if(lt(random(0),0.3),0,random(1))':eval=frame",
		440+110*ep, seriesEpisodeSeconds)
}

// episode is an H.264/AAC Matroska episode: a test pattern and
// soundtrack(ep), its audio tagged English (the track a fit measures
// against when the title's original language is unknown).
func (f ffmpegRunner) episode(out string, ep int) error {
	return f.run(out,
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc=size=320x240:rate=25:duration=%d", seriesEpisodeSeconds),
		"-f", "lavfi", "-i", soundtrack(ep),
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-g", "50",
		"-c:a", "aac", "-b:a", "96k", "-ac", "1", "-metadata:s:a:0", "language=eng", "-shortest")
}

// voiceOver is episode ep's soundtrack delayed by voiceOverDelayMs (so
// that much longer), FLAC in Matroska, tagged Russian.
func (f ffmpegRunner) voiceOver(out string, ep int) error {
	return f.run(out,
		"-f", "lavfi", "-i", soundtrack(ep),
		"-af", fmt.Sprintf("adelay=%d:all=1", voiceOverDelayMs),
		"-c:a", "flac", "-ac", "1", "-metadata:s:a:0", "language=rus", "-f", "matroska")
}

// song is a FLAC tone with tags.
func (f ffmpegRunner) song(out string, tone, seconds int, tags map[string]string) error {
	args := []string{"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=44100:duration=%d", tone, seconds),
		"-c:a", "flac"}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	all := append([]string{"-hide_banner", "-loglevel", "error", "-y", "-threads", "1"}, args...)
	all = append(all, "-map_metadata", "-1", "-fflags", "+bitexact", "-flags:a", "+bitexact")
	for _, k := range keys {
		all = append(all, "-metadata", k+"="+tags[k])
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	return f.exec(out, all)
}

// image is a small square JPEG (one test-pattern frame).
func (f ffmpegRunner) image(out string) error {
	return f.run(out, "-f", "lavfi", "-i", "testsrc=size=300x300:rate=1:duration=1",
		"-frames:v", "1", "-q:v", "4", "-f", "image2")
}

// epubTime is every zip entry's time (the same bytes on every run).
var epubTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// epub is a minimal EPUB 3 book: the stored mimetype first, the container,
// the package document with its title and author, a nav and one chapter.
func epub(title, author string, year int) ([]byte, error) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	entries := []struct {
		name, body string
		store      bool
	}{
		{"mimetype", "application/epub+zip", true},
		{"META-INF/container.xml", `<?xml version="1.0" encoding="UTF-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>
`, false},
		{"OEBPS/content.opf", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="id" xml:lang="ru">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="id">urn:uuid:7f3c1c1e-2d00-4d00-8000-000000000001</dc:identifier>
    <dc:title>%s</dc:title>
    <dc:creator>%s</dc:creator>
    <dc:language>ru</dc:language>
    <dc:date>%d</dc:date>
    <meta property="dcterms:modified">2024-01-01T00:00:00Z</meta>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="ch1" href="ch1.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine><itemref idref="ch1"/></spine>
</package>
`, title, author, year), false},
		{"OEBPS/nav.xhtml", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops" xml:lang="ru">
<head><title>%s</title></head>
<body><nav epub:type="toc"><ol><li><a href="ch1.xhtml">Глава 1</a></li></ol></nav></body>
</html>
`, title), false},
		{"OEBPS/ch1.xhtml", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml" xml:lang="ru">
<head><title>Глава 1</title></head>
<body><h1>Глава 1</h1><p>%s — текст для проверки.</p></body>
</html>
`, title), false},
	}
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: epubTime}
		if e.store {
			h.Method = zip.Store
		}
		w, err := z.CreateHeader(h)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
