package acquire

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

// ErrNoFormat comes with ErrNoRelease when releases of the album exist,
// but none in «Формат загрузки»: «Скачать в другом формате» (AnyFormat)
// takes them.
var ErrNoFormat = errors.New("acquire: no release in the wanted format")

// noFormatErr is «Нет варианта в нужном формате (MP3 320)».
func noFormatErr(p *indexer.Prefs) error {
	msg := "Нет варианта в нужном формате"
	if p != nil {
		msg = fmt.Sprintf("Нет варианта в нужном формате (%s)", p.MusicFormatLabel())
	}
	return wrapErr(ErrNoRelease, msg, ErrNoFormat)
}

// formatReleases: the music releases an automatic pick may take — those
// of another format left out unless anyFormat; dropped tells some were.
func formatReleases(resp indexer.SearchResponse, anyFormat bool) (rels []indexer.ScoredRelease, dropped bool) {
	if anyFormat {
		return resp.Releases, false
	}
	for _, r := range resp.Releases {
		if r.FormatMismatch {
			dropped = true
			continue
		}
		rels = append(rels, r)
	}
	return rels, dropped
}

// filesMismatch: a release whose name leaves the format or the bitrate
// out is of another format by its file list — the songs' extensions
// (.flac, .mp3) and the folders' names («MP3 320», «V0»).
func filesMismatch(title string, files []ReleaseFile, p *indexer.Prefs) bool {
	if p == nil {
		return false
	}
	a := release.ParseAudio(title)
	exts := map[string]bool{}
	var dirs []string
	for _, f := range files {
		if f.Kind != fileKindTrack {
			continue
		}
		exts[strings.ToLower(path.Ext(f.Path))] = true
		dirs = append(dirs, path.Dir(f.Path))
	}
	if a.Format == "" && len(exts) == 1 {
		switch {
		case exts[".flac"]:
			a.Format, a.Lossless = "FLAC", true
		case exts[".mp3"]:
			a.Format = "MP3"
		case exts[".ape"], exts[".wv"]:
			a.Format, a.Lossless = "APE", true
		}
	}
	if a.Format == "MP3" && a.Bitrate == 0 && a.VBR == "" {
		d := release.ParseAudio("MP3 " + strings.Join(dirs, " "))
		a.Bitrate, a.VBR = d.Bitrate, d.VBR
	}
	return indexer.MusicFit(a, *p) == indexer.FitNo
}
