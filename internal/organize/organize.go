// Package organize places a finished file set into the library: a hard
// link (or a copy), subtitles and audio tracks next to the video, an .nfo
// with the TMDB id. What produced the files (a download, a plugin) does not
// matter.
package organize

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/extaudio"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/media"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/subtitles"
)

// File is one file of a finished file set (a download's, a plugin's).
type File struct {
	Path     string // inside the set, "/"-separated, with its top folder
	FullPath string // on disk; "" = not there
	Size     int64
}

// Job describes a finished file set to put into the library.
type Job struct {
	Kind            string // library.KindMovie | library.KindSeries | library.KindMusic | library.KindBook | library.KindAudiobook | library.KindComic
	Title           string // as in TMDB (Russian); the album, book or comic series
	Year            int
	TMDBID          int
	Season, Episode int // the request; used for files without SxxEyy
	Files           []File
	MoviesDir       string
	SeriesDir       string
	// SeriesFolder is the existing folder of this series under SeriesDir
	// (e.g. "Planet Earth II"): new seasons go next to the ones on disk
	// instead of into a second folder named after the TMDB title.
	SeriesFolder string
	// Have: episodes {season, episode} the library already has a file for;
	// a season pack places only the others (no second copy next to an
	// existing episode). The torrent still seeds all of them.
	Have map[[2]int]bool
	// Probe (optional) reads a video's duration and an audio track's
	// stream: a release's audio file of another length than its video
	// (another cut) is not placed. nil places them unchecked.
	Probe extaudio.Prober

	// Music, books, audiobooks and comics (see planMedia).
	MusicDir, EbooksDir, AudiobooksDir, ComicsDir string
	Artist                                        string // music: the album artist
	Author                                        string // books and audiobooks
	// Music: the chosen release name and the album's aliases (evidence for
	// the other-album check, see otherAlbum); Force skips that check (the
	// admin's «Разложить всё равно»).
	Release string
	Aliases []string
	Force   bool
	// HandPicked: the admin chose Release (only then it is evidence).
	HandPicked bool
	// Picked: the admin chose the files of the release (a changed selection
	// or several albums): no other-album check; a folder gets the catalog
	// title and year only when it is RequestedRoot (the requested album's
	// folder in the torrent), the others are named from their folders, and
	// pictures above the album folders go to RequestedRoot when it is kept.
	Picked        bool
	RequestedRoot string
	// AlbumDir: the library folder of an album the library has already
	// (the music import's partial download of it): the selected tracks go
	// there (disc folders kept) without pictures, CUE sheets, logs or
	// lyrics, and with no other-album check. "" = a new album folder.
	AlbumDir string
	// AlbumDirs: album folders of the torrent (as RequestedRoot names one)
	// whose album the library has, to that album's library folder — songs
	// picked by hand («Выбрать треки…») join it (intoRoot); the other
	// album folders are placed as usual.
	AlbumDirs map[string]string
}

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".m4v": true, ".avi": true, ".mov": true, ".webm": true, ".ts": true, ".m2ts": true}

var (
	reSample  = regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}])(sample|trailer|teaser)([^\p{L}\p{N}]|$)`)
	rePart    = regexp.MustCompile(`(?i)[\s._-]*(?:cd|disc|disk|part|pt)[\s._-]?(\d{1,2})$`)
	reLeadNum = regexp.MustCompile(`^\s*(\d{1,3})(?:[^\p{L}\p{N}]|$)`)
)

// LeadNumber reads a leading number of a file name ("03. Название.mkv").
func LeadNumber(base string) (int, bool) {
	m := reLeadNum.FindStringSubmatch(base)
	if m == nil {
		return 0, false
	}
	n, _ := strconv.Atoi(m[1])
	return n, true
}

// PartialSuffix marks a copy in progress.
const PartialSuffix = ".vault-partial"

// linkFile is os.Link; tests replace it to force the copy path.
var linkFile = os.Link

// IsVideo: a video file that is not a sample or a trailer.
func IsVideo(name string) bool {
	if !videoExt[strings.ToLower(filepath.Ext(name))] {
		return false
	}
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	return !reSample.MatchString(base)
}

// SafeName makes a title usable as a file or folder name on any file
// system: "AC/DC: Live" → "AC-DC - Live".
func SafeName(s string) string {
	s = strings.ReplaceAll(s, ": ", " - ")
	s = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':':
			return '-'
		case '*', '?', '"', '<', '>', '|':
			return -1
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimRight(strings.TrimLeft(s, ". "), ". ")
	// Keep names well under the 255-byte limit of most file systems.
	for len(s) > 150 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = strings.TrimRight(s[:len(s)-n], ". ")
	}
	if s == "" {
		return "Без названия"
	}
	return s
}

type placement struct{ src, dst string }

// plan decides where each video file of the torrent goes (planMedia the
// files of music, books, audiobooks and comics).
func plan(j Job) ([]placement, error) {
	if IsMediaKind(j.Kind) {
		return planMedia(j)
	}
	var videos []File
	for _, f := range j.Files {
		if f.FullPath != "" && IsVideo(f.Path) {
			videos = append(videos, f)
		}
	}
	if len(videos) == 0 {
		return nil, i18n.Errorf("Среди файлов нет видео")
	}
	title := SafeName(j.Title)
	switch j.Kind {
	case library.KindMovie:
		name := title
		if j.Year > 0 {
			name = fmt.Sprintf("%s (%d)", title, j.Year)
		}
		dir := filepath.Join(j.MoviesDir, name)
		parts := movieParts(videos)
		if len(parts) > 1 {
			out := make([]placement, len(parts))
			for i, f := range parts {
				out[i] = placement{f.FullPath, filepath.Join(dir, fmt.Sprintf("%s - cd%d%s", name, i+1, strings.ToLower(filepath.Ext(f.Path))))}
			}
			return out, nil
		}
		best := videos[0]
		for _, f := range videos[1:] {
			if f.Size > best.Size {
				best = f
			}
		}
		return []placement{{best.FullPath, filepath.Join(dir, name+strings.ToLower(filepath.Ext(best.Path)))}}, nil
	case library.KindSeries:
		var out []placement
		seen := map[string]bool{}
		had := 0
		for _, f := range videos {
			season, ep, end := episodeOf(f, j, len(videos))
			if ep == 0 {
				continue // cannot tell which episode this is
			}
			code := fmt.Sprintf("S%02dE%02d", season, ep)
			if end > ep {
				code += fmt.Sprintf("-E%02d", end)
			}
			dst := filepath.Join(j.SeriesDir, seriesFolder(j), fmt.Sprintf("Season %02d", season), title+" - "+code+strings.ToLower(filepath.Ext(f.Path)))
			if seen[dst] {
				continue
			}
			seen[dst] = true
			if hasAll(j.Have, season, ep, end) && !placedBefore(f.FullPath, dst) {
				had++ // in the library already (another file): keep that one
				continue
			}
			out = append(out, placement{f.FullPath, dst})
		}
		if len(out) == 0 && had > 0 {
			return nil, nil // every episode is in the library already
		}
		if len(out) == 0 {
			return nil, i18n.Errorf("Не удалось понять, какие это серии")
		}
		sort.Slice(out, func(a, b int) bool { return out[a].dst < out[b].dst })
		return out, nil
	}
	return nil, i18n.Errorf("Неизвестный тип %q", j.Kind)
}

// hasAll: every episode ep..end of the season is in have.
func hasAll(have map[[2]int]bool, season, ep, end int) bool {
	if len(have) == 0 {
		return false
	}
	for e := ep; e <= max(ep, end); e++ {
		if !have[[2]int{season, e}] {
			return false
		}
	}
	return true
}

// placedBefore: an earlier organize of this torrent already put src at
// dst (or at a "(n)" variant, as place names them); a re-run keeps it.
func placedBefore(src, dst string) bool {
	si, err := os.Stat(src)
	if err != nil {
		return false
	}
	ext := filepath.Ext(dst)
	stem := strings.TrimSuffix(dst, ext)
	for n := 1; n <= 20; n++ {
		cand := dst
		if n > 1 {
			cand = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		di, err := os.Stat(cand)
		if err != nil {
			return false
		}
		if os.SameFile(si, di) || di.Size() == si.Size() && sameMtime(di.ModTime(), si.ModTime()) {
			return true
		}
	}
	return false
}

// seriesFolder is the series' folder under SeriesDir: the existing one when
// known (and a plain folder name), else the TMDB title.
func seriesFolder(j Job) string {
	if f := j.SeriesFolder; f != "" && SafeName(f) == f && validFolder(f) {
		return f
	}
	return SafeName(j.Title)
}

func validFolder(f string) bool { return f != "." && f != ".." && filepath.Base(f) == f }

// movieParts returns the parts (cd1, cd2…) in order when the movie is split,
// otherwise nil.
func movieParts(videos []File) []File {
	type part struct {
		n int
		f File
	}
	var parts []part
	for _, f := range videos {
		base := strings.TrimSuffix(filepath.Base(f.Path), filepath.Ext(f.Path))
		m := rePart.FindStringSubmatch(base)
		if m == nil {
			return nil
		}
		n, _ := strconv.Atoi(m[1])
		parts = append(parts, part{n, f})
	}
	if len(parts) < 2 {
		return nil
	}
	sort.Slice(parts, func(a, b int) bool { return parts[a].n < parts[b].n })
	out := make([]File, len(parts))
	for i, p := range parts {
		out[i] = p.f
	}
	return out
}

// episodeOf finds season/episode of a file: SxxEyy in its name, else a
// leading number ("03. Название.mkv") in a season pack, else the request
// when the torrent has a single video.
func episodeOf(f File, j Job, videos int) (season, ep, end int) {
	base := filepath.Base(f.Path)
	info := release.Parse(base)
	season, ep, end = info.Season, info.Episode, info.EpisodeEnd
	if season == 0 {
		// "Сёгун S01/03.mkv", "Season 3/Disc 1/01.mkv": the nearest folder
		// naming one season (as the season's selection reads it,
		// FileSeason); the request's only when no folder names a season
		season = FolderSeason(filepath.ToSlash(f.Path))
		if season == 0 {
			season = max(j.Season, 1)
		}
	}
	if ep == 0 {
		// «[Group] Show - 08 [1080p]»: the number the pinned release
		// counted the file by (release.DashEpisodes, shared)
		if first, last := release.DashEpisodes(base); first > 0 {
			ep = first
			if last > first {
				end = last
			}
		} else if n, ok := LeadNumber(base); ok && videos > 1 {
			ep = n
		} else if videos == 1 && j.Episode > 0 {
			season, ep = max(j.Season, 1), j.Episode
		}
	}
	return season, ep, end
}

// SeriesEpisodes are the episodes plan places the video files paths of a
// series torrent as (episodeOf; season, episode: the request), each
// {season, first, last}; a file it cannot tell is left out. What a
// download not organized yet will bring (acquire's Pending).
func SeriesEpisodes(paths []string, season, episode int) [][3]int {
	var videos []File
	for _, p := range paths {
		if IsVideo(p) {
			videos = append(videos, File{Path: p})
		}
	}
	j := Job{Kind: library.KindSeries, Season: season, Episode: episode}
	var out [][3]int
	for _, f := range videos {
		s, ep, end := episodeOf(f, j, len(videos))
		if ep == 0 {
			continue
		}
		out = append(out, [3]int{s, ep, max(ep, end)})
	}
	return out
}

// Organize places the torrent's video files (or music, book, audiobook
// and comic files) into the library: a hard link when the library is on
// the same file system, otherwise a copy. The torrent keeps seeding its
// own files. A .nfo with the TMDB id is written next to videos. Running it
// again is harmless. Returns the library paths.
func Organize(ctx context.Context, j Job) ([]string, error) {
	ps, err := plan(j)
	if err != nil {
		return nil, err
	}
	if len(ps) == 0 {
		return nil, nil // nothing new for the library (see Job.Have)
	}
	var out []string
	for _, p := range ps {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		dst, err := Place(ctx, p.src, p.dst)
		if err != nil {
			return out, HumanFSError(err, filepath.Dir(p.dst))
		}
		out = append(out, dst)
	}
	if err := writeNFO(j, out); err != nil {
		return out, HumanFSError(err, filepath.Dir(out[0]))
	}
	if !IsMediaKind(j.Kind) {
		placed := make([]placement, len(out))
		for i, p := range out {
			placed[i] = placement{ps[i].src, p}
		}
		placeSubs(ctx, j, placed)
		placeAudio(ctx, j, placed)
	}
	return out, nil
}

// maxSubBytes skips anything too big to be a subtitle file.
const maxSubBytes = 10 << 20

// placeSubs links the torrent's subtitle files (.srt/.ass/.vtt) next to
// the videos as "<video stem>.<lang><ext>": for a series by the SxxEyy in
// the file or folder name, for a movie all of them; a movie in parts gets
// each subtitle at the part with the same number (cd1, part2), one
// without a number at the first part. The language comes from the name
// ("2_Russian.srt" → ru), else from the text (Cyrillic → ru); a second one
// in the same language at the same video is ".ru.2.srt". Best effort:
// subtitles never fail the organize.
func placeSubs(ctx context.Context, j Job, videos []placement) {
	var subs []File
	for _, f := range j.Files {
		if f.FullPath != "" && subtitles.IsSubtitle(f.Path) && f.Size <= maxSubBytes && !reSample.MatchString(path.Base(f.Path)) {
			subs = append(subs, f)
		}
	}
	if len(subs) == 0 || len(videos) == 0 {
		return
	}
	sort.Slice(subs, func(a, b int) bool { return subs[a].Path < subs[b].Path })
	used := map[string]int{} // target stem + lang → subtitles placed
	for _, s := range subs {
		if ctx.Err() != nil {
			return
		}
		target, ok := subTarget(j.Kind, s.Path, videos)
		if !ok {
			continue
		}
		lang := subtitles.LangOf(s.Path)
		if lang == "" {
			lang = subtitles.LangOf(path.Base(path.Dir(s.Path)))
		}
		if lang == "" {
			if b, err := os.ReadFile(s.FullPath); err == nil {
				lang = subtitles.SniffLang(b)
			}
		}
		stem := strings.TrimSuffix(target, filepath.Ext(target))
		key := stem + "|" + lang
		used[key] = placeSub(ctx, s.FullPath, stem, lang, strings.ToLower(filepath.Ext(s.Path)), used[key])
	}
}

// placeAudio links (or copies) the torrent's audio tracks of each placed
// video beside it — what extaudio.Find finds next to the torrent's own
// file: «RUS Sound/[LostFilm]/X.S01E02.ac3», «X.eng.mka» — as
// "<video stem>.<lang>.<title><ext>" (".<lang>.<title>.N" for a second
// one), which Find in the library reads back with the same language and
// studio. Only files of this torrent are taken, and with j.Probe only
// those of the video's length (extaudio.Describe). Best effort: audio
// never fails the organize.
func placeAudio(ctx context.Context, j Job, videos []placement) {
	mine := map[string]bool{}
	for _, f := range j.Files {
		if f.FullPath != "" && extaudio.IsAudio(f.Path) {
			mine[filepath.Clean(f.FullPath)] = true
		}
	}
	if len(mine) == 0 {
		return
	}
	for _, v := range videos {
		stem := strings.TrimSuffix(v.dst, filepath.Ext(v.dst))
		used := map[string]int{}
		var videoMs int64 = -1 // probed on the first track
		for _, t := range extaudio.Find(v.src) {
			if ctx.Err() != nil {
				return
			}
			if !mine[filepath.Clean(t.Path)] {
				continue
			}
			if j.Probe != nil {
				if videoMs < 0 {
					videoMs = 0 // unknown: only readable audio is checked
					if p, err := j.Probe(ctx, v.src); err == nil {
						videoMs = p.DurationMs
					}
				}
				if _, ok := extaudio.Describe(ctx, t, j.Probe, videoMs); !ok {
					continue // unreadable, silent, or of another cut
				}
			}
			tag := AudioTag(t)
			used[tag] = placeAudioFile(ctx, t.Path, stem+tag, strings.ToLower(filepath.Ext(t.Path)), used[tag])
		}
	}
}

// AudioTag is what a placed track's name adds to its video's stem:
// ".<lang>.<title>" (either left out when unknown), read back by
// extaudio.Find with the same language and studio.
func AudioTag(t extaudio.Track) string {
	tag := ""
	if t.Lang != "" && t.Lang != "und" {
		tag = "." + t.Lang
	}
	if title := strings.Trim(SafeName(t.Title), ". "); title != "" && t.Title != "" {
		tag += "." + title
	}
	return tag
}

// MediaProber is Job.Probe with the server's ffprobe: an audio
// file is read with the audio demuxers only (media.ProbeAudioFile), a
// video like the library's probe.
func MediaProber(ffprobe string) extaudio.Prober {
	return func(ctx context.Context, path string) (media.ProbeResult, error) {
		if extaudio.IsAudio(path) {
			return media.ProbeAudioFile(ctx, ffprobe, path)
		}
		return media.Probe(ctx, ffprobe, path)
	}
}

// placeAudioFile links (or copies) src as name<ext>, then name.2<ext>…
// past names taken by other files; n is how many tracks this name already
// got in this run. Returns the new count.
func placeAudioFile(ctx context.Context, src, name, ext string, n int) int {
	k, _, _ := PlaceAudioFileAt(ctx, src, name, ext, n, nil)
	return k
}

// PlaceAudioFileAt is placeAudioFile that also returns the track's path
// in the library ("" when not placed) and whether this run placed it
// (false: an earlier run did); before (nil: none) runs just before the
// link is made, and an error of it places nothing.
func PlaceAudioFileAt(ctx context.Context, src, name, ext string, n int, before func(dst string) error) (int, string, bool) {
	si, err := os.Stat(src)
	if err != nil {
		return n, "", false
	}
	for k := n + 1; k <= n+20; k++ {
		dst := name + ext
		if k > 1 {
			dst = name + "." + strconv.Itoa(k) + ext
		}
		di, err := os.Stat(dst)
		if err == nil {
			if os.SameFile(si, di) || di.Size() == si.Size() && sameMtime(di.ModTime(), si.ModTime()) {
				return k, dst, false // placed by an earlier run
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return n, "", false
		}
		if before != nil && before(dst) != nil {
			return n, "", false
		}
		if linkFile(src, dst) != nil && copyFile(ctx, src, dst) != nil {
			return n, "", false
		}
		return k, dst, true
	}
	return n, "", false
}

// subTarget picks the placed video a subtitle (its in-torrent path) belongs to.
func subTarget(kind, p string, videos []placement) (string, bool) {
	base := strings.TrimSuffix(path.Base(p), path.Ext(p))
	parent := path.Base(path.Dir(p))
	if kind == library.KindSeries {
		info := release.Parse(path.Base(p))
		if info.Episode == 0 {
			info = release.Parse(parent)
		}
		if info.Episode == 0 {
			return "", false
		}
		for _, v := range videos {
			vi := release.Parse(filepath.Base(v.dst))
			if vi.Episode == info.Episode && (vi.Season == info.Season || info.Season == 0) {
				return v.dst, true
			}
		}
		return "", false
	}
	if len(videos) > 1 {
		if n := partNo(base, parent); n > 0 {
			for _, v := range videos {
				if partNo(strings.TrimSuffix(filepath.Base(v.src), filepath.Ext(v.src))) == n {
					return v.dst, true
				}
			}
		}
	}
	return videos[0].dst, true
}

// partNo is the part number (cd1, part2) at the end of a name or of the
// name without its language/flag suffixes ("X CD2.ru.forced"); 0 = none.
func partNo(names ...string) int {
	for _, n := range names {
		for i := 0; i < 4 && n != ""; i++ {
			if m := rePart.FindStringSubmatch(n); m != nil {
				k, _ := strconv.Atoi(m[1])
				return k
			}
			dot := strings.LastIndex(n, ".")
			if dot < 0 {
				break
			}
			n = n[:dot]
		}
	}
	return 0
}

// placeSub links (or copies) src as stem.<lang>[.N]<ext>, skipping names
// taken by other files; n is how many subtitles in this language the
// video already got in this run. Returns the new count.
func placeSub(ctx context.Context, src, stem, lang, ext string, n int) int {
	si, err := os.Stat(src)
	if err != nil {
		return n
	}
	tag := lang
	for k := n + 1; k <= n+20; k++ {
		name := stem
		switch {
		case k == 1 && tag != "":
			name += "." + tag
		case k > 1:
			if tag == "" {
				tag = "und"
			}
			name += fmt.Sprintf(".%s.%d", tag, k)
		}
		dst := name + ext
		di, err := os.Stat(dst)
		if err == nil {
			if os.SameFile(si, di) || di.Size() == si.Size() && sameMtime(di.ModTime(), si.ModTime()) {
				return k // placed by an earlier run
			}
			continue // someone else's subtitle (Auto, by hand)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return n
		}
		if linkFile(src, dst) != nil && copyFile(ctx, src, dst) != nil {
			return n
		}
		return k
	}
	return n
}

// place links or copies src to dst. An existing dst that is the same file
// (or a complete earlier copy) is kept; a different file with that name
// gets a numbered sibling instead of being overwritten.
func Place(ctx context.Context, src, dst string) (string, error) {
	si, err := os.Stat(src)
	if err != nil {
		return "", i18n.Errorf(msgSourceMissing, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	ext := filepath.Ext(dst)
	stem := strings.TrimSuffix(dst, ext)
	for n := 1; ; n++ {
		cand := dst
		if n > 1 {
			cand = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		di, err := os.Stat(cand)
		if err == nil {
			// our hard link, or our earlier copy (same size and the source's
			// mtime, which copyFile sets); anything else is someone else's file
			if os.SameFile(si, di) || di.Size() == si.Size() && sameMtime(di.ModTime(), si.ModTime()) {
				return cand, nil
			}
			if n > 20 {
				return "", i18n.Errorf(msgExists, dst)
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := linkFile(src, cand); err == nil {
			return cand, nil
		}
		return cand, copyFile(ctx, src, cand)
	}
}

// sameMtime compares modification times at the coarsest precision of the
// file systems a copy may land on (exFAT/FAT keep 2 s, some network mounts
// 1 s): a copy with the source's mtime counts as ours.
func sameMtime(a, b time.Time) bool {
	d := a.Sub(b)
	return d < 2*time.Second && d > -2*time.Second
}

// SyncDir flushes a directory entry (a rename into it) to disk; best effort.
func SyncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// NewCtxReader is r that stops reading when ctx ends.
func NewCtxReader(ctx context.Context, r io.Reader) io.Reader { return ctxReader{ctx, r} }

// ctxReader stops a copy when ctx ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func copyFile(ctx context.Context, src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + PartialSuffix
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, ctxReader{ctx, in}); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil { // the rename must not land before the data
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if st, err := in.Stat(); err == nil {
		os.Chtimes(tmp, st.ModTime(), st.ModTime()) // marks the copy as ours (see place)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	SyncDir(filepath.Dir(dst))
	return nil
}

func xmlText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// writeNFO writes <movie>.nfo next to a movie, tvshow.nfo into a series
// folder. Existing nfo files are left alone (they may carry richer data).
func writeNFO(j Job, paths []string) error {
	var p, body string
	year := ""
	if j.Year > 0 {
		year = fmt.Sprintf("\n  <year>%d</year>", j.Year)
	}
	switch j.Kind {
	case library.KindMovie:
		first := paths[0]
		stem := strings.TrimSuffix(first, filepath.Ext(first))
		if len(paths) > 1 { // "Title (Year) - cd1" → "Title (Year).nfo"
			stem = rePart.ReplaceAllString(stem, "")
		}
		p = stem + ".nfo"
		body = fmt.Sprintf("<movie>\n  <title>%s</title>%s\n  <tmdbid>%d</tmdbid>\n  <uniqueid type=\"tmdb\" default=\"true\">%d</uniqueid>\n</movie>\n",
			xmlText(j.Title), year, j.TMDBID, j.TMDBID)
	case library.KindSeries:
		p = filepath.Join(j.SeriesDir, seriesFolder(j), "tvshow.nfo")
		body = fmt.Sprintf("<tvshow>\n  <title>%s</title>%s\n  <tmdbid>%d</tmdbid>\n  <uniqueid type=\"tmdb\" default=\"true\">%d</uniqueid>\n</tvshow>\n",
			xmlText(j.Title), year, j.TMDBID, j.TMDBID)
	default:
		return nil
	}
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return os.WriteFile(p, []byte(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`+"\n"+body), 0o644)
}

// msgSourceMissing / msgExists: place's errors HumanFSError tells apart.
const (
	msgSourceMissing = "исходный файл не найден: %w"
	msgExists        = "%s уже существует"
)

// HumanFSError explains a library write failure in Russian.
func HumanFSError(err error, dir string) error {
	switch {
	case errors.Is(err, os.ErrPermission), errors.Is(err, syscall.EROFS):
		return i18n.Errorf("Нет доступа на запись в папку медиатеки %s. Проверьте права или что диск не подключён только для чтения", dir)
	case errors.Is(err, syscall.ENOSPC):
		return i18n.Errorf("На диске закончилось место — не удалось добавить файл в %s", dir)
	}
	if ie := (*i18n.Error)(nil); errors.As(err, &ie) && (ie.Key == msgSourceMissing || ie.Key == msgExists) {
		return i18n.Errorf("Не удалось добавить в медиатеку: %s", err.Error())
	}
	return i18n.Errorf("Не удалось добавить в медиатеку: %w", err)
}
