package watch

import (
	"os"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/extaudio"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/subtitles"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
	"github.com/antresol-app/antresol/addonkit"
)

// maxSub bounds a subtitle file offered as a sidecar (the core reads at
// most this much of one: a larger one would only fail there).
const maxSub = 8 << 20

// mediaPath is the socket route of file idx of torrent ih.
func mediaPath(ih string, idx int) string { return "/media/" + ih + "/" + strconv.Itoa(idx) }

// sidecarPath is the socket route of sidecar file n of video idx.
func sidecarPath(ih string, idx, n int) string { return mediaPath(ih, idx) + "/s/" + strconv.Itoa(n) }

// sidecarFiles are the torrent files beside video among the selected ones
// (a file left out of a partial download is never read): the release's
// audio files (extaudio.FindIn), then its subtitle files
// (subtitles.FindExternalIn, those over maxSub left out), each in its
// finder's order, as the input's relay parts go (audio, then subtitles).
func sidecarFiles(video torrent.FileInfo, files []torrent.FileInfo) (audio []torrent.FileInfo, subs []subFile) {
	byPath := map[string]torrent.FileInfo{}
	var paths []string
	for _, f := range files {
		if f.Selected && f.Index != video.Index {
			byPath[f.Path] = f
			paths = append(paths, f.Path)
		}
	}
	for _, t := range extaudio.FindIn(video.Path, paths) {
		audio = append(audio, byPath[t.Path])
	}
	for _, e := range subtitles.FindExternalIn(video.Path, paths) {
		if f := byPath[e.Path]; f.Size <= maxSub {
			subs = append(subs, subFile{f, e.Lang})
		}
	}
	return audio, subs
}

type subFile struct {
	torrent.FileInfo
	lang string
}

// sidecarsOf is the input's sidecars of video idx: each served at
// /media/{ih}/{idx}/s/{n}, n its index in the torrent, named by its path
// in the torrent (the core reads language and title from the names).
func sidecarsOf(ih string, idx int, video torrent.FileInfo, files []torrent.FileInfo) addonkit.InputSidecars {
	audio, subs := sidecarFiles(video, files)
	out := addonkit.InputSidecars{Audio: []addonkit.AudioSidecar{}, Subtitles: []addonkit.SubtitleSidecar{}}
	for _, f := range audio {
		out.Audio = append(out.Audio, addonkit.AudioSidecar{Path: sidecarPath(ih, idx, f.Index), Name: f.Path, Size: f.Size})
	}
	for _, f := range subs {
		out.Subtitles = append(out.Subtitles, addonkit.SubtitleSidecar{Path: sidecarPath(ih, idx, f.Index), Name: f.Path, Lang: f.lang})
	}
	return out
}

// isSidecar: file n is one of video's sidecars.
func isSidecar(video torrent.FileInfo, files []torrent.FileInfo, n int) bool {
	audio, subs := sidecarFiles(video, files)
	for _, f := range audio {
		if f.Index == n {
			return true
		}
	}
	for _, f := range subs {
		if f.Index == n {
			return true
		}
	}
	return false
}

func statFile(p string) (os.FileInfo, error) { return os.Stat(p) }

func sameFile(a, b os.FileInfo) bool { return os.SameFile(a, b) }
