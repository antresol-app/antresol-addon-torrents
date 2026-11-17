package cardigann

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/unicode"
)

const (
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"
)

// maxBody limits a response body; larger responses are an error (a variable
// for tests).
var maxBody int64 = 32 << 20

// lookupEncoding resolves a definition charset; nil means UTF-8.
func lookupEncoding(name string) (encoding.Encoding, error) {
	e, err := htmlindex.Get(name)
	if err != nil {
		return nil, fmt.Errorf("unsupported encoding %q", name)
	}
	if e == unicode.UTF8 {
		return nil, nil
	}
	return e, nil
}

func encodeString(enc encoding.Encoding, s string) string {
	if enc == nil {
		return s
	}
	b, err := enc.NewEncoder().String(s)
	if err != nil {
		// characters the charset cannot represent: keep what we can
		b, _ = encoding.ReplaceUnsupported(enc.NewEncoder()).String(s)
	}
	return b
}

func decodeBytes(enc encoding.Encoding, b []byte) string {
	if enc == nil {
		return string(b)
	}
	s, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		return string(b)
	}
	return string(s)
}

// urlEncode is WebUtility.UrlEncode in the definition charset (space = "+").
func urlEncode(enc encoding.Encoding, s string) string { return url.QueryEscape(encodeString(enc, s)) }

func urlDecode(enc encoding.Encoding, s string) (string, error) {
	raw, err := url.QueryUnescape(s)
	if err != nil {
		return s, err
	}
	return decodeBytes(enc, []byte(raw)), nil
}

// formPair is a query/form parameter; raw pairs are already encoded ($raw).
type formPair struct {
	key, value string
	raw        bool
}

func encodePairs(enc encoding.Encoding, pairs []formPair, sep string) string {
	if sep == "" {
		sep = "&"
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if p.raw {
			parts = append(parts, p.key+"="+p.value)
			continue
		}
		parts = append(parts, urlEncode(enc, p.key)+"="+urlEncode(enc, p.value))
	}
	return strings.Join(parts, sep)
}

type request struct {
	method      string
	url         string
	form        []formPair // POST body (urlencoded)
	body        []byte     // raw POST body (multipart)
	contentType string
	headers     map[string]string
	referer     string
	follow      bool
}

type response struct {
	status   int
	header   http.Header
	body     []byte
	url      *url.URL // final URL
	location string   // redirect target when the redirect was not followed
	solved   bool     // body came from FlareSolverr and is already text
}

func (r *Runner) text(resp *response) string {
	if resp.solved {
		return string(resp.body)
	}
	return decodeBytes(r.enc, resp.body)
}

func (resp *response) isRedirect() bool { return resp.status >= 300 && resp.status < 400 }

// do sends one request, honouring the definition's request delay, and falls
// back to FlareSolverr when a Cloudflare/DDoS-Guard challenge is detected.
func (r *Runner) do(ctx context.Context, req *request) (*response, error) {
	if err := r.throttle(ctx); err != nil {
		return nil, err
	}
	hreq, client, err := r.httpRequest(ctx, req)
	if err != nil {
		return nil, err
	}
	hresp, err := client.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("cardigann: %s: %w", r.Def.ID, netError(err))
	}
	defer hresp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(hresp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("cardigann: %s: read response: %w", r.Def.ID, err)
	}
	if int64(len(data)) > maxBody {
		return nil, fmt.Errorf("%w: %s прислал слишком большой ответ (больше %d МБ)", ErrBadResponse, r.Def.Name, maxBody>>20)
	}
	resp := &response{status: hresp.StatusCode, header: hresp.Header, body: data, url: hresp.Request.URL}
	if resp.isRedirect() {
		if loc := hresp.Header.Get("Location"); loc != "" {
			if u, err := hresp.Request.URL.Parse(loc); err == nil {
				resp.location = u.String()
			} else {
				resp.location = loc
			}
		}
	}
	if isChallenge(resp) {
		if r.FlareSolverr == "" {
			return nil, fmt.Errorf("%w (%s)", ErrCloudflare, r.Def.Name)
		}
		return r.solve(ctx, req)
	}
	return resp, nil
}

// httpRequest builds the HTTP request of req (the runner's user agent,
// browser-like headers, the form) and a client that follows redirects only
// when req.follow (at most 10, http and https only; Strict: at most 5 and
// never from https to http).
func (r *Runner) httpRequest(ctx context.Context, req *request) (*http.Request, *http.Client, error) {
	var body io.Reader
	contentType := req.contentType
	if req.method == http.MethodPost {
		if req.body != nil {
			body = bytes.NewReader(req.body)
		} else {
			body = strings.NewReader(encodePairs(r.enc, req.form, "&"))
			contentType = "application/x-www-form-urlencoded"
		}
	}
	hreq, err := http.NewRequestWithContext(ctx, req.method, req.url, body)
	if err != nil {
		return nil, nil, fmt.Errorf("cardigann: bad request URL %q: %w", RedactURL(req.url), redactErr(err))
	}
	hreq.Header.Set("User-Agent", r.ua())
	hreq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,application/json;q=0.8,*/*;q=0.7")
	hreq.Header.Set("Accept-Language", "ru-RU,ru;q=0.9,en-US;q=0.8,en;q=0.7")
	if contentType != "" {
		hreq.Header.Set("Content-Type", contentType)
	}
	if req.referer != "" {
		hreq.Header.Set("Referer", req.referer)
	}
	for k, v := range req.headers {
		hreq.Header.Set(k, v)
	}
	client := *r.client
	follow := req.follow
	limit := 10
	if r.Strict {
		limit = 5
	}
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if !follow || (next.URL.Scheme != "http" && next.URL.Scheme != "https") {
			return http.ErrUseLastResponse
		}
		if len(via) >= limit {
			return errors.New("too many redirects")
		}
		if r.Strict && via[len(via)-1].URL.Scheme == "https" && next.URL.Scheme != "https" {
			return fmt.Errorf("%w: redirect from https to http", ErrBadResponse)
		}
		return nil
	}
	return hreq, &client, nil
}

// netError classifies a transport error. Its text carries the request URL,
// so credentials in it are redacted first.
func netError(err error) error {
	err = redactErr(err)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return fmt.Errorf("%w: %w", ErrUnavailable, err) // keeps a dialer guard's error
	}
	return err
}

func (r *Runner) throttle(ctx context.Context) error {
	if r.Def.RequestDelay <= 0 {
		return nil
	}
	delay := time.Duration(r.Def.RequestDelay * float64(time.Second))
	r.mu.Lock()
	wait := time.Until(r.lastReq.Add(delay))
	if wait < 0 {
		wait = 0
	}
	r.lastReq = time.Now().Add(wait)
	r.mu.Unlock()
	if wait == 0 {
		return nil
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Runner) ua() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.userAgent != "" {
		return r.userAgent
	}
	return defaultUserAgent
}

var challengeMarkers = []string{"cf-chl", "challenge-platform", "just a moment...", "cf_chl_opt", "ddos-guard", "attention required! | cloudflare", "cf-browser-verification"}

func isChallenge(resp *response) bool {
	if resp.solved || (resp.status != http.StatusForbidden && resp.status != http.StatusServiceUnavailable && resp.status != http.StatusTooManyRequests) {
		return false
	}
	server := strings.ToLower(resp.header.Get("Server"))
	head := resp.body
	if len(head) > 64<<10 {
		head = head[:64<<10]
	}
	lower := strings.ToLower(string(head))
	for _, m := range challengeMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return (strings.Contains(server, "cloudflare") || strings.Contains(server, "ddos-guard")) && resp.status != http.StatusTooManyRequests
}

type fsCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain,omitempty"`
	Path   string `json:"path,omitempty"`
}

type fsResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Solution struct {
		URL       string     `json:"url"`
		Status    int        `json:"status"`
		Response  string     `json:"response"`
		Cookies   []fsCookie `json:"cookies"`
		UserAgent string     `json:"userAgent"`
	} `json:"solution"`
}

// solve repeats req through FlareSolverr and adopts its cookies and user agent
// so that following requests pass the challenge directly.
func (r *Runner) solve(ctx context.Context, req *request) (*response, error) {
	payload := map[string]any{"cmd": "request.get", "url": req.url, "maxTimeout": 60000}
	if req.method == http.MethodPost {
		payload["cmd"] = "request.post"
		payload["postData"] = encodePairs(r.enc, req.form, "&")
	}
	target, err := url.Parse(req.url)
	if err != nil {
		return nil, err
	}
	if jar := r.client.Jar; jar != nil {
		var cs []fsCookie
		for _, c := range jar.Cookies(target) {
			cs = append(cs, fsCookie{Name: c.Name, Value: c.Value})
		}
		if len(cs) > 0 {
			payload["cookies"] = cs
		}
	}
	buf, _ := json.Marshal(payload)
	endpoint := strings.TrimRight(r.FlareSolverr, "/") + "/v1"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("cardigann: flaresolverr URL: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	fsTransport := r.client.Transport
	if r.SolverHTTP != nil {
		fsTransport = r.SolverHTTP.Transport
	}
	fsClient := &http.Client{Transport: fsTransport, Timeout: 90 * time.Second}
	hresp, err := fsClient.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("%w: FlareSolverr недоступен: %v", ErrCloudflare, err)
	}
	defer hresp.Body.Close()
	var out fsResponse
	if err := json.NewDecoder(io.LimitReader(hresp.Body, maxBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: ответ FlareSolverr не прочитан: %v", ErrCloudflare, err)
	}
	if out.Status != "ok" {
		return nil, fmt.Errorf("%w: FlareSolverr: %s", ErrCloudflare, out.Message)
	}
	sol := out.Solution
	final := target
	if u, err := url.Parse(sol.URL); err == nil && u.Host != "" {
		final = u
	}
	if r.client.Jar != nil && len(sol.Cookies) > 0 {
		var cs []*http.Cookie
		for _, c := range sol.Cookies {
			hc := &http.Cookie{Name: c.Name, Value: c.Value, Path: c.Path}
			if hc.Path == "" {
				hc.Path = "/"
			}
			if d := strings.TrimPrefix(c.Domain, "."); d != "" && net.ParseIP(d) == nil {
				hc.Domain = d
			}
			cs = append(cs, hc)
		}
		r.client.Jar.SetCookies(final, cs)
	}
	if sol.UserAgent != "" {
		r.mu.Lock()
		r.userAgent = sol.UserAgent
		r.mu.Unlock()
	}
	status := sol.Status
	if status == 0 {
		status = http.StatusOK
	}
	return &response{status: status, header: http.Header{"Content-Type": {"text/html; charset=utf-8"}}, body: []byte(sol.Response), url: final, solved: true}, nil
}

// resolve resolves a (possibly relative) link against base.
func resolve(base *url.URL, link string) (string, error) {
	link = strings.TrimSpace(link)
	u, err := url.Parse(link)
	if err != nil {
		u, err = url.Parse(strings.ReplaceAll(link, " ", "%20"))
		if err != nil {
			return "", fmt.Errorf("bad link %q: %w", link, err)
		}
	}
	return base.ResolveReference(u).String(), nil
}
