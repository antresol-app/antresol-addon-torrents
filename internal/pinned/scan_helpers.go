package pinned

import "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"

func ofKind(all []tracking.Source, kind string) []tracking.Source {
	var out []tracking.Source
	for _, x := range all {
		if x.Kind == kind {
			out = append(out, x)
		}
	}
	return out
}

// keepPrev adds to out the previous rows keep chooses that out lacks.
func keepPrev(out, prev []tracking.Source, keep func(tracking.Source) bool) []tracking.Source {
	type key struct {
		s, e int
		ref  string
	}
	have := map[key]bool{}
	for _, x := range out {
		have[key{x.Season, x.Episode, x.Ref}] = true
	}
	for _, x := range prev {
		k := key{x.Season, x.Episode, x.Ref}
		if keep(x) && !have[k] {
			have[k] = true
			out = append(out, x)
		}
	}
	return out
}
