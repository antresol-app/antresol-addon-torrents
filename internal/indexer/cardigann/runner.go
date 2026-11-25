package cardigann

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding"
)

// Errors returned by Runner methods; wrap with %w and test with errors.Is.
// Messages are user-facing (Russian).
var (
	ErrLoginFailed = errors.New("не удалось войти на сайт")
	ErrCaptcha     = errors.New("сайт просит ввести капчу")
	ErrCloudflare  = errors.New("сайт закрыт защитой от ботов (Cloudflare)")
	// ErrNoSolver comes with ErrCloudflare when no FlareSolverr address is set
	ErrNoSolver    = errors.New("FlareSolverr не задан")
	ErrUnavailable = errors.New("сайт недоступен")
	// ErrLocalAddress: FlareSolverr was not asked to fetch a page of an
	// address on the local network (see Runner.SolverHostOK)
	ErrLocalAddress = errors.New("адрес сайта в домашней сети")
	ErrNotTorrent   = errors.New("сайт вернул не торрент-файл")
	ErrBadResponse  = errors.New("сайт ответил не так, как ожидалось")
)

// Runner executes one definition with one set of user settings.
// Settings keys are setting names (username, password, cookie, …); select
// values are option keys. The extra key "sitelink" overrides the base URL.
// A Runner is safe for concurrent use.
type Runner struct {
	Def          *Definition
	Settings     map[string]string
	HTTP         *http.Client // a cookie jar is added when missing
	FlareSolverr string       // base URL of FlareSolverr, e.g. http://flaresolverr:8191
	Logger       *slog.Logger // optional
	// SolverHTTP reaches FlareSolverr (default: HTTP's transport). Set it
	// when HTTP refuses the home network, where FlareSolverr usually runs.
	SolverHTTP *http.Client
	// SolverHostOK, when set, is asked whether FlareSolverr may fetch a
	// host. Its browser resolves names itself and follows redirects, past
	// the dial check HTTP has: a site that answered our guarded request
	// with a challenge must not get a LAN page fetched and parsed through
	// the solver. It is asked about the page's host before a solve and
	// about the host the solve ended on.
	SolverHostOK func(ctx context.Context, host string) bool
	// SolverGate admits one FlareSolverr request at a time; share one
	// between every runner of the process. Nil: a gate of this runner's own.
	SolverGate *SolverGate
	// OnSolve, when set, hears how each request to FlareSolverr ended: nil
	// when it solved the page, else the error (an ErrCloudflare for a
	// FlareSolverr that is unreachable, too slow or did not get through).
	// It runs on the goroutine of the request, which may be one whose asker
	// has gone, and before the next request is let through. ctx carries the
	// values of the asker's context (who asked: see indexer.Background) and
	// none of its cancellation.
	OnSolve func(ctx context.Context, err error)
	// Now is the clock the backoff of failed sign-ins counts by (nil:
	// time.Now); the sources manager passes its own, which its tests set.
	Now func() time.Time
	// Strict: redirects stop at 5 and never go from https to http (sources
	// whose page content picks the next request: direct downloads).
	Strict bool
	// OnCookies, when set, receives the session cookies ("a=b; c=d") of a
	// definition that signs in with a cookie (login.method: cookie) after
	// the site changed them with Set-Cookie, as Prowlarr keeps them: so the
	// caller can store them and a refreshed session does not go stale with
	// the configured cookie. Only cookies of signed-in responses are
	// reported, at most once per CookieInterval (later changes come with the
	// next request after it). It runs on the request's goroutine.
	OnCookies func(cookie string)

	once     sync.Once
	setupErr error
	enc      encoding.Encoding
	cats     *categoryMap
	base     *url.URL
	client   *http.Client

	mu        sync.Mutex
	loggedIn  bool
	userAgent string
	lastReq   time.Time
	loginMu   sync.Mutex
	// the backoff of failed sign-ins (see noteSignIn): how many in a row,
	// when the next automatic one may go, the last one's error
	loginFails   int
	loginRetryAt time.Time
	loginErr     error
	// the cookie session (login.method: cookie): the cookies of the last
	// signed-in response, the set last reported to OnCookies or applied at
	// sign-in (sorted name=value pairs) and when it was reported
	liveCookie string
	sentSet    string
	sentAt     time.Time
}

// CookieInterval is the least time between two OnCookies calls of a runner.
var CookieInterval = time.Minute

// Query is a search request.
type Query struct {
	Keywords   string
	Categories []int // Newznab ids
	Season     int
	Episode    int
	IMDBID     string
	Type       string // search | tvsearch | movie | music | book; derived when empty
	// Config overrides the source's settings for this search only
	// (.Config.<name> in the templates; a checkbox is "True" or "").
	Config map[string]string
}

// Result is one release.
type Result struct {
	Title, Details, Link, Magnet, InfoHash string
	Size                                   int64
	Seeders, Leechers                      int
	Category                               []int
	PublishDate                            time.Time
	DownloadVolumeFactor                   float64
	UploadVolumeFactor                     float64
}

// DownloadResult holds either a magnet link or a .torrent file.
type DownloadResult struct {
	Magnet  string
	Torrent []byte
}

func (r *Runner) setup() error {
	r.once.Do(func() {
		if r.Def == nil {
			r.setupErr = errors.New("cardigann: runner without definition")
			return
		}
		if r.SolverGate == nil {
			r.SolverGate = NewSolverGate()
		}
		if r.enc, r.setupErr = lookupEncoding(r.Def.Encoding); r.setupErr != nil {
			return
		}
		r.cats = newCategoryMap(r.Def.Caps)
		link := r.Def.Links[0]
		if s := strings.TrimSpace(r.Settings["sitelink"]); s != "" && !slices.Contains(r.Def.LegacyLinks, s) {
			link = s
		}
		if !strings.HasSuffix(link, "/") {
			link += "/"
		}
		if r.base, r.setupErr = url.Parse(link); r.setupErr != nil {
			return
		}
		switch {
		case r.HTTP == nil:
			jar, _ := cookiejar.New(nil)
			r.client = &http.Client{Jar: jar, Timeout: 60 * time.Second}
		case r.HTTP.Jar == nil:
			c := *r.HTTP
			c.Jar, _ = cookiejar.New(nil)
			r.client = &c
		default:
			r.client = r.HTTP
		}
	})
	return r.setupErr
}

func (r *Runner) log() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// Cookies returns the session cookies for the site as a Cookie header value.
func (r *Runner) Cookies() string {
	if r.setup() != nil || r.client.Jar == nil {
		return ""
	}
	var parts []string
	for _, c := range r.client.Jar.Cookies(r.base) {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// Test logs in (when the definition has a login) and runs a keyword-less
// search to check that the site answers and can be parsed.
func (r *Runner) Test(ctx context.Context) error {
	if err := r.Login(ctx); err != nil {
		return err
	}
	_, err := r.Search(ctx, Query{})
	return err
}

func (r *Runner) filterEnv(vars map[string]any) *filterEnv {
	return &filterEnv{enc: r.enc, vars: vars, now: time.Now, log: r.log()}
}

// baseVars builds template variables: .Config.*, .True, .False, .Today.Year.
func (r *Runner) baseVars() map[string]any {
	cfg := map[string]any{"sitelink": r.base.String()}
	for _, s := range r.Def.Settings {
		v, set := r.Settings[s.Name]
		switch s.Type {
		case "checkbox":
			if !set {
				v = s.Default
			}
			cfg[s.Name] = boolVar(v)
		case "select":
			cfg[s.Name] = selectValue(s, v, set)
		case "info", "info_cookie", "info_flaresolverr", "info_useragent", "info_category_8000", "cardigannCaptcha":
		default: // text, password and unknown types
			if !set {
				v = s.Default
			}
			cfg[s.Name] = v
		}
	}
	for k, v := range r.Settings {
		if _, ok := cfg[k]; !ok {
			cfg[k] = v
		}
	}
	return map[string]any{
		"Config":     cfg,
		"True":       "True",
		"False":      "",
		"Today":      map[string]any{"Year": strconv.Itoa(time.Now().Year())},
		"Query":      map[string]any{},
		"Result":     map[string]any{},
		"Keywords":   "",
		"Categories": []string{},
	}
}

func boolVar(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "on", "yes", ".true":
		return "True"
	}
	return ""
}

func selectValue(s Setting, v string, set bool) string {
	if set {
		if _, ok := s.Options.Get(v); ok {
			return v
		}
		if n, err := strconv.Atoi(v); err == nil {
			if k, ok := s.OptionKeyByIndex(n); ok {
				return k // value stored by Prowlarr as an index
			}
		}
	}
	if _, ok := s.Options.Get(s.Default); ok || len(s.Options) == 0 {
		return s.Default
	}
	return s.SortedOptionKeys()[0]
}

// Download fetches a release link: a magnet is returned as is, otherwise the
// download block (if any) is applied and the .torrent file is fetched.
func (r *Runner) Download(ctx context.Context, link string) (DownloadResult, error) {
	if strings.HasPrefix(link, "magnet:") {
		return DownloadResult{Magnet: link}, nil
	}
	if err := r.setup(); err != nil {
		return DownloadResult{}, err
	}
	if err := r.ensureLogin(ctx); err != nil {
		return DownloadResult{}, err
	}
	d := r.Def.Download
	if d != nil && d.Direct {
		return DownloadResult{}, fmt.Errorf("%w (%s): это прямая загрузка файла", ErrNotTorrent, r.Def.Name)
	}
	if d == nil {
		return r.fetchTorrent(ctx, link, http.MethodGet)
	}
	method := http.MethodGet
	if strings.EqualFold(d.Method, "post") {
		method = http.MethodPost
	}
	dl, err := r.prepareDownload(ctx, link)
	if err != nil {
		return DownloadResult{}, err
	}
	if ih := d.Infohash; ih != nil && ih.Hash != nil && ih.Title != nil {
		resp := dl.before
		if !ih.UseBeforeResponse || resp == nil {
			if resp, err = dl.page(); err != nil {
				return DownloadResult{}, err
			}
		}
		hash, err := r.matchSelector(resp, ih.Hash, dl.vars)
		if err != nil {
			return DownloadResult{}, err
		}
		title, err := r.matchSelector(resp, ih.Title, dl.vars)
		if err != nil {
			return DownloadResult{}, err
		}
		return DownloadResult{Magnet: buildMagnet(hash, title)}, nil
	}
	if len(d.Selectors) == 0 {
		return r.fetchTorrent(ctx, link, method)
	}
	var lastErr error
	for i := range d.Selectors {
		sel := &d.Selectors[i]
		resp := dl.before
		if !sel.UseBeforeResponse || resp == nil {
			if resp, err = dl.page(); err != nil {
				return DownloadResult{}, err
			}
		}
		req, err := r.followSteps(ctx, resp, sel, dl.vars, method, dl.headers)
		if err != nil {
			lastErr = err
			continue
		}
		if strings.HasPrefix(req.url, "magnet:") {
			return DownloadResult{Magnet: req.url}, nil
		}
		res, err := r.fetchTorrentReq(ctx, req)
		if err == nil {
			return res, nil
		}
		lastErr = err // try the next selector, as Prowlarr does
	}
	if lastErr == nil {
		lastErr = errors.New("download selectors matched nothing")
	}
	return DownloadResult{}, fmt.Errorf("cardigann: %s: download %s: %w", r.Def.ID, RedactURL(link), lastErr)
}

// download is a release link being resolved: the template variables, the
// details page (fetched on demand) and the response of the "before"
// request, if any.
type download struct {
	vars    map[string]any
	headers map[string]string // the download block's (or search's) headers
	page    func() (*response, error)
	before  *response
}

// prepareDownload sets up the variables of link, its details page and runs
// the download block's "before" request.
func (r *Runner) prepareDownload(ctx context.Context, link string) (*download, error) {
	u, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("cardigann: bad download link %q: %w", RedactURL(link), redactErr(err))
	}
	vars := r.baseVars()
	q := map[string]any{}
	for k, v := range u.Query() {
		q[k] = v[0]
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	vars["DownloadUri"] = map[string]any{
		"AbsoluteUri": u.String(), "AbsolutePath": u.EscapedPath(), "Scheme": u.Scheme, "Host": u.Hostname(),
		"Port": port, "PathAndQuery": u.RequestURI(), "Query": q,
	}
	d := r.Def.Download
	headers, err := r.headers(firstHeaders(d.Headers, r.Def.Search.Headers), vars)
	if err != nil {
		return nil, err
	}
	dl := &download{vars: vars, headers: headers}
	// details pages need not carry the login test element: only redirects
	// and 401/403 mean the session is gone
	dl.page = func() (*response, error) {
		return r.fetchRelogin(ctx, &request{method: http.MethodGet, url: link, headers: headers, follow: true}, sessionLost)
	}
	if b := d.Before; b != nil {
		if b.PathSelector != nil {
			resp, err := dl.page()
			if err != nil {
				return nil, err
			}
			p, err := r.matchSelector(resp, b.PathSelector, vars)
			if err != nil {
				return nil, err
			}
			b = &BeforeBlock{RequestBlock: b.RequestBlock}
			b.Path = p
		}
		if dl.before, err = r.handleRequest(ctx, b.RequestBlock, vars, link); err != nil {
			return nil, err
		}
	}
	return dl, nil
}

func sessionLost(resp *response) bool {
	return resp.isRedirect() && !strings.HasPrefix(resp.location, "magnet:") ||
		resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden
}

// notTorrent: an HTML page instead of a .torrent usually is the login page.
func notTorrent(resp *response) bool {
	if sessionLost(resp) {
		return true
	}
	if resp.status != http.StatusOK || strings.HasPrefix(resp.location, "magnet:") {
		return false
	}
	body := bytes.TrimLeft(bytes.TrimPrefix(resp.body, []byte("\xef\xbb\xbf")), " \r\n\t")
	return len(body) == 0 || body[0] != 'd'
}

func (r *Runner) fetchTorrent(ctx context.Context, link, method string) (DownloadResult, error) {
	return r.fetchTorrentReq(ctx, &request{method: method, url: link, follow: true, referer: r.base.String()})
}

func (r *Runner) fetchTorrentReq(ctx context.Context, req *request) (DownloadResult, error) {
	req.limit = maxBody
	resp, err := r.fetchRelogin(ctx, req, notTorrent)
	if err != nil {
		if errors.Is(err, errSessionLost) {
			return DownloadResult{}, fmt.Errorf("%w (%s)", ErrNotTorrent, r.Def.Name)
		}
		return DownloadResult{}, err
	}
	if strings.HasPrefix(resp.location, "magnet:") {
		return DownloadResult{Magnet: resp.location}, nil
	}
	if resp.status != http.StatusOK {
		return DownloadResult{}, fmt.Errorf("%w: %s ответил %d", ErrBadResponse, r.Def.Name, resp.status)
	}
	body := bytes.TrimLeft(bytes.TrimPrefix(resp.body, []byte("\xef\xbb\xbf")), " \r\n\t")
	if len(body) == 0 || body[0] != 'd' {
		return DownloadResult{}, fmt.Errorf("%w (%s)", ErrNotTorrent, r.Def.Name)
	}
	return DownloadResult{Torrent: resp.body}, nil
}

// handleRequest issues a templated RequestBlock (download "before").
func (r *Runner) handleRequest(ctx context.Context, rb RequestBlock, vars map[string]any, referer string) (*response, error) {
	p, err := renderTemplate(rb.Path, vars, nil)
	if err != nil {
		return nil, err
	}
	target, err := resolve(r.base, p)
	if err != nil {
		return nil, err
	}
	var pairs []formPair
	for _, in := range rb.Inputs {
		v, err := renderTemplate(in.Value, vars, nil)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, formPair{key: in.Key, value: v})
	}
	req := &request{method: http.MethodGet, url: target, referer: referer, follow: true}
	if strings.EqualFold(rb.Method, "post") {
		req.method, req.form = http.MethodPost, pairs
	} else if len(pairs) > 0 {
		req.url = appendQuery(target, encodePairs(r.enc, pairs, rb.QuerySeparator))
	}
	return r.fetchRelogin(ctx, req, sessionLost)
}

func appendQuery(u, q string) string {
	if q == "" {
		return u
	}
	if strings.Contains(u, "?") {
		if strings.HasSuffix(u, "?") || strings.HasSuffix(u, "&") {
			return u + q
		}
		return u + "&" + q
	}
	return u + "?" + q
}

func firstHeaders(hs ...map[string]StrList) map[string]StrList {
	for _, h := range hs {
		if h != nil {
			return h
		}
	}
	return nil
}

func (r *Runner) headers(h map[string]StrList, vars map[string]any) (map[string]string, error) {
	if len(h) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) == 0 {
			continue
		}
		s, err := renderTemplate(v[0], vars, nil)
		if err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, nil
}

var magnetTrackers = []string{
	"http://tracker.opentrackr.org:1337/announce",
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://exodus.desync.com:6969/announce",
	"udp://tracker.theoks.net:6969/announce",
	"udp://explodie.org:6969/announce",
}

func buildMagnet(hash, title string) string {
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:" + hash)
	if title != "" {
		b.WriteString("&dn=" + url.QueryEscape(title))
	}
	for _, t := range magnetTrackers {
		b.WriteString("&tr=" + url.QueryEscape(t))
	}
	return b.String()
}

// infoHashFromMagnet extracts the btih hash of a magnet link.
func infoHashFromMagnet(m string) string {
	for _, xt := range strings.Split(queryArgAll(m, "xt"), "\n") {
		if strings.HasPrefix(strings.ToLower(xt), "urn:btih:") {
			return xt[len("urn:btih:"):]
		}
	}
	return ""
}

func queryArgAll(link, name string) string {
	_, qs, ok := strings.Cut(link, "?")
	if !ok {
		return ""
	}
	v, _ := url.ParseQuery(qs)
	return strings.Join(v[name], "\n")
}

// normalizeHash lower-cases hex v1 info hashes (base32 ones are kept).
func normalizeHash(h string) string {
	h = strings.TrimSpace(h)
	if len(h) != 40 || strings.IndexFunc(h, func(c rune) bool {
		return !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F')
	}) >= 0 {
		return h
	}
	return strings.ToLower(h)
}

func isIP(host string) bool { return net.ParseIP(host) != nil }
