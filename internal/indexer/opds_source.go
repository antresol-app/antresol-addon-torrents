package indexer

import (
	"context"
	"errors"
	"fmt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/version"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/opds"
)

// OPDSDefinitionID is the synthetic catalog entry of OPDS sources: they are
// not Cardigann trackers, a source's settings name the catalog.
const OPDSDefinitionID = "opds"

// opdsEntry is the «add source» form of an OPDS catalog. allow_local is the
// admin's explicit permission to reach the home network (SSRF guard).
var opdsEntry = CatalogEntry{
	ID: OPDSDefinitionID, Name: "OPDS-каталог", Description: "Книжная библиотека по протоколу OPDS (например, Flibusta)",
	Language: "ru-RU", Type: "public",
	Settings: []SettingSpec{
		{Name: "url", Label: "Адрес каталога", Help: "Например, https://flibusta.is/opds", Required: true},
		{Name: "username", Label: "Логин", Help: "Если каталог требует вход"},
		{Name: "password", Type: "password", Label: "Пароль", Secret: true},
		allowLocalSpec,
	},
}

// allowLocalSpec lets an OPDS or direct-download source reach the home
// network.
var allowLocalSpec = SettingSpec{Name: "allow_local", Type: "checkbox", Label: "Разрешить адреса домашней сети",
	Help: "Включайте только для своего каталога дома (например, Calibre). Чужой каталог с этим разрешением сможет через Antresol обращаться к роутеру и другим устройствам вашей сети.", Advanced: true}

// maxDirectBytes bounds one direct download.
const maxDirectBytes = 500 << 20

// directTimeout bounds one direct download from start to the last byte.
var directTimeout = 10 * time.Minute

// FetchInfo describes a direct download: the file name (Content-Disposition
// or the link's path), its type and size (-1 = unknown).
type FetchInfo struct {
	Name, ContentType string
	Size              int64
}

func isOPDS(s Source) bool { return s.DefinitionID == OPDSDefinitionID }

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

var (
	opdsClientsOnce sync.Once
	opdsGuarded     *http.Client
	opdsLocal       *http.Client
)

// opdsHTTP is the client of a source: one that refuses the home network
// unless the admin allowed it (shared, so connections are reused).
func opdsHTTP(s Source) *http.Client {
	opdsClientsOnce.Do(func() {
		opdsGuarded, opdsLocal = opds.NewHTTPClient(false), opds.NewHTTPClient(true)
	})
	if truthy(s.Settings["allow_local"]) {
		return opdsLocal
	}
	return opdsGuarded
}

func opdsClient(s Source) *opds.Client {
	return &opds.Client{URL: s.Settings["url"], Username: s.Settings["username"], Password: s.Settings["password"], HTTP: opdsHTTP(s)}
}

// testOPDS reads the root feed of the catalog.
func (m *Manager) testOPDS(ctx context.Context, s Source) error {
	tctx, cancel := context.WithTimeout(ctx, m.testTimeout)
	defer cancel()
	host, err := opdsClient(s).CatalogHost(tctx)
	if err == nil {
		rememberCatalogHost(s, host)
	}
	if err != nil && ctx.Err() != nil {
		m.setStatus(ctx, s.ID, StatusUnknown, "")
		return ctx.Err()
	}
	if err != nil {
		if tctx.Err() != nil {
			err = fmt.Errorf("%w: %w", errSourceTimeout, err)
		}
		m.log.Info("indexer: OPDS source test failed", "source", s.Name, "err", err)
		m.setStatus(ctx, s.ID, StatusError, humanError(err))
		return humanize("", err)
	}
	m.setStatus(ctx, s.ID, StatusOK, "")
	return nil
}

// opdsKind: OPDS catalogs serve e-books and comics only.
func opdsKind(kind string) bool { return kind == "book" || kind == "comic" }

// directTag names a direct release's format and language for scoring
// ("EPUB RUS"): its title has neither.
type directTag = string

// searchOPDS asks the catalog for each query; every entry with a link the
// scanner of the kind opens becomes one release with the best of them
// (EPUB > FB2 > MOBI > PDF; CBZ > CBR > CB7), titled "<Автор> — <Название>".
// tags maps each release link to its directTag.
func (m *Manager) searchOPDS(ctx context.Context, s Source, q SearchRequest, queries []string) ([]Result, map[string]directTag, error) {
	sctx, cancel := context.WithTimeout(ctx, m.searchTimeout)
	defer cancel()
	c := opdsClient(s)
	order := opds.BookFormats
	if q.Kind == "comic" {
		order = opds.ComicFormats
	}
	var results []Result
	tags := map[string]directTag{}
	ok := false
	tmpl, terr := c.Template(sctx)
	host := ""
	if u, err := url.Parse(strings.TrimSpace(s.Settings["url"])); err == nil {
		host = u.Hostname()
	}
	catHost := tmpl.Host()
	if terr == nil {
		rememberCatalogHost(s, catHost)
	}
	for _, kw := range queries {
		var entries []opds.Entry
		err := terr
		if err == nil {
			entries, err = c.SearchWith(sctx, tmpl, kw)
		}
		if err != nil {
			if ctx.Err() != nil {
				return results, tags, ctx.Err()
			}
			if sctx.Err() != nil {
				err = fmt.Errorf("%w: %w", errSourceTimeout, err)
			}
			m.log.Info("indexer: OPDS search failed", "source", s.Name, "query", kw, "err", err)
			if ok {
				return results, tags, nil
			}
			m.setStatus(ctx, s.ID, StatusError, humanError(err))
			return nil, nil, humanize("", err)
		}
		ok = true
		for _, e := range entries {
			l, found := opds.Best(e.Links, order)
			if _, dup := tags[l.Href]; !found || dup || (!sameHost(l.Href, host) && !sameHost(l.Href, catHost)) {
				continue // Fetch would refuse a link to another site
			}
			title := e.Title
			if e.Author != "" {
				title = e.Author + " — " + e.Title
			}
			tag := opds.FormatOf(l.Type)
			if t := release.LangTag(release.LangCode(e.Lang)); t != "" { // dc:language
				tag += " " + t // ScoreMedia's «на русском»
			}
			tags[l.Href] = tag
			results = append(results, Result{Title: title, Link: l.Href, Details: e.Details, Size: l.Size})
		}
	}
	m.setStatus(ctx, s.ID, StatusOK, "")
	return results, tags, nil
}

func sameHost(link, host string) bool {
	u, err := url.Parse(link)
	return err == nil && host != "" && strings.EqualFold(u.Hostname(), host)
}

// scoreDirect rates a direct download as ScoreMedia rates a torrent of the
// same format (tag) and title, without the seeders: a direct link has none,
// the log scale would give 0. It gets +15 instead.
func scoreDirect(r Result, tag string, want Wanted, p Prefs) (int, []string) {
	r.Seeders, r.Leechers = 0, 0
	if tag != "" {
		r.Title += " [" + tag + "]"
	}
	p.MinSources = 0
	score, reasons := ScoreMedia(r, want.Kind, want, p)
	if score == Excluded {
		return score, reasons
	}
	return score + 15, append(reasons, "прямая загрузка")
}

// catalogHosts remembers the host each source's url redirects to (the
// catalog's host after its root feed), keyed by source id and url.
var catalogHosts sync.Map

func catalogKey(s Source) string { return fmt.Sprintf("%d\x00%s", s.ID, s.Settings["url"]) }

func rememberCatalogHost(s Source, host string) {
	if host != "" {
		catalogHosts.Store(catalogKey(s), host)
	}
}

// catalogHost is the remembered catalog host of s, else read from its root
// feed now ("" when it cannot be read).
func (m *Manager) catalogHost(ctx context.Context, s Source) string {
	if h, ok := catalogHosts.Load(catalogKey(s)); ok {
		return h.(string)
	}
	tctx, cancel := context.WithTimeout(ctx, m.searchTimeout)
	defer cancel()
	h, err := opdsClient(s).CatalogHost(tctx)
	if err != nil {
		m.log.Info("indexer: OPDS catalog host", "source", s.Name, "err", err)
		return ""
	}
	rememberCatalogHost(s, h)
	return h
}

// errForeignLink: a direct link that leaves the catalog's site.
var errForeignLink = errors.New("Ссылка ведёт на другой сайт")

// Fetch opens a direct download of an OPDS source (only http(s) links on
// the host of the source's url, with its login) or of a direct-download
// definition (fetchCardigann), up to 500 MB. The body errors past the
// limit; the caller closes it.
func (m *Manager) Fetch(ctx context.Context, sourceID int64, link string) (io.ReadCloser, FetchInfo, error) {
	s, err := m.Get(ctx, sourceID)
	if err != nil {
		return nil, FetchInfo{}, err
	}
	if m.directDef(s) {
		return m.fetchCardigann(ctx, s, link)
	}
	if !isOPDS(s) {
		return nil, FetchInfo{}, i18n.Errorf("%s: у источника нет прямых загрузок", s.Name)
	}
	base, err := url.Parse(strings.TrimSpace(s.Settings["url"]))
	if err != nil {
		return nil, FetchInfo{}, errForeignLink
	}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, FetchInfo{}, errForeignLink
	}
	if !strings.EqualFold(u.Hostname(), base.Hostname()) {
		// the catalog may live on the host its url redirects to (a mirror)
		if !strings.EqualFold(u.Hostname(), m.catalogHost(ctx, s)) {
			return nil, FetchInfo{}, errForeignLink
		}
	}
	dctx, cancel := context.WithTimeout(ctx, directTimeout)
	req, err := http.NewRequestWithContext(dctx, http.MethodGet, u.String(), nil)
	if err != nil {
		cancel()
		return nil, FetchInfo{}, errForeignLink
	}
	req.Header.Set("User-Agent", version.UserAgent)
	if user := s.Settings["username"]; user != "" && strings.EqualFold(u.Hostname(), base.Hostname()) {
		req.SetBasicAuth(user, s.Settings["password"]) // the login goes to the configured host only
	}
	// no https → http downgrade (the login would go in the clear); another
	// host is allowed (a CDN), the login is dropped there
	resp, err := opds.SameSite(opdsHTTP(s), base.Hostname(), false).Do(req)
	if err != nil {
		cancel()
		return nil, FetchInfo{}, humanize(s.Name, err)
	}
	fail := func(human error) (io.ReadCloser, FetchInfo, error) {
		resp.Body.Close()
		cancel()
		return nil, FetchInfo{}, named(s.Name, human, nil)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fail(i18n.Errorf(opds.ErrAuth.Msg))
	case resp.StatusCode != http.StatusOK:
		return fail(i18n.Errorf("каталог не отдал файл (HTTP %d)", resp.StatusCode))
	case resp.ContentLength > maxDirectBytes:
		return fail(i18n.Errorf("файл больше 500 МБ"))
	}
	info := FetchInfo{Name: fetchName(resp, u), ContentType: mediaTypeOf(resp.Header.Get("Content-Type")), Size: resp.ContentLength}
	return &limitedBody{r: resp.Body, left: maxDirectBytes, cancel: cancel}, info, nil
}

// fetchCardigann opens a release of a direct-download definition: an
// http(s) link on one of the definition's sites (or the source's mirror),
// followed through the definition's download steps to the file, which may
// live on another host (a file server of the site).
func (m *Manager) fetchCardigann(ctx context.Context, s Source, link string) (io.ReadCloser, FetchInfo, error) {
	def, err := m.cat.Definition(s.DefinitionID)
	if err != nil {
		return nil, FetchInfo{}, err
	}
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !siteHost(def.Links, s.Settings["sitelink"], u.Hostname()) {
		return nil, FetchInfo{}, errForeignLink
	}
	r, err := m.runner(s)
	if err != nil {
		return nil, FetchInfo{}, err
	}
	dctx, cancel := context.WithTimeout(ctx, directTimeout)
	resp, err := r.OpenDirect(dctx, u.String())
	if err != nil {
		cancel()
		m.log.Info("indexer: direct download failed", "source", s.Name, "err", cardigann.RedactText(err.Error()))
		return nil, FetchInfo{}, humanize(s.Name, err)
	}
	if resp.ContentLength > maxDirectBytes {
		resp.Body.Close()
		cancel()
		return nil, FetchInfo{}, named(s.Name, i18n.Errorf("файл больше 500 МБ"), nil)
	}
	info := FetchInfo{Name: fetchName(resp, resp.Request.URL), ContentType: mediaTypeOf(resp.Header.Get("Content-Type")), Size: resp.ContentLength}
	return &limitedBody{r: resp.Body, left: maxDirectBytes, cancel: cancel}, info, nil
}

// siteHost: host is the host of one of links or of the mirror sitelink.
func siteHost(links []string, sitelink, host string) bool {
	for _, l := range append([]string{sitelink}, links...) {
		if u, err := url.Parse(strings.TrimSpace(l)); err == nil && u.Hostname() != "" && strings.EqualFold(u.Hostname(), host) {
			return true
		}
	}
	return false
}

// fetchName is the file name of a download: Content-Disposition, else the
// last element of the path. Directory parts are dropped.
func fetchName(resp *http.Response, u *url.URL) string {
	name := ""
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		name = params["filename"]
	}
	if name == "" {
		name = path.Base(u.Path)
	}
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(name)
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	return name
}

func mediaTypeOf(t string) string {
	mt, _, err := mime.ParseMediaType(t)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(t))
	}
	return mt
}

// limitedBody reads at most left bytes and fails beyond them; Close ends
// the request.
type limitedBody struct {
	r      io.ReadCloser
	left   int64
	cancel context.CancelFunc
}

var errDirectTooLarge = errors.New("Файл больше 500 МБ")

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		if n, _ := b.r.Read(one[:]); n > 0 {
			return 0, errDirectTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error {
	err := b.r.Close()
	b.cancel()
	return err
}
