// Package boot starts the torrents addon in the order no-seeding needs:
// the setting is decided, and written to data/no-seeding.state,
// before the engine exists; NoSeed (noseed.go) owns that value ever after.
package boot

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/antresol-app/antresol/addonkit"
)

// The addon's variables: the old build's VAULT_ names with the
// prefix replaced.
const (
	envNoSeeding        = "ANTRESOL_TORRENTS_NO_SEEDING"
	envDir              = "ANTRESOL_TORRENTS_DIR"
	envStreamCacheDir   = "ANTRESOL_TORRENTS_STREAM_CACHE_DIR"
	envStreamCacheBytes = "ANTRESOL_TORRENTS_STREAM_CACHE_BYTES"
	envPort             = "ANTRESOL_TORRENTS_TORRENT_PORT"
	envUPnP             = "ANTRESOL_TORRENTS_TORRENT_UPNP"
	envFlareSolverr     = "ANTRESOL_TORRENTS_FLARESOLVERR_URL"
	envProwlarrDB       = "ANTRESOL_TORRENTS_PROWLARR_DB"
	envQBURL            = "ANTRESOL_TORRENTS_QBITTORRENT_URL"
	envQBUser           = "ANTRESOL_TORRENTS_QBITTORRENT_USER"
	envQBPassword       = "ANTRESOL_TORRENTS_QBITTORRENT_PASSWORD"
	envImportFrom       = "ANTRESOL_TORRENTS_IMPORT_FROM"
	envImportDataDir    = "ANTRESOL_TORRENTS_IMPORT_DATA_DIR"
	envImportDir        = "ANTRESOL_TORRENTS_IMPORT_DIR"
	envLocalOnly        = "ANTRESOL_TORRENTS_LOCAL_ONLY"
)

// ParseNoSeeding reads ANTRESOL_TORRENTS_NO_SEEDING exactly: unset,
// empty or "off" leave it to the setting; "on" forces it; anything else
// (ON, 1, true, " on") is an error, and main exits 1 on it before anything
// else.
func ParseNoSeeding(getenv func(string) string) (forced bool, err error) {
	switch getenv(envNoSeeding) {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	}
	return false, errors.New("ANTRESOL_TORRENTS_NO_SEEDING must be on or off")
}

// Conf is the addon's start configuration.
type Conf struct {
	DataDir, DownloadDir, StreamDir string
	Port                            int
	UPnP, LocalOnly                 bool
	FlareSolverr, ProwlarrDB        string
	QBURL, QBUser, QBPassword       string
	StreamCacheBytes                int64
	ForcedNoSeeding                 bool
	ImportFrom, ImportDataDir       string
	// ImportDir is where the old services' data is mounted read-only for
	// «Переезд» (the old VAULT_IMPORT_DIR).
	ImportDir string
	// StorageDirs: the folders that tell the media disk is mounted (the
	// media root, the movies and series roots of /info); the engine runs
	// no torrent while one is missing.
	StorageDirs []string
}

// LoadConf reads the addon's variables with the old build's
// defaults; a relative folder is taken against info.MediaRoot, as the
// old build's cfg.Abs did.
func LoadConf(getenv func(string) string, forced bool, info addonkit.Info, dataDir string) (Conf, error) {
	or := func(name, def string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return def
	}
	abs := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(info.MediaRoot, p)
	}
	c := Conf{
		DataDir:          dataDir,
		DownloadDir:      abs(or(envDir, "torrents")),
		StreamDir:        abs(or(envStreamCacheDir, "tmp/vault-stream")),
		Port:             42069,
		UPnP:             !strings.EqualFold(strings.TrimSpace(getenv(envUPnP)), "off"),
		FlareSolverr:     getenv(envFlareSolverr),
		ProwlarrDB:       or(envProwlarrDB, "/prowlarr/prowlarr.db"),
		QBURL:            getenv(envQBURL),
		QBUser:           getenv(envQBUser),
		QBPassword:       getenv(envQBPassword),
		StreamCacheBytes: 20 << 30,
		ForcedNoSeeding:  forced,
		ImportFrom:       getenv(envImportFrom),
		ImportDataDir:    getenv(envImportDataDir),
		ImportDir:        or(envImportDir, "/import"),
	}
	if dataDir == "" {
		return Conf{}, errors.New("boot: no data dir")
	}
	if v := getenv(envPort); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 65535 {
			return Conf{}, fmt.Errorf("%s: %q is not a port", envPort, v)
		}
		c.Port = n
	}
	if v := getenv(envStreamCacheBytes); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return Conf{}, fmt.Errorf("%s: %q is not a size", envStreamCacheBytes, v)
		}
		c.StreamCacheBytes = n
	}
	switch v := getenv(envLocalOnly); v {
	case "", "off":
	case "on":
		c.LocalOnly = true
	default:
		return Conf{}, fmt.Errorf("%s must be on or off", envLocalOnly)
	}
	for _, d := range []string{info.MediaRoot, info.LibraryRoots.Movies, info.LibraryRoots.Series} {
		if d != "" {
			c.StorageDirs = append(c.StorageDirs, d)
		}
	}
	return c, nil
}
