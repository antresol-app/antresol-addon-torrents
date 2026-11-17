package torrent

import "golang.org/x/time/rate"

// SetUploadLimit limits seeding to bps bytes per second (0 = unlimited).
// It applies at once to all torrents.
func (e *Engine) SetUploadLimit(bps int64) {
	if bps <= 0 {
		e.upLimit.SetLimit(rate.Inf)
		return
	}
	e.upLimit.SetLimit(rate.Limit(bps))
}

// UploadLimit returns the current limit in bytes per second (0 = unlimited).
func (e *Engine) UploadLimit() int64 {
	l := e.upLimit.Limit()
	if l == rate.Inf {
		return 0
	}
	return int64(l)
}
