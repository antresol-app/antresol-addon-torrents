package organize

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// FileSeason is the season of a release's file p ("/"-separated): the code
// in its name (S03E01, S03), else the nearest folder naming exactly one
// season («Season 3», «Suits.S03.1080p»); 0 when none does, or the nearest
// folder naming seasons names several («S01-S03»).
func FileSeason(p string) int {
	if s := release.Parse(path.Base(p)).Season; s > 0 {
		return s
	}
	return FolderSeason(p)
}

// FolderSeason is the season the nearest folder of p naming seasons names
// («Season 3», «Сезон 3», «S03», «3 сезон», «Suits.S03.1080p»); 0 when no
// folder names one, or the nearest names several («S01-S03»).
func FolderSeason(p string) int {
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if lo, hi := FolderSeasons(path.Base(d)); lo > 0 {
			if lo == hi {
				return lo
			}
			return 0
		}
	}
	return 0
}

// reSeasonFolder: a folder named for its season alone («Season 3», «Сезон 3»).
var reSeasonFolder = regexp.MustCompile(`(?i)^(?:season|сезон)[\s._-]*(\d{1,2})$`)

// FolderSeasons are the seasons a folder name names (0, 0: none): lo < hi
// for a range or several.
func FolderSeasons(name string) (lo, hi int) {
	if m := reSeasonFolder.FindStringSubmatch(strings.TrimSpace(name)); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n, n
	}
	if fi := release.Parse(name); fi.Episode > 0 && !fi.IsSeasonPack {
		return 0, 0 // an episode's folder
	}
	return release.SeasonRange(name)
}
