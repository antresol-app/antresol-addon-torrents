package coreadapt

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/musicbrainz"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/openlibrary"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/tmdb"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata/wikidata"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/version"
	"github.com/antresol-app/antresol/addonkit"
)

// Image hosts the pass-through does not proxy: the clients name images
// there and the core's image cache (Images) fetches them.
const (
	coverArtBase     = "https://coverartarchive.org"
	openLibraryCover = "https://covers.openlibrary.org"
)

// probeTimeout bounds the probe of one source.
const probeTimeout = 10 * time.Second

// Metadata is the copied metadata clients pointed at the core's
// pass-through (BaseURL = MetadataBase(source), HTTP = MetadataHTTP(), no
// key: the core adds its own). A source the core has off (its probe is
// 404) is nil. info is the core's /info (kept for the callers' wiring;
// the sources are probed, /info does not list them).
func Metadata(c *addonkit.Client, info addonkit.Info) (tm *tmdb.Client, mb *musicbrainz.Client, ol *openlibrary.Client, wd *wikidata.Client) {
	_ = info
	hc := c.MetadataHTTP()
	if on(c, hc, "tmdb") {
		tm = &tmdb.Client{BaseURL: c.MetadataBase("tmdb"), HTTP: hc}
	}
	if on(c, hc, "musicbrainz") {
		mb = &musicbrainz.Client{BaseURL: c.MetadataBase("musicbrainz"), CoverBase: coverArtBase,
			UserAgent: version.UserAgent, HTTP: hc}
	}
	if on(c, hc, "openlibrary") {
		ol = &openlibrary.Client{BaseURL: c.MetadataBase("openlibrary"), CoversBase: openLibraryCover,
			UserAgent: version.UserAgent, HTTP: hc}
	}
	if on(c, hc, "wikidata") {
		wd = &wikidata.Client{BaseURL: c.MetadataBase("wikidata"), UserAgent: version.UserAgent, HTTP: hc}
	}
	return tm, mb, ol, wd
}

// on probes source: HEAD of its root with no path is 404 when the core
// has it off and 400 when on, without asking the provider (contract,
// "Metadata pass-through"). Any other outcome keeps the client, so a
// wrong permission or a core that is down shows on the first real call.
func on(c *addonkit.Client, hc *http.Client, source string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.MetadataBase(source), nil)
	if err != nil {
		return true
	}
	resp, err := hc.Do(req)
	if err != nil {
		slog.Warn("coreadapt: metadata probe", "source", source, "err", err)
		return true
	}
	resp.Body.Close()
	return resp.StatusCode != http.StatusNotFound
}
