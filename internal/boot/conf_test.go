package boot

import (
	"slices"
	"testing"

	"github.com/antresol-app/antresol/addonkit"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestParseNoSeeding(t *testing.T) {
	for v, want := range map[string]bool{"": false, "off": false, "on": true} {
		forced, err := ParseNoSeeding(env(map[string]string{"ANTRESOL_TORRENTS_NO_SEEDING": v}))
		if err != nil || forced != want {
			t.Fatalf("%q: %v, %v; want %v", v, forced, err, want)
		}
	}
	if forced, err := ParseNoSeeding(env(nil)); err != nil || forced {
		t.Fatalf("unset: %v, %v", forced, err)
	}
	for _, v := range []string{"ON", "1", "true", " on", "yes", "on ", "Off", "0"} {
		forced, err := ParseNoSeeding(env(map[string]string{"ANTRESOL_TORRENTS_NO_SEEDING": v}))
		if err == nil || err.Error() != "ANTRESOL_TORRENTS_NO_SEEDING must be on or off" || forced {
			t.Fatalf("%q: %v, %v; want the error", v, forced, err)
		}
	}
}

func TestLoadConf_Defaults(t *testing.T) {
	info := addonkit.Info{MediaRoot: "/m", LibraryRoots: addonkit.LibraryRoots{Movies: "/m/movies", Series: "/m/series"}}
	c, err := LoadConf(env(nil), true, info, "/data")
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/data" || c.DownloadDir != "/m/torrents" || c.StreamDir != "/m/tmp/vault-stream" ||
		c.Port != 42069 || !c.UPnP || c.LocalOnly || c.ProwlarrDB != "/prowlarr/prowlarr.db" ||
		c.StreamCacheBytes != 20<<30 || !c.ForcedNoSeeding || c.ImportFrom != "" ||
		!slices.Equal(c.StorageDirs, []string{"/m", "/m/movies", "/m/series"}) {
		t.Fatalf("Conf = %+v", c)
	}
}

func TestLoadConf_Values(t *testing.T) {
	c, err := LoadConf(env(map[string]string{
		"ANTRESOL_TORRENTS_DIR":                  "/media/.torrents",
		"ANTRESOL_TORRENTS_STREAM_CACHE_DIR":     "cache",
		"ANTRESOL_TORRENTS_TORRENT_PORT":         "5000",
		"ANTRESOL_TORRENTS_TORRENT_UPNP":         "off",
		"ANTRESOL_TORRENTS_FLARESOLVERR_URL":     "http://fs",
		"ANTRESOL_TORRENTS_PROWLARR_DB":          "/p.db",
		"ANTRESOL_TORRENTS_QBITTORRENT_URL":      "http://qb",
		"ANTRESOL_TORRENTS_QBITTORRENT_USER":     "u",
		"ANTRESOL_TORRENTS_QBITTORRENT_PASSWORD": "p",
		"ANTRESOL_TORRENTS_STREAM_CACHE_BYTES":   "100",
		"ANTRESOL_TORRENTS_IMPORT_FROM":          "/old/vault.db",
		"ANTRESOL_TORRENTS_IMPORT_DATA_DIR":      "/old",
		"ANTRESOL_TORRENTS_LOCAL_ONLY":           "on",
	}), false, addonkit.Info{MediaRoot: "/m"}, "/data")
	if err != nil {
		t.Fatal(err)
	}
	if c.DownloadDir != "/media/.torrents" || c.StreamDir != "/m/cache" || c.Port != 5000 || c.UPnP ||
		c.FlareSolverr != "http://fs" || c.ProwlarrDB != "/p.db" || c.QBURL != "http://qb" || c.QBUser != "u" ||
		c.QBPassword != "p" || c.StreamCacheBytes != 100 || c.ImportFrom != "/old/vault.db" ||
		c.ImportDataDir != "/old" || !c.LocalOnly || c.ForcedNoSeeding || !slices.Equal(c.StorageDirs, []string{"/m"}) {
		t.Fatalf("Conf = %+v", c)
	}
}

func TestLoadConf_Bad(t *testing.T) {
	for k, v := range map[string]string{
		"ANTRESOL_TORRENTS_TORRENT_PORT":       "x",
		"ANTRESOL_TORRENTS_STREAM_CACHE_BYTES": "-1",
		"ANTRESOL_TORRENTS_LOCAL_ONLY":         "yes",
	} {
		if _, err := LoadConf(env(map[string]string{k: v}), false, addonkit.Info{}, "/data"); err == nil {
			t.Fatalf("%s=%q accepted", k, v)
		}
	}
	if _, err := LoadConf(env(nil), false, addonkit.Info{}, ""); err == nil {
		t.Fatal("no data dir accepted")
	}
}

// The old services' data is under ANTRESOL_TORRENTS_IMPORT_DIR (the old
// VAULT_IMPORT_DIR), /import when unset.
func TestLoadConf_ImportDir(t *testing.T) {
	c, err := LoadConf(env(nil), false, addonkit.Info{MediaRoot: "/m"}, "/data")
	if err != nil || c.ImportDir != "/import" {
		t.Fatalf("default %q %v", c.ImportDir, err)
	}
	c, err = LoadConf(env(map[string]string{"ANTRESOL_TORRENTS_IMPORT_DIR": "/old-services"}), false, addonkit.Info{MediaRoot: "/m"}, "/data")
	if err != nil || c.ImportDir != "/old-services" {
		t.Fatalf("set %q %v", c.ImportDir, err)
	}
}
