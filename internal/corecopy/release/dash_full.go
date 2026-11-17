// Code copied from the core by tools/corecopy.sh. DO NOT EDIT.

package release

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// reDashEpisode: an anime-style episode number, « - 08» or a range « - 08-09»
// («[Group] Show - 08 [1080p]», «Show - 09-10v2 (1080p)»).
var reDashEpisode = regexp.MustCompile(`(?:^|\s)-\s+(\d{1,4})(?:v\d)?(?:\s*-\s*(\d{1,4})(?:v\d)?)?(?:\s|\[|\(|$)`)

// DashEpisodes are the episode numbers an anime-style file name gives after
// « - » when Parse finds none: first..last (last = first for one episode),
// 0, 0 for none (also for a year, 1900–2099). The extension is ignored. Both the pinned releases and
// organize number files by it, so a file counted as an episode is also
// placed as one.
func DashEpisodes(name string) (first, last int) {
	base := path.Base(strings.ReplaceAll(name, `\`, "/"))
	stem := strings.TrimSuffix(base, path.Ext(base))
	m := reDashEpisode.FindStringSubmatch(stem)
	if m == nil {
		return 0, 0
	}
	first, _ = strconv.Atoi(m[1])
	if first <= 0 || len(m[1]) == 4 && first >= 1900 && first <= 2099 {
		return 0, 0 // «Planet Earth - 2006 [BDRip]»: a year, not an episode
	}
	last = first
	if m[2] != "" {
		if n, _ := strconv.Atoi(m[2]); n > first && n-first < 500 {
			last = n
		}
	}
	return first, last
}
