// Package organize places a finished download into the library. It is
// acquisition code (full build only); the media kinds it works with are
// library.Kind*.
package organize

import "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"

// IsMediaKind: music, books, audiobooks and comics (not from TMDB, download
// only).
func IsMediaKind(kind string) bool {
	switch kind {
	case library.KindMusic, library.KindBook, library.KindAudiobook, library.KindComic:
		return true
	}
	return false
}
