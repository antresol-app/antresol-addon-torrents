package migrate

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

// Remote is how to reach an old service over the network when its files
// are not mounted: Sonarr, Radarr and Prowlarr by address and API key,
// qBittorrent by address, user name and password.
type Remote struct {
	URL      string
	APIKey   string
	Username string
	Password string
}

// RemoteKind is what a source's connection form asks for: "apikey"
// (address and API key) or "login" (address, user name, password).
func RemoteKind(id string) string {
	switch id {
	case SourceProwlarr, SourceSonarr, SourceRadarr:
		return "apikey"
	case SourceQBittorrent:
		return "login"
	}
	return ""
}

// RemoteView is a connection as the page sees it: never the key or the
// password, only whether one is kept.
type RemoteView struct {
	Kind      string `json:"kind"`
	URL       string `json:"url"`
	Username  string `json:"username,omitempty"`
	HasSecret bool   `json:"hasSecret"`
	Connected bool   `json:"connected"`
}

// remotes keeps the connections for the migration; they are sealed into
// the addon's settings (remote_store.go) and so survive a restart and an
// addon update; they are never logged or answered.
type remotes struct {
	mu  sync.RWMutex
	m   map[string]Remote
	gen int // changes with every Set and Forget (the screen's cache key)
}

func (r *remotes) get(id string) (Remote, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.m[id]
	return v, ok
}

func (r *remotes) set(id string, v Remote) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]Remote{}
	}
	r.m[id] = v
	r.gen++
}

func (r *remotes) forget(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, id)
	r.gen++
}

func (r *remotes) generation() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.gen
}

// ErrUnknownRemote: the source cannot be reached over the network.
var ErrUnknownRemote = errors.New("migrate: the source has no API connection")

// errMounted: the files of the source are mounted; they are used.
var errMounted = errors.New("migrate: the source's files are mounted")

// ErrMounted is errMounted for the API.
var ErrMounted = errMounted

// remoteSource is a source that can be reached over its API when its
// files are not mounted (the runner's Discover asks).
type remoteSource interface {
	// overAPI: the files are not mounted and a connection is kept.
	overAPI() bool
}

// mounted: the source's files are there (they win over a connection).
func mounted(src Source) bool { return present(src.Path(), src.IsDir()) }

// Remote is the connection kept for the source id, as the page sees it,
// and false when id has no connection form or its files are mounted.
func (s *Service) Remote(id string) (RemoteView, bool) {
	kind := RemoteKind(id)
	src := s.source(id)
	if kind == "" || src == nil || mounted(src) {
		return RemoteView{}, false
	}
	v := RemoteView{Kind: kind}
	if r, ok := s.remotes.get(id); ok {
		v.URL, v.Username, v.HasSecret, v.Connected = r.URL, r.Username, r.APIKey != "" || r.Password != "", true
	}
	return v, true
}

// How Prowlarr's tracker list is reached (ProwlarrLink): its prowlarr.db
// mounted under /import, a connection over its API kept (Connect), or
// neither.
const (
	ProwlarrFile = "file"
	ProwlarrAPI  = "api"
	ProwlarrNone = "none"
)

// ProwlarrLink is how Prowlarr is reached now, for «Источники»: only the
// way, never its address, key or password. Prowlarr only lends its
// tracker list: searches ask each source directly.
func (s *Service) ProwlarrLink() string {
	src := s.source(SourceProwlarr)
	switch {
	case src == nil:
		return ProwlarrNone
	case mounted(src):
		return ProwlarrFile
	}
	if _, ok := s.remotes.get(SourceProwlarr); ok {
		return ProwlarrAPI
	}
	return ProwlarrNone
}

// ImportProwlarrAPI is «Источники» → «Импортировать из Prowlarr» over the
// Prowlarr connection kept on «Переезд» (Connect): its indexers are read
// over the API and added as the file import adds them. ok false: no
// connection is kept (the file is used). A connection problem is an
// *indexer.ImportError (its Russian message, never the key).
func (s *Service) ImportProwlarrAPI(ctx context.Context) (res []indexer.Imported, ok bool, err error) {
	if s == nil || s.remotes == nil {
		return nil, false, nil
	}
	r, kept := s.remotes.get(SourceProwlarr)
	if !kept || s.Indexer == nil {
		return nil, false, nil
	}
	res, err = prowlarrOverAPI(ctx, ArrOptions{ProwlarrAPI: &r, HTTP: s.HTTP}, s.Indexer)
	var re *RemoteError
	if errors.As(err, &re) {
		return nil, true, &indexer.ImportError{Msg: re.Msg}
	}
	return res, true, err
}

// Connect checks r against the service id (its address answers, the key
// or the login is taken) and keeps it for the migration; detail is what
// it found («312 сериалов, 40 отслеживается»). A blank key or password
// keeps the one already kept for the same address. The error is a
// *RemoteError to show, ErrUnknownRemote, ErrMounted, ErrBusy or ctx's.
func (s *Service) Connect(ctx context.Context, id string, r Remote) (string, error) {
	src := s.source(id)
	if RemoteKind(id) == "" || src == nil {
		return "", ErrUnknownRemote
	}
	if mounted(src) {
		return "", ErrMounted
	}
	if s.Running() != "" {
		return "", ErrBusy // a run reads the connection it started with
	}
	r.URL = strings.TrimRight(strings.TrimSpace(r.URL), "/")
	r.Username = strings.TrimSpace(r.Username)
	r.APIKey = strings.TrimSpace(r.APIKey)
	if old, ok := s.remotes.get(id); ok && old.URL == r.URL {
		if r.APIKey == "" {
			r.APIKey = old.APIKey
		}
		if r.Password == "" && r.Username == old.Username {
			r.Password = old.Password
		}
	}
	detail, err := s.remoteDetail(ctx, id, r)
	if err != nil {
		return "", err
	}
	s.remotes.set(id, r)
	s.persistRemotes()
	s.cache.forget()
	return detail, nil
}

// Forget drops the connection kept for id (after the move, or to enter
// another one).
func (s *Service) Forget(id string) error {
	if RemoteKind(id) == "" {
		return ErrUnknownRemote
	}
	if s.Running() != "" {
		return ErrBusy
	}
	s.remotes.forget(id)
	s.persistRemotes()
	s.cache.forget()
	return nil
}

// remoteFor is the connection a run or the screen uses for id: kept, and
// the files not mounted.
func (s *Service) remoteFor(id string) (*Remote, bool) {
	src := s.source(id)
	if src == nil || mounted(src) {
		return nil, false
	}
	r, ok := s.remotes.get(id)
	if !ok {
		return nil, false
	}
	return &r, true
}

// remoteDetail checks the connection and counts what it has.
func (s *Service) remoteDetail(ctx context.Context, id string, r Remote) (string, error) {
	switch id {
	case SourceQBittorrent:
		o := s.QB
		o.WebURL, o.Username, o.Password = r.URL, r.Username, r.Password
		if o.HTTP == nil {
			o.HTTP = s.HTTP
		}
		n, err := qbAPICount(ctx, o)
		if err != nil {
			return "", err
		}
		return i18n.Plural(ctx, n, "раздача", "раздачи", "раздач"), nil
	case SourceProwlarr:
		c := newArrClient("Prowlarr", r, s.HTTP)
		if err := c.check(ctx); err != nil {
			return "", err
		}
		list, err := c.prowlarrIndexers(ctx)
		if err != nil {
			return "", err
		}
		return i18n.Plural(ctx, len(list), "индексатор", "индексатора", "индексаторов"), nil
	case SourceSonarr:
		c := newArrClient("Sonarr", r, s.HTTP)
		if err := c.check(ctx); err != nil {
			return "", err
		}
		all, err := c.allSeries(ctx)
		if err != nil {
			return "", err
		}
		mon := 0
		for _, x := range all {
			if x.Monitored {
				mon++
			}
		}
		return i18n.T(ctx, "%s, %d отслеживается", i18n.Plural(ctx, len(all), "сериал", "сериала", "сериалов"), mon), nil
	}
	c := newArrClient("Radarr", r, s.HTTP)
	if err := c.check(ctx); err != nil {
		return "", err
	}
	all, err := c.movies(ctx)
	if err != nil {
		return "", err
	}
	mon := 0
	for _, x := range all {
		if x.Monitored {
			mon++
		}
	}
	return i18n.T(ctx, "%s, %d отслеживается", i18n.Plural(ctx, len(all), "фильм", "фильма", "фильмов"), mon), nil
}

// checkRemoteURL validates an address typed for app: http(s) with a host,
// and nothing else — no login or password in it (they would be kept,
// answered, logged and written into reports) and no query or fragment (an
// API key pasted as ?apikey=...). Those are entered in their own fields.
func checkRemoteURL(app, base string) *RemoteError {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return remoteErr(err, "Адрес %s должен начинаться с http:// или https://", app)
	}
	if u.User != nil || strings.ContainsAny(base, "?#") {
		return remoteErr(nil, "Адрес %s не должен содержать логин, пароль или параметры — их вводят в отдельных полях", app)
	}
	// the old services run on this host or in the LAN; a link-local address
	// (169.254.0.0/16, fe80::/10: a cloud's metadata service) is never one
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && (ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
		return remoteErr(nil, "Адрес %s не может быть link-local адресом (169.254.x.x, fe80::)", app)
	}
	return nil
}

// redactURL is the address as it may be shown, logged or written into a
// report: scheme, host and path only — never a login, a password, a query
// or a fragment (an address from the environment is not validated by
// checkRemoteURL). An address that does not parse shows nothing.
func redactURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/")
}

// errCrossHostRedirect: the service answered with a redirect to another
// address; the key or the password is never sent there.
var errCrossHostRedirect = errors.New("migrate: redirect to another host")

// sameHostOnly is an http.Client's CheckRedirect: a redirect may only stay on
// the host the request went to (and never go from https to http), so the
// API key header and the login form never reach another address.
func sameHostOnly(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	first := via[0].URL
	if req.URL.Host != first.Host || (first.Scheme == "https" && req.URL.Scheme != "https") {
		return errCrossHostRedirect
	}
	return nil
}

// sameHostClient is hc (a default one when nil) that follows redirects on
// the same host only.
func sameHostClient(hc *http.Client, timeout time.Duration) *http.Client {
	if hc == nil {
		hc = &http.Client{Timeout: timeout}
	}
	c := *hc
	c.CheckRedirect = sameHostOnly
	return &c
}
