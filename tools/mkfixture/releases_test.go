package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
)

func needFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not on PATH")
	}
}

func releases(t *testing.T) Manifest {
	t.Helper()
	root := t.TempDir()
	m, err := buildReleases(filepath.Join(root, "downloads"), filepath.Join(root, "meta"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// verify checks every piece of mi against the payload under dir.
func verify(t *testing.T, mi *metainfo.MetaInfo, dir string) {
	t.Helper()
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	for _, f := range info.UpvertedFiles() {
		p := filepath.Join(append([]string{dir, info.Name}, f.BestPath()...)...)
		if len(info.Files) == 0 {
			p = filepath.Join(dir, info.Name)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", info.Name, err)
		}
		if int64(len(b)) != f.Length {
			t.Fatalf("%s: %d bytes on disk, %d in the metafile", p, len(b), f.Length)
		}
		data.Write(b)
	}
	if n := info.NumPieces(); n == 0 || n*20 != len(info.Pieces) {
		t.Fatalf("%s: %d pieces, %d hash bytes", info.Name, n, len(info.Pieces))
	}
	all := data.Bytes()
	for i := 0; i < info.NumPieces(); i++ {
		end := min(int64(i+1)*info.PieceLength, int64(len(all)))
		sum := sha1.Sum(all[int64(i)*info.PieceLength : end])
		if !bytes.Equal(sum[:], info.Pieces[i*20:(i+1)*20]) {
			t.Fatalf("%s: piece %d does not match the payload", info.Name, i)
		}
	}
}

// The releases' payloads are on disk, each metafile verifies against its
// payload and is named by its infohash, and the manifest says what is there.
func TestReleases_PayloadsVerify(t *testing.T) {
	needFFmpeg(t)
	m := releases(t)
	keys := map[string]Release{}
	for _, r := range m.Releases {
		keys[r.Key] = r
		mi, err := metainfo.LoadFromFile(r.Metafile)
		if err != nil {
			t.Fatalf("%s: %v", r.Key, err)
		}
		if got := mi.HashInfoBytes().HexString(); got != r.Infohash || filepath.Base(r.Metafile) != r.Infohash+".torrent" {
			t.Fatalf("%s: metafile %s (%s), manifest %s", r.Key, got, r.Metafile, r.Infohash)
		}
		verify(t, mi, m.DownloadDir)
		var total int64
		for _, f := range r.Files {
			total += f.Length
		}
		if total != r.Size || r.Size == 0 {
			t.Fatalf("%s: size %d, files %d", r.Key, r.Size, total)
		}
	}
	for _, k := range []string{"movie", "watch", "series", "voiceover", "album", "book", "hold", "broken"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("no %s release in %+v", k, m.Releases)
		}
	}
	// the album: six songs in order, plus the cover, cue and log
	var songs []string
	for _, f := range keys["album"].Files {
		if strings.HasSuffix(f.Path, ".flac") {
			songs = append(songs, f.Path)
		}
	}
	if len(songs) != 6 || len(keys["album"].Files) != 9 || songs[0] != "01 - Группа крови.flac" || songs[5] != "06 - Бошетунмай.flac" {
		t.Fatalf("album files %+v", keys["album"].Files)
	}
	// «Смотреть сейчас» has its own title, so a library copy of the movie
	// never answers it (the second fake movie of fakes.ts)
	if w := keys["watch"]; w.Title != "Second Test Movie" || w.Year != 2023 || w.Name != "Second.Test.Movie.2023.720p.WEB-DL.mkv" {
		t.Fatalf("watch release %+v", w)
	}
	// the series: a season folder of the fake series with its first two episodes
	if sr := keys["series"]; sr.Kind != "series" || sr.Title != "Test Show" || sr.Year != 2023 || sr.Name != "Test.Show.S01.720p.WEB-DL" ||
		len(sr.Files) != 2 || sr.Files[0].Path != "Test.Show.S01E01.720p.WEB-DL.mkv" || sr.Files[1].Path != "Test.Show.S01E02.720p.WEB-DL.mkv" {
		t.Fatalf("series release %+v", sr)
	}
	// its voice-over: a folder of one Russian .mka per episode
	if vo := keys["voiceover"]; vo.Kind != "series" || vo.Title != "Test Show" || vo.Name != "Test.Show.S01.RUS.Sound" ||
		len(vo.Files) != 2 || vo.Files[0].Path != "Test.Show.S01E01.rus.mka" || vo.Files[1].Path != "Test.Show.S01E02.rus.mka" {
		t.Fatalf("voiceover release %+v", vo)
	}
	// hold: a second book, never placed by the e2e (it waits); broken: a
	// third one whose folder holds no book, so organize fails it
	if h := keys["hold"]; h.Kind != "book" || h.Title != "Собачье сердце" || h.Name != "Михаил Булгаков - Собачье сердце.epub" {
		t.Fatalf("hold release %+v", h)
	}
	if b := keys["broken"]; b.Kind != "book" || b.Title != "Белая гвардия" || len(b.Files) != 1 || b.Files[0].Path != "readme.txt" {
		t.Fatalf("broken release %+v", b)
	}
	// the movies are H.264/AAC and about as long as asked
	if _, err := exec.LookPath("ffprobe"); err == nil {
		for k, want := range map[string]string{"movie": "8.", "watch": "6."} {
			out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_name",
				"-of", "csv=p=0", filepath.Join(m.DownloadDir, keys[k].Name)).Output()
			if err != nil {
				t.Fatal(err)
			}
			s := string(out)
			if !strings.Contains(s, "h264") || !strings.Contains(s, "aac") || !strings.Contains(s, "\n"+want) {
				t.Fatalf("%s: ffprobe %q", k, s)
			}
		}
	}
	// the episodes: H.264 with English AAC of seriesEpisodeSeconds; each
	// voice-over track: Russian FLAC, its episode's soundtrack
	// voiceOverDelayMs later (the first 1.5 s silent, then the episode's
	// audio), within 10 % of the episode's length
	t.Run("episodes and voice-over", func(t *testing.T) {
		if _, err := exec.LookPath("ffprobe"); err != nil {
			t.Skip("ffprobe not on PATH: the episodes and the voice-over are not checked")
		}
		for i := 1; i <= 2; i++ {
			ep := filepath.Join(m.DownloadDir, keys["series"].Name, keys["series"].Files[i-1].Path)
			vo := filepath.Join(m.DownloadDir, keys["voiceover"].Name, keys["voiceover"].Files[i-1].Path)
			if s := probe(t, ep); !strings.Contains(s, "h264") || !strings.Contains(s, "aac,eng") || !strings.Contains(s, "\n20.") {
				t.Fatalf("episode %d: ffprobe %q", i, s)
			}
			if s := probe(t, vo); !strings.Contains(s, "flac,rus") || !strings.Contains(s, "\n21.5") {
				t.Fatalf("voice-over %d: ffprobe %q", i, s)
			}
			a, b := pcm(t, ep), pcm(t, vo)
			if lag := bestLag(a, b, 3000); lag != voiceOverDelayMs {
				t.Fatalf("voice-over %d starts %d ms after its episode's audio, want %d", i, lag, voiceOverDelayMs)
			}
		}
	})
	// the book is an EPUB: the stored mimetype first, the title in the package
	z, err := zip.OpenReader(filepath.Join(m.DownloadDir, keys["book"].Name))
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	if z.File[0].Name != "mimetype" || z.File[0].Method != zip.Store {
		t.Fatalf("first entry %s method %d", z.File[0].Name, z.File[0].Method)
	}
	for _, f := range z.File {
		if f.Name == "OEBPS/content.opf" {
			r, _ := f.Open()
			b, _ := io.ReadAll(r)
			r.Close()
			if !strings.Contains(string(b), "<dc:title>Мастер и Маргарита</dc:title>") {
				t.Fatalf("opf %s", b)
			}
		}
	}
}

// probe is a file's streams (codec,language) and its duration, one per line.
func probe(t *testing.T, p string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_name:stream_tags=language",
		"-of", "csv=p=0", p).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", p, err)
	}
	return string(out)
}

// pcm is a file's audio as a 1 kHz envelope: the mean absolute sample of
// each millisecond (8 kHz mono, signed 16 bit).
func pcm(t *testing.T, p string) []float64 {
	t.Helper()
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", p, "-map", "0:a:0", "-ac", "1", "-ar", "8000", "-f", "s16le", "-").Output()
	if err != nil {
		t.Fatalf("decode %s: %v", p, err)
	}
	env := make([]float64, len(out)/16)
	for i := range env {
		var sum float64
		for j := 0; j < 8; j++ {
			v := int16(uint16(out[(i*8+j)*2]) | uint16(out[(i*8+j)*2+1])<<8)
			sum += math.Abs(float64(v))
		}
		env[i] = sum / 8
	}
	return env
}

// bestLag is the lag (ms, 0..max) at which b matches a best: b[i+lag] ≈ a[i].
func bestLag(a, b []float64, max int) int {
	best, bestLag := -1.0, -1
	for lag := 0; lag <= max; lag++ {
		var dot float64
		for i := 0; i+lag < len(b) && i < len(a); i++ {
			dot += a[i] * b[i+lag]
		}
		if dot > best {
			best, bestLag = dot, lag
		}
	}
	return bestLag
}

// The same ffmpeg gives the same payloads, so the same infohashes (the
// e2e caches the releases by them).
func TestReleases_StableInfohashes(t *testing.T) {
	needFFmpeg(t)
	a, b := releases(t), releases(t)
	if len(a.Releases) != len(b.Releases) {
		t.Fatal("release count differs")
	}
	for i := range a.Releases {
		if a.Releases[i].Infohash != b.Releases[i].Infohash {
			t.Fatalf("%s: %s then %s", a.Releases[i].Key, a.Releases[i].Infohash, b.Releases[i].Infohash)
		}
	}
}
