package cardigann

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Vault extensions to the download block, for sites that serve files
// (e-books) instead of torrents, behind a few pages:
//
//	download:
//	  direct: true                  # the release is a file, not a torrent
//	  selectors:                    # alternatives, tried in order
//	    - selector: form:has(input[value$=".epub"])
//	      submit: true              # submit the form instead of reading a link
//	      follow:                   # then on the page the form leads to:
//	        - selector: meta[http-equiv="Refresh"]
//	          attribute: content
//	          filters: [{name: regexp, args: "url=(.+)"}]
//
// submit and follow also work for torrent downloads.

// ErrNotFile: a direct download led to a web page instead of a file.
var ErrNotFile = errors.New("сайт вернул страницу вместо файла")

// followSteps applies sel to resp, then each of its Follow steps to the page
// the previous step leads to, and returns the request of the last step.
// method is the method of a link the last step reads (pages in between are
// fetched with GET). Every step carries the download block's headers.
func (r *Runner) followSteps(ctx context.Context, resp *response, sel *SelectorField, vars map[string]any, method string, headers map[string]string) (*request, error) {
	m := method
	if len(sel.Follow) > 0 {
		m = http.MethodGet
	}
	req, err := r.stepRequest(resp, sel, vars, m)
	if err == nil {
		req.headers = headers
	}
	for i := range sel.Follow {
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(req.url, "magnet:") {
			return req, nil
		}
		var page *response
		if page, err = r.fetchRelogin(ctx, req, sessionLost); err != nil {
			return nil, err
		}
		if page.status != http.StatusOK {
			return nil, fmt.Errorf("%w: %s ответил %d", ErrBadResponse, r.Def.Name, page.status)
		}
		m = http.MethodGet
		if i == len(sel.Follow)-1 {
			m = method
		}
		if req, err = r.stepRequest(page, &sel.Follow[i], vars, m); err == nil {
			req.headers = headers
		}
	}
	return req, err
}

// stepRequest is where one selector of a download page leads: its link
// (resolved against the page) or, with Submit, its submitted form.
func (r *Runner) stepRequest(resp *response, sel *SelectorField, vars map[string]any, method string) (*request, error) {
	if sel.Submit {
		el, err := r.selectElement(resp, sel, vars)
		if err != nil {
			return nil, err
		}
		return r.formRequest(resp, el)
	}
	href, err := r.matchSelector(resp, sel, vars)
	if err != nil {
		return nil, err
	}
	target, err := resolve(resp.url, href)
	if err != nil {
		return nil, err
	}
	return &request{method: method, url: target, follow: true, referer: r.base.String()}, nil
}

// formRequest is the request a browser sends submitting form: its method
// (GET by default) and action (default: the page itself), and its
// successful fields: named, enabled inputs (checked ones of checkboxes and
// radios; no buttons or files), selects (the selected option, else the
// first one) and textareas.
func (r *Runner) formRequest(resp *response, form *goquery.Selection) (*request, error) {
	if goquery.NodeName(form) != "form" {
		return nil, fmt.Errorf("%w: submit: <%s> is not a form", errNoMatch, goquery.NodeName(form))
	}
	page := resp.url.String()
	target := page
	if action := strings.TrimSpace(form.AttrOr("action", "")); action != "" {
		var err error
		if target, err = resolve(resp.url, action); err != nil {
			return nil, err
		}
	}
	var pairs []formPair
	form.Find("input[name], select[name], textarea[name]").Each(func(_ int, f *goquery.Selection) {
		if _, off := f.Attr("disabled"); off {
			return
		}
		name := f.AttrOr("name", "")
		switch goquery.NodeName(f) {
		case "input":
			value := f.AttrOr("value", "")
			switch strings.ToLower(f.AttrOr("type", "text")) {
			case "submit", "image", "button", "reset", "file":
				return
			case "checkbox", "radio":
				if _, on := f.Attr("checked"); !on {
					return
				}
				if _, ok := f.Attr("value"); !ok {
					value = "on"
				}
			}
			pairs = append(pairs, formPair{key: name, value: value})
		case "select":
			opt := f.Find("option[selected]").First()
			if opt.Length() == 0 {
				opt = f.Find("option").First()
			}
			if opt.Length() == 0 {
				return
			}
			v, ok := opt.Attr("value")
			if !ok {
				v = strings.TrimSpace(opt.Text())
			}
			pairs = append(pairs, formPair{key: name, value: v})
		case "textarea":
			pairs = append(pairs, formPair{key: name, value: f.Text()})
		}
	})
	req := &request{method: http.MethodGet, url: target, follow: true, referer: page}
	if strings.EqualFold(strings.TrimSpace(form.AttrOr("method", "")), "post") {
		req.method, req.form = http.MethodPost, pairs
		return req, nil
	}
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("bad form action %q: %w", RedactURL(target), err)
	}
	u.RawQuery, u.Fragment = encodePairs(r.enc, pairs, "&"), ""
	req.url = u.String()
	return req, nil
}

// OpenDirect opens the file of a release of a direct-download definition
// (Download.Direct): the link itself when the download block has no
// selectors, else where the first selector that leads to a file (through
// its form and follow steps) points. The response is 200 with a body that
// is not a web page; the caller closes it. The body is not limited.
func (r *Runner) OpenDirect(ctx context.Context, link string) (*http.Response, error) {
	if err := r.setup(); err != nil {
		return nil, err
	}
	d := r.Def.Download
	if d == nil || !d.Direct {
		return nil, fmt.Errorf("cardigann: %s: not a direct-download definition", r.Def.ID)
	}
	if err := r.ensureLogin(ctx); err != nil {
		return nil, err
	}
	if len(d.Selectors) == 0 {
		return r.openFile(ctx, &request{method: http.MethodGet, url: link, follow: true, referer: r.base.String()})
	}
	dl, err := r.prepareDownload(ctx, link)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i := range d.Selectors {
		sel := &d.Selectors[i]
		resp := dl.before
		if !sel.UseBeforeResponse || resp == nil {
			if resp, err = dl.page(); err != nil {
				return nil, err
			}
		}
		req, err := r.followSteps(ctx, resp, sel, dl.vars, http.MethodGet, dl.headers)
		if err == nil {
			var f *http.Response
			if f, err = r.openFile(ctx, req); err == nil {
				return f, nil
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if lastErr == nil || errors.Is(lastErr, errNoMatch) {
			lastErr = err // a selector that matched says more than one that did not
		}
	}
	return nil, fmt.Errorf("cardigann: %s: download %s: %w", r.Def.ID, RedactURL(link), lastErr)
}

// DirectIdleTimeout bounds a pause in a direct download's body: no byte
// for that long ends the transfer (a variable for tests).
var DirectIdleTimeout = 60 * time.Second

// openFile sends req and returns the response while its body is unread: a
// 200 that is not a web page (HTML, plain text, JSON: error messages). A
// Cloudflare challenge is ErrCloudflare (FlareSolverr cannot fetch files;
// the cookies it left usually pass). The client's own Timeout does not
// apply (it would cut a long transfer): ctx bounds the whole download, and
// a body that stalls for DirectIdleTimeout fails. The caller closes the
// body.
func (r *Runner) openFile(ctx context.Context, req *request) (*http.Response, error) {
	if !strings.HasPrefix(req.url, "http://") && !strings.HasPrefix(req.url, "https://") {
		return nil, fmt.Errorf("%w: %s", ErrNotFile, RedactURL(req.url))
	}
	if err := r.throttle(ctx); err != nil {
		return nil, err
	}
	fctx, cancel := context.WithCancel(ctx)
	hreq, client, err := r.httpRequest(fctx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	client.Timeout = 0
	idle := time.AfterFunc(DirectIdleTimeout, cancel) // also until the headers
	hresp, err := client.Do(hreq)
	if err != nil {
		idle.Stop()
		cancel()
		return nil, fmt.Errorf("cardigann: %s: %w", r.Def.ID, netError(err))
	}
	if hresp.StatusCode == http.StatusOK && !notFileType(hresp.Header.Get("Content-Type")) {
		idle.Reset(DirectIdleTimeout)
		hresp.Body = &idleBody{r: hresp.Body, idle: idle, cancel: cancel}
		return hresp, nil
	}
	head, _ := io.ReadAll(io.LimitReader(hresp.Body, 64<<10))
	hresp.Body.Close()
	idle.Stop()
	cancel()
	resp := &response{status: hresp.StatusCode, header: hresp.Header, body: head, url: hresp.Request.URL}
	switch {
	case isChallenge(resp):
		return nil, fmt.Errorf("%w (%s)", ErrCloudflare, r.Def.Name)
	case hresp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: %s ответил %d", ErrBadResponse, r.Def.Name, hresp.StatusCode)
	}
	return nil, fmt.Errorf("%w (%s)", ErrNotFile, r.Def.Name)
}

// idleBody restarts the idle timer on every read; Close ends the request.
type idleBody struct {
	r      io.ReadCloser
	idle   *time.Timer
	cancel context.CancelFunc
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if n > 0 {
		b.idle.Reset(DirectIdleTimeout)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.idle.Stop()
	err := b.r.Close()
	b.cancel()
	return err
}

// notFileType: a content type no book or comic is served as: web pages,
// plain text and JSON (error messages of file hosts).
func notFileType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(ct))
	}
	switch mt {
	case "text/html", "application/xhtml+xml", "text/plain", "application/json", "text/json":
		return true
	}
	return false
}
