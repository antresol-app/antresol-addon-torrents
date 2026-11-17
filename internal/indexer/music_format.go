package indexer

import (
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/releaseprefs"
)

// «Настройки» → «Музыка» → «Формат загрузки» (releaseprefs).
const (
	MusicLossless = releaseprefs.MusicLossless
	MusicMP3      = releaseprefs.MusicMP3
	MusicAny      = releaseprefs.MusicAny
)

// «Битрейт» of MP3 (releaseprefs).
const (
	Bitrate320 = releaseprefs.Bitrate320
	BitrateV0  = releaseprefs.BitrateV0
	Bitrate256 = releaseprefs.Bitrate256
	BitrateAny = releaseprefs.BitrateAny
)

// Fit is how a release's format meets the setting.
type Fit int

const (
	FitUnknown Fit = iota // the name does not tell (the file list may)
	FitYes
	FitNo
)

func (f Fit) String() string { return [...]string{"unknown", "yes", "no"}[f] }

// MusicFit: does a release (what its name or file list says, a) hold the
// wanted format? A named format or bitrate that differs is FitNo, one
// the name leaves out is FitUnknown.
func MusicFit(a release.AudioInfo, p Prefs) Fit {
	switch p.MusicFormatOrDefault() {
	case MusicAny:
		return FitYes
	case MusicLossless:
		switch {
		case a.Lossless:
			return FitYes
		case a.Format != "" || a.Bitrate > 0 || a.VBR != "":
			return FitNo
		}
		return FitUnknown
	}
	switch {
	case a.Lossless || a.Format != "" && a.Format != "MP3":
		return FitNo
	case a.Format == "" && a.Bitrate == 0 && a.VBR == "":
		return FitUnknown
	}
	switch p.MP3BitrateOrDefault() {
	case BitrateAny:
		if a.Format == "" {
			return FitUnknown
		}
		return FitYes
	case Bitrate320:
		if a.VBR != "" {
			return FitNo
		}
		if a.Bitrate == 0 {
			return FitUnknown
		}
		return pickFit(a.Bitrate >= 320)
	case BitrateV0:
		if a.VBR != "" {
			return pickFit(a.VBR == "V0")
		}
		if a.Bitrate > 0 {
			return FitNo
		}
		return FitUnknown
	default: // 256 and more
		if a.VBR != "" {
			return pickFit(a.VBR == "V0")
		}
		if a.Bitrate == 0 {
			return FitUnknown
		}
		return pickFit(a.Bitrate >= 256)
	}
}

func pickFit(ok bool) Fit {
	if ok {
		return FitYes
	}
	return FitNo
}

// formatMismatchPenalty sends a release of another format below every
// fitting one (seeders give 30 at most).
const formatMismatchPenalty = 100

// musicFormatScore: the format's points and reason of a music release.
func musicFormatScore(a release.AudioInfo, p Prefs) (int, []string) {
	label := a.Label()
	var quality int // the better of other formats first
	switch {
	case a.Lossless:
		quality = 30
	case a.Bitrate >= 320 || a.VBR == "V0":
		quality = 20
	case a.Format != "":
		quality = 5
	}
	switch fit := MusicFit(a, p); {
	case fit == FitNo:
		return quality - formatMismatchPenalty, []string{"не тот формат", label}
	case fit == FitUnknown:
		return 0, []string{"формат не указан"}
	case p.MusicFormatOrDefault() != MusicAny:
		quality = 30
	}
	if label == "" {
		return quality, nil
	}
	return quality, []string{label}
}
