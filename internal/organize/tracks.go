package organize

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/musicbrainz"
)

// reTrackNo is a track number prefix: «01 », «01. », «1-01 - », «A1 ».
var reTrackNo = regexp.MustCompile(`^\s*(?:(\d{1,2})\s*[-.]\s*(\d{1,3})|[A-Da-d](\d{1,2})|(\d{1,3}))(?:\s*[-.)_]\s*|\s+)`)

// TrackFile is a track file's number and the names its title may be.
type TrackFile struct {
	Disc, No int
	Names    []string // normalized
}

// ParseTrackFile reads a track file name: its number prefix (and disc,
// from «1-03» or a disc folder) and its title candidates: the whole name,
// without the number, and either without an «Artist - » prefix.
func ParseTrackFile(p string) TrackFile {
	base := path.Base(p)
	base = strings.TrimSuffix(base, path.Ext(base))
	var tf TrackFile
	if d := DiscFolder(path.Base(path.Dir(p))); d != "" {
		if m := reDiscPrefix.FindStringSubmatch(d); m != nil {
			tf.Disc, _ = strconv.Atoi(m[1])
		}
	}
	rest := base
	if m := reTrackNo.FindStringSubmatch(base); m != nil {
		switch {
		case m[1] != "":
			tf.Disc, _ = strconv.Atoi(m[1])
			tf.No, _ = strconv.Atoi(m[2])
		case m[3] != "":
			tf.No, _ = strconv.Atoi(m[3])
		default:
			tf.No, _ = strconv.Atoi(m[4])
		}
		rest = base[len(m[0]):]
	}
	seen := map[string]bool{}
	add := func(s string) {
		if n := SongKey(s); n != "" && !seen[n] {
			seen[n] = true
			tf.Names = append(tf.Names, n)
		}
	}
	for _, s := range []string{base, rest} {
		add(s)
		if _, after, ok := strings.Cut(s, " - "); ok {
			add(after)
		}
	}
	return tf
}

// SongKey is a song title as matched against file names: editions
// («(Remastered 2011)») dropped, normalized.
func SongKey(title string) string { return musicbrainz.Norm(musicbrainz.StripEditions(title)) }
