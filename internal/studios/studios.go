// Package studios names the voice-over studios this addon knows beyond the
// core's own list (release.AddStudios / AddAliases): the core no longer
// ships them. Register runs at boot (boot.Build) and from the TestMain of
// every package whose tests read release names with them.
package studios

import (
	"sync"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

var once sync.Once

// Register adds the addon's studios and spellings to the release tables;
// a second call does nothing. Call it before anything parses release names.
func Register() {
	once.Do(func() {
		release.AddStudios(2, "HDRezka Studio", "hdrezka", "hd rezka", "rezka", "резка")
		release.AddAliases("LostFilm", "lostfilm.tv")
	})
}
