package organize

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/comics"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// The file types come from the library scanners (library.IsMusicFile and
// the others): organize places, and binds to the torrent, exactly what
// they index. Besides those it places what they read next to it.
var (
	musicSideExt = map[string]bool{".cue": true, ".log": true, ".lrc": true}
	junkNames    = map[string]bool{"__macosx": true, "@eadir": true, ".appledouble": true, ".ds_store": true, "thumbs.db": true}

	// a disc folder of an album: "CD1", "Disc 2", "Диск 1" (kept as is) or
	// "The Wall (Disc 1)" (renamed "CD1" so the scanner folds it in)
	reDiscPrefix = regexp.MustCompile(`(?i)^(?:cd|disc|disk|диск)\s*(\d+)`)
	reDiscSuffix = regexp.MustCompile(`(?i)(?:^|[\s\(\[\-_.,])[\(\[]?(?:cd|disc|disk|диск)\s*(\d+)[\)\]]?$`)
)

// Bindable: the library keeps a file row for p (the scanner indexes it), so
// it can be bound to its torrent; CUE sheets, logs and pictures cannot (a
// folder of comic pages is bound as a folder, see PageFolders).
func Bindable(kind, p string) bool {
	switch kind {
	case library.KindMusic:
		return library.IsMusicFile(p)
	case library.KindAudiobook:
		return library.IsAudiobookFile(p)
	case library.KindBook:
		return library.IsBookFile(p)
	case library.KindComic:
		return library.IsComicVolume(p)
	}
	return true
}

// PageFolders returns the folders of comic pages among the placed paths
// (folder volumes: their library file row is the folder).
func PageFolders(paths []string) []string {
	archives := map[string]bool{}
	for _, p := range paths {
		if comics.IsArchive(filepath.Base(p)) {
			archives[filepath.Dir(p)] = true
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		d := filepath.Dir(p)
		if comics.IsPage(filepath.Base(p)) && !archives[d] && !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// SetFile is a file of the set with its in-set path checked: "/"-separated,
// relative, no "..", no macOS/NAS junk.
type SetFile struct {
	f    File
	p    string // clean in-set path
	dir  string // path.Dir(p), "." at the top
	base string
	ext  string // lower-cased
}

// Path is the file's clean in-set path.
func (f SetFile) Path() string { return f.p }

// Base is the file's name.
func (f SetFile) Base() string { return f.base }

// SetFiles checks the files of a set (those on disk) and drops the junk
// and unsafe paths.
func SetFiles(files []File) []SetFile {
	var out []SetFile
	for _, f := range files {
		if f.FullPath == "" {
			continue
		}
		p := path.Clean(strings.ReplaceAll(f.Path, "\\", "/"))
		if p == "." || path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			continue
		}
		ok := true
		for _, c := range strings.Split(p, "/") {
			// "...And Justice for All" is a name; "._01.flac" a resource fork
			if strings.HasPrefix(c, "._") || junkNames[strings.ToLower(c)] {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, SetFile{f: f, p: p, dir: path.Dir(p), base: path.Base(p), ext: strings.ToLower(path.Ext(p))})
		}
	}
	return out
}

// libRel is an in-torrent relative path as it goes into the library: the
// scanners skip hidden names, so leading dots are dropped ("" = nothing
// left to name it).
func libRel(rel string) string {
	parts := strings.Split(rel, "/")
	for i, c := range parts {
		if parts[i] = strings.TrimLeft(c, "."); parts[i] == "" {
			return ""
		}
	}
	return filepath.Join(parts...)
}

// commonDir is the deepest folder holding all dirs ("." = the torrent top).
func commonDir(dirs []string) string {
	if len(dirs) == 0 {
		return "."
	}
	common := strings.Split(dirs[0], "/")
	for _, d := range dirs[1:] {
		parts := strings.Split(d, "/")
		n := 0
		for n < len(common) && n < len(parts) && common[n] == parts[n] {
			n++
		}
		common = common[:n]
	}
	if len(common) == 0 || common[0] == "." {
		return "."
	}
	return strings.Join(common, "/")
}

// Under: p is inside dir (dir "." holds everything).
func Under(dir, p string) bool { return dir == "." || strings.HasPrefix(p, dir+"/") }

// ancestorOf: dir is root or a folder above it.
func ancestorOf(dir, root string) bool {
	return dir == "." || dir == root || strings.HasPrefix(root, dir+"/")
}

func relTo(dir, p string) string {
	if dir == "." {
		return p
	}
	return strings.TrimPrefix(p, dir+"/")
}

// FolderOf is "<base>/<SafeName(name)>", or base when name is empty.
func FolderOf(base, name string) string {
	if strings.TrimSpace(name) == "" {
		return base
	}
	return filepath.Join(base, SafeName(name))
}

// planMedia decides where the files of a music, book, audiobook or comic
// torrent go, in the layouts the library scanners read:
//   - music: MusicDir/<Artist>/<Album (Year)>/ for every album of the
//     torrent (one, or each album of a discography), with the audio, .cue,
//     .log, .lrc and pictures relative to the album's folder in the torrent
//     (disc folders CD1/CD2 and Covers/ kept), so the scanner sees one
//     album per folder;
//   - book: EbooksDir/<Author>/<Title>.<ext> for every format the reader
//     opens (no DJVU); collected works keep their names, prefixed with
//     their volume folders («Том 1 - book.fb2»);
//   - audiobook: AudiobooksDir/<Author>/<Title>/ with the audio and
//     pictures, part folders kept;
//   - comic: ComicsDir/<Series>/ with the archives and folders of pages.
//
// The folders Vault names go through SafeName; file names inside them stay
// as in the torrent (a CUE sheet names its audio file). No file is dropped
// for its name: distinct files with the same target get " (2)", " (3)"….
func planMedia(j Job) ([]placement, error) {
	files := SetFiles(j.Files)
	var out []placement
	var err error
	switch j.Kind {
	case library.KindMusic:
		out, err = planAlbums(j, files)
	case library.KindBook:
		out, err = planBook(j, files)
	case library.KindAudiobook:
		out, err = planAudiobook(j, files)
	case library.KindComic:
		out, err = planComic(j, files)
	default:
		return nil, i18n.Errorf("Неизвестный тип %q", j.Kind)
	}
	if err != nil {
		return nil, err
	}
	return uniqueTargets(out), nil
}

// uniqueTargets drops repeated (src, dst) pairs and numbers the targets of
// distinct sources that would land on the same name, in a stable order.
func uniqueTargets(ps []placement) []placement {
	sort.Slice(ps, func(a, b int) bool {
		if ps[a].dst != ps[b].dst {
			return ps[a].dst < ps[b].dst
		}
		return ps[a].src < ps[b].src
	})
	used := map[string]string{} // dst -> src
	out := make([]placement, 0, len(ps))
	for _, p := range ps {
		if src, ok := used[p.dst]; ok && src == p.src {
			continue
		}
		dst := p.dst
		ext := filepath.Ext(dst)
		if strings.HasSuffix(strings.ToLower(dst), ".fb2.zip") {
			ext = dst[len(dst)-len(".fb2.zip"):]
		}
		stem := strings.TrimSuffix(dst, ext)
		for n := 2; ; n++ {
			if _, taken := used[dst]; !taken {
				break
			}
			dst = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		used[dst] = p.src
		out = append(out, placement{p.src, dst})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].dst < out[b].dst })
	return out
}

// DiscFolder returns the folder name a disc folder gets in the library
// ("" when name is not a disc folder).
func DiscFolder(name string) string {
	if reDiscPrefix.MatchString(strings.TrimSpace(name)) {
		return name
	}
	if m := reDiscSuffix.FindStringSubmatchIndex(name); m != nil && m[0] > 0 {
		n, _ := strconv.Atoi(name[m[2]:m[3]])
		return fmt.Sprintf("CD%d", n)
	}
	return ""
}

func inSkipDir(dir string) bool {
	for _, c := range strings.Split(dir, "/") {
		if library.IsMusicSkipDir(c) {
			return true
		}
	}
	return false
}

// album is one album folder of a music torrent.
type album struct {
	root  string            // its folder in the torrent ("." = the top)
	discs map[string]string // its disc folders in the torrent -> library name
}

// IsTrack: the organizer places f as a track of an album (audio the
// music scanner indexes, not under Scans/, Artwork/…).
func IsTrack(f SetFile) bool { return library.IsMusicFile(f.base) && !inSkipDir(f.dir) }

// AlbumRootOf is the album folder of a track and, for a track in a disc
// folder (folded into its album), that folder's library name.
func AlbumRootOf(f SetFile) (root, disc string) {
	root = f.dir
	if f.dir != "." {
		if d := DiscFolder(path.Base(f.dir)); d != "" {
			root, disc = path.Dir(f.dir), d
		}
	}
	return root, disc
}

func planAlbums(j Job, files []SetFile) ([]placement, error) {
	albums := map[string]*album{}
	for _, f := range files {
		if !IsTrack(f) {
			continue
		}
		root, disc := AlbumRootOf(f)
		a := albums[root]
		if a == nil {
			a = &album{root: root, discs: map[string]string{}}
			albums[root] = a
		}
		if disc != "" {
			a.discs[f.dir] = disc
		}
	}
	if len(albums) == 0 {
		return nil, i18n.Errorf("Среди файлов нет музыки")
	}
	if j.AlbumDir != "" {
		return intoAlbumDir(j, files, albums)
	}
	if len(albums) == 1 && !j.Picked {
		for _, a := range albums {
			if title, year, other := otherAlbum(j, files, a.root); other {
				return nil, i18n.Errorf("Скачан другой альбом: %s", titleYear(title, year))
			}
		}
	}
	artistDir := FolderOf(j.MusicDir, j.Artist)
	dirOf := map[string]string{}
	for root := range albums {
		only := len(albums) == 1
		if j.Picked {
			only = root == j.RequestedRoot
		}
		dirOf[root] = filepath.Join(artistDir, AlbumFolderName(j, root, only))
	}
	// owner: the deepest album folder holding p (a discography may keep
	// tracks of its own next to the album folders)
	depth := func(root string) int {
		if root == "." {
			return 0
		}
		return strings.Count(root, "/") + 1
	}
	owner := func(p string) *album {
		var best *album
		for _, a := range albums {
			if Under(a.root, p) && (best == nil || depth(a.root) > depth(best.root)) {
				best = a
			}
		}
		return best
	}
	var single *album
	for _, a := range albums {
		single = a
	}
	var out []placement
	for _, f := range files {
		picture := library.IsPicture(f.base)
		if !IsTrack(f) && !musicSideExt[f.ext] && !picture {
			continue
		}
		a := owner(f.p)
		var rel string
		switch {
		case a != nil:
			rel = relTo(a.root, f.p)
			for from, to := range a.discs {
				if Under(from, f.p) {
					rel = path.Join(to, relTo(from, f.p))
				}
			}
		case picture && j.Picked && albums[j.RequestedRoot] != nil && ancestorOf(f.dir, j.RequestedRoot):
			a, rel = albums[j.RequestedRoot], f.base // a cover above the requested album's folder
		case picture && len(albums) == 1 && ancestorOf(f.dir, single.root):
			a, rel = single, f.base // a cover next to the album folder
		default:
			continue
		}
		if j.AlbumDirs[a.root] != "" {
			continue // placed below, into the library's album
		}
		if rel = libRel(rel); rel != "" {
			out = append(out, placement{f.f.FullPath, filepath.Join(dirOf[a.root], rel)})
		}
	}
	for root, dir := range j.AlbumDirs {
		if albums[root] != nil && dir != "" {
			out = append(out, intoRoot(files, root, dir)...)
		}
	}
	return out, nil
}

// intoAlbumDir places the selected tracks of the requested album's folder
// in the torrent (Job.RequestedRoot, or the only album's) into the
// library folder of an album the library has (Job.AlbumDir), as
// intoRoot does. The album is the library's, so there is no other-album
// check.
func intoAlbumDir(j Job, files []SetFile, albums map[string]*album) ([]placement, error) {
	root := j.RequestedRoot
	if albums[root] == nil && len(albums) == 1 {
		for r := range albums {
			root = r
		}
	}
	if albums[root] == nil {
		return nil, i18n.Errorf("Среди файлов нет альбома «%s»", j.Title)
	}
	return intoRoot(files, root, j.AlbumDir), nil
}

// intoRoot places the tracks of one album folder of the torrent (root)
// into dir, the library folder of an album the library has: a track of a
// disc folder into that disc's folder, any other by its file name. A song
// the folder holds already is left out (AlbumSongs.has: an earlier pick,
// or another release's copy, «03. Война.mp3» next to «03 - Война.flac»);
// what this torrent placed before is kept. Tracks of other album folders,
// pictures, CUE sheets, logs and lyrics stay out (the album has its own).
func intoRoot(files []SetFile, root, dir string) []placement {
	have := AlbumSongsIn(dir)
	var out []placement
	for _, f := range files {
		if !IsTrack(f) {
			continue
		}
		r, disc := AlbumRootOf(f)
		if r != root {
			continue
		}
		rel := f.base
		if disc != "" {
			rel = path.Join(disc, f.base)
		}
		if rel = libRel(rel); rel == "" {
			continue
		}
		dst := filepath.Join(dir, rel)
		if !placedBefore(f.f.FullPath, dst) {
			if _, err := os.Lstat(dst); err == nil || have.Has(rel) {
				continue // the album has this song already
			}
		}
		out = append(out, placement{f.f.FullPath, dst})
	}
	return out
}

// AlbumSongs are the songs of a library album folder (its tracks and those
// of its disc folders), as their file names tell them.
type AlbumSongs []TrackFile

// AlbumSongsIn reads the songs of dir ("" or unreadable: none).
func AlbumSongsIn(dir string) AlbumSongs {
	var out AlbumSongs
	if dir == "" {
		return out
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			if DiscFolder(e.Name()) == "" {
				continue
			}
			sub, err := os.ReadDir(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			for _, s := range sub {
				if !s.IsDir() && library.IsMusicFile(s.Name()) {
					rel := e.Name() + "/" + s.Name()
					out = append(out, numbered(ParseTrackFile(rel), rel))
				}
			}
			continue
		}
		if library.IsMusicFile(e.Name()) {
			out = append(out, numbered(ParseTrackFile(e.Name()), e.Name()))
		}
	}
	return out
}

// has: the album holds the song of the track file rel (relative to the
// album folder, a disc folder kept): one of the same title (number,
// «Artist - » prefix and editions dropped), or, when either name is only a
// number, one of the same disc and number.
func (a AlbumSongs) Has(rel string) bool {
	tf := numbered(ParseTrackFile(rel), rel)
	titles := songTitles(tf)
	for _, x := range a {
		xt := songTitles(x)
		if len(titles) > 0 && len(xt) > 0 {
			if tf.Disc > 0 && x.Disc > 0 && tf.Disc != x.Disc {
				continue // the same title on another disc is another song
			}
			for _, t := range titles {
				if slices.Contains(xt, t) {
					return true
				}
			}
			continue
		}
		if tf.No > 0 && tf.No == x.No && max(tf.Disc, 1) == max(x.Disc, 1) {
			return true
		}
	}
	return false
}

// numbered gives a name that is only a number («05.flac») that number.
func numbered(tf TrackFile, p string) TrackFile {
	stem := strings.TrimSuffix(path.Base(p), path.Ext(p))
	if n, err := strconv.Atoi(strings.TrimSpace(stem)); tf.No == 0 && err == nil && n > 0 && n < 1000 {
		tf.No = n
	}
	return tf
}

// songTitles are a track file's title candidates without its number (a
// name that is only a number has none).
func songTitles(tf TrackFile) []string {
	names := tf.Names
	if tf.No > 0 && len(names) > 1 {
		names = names[1:] // the whole name, number and all
	} else if tf.No > 0 {
		return nil
	}
	var out []string
	for _, n := range names {
		if strings.Trim(n, "0123456789 ") != "" && !versionOnly(n) {
			out = append(out, n)
		}
	}
	return out
}

// versionOnly: a title candidate that is only version or edition words
// («live», what « - Live» leaves after the dash) names no song.
func versionOnly(n string) bool {
	for _, w := range strings.Fields(n) {
		if !slices.Contains(release.VersionWords, w) && !slices.Contains(release.EditionWords, w) {
			return false
		}
	}
	return true
}

// maxTagReads bounds the tracks otherAlbum reads tags from when the
// torrent has no CUE sheet.
const maxTagReads = 3

// otherAlbum: the only album of a music torrent is clearly another album
// of the same artist than the requested one. Only strong evidence counts:
// the first CUE sheet (or else track tags) that names an album (its disc
// marker and edition suffixes dropped: AlbumTitle), by the
// requested artist (or no artist), not a compilation, whose title (the
// artist prefix dropped, folded across spellings: release.FoldLatin) is in
// the request's script and matches neither the requested title, nor its
// aliases, nor the release name the admin picked (an automatic pick is no
// evidence: Job.HandPicked), and shares no word with the title
// (for a self-titled album: no word besides the artist's, and is not a
// nickname). Years are not evidence: a remaster's CUE dates the remaster.
// Various-artists requests and Job.Force skip the check.
func otherAlbum(j Job, files []SetFile, root string) (string, int, bool) {
	if j.Force || strings.TrimSpace(j.Title) == "" || variousArtists(j.Artist) {
		return "", 0, false
	}
	var cues, tracks []SetFile
	for _, f := range files {
		switch {
		case !Under(root, f.p):
		case f.ext == ".cue":
			cues = append(cues, f)
		case library.IsMusicFile(f.base) && !inSkipDir(f.dir):
			tracks = append(tracks, f)
		}
	}
	if len(cues) == 0 {
		cues = tracks[:min(len(tracks), maxTagReads)]
	}
	for _, f := range cues {
		info, ok := library.AlbumTag(f.f.FullPath)
		if !ok {
			continue
		}
		if info.Compilation || variousArtists(info.Artist) || info.Artist != "" && !sameName(info.Artist, j.Artist) {
			return "", 0, false
		}
		title := stripArtist(AlbumTitle(info.Title), j.Artist)
		if release.FoldLatin(title) == "" {
			continue
		}
		if clearlyOther(title, j) {
			return title, info.Year, true
		}
		return "", 0, false
	}
	return "", 0, false
}

// variousArtists: a compilation's artist («Various Artists», «VA», «OST»,
// «Сборник»).
func variousArtists(name string) bool {
	switch release.FoldLatin(name) {
	case "various artists", "various", "va", "v a", "ost", "soundtrack", "sbornik", "raznye ispolniteli", "raznye":
		return true
	}
	return false
}

// sameName: two spellings of one name («Кино» = «Kino», «AC/DC» = «ACDC»
// is not, «The Beatles» = «Beatles»).
func sameName(a, b string) bool {
	fa, fb := release.FoldLatin(a), release.FoldLatin(b)
	if fa == "" || fb == "" {
		return false
	}
	return fa == fb || containsWords(fa, fb) || containsWords(fb, fa)
}

func containsWords(s, sub string) bool { return strings.Contains(" "+s+" ", " "+sub+" ") }

var (
	// reDiscMark: a disc marker in a CUE or tag title: «(Disc 1)», «CD 2»,
	// «[CD1]», «- Диск 2».
	reDiscMark = regexp.MustCompile(`(?i)[\(\[]?\s*\b(?:cd|disc|disk|диск)\s*\d+\s*[\)\]]?`)
	// reBracket: a bracketed part of a title.
	reBracket = regexp.MustCompile(`[\(\[][^\(\)\[\]]*[\)\]]`)
	// reTailPart: a last " - part" / ": part" of a title.
	reTailPart = regexp.MustCompile(`\s*(?:\s[-–—]\s|:\s)([^-–—:]*)$`)
)

// AlbumTitle is a CUE or tag album title without its disc marker and
// edition suffixes: «Led Zeppelin (Deluxe Edition) [Disc 2]» → «Led
// Zeppelin», «Metallica - CD 1» → «Metallica», «The Wall: Remastered» →
// «The Wall».
func AlbumTitle(title string) string {
	t := reDiscMark.ReplaceAllString(title, " ")
	t = reBracket.ReplaceAllStringFunc(t, func(b string) string {
		if release.IsReissue(b) || strings.Trim(b, "()[] ") == "" {
			return " "
		}
		return b
	})
	for {
		m := reTailPart.FindStringSubmatchIndex(t)
		if m == nil || !release.IsReissue(t[m[2]:m[3]]) && strings.TrimSpace(t[m[2]:m[3]]) != "" {
			break
		}
		t = t[:m[0]]
	}
	t = strings.Trim(strings.Join(strings.Fields(t), " "), " -–—:/")
	if t == "" {
		return strings.TrimSpace(title)
	}
	return t
}

// stripArtist drops "<artist> - ", "<artist>: ", "<artist> / " before an
// album title.
func stripArtist(title, artist string) string {
	title = strings.TrimSpace(title)
	for _, sep := range []string{" - ", " – ", " — ", ": ", " / ", "/"} {
		if i := strings.Index(title, sep); i > 0 && sameName(title[:i], artist) {
			return strings.TrimSpace(title[i+len(sep):])
		}
	}
	return title
}

// numFolded spells numeral words in arabic ("ii" → "2").
func numFolded(s string) []string {
	words := strings.Fields(s)
	for i, w := range words {
		if n, ok := release.Numeral(w); ok {
			words[i] = n
		}
	}
	return words
}

// titlesMatch: a and b (FoldLatin) name one album — equal, or one holds the
// other plus words that are not a number («Metallica (Remastered)» is
// «Metallica»; «Led Zeppelin II» is not «Led Zeppelin»).
func titlesMatch(a, b string) bool {
	wa, wb := numFolded(a), numFolded(b)
	if len(wa) < len(wb) {
		wa, wb = wb, wa
	}
	if len(wb) == 0 {
		return false
	}
	for i := 0; i+len(wb) <= len(wa); i++ {
		if !slices.Equal(wa[i:i+len(wb)], wb) {
			continue
		}
		for _, w := range append(slices.Clone(wa[:i]), wa[i+len(wb):]...) {
			if _, n := release.Numeral(w); n {
				return false
			}
		}
		return true
	}
	return false
}

// reNickname: «Black Album», «The Blue Album», «Черный альбом» (FoldLatin).
var reNickname = regexp.MustCompile(`^(?:the )?\S+ (?:album|albom)$`)

// clearlyOther: title (from the torrent's CUE or tags) names another album
// than j's (see otherAlbum).
func clearlyOther(title string, j Job) bool {
	if release.HasCyrillic(title) != release.HasCyrillic(j.Title) {
		return false // «Gruppa krovi», «Blood Type»: a spelling or a translation
	}
	t, w, artist := release.FoldLatin(title), release.FoldLatin(j.Title), release.FoldLatin(j.Artist)
	if titlesMatch(t, w) {
		return false
	}
	for _, a := range release.UsefulAliases(j.Aliases) {
		if titlesMatch(t, release.FoldLatin(a)) {
			return false
		}
	}
	// the release name is evidence only when the admin picked it: an
	// automatic pick is what the check is there to catch
	if r := release.FoldLatin(j.Release); j.HandPicked && r != "" && (containsWords(r, t) || containsWords(r, strings.TrimPrefix(t, "the "))) {
		return false
	}
	selfTitled := true
	for _, x := range strings.Fields(w) {
		selfTitled = selfTitled && slices.Contains(strings.Fields(artist), x)
	}
	if selfTitled && reNickname.MatchString(t) {
		return false
	}
	for _, x := range strings.Fields(t) {
		if len([]rune(x)) < 3 || slices.Contains(strings.Fields(artist), x) {
			continue
		}
		if slices.Contains(strings.Fields(w), x) {
			return false // a word in common: maybe an edition's title
		}
	}
	return true
}

// titleYear is "Title (Year)", or the title alone without a year.
func titleYear(title string, year int) string {
	if year > 0 {
		return fmt.Sprintf("%s (%d)", title, year)
	}
	return title
}

// AlbumFolderName names an album's folder: the requested album keeps its
// catalog title and year (the only album of the torrent, or the one whose
// folder has its name); the others of a discography are named from their
// folders ("1985 - Мания величия" → "Мания величия (1985)").
func AlbumFolderName(j Job, root string, only bool) string {
	title, year := strings.TrimSpace(j.Title), j.Year
	folder := path.Base(root)
	if root == "." {
		folder = ""
	}
	_, parsed, pyear := library.ParseAlbumFolder(folder)
	switch {
	case title != "" && (only || release.Normalize(parsed) == release.Normalize(title)):
		if year == 0 {
			year = pyear
		}
	case parsed != "":
		title, year = parsed, pyear
	case folder != "":
		title, year = folder, pyear
	}
	name := SafeName(title)
	if year > 0 {
		name = fmt.Sprintf("%s (%d)", name, year)
	}
	return name
}

func planBook(j Job, files []SetFile) ([]placement, error) {
	count := map[string]int{}
	var books []SetFile
	var dirs []string
	for _, f := range files {
		if library.IsBookFile(f.base) {
			books = append(books, f)
			dirs = append(dirs, f.dir)
			count[library.BookExt(f.base)]++
		}
	}
	if len(books) == 0 {
		return nil, i18n.Errorf("Среди файлов нет книг в форматах, которые открывает Antresol")
	}
	collection := false
	for _, n := range count {
		collection = collection || n > 1
	}
	root := commonDir(dirs)
	dir := FolderOf(j.EbooksDir, j.Author)
	out := make([]placement, 0, len(books))
	for _, f := range books {
		name := SafeName(j.Title) + library.BookExt(f.base)
		if collection {
			// several books: each keeps its name, prefixed with its volume
			// folders («Том 1 - book.fb2»)
			var parts []string
			if sub := relTo(root, f.dir); f.dir != root && sub != "." {
				for _, c := range strings.Split(sub, "/") {
					parts = append(parts, SafeName(c))
				}
			}
			name = strings.Join(append(parts, strings.TrimLeft(f.base, ".")), " - ")
		}
		out = append(out, placement{f.f.FullPath, filepath.Join(dir, name)})
	}
	return out, nil
}

func planAudiobook(j Job, files []SetFile) ([]placement, error) {
	var dirs []string
	for _, f := range files {
		if library.IsAudiobookFile(f.base) {
			dirs = append(dirs, f.dir)
		}
	}
	if len(dirs) == 0 {
		return nil, i18n.Errorf("Среди файлов нет аудиокниги")
	}
	root := commonDir(dirs)
	dir := filepath.Join(FolderOf(j.AudiobooksDir, j.Author), SafeName(j.Title))
	var out []placement
	for _, f := range files {
		picture := library.IsPicture(f.base)
		var rel string
		switch {
		case !library.IsAudiobookFile(f.base) && !picture:
			continue
		case Under(root, f.p):
			rel = relTo(root, f.p)
		case picture && ancestorOf(f.dir, root):
			rel = f.base
		default:
			continue
		}
		if rel = libRel(rel); rel != "" {
			out = append(out, placement{f.f.FullPath, filepath.Join(dir, rel)})
		}
	}
	return out, nil
}

func planComic(j Job, files []SetFile) ([]placement, error) {
	archiveDirs := map[string]bool{}
	for _, f := range files {
		if comics.IsArchive(f.base) {
			archiveDirs[f.dir] = true
		}
	}
	// the units of the series: folders of archives, and the folders that
	// hold a volume folder of pages
	var units []string
	for d := range archiveDirs {
		units = append(units, d)
	}
	for _, f := range files {
		if comics.IsPage(f.base) && !archiveDirs[f.dir] {
			units = append(units, path.Dir(f.dir))
		}
	}
	if len(units) == 0 {
		return nil, i18n.Errorf("Среди файлов нет томов манги или комиксов (архивов или папок с картинками)")
	}
	sort.Strings(units)
	root := commonDir(units)
	dir := filepath.Join(j.ComicsDir, SafeName(j.Title))
	var out []placement
	for _, f := range files {
		keep := comics.IsArchive(f.base) || comics.IsPage(f.base) || strings.EqualFold(f.base, "ComicInfo.xml")
		if !keep || !Under(root, f.p) {
			continue
		}
		if rel := libRel(relTo(root, f.p)); rel != "" {
			out = append(out, placement{f.f.FullPath, filepath.Join(dir, rel)})
		}
	}
	return out, nil
}
