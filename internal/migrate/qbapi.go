package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// qbClient talks to the qBittorrent Web API. The only writes it can make
// are stop/pause and start/resume of a single infohash.
type qbClient struct {
	base        string
	user, pass  string
	http        *http.Client
	cookies     []*http.Cookie // from the login (SID)
	stopTimeout time.Duration
	poll        time.Duration
}

func newQBClient(o QBOptions) *qbClient {
	c := &qbClient{
		base:        strings.TrimRight(o.WebURL, "/"),
		user:        o.Username,
		pass:        o.Password,
		http:        o.HTTP,
		stopTimeout: o.StopTimeout,
		poll:        500 * time.Millisecond,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 30 * time.Second}
	}
	if c.stopTimeout <= 0 {
		c.stopTimeout = 10 * time.Second
	}
	return c
}

// do sends one request; form != nil makes it a form POST. It returns the
// status code and up to 1 MiB of the body. With a user set, a 403 (the
// session expired, e.g. during a long hash check) logs in again once and
// repeats the request.
func (c *qbClient) do(ctx context.Context, method, path string, query, form url.Values) (int, []byte, *http.Response, error) {
	code, b, resp, err := c.send(ctx, method, path, query, form)
	if err == nil && code == http.StatusForbidden && c.user != "" && path != loginPath {
		if lerr := c.login(ctx); lerr != nil {
			return code, b, resp, nil
		}
		return c.send(ctx, method, path, query, form)
	}
	return code, b, resp, err
}

func (c *qbClient) send(ctx context.Context, method, path string, query, form url.Values) (int, []byte, *http.Response, error) {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return 0, nil, nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	// Matches the Host sent, so qBittorrent's CSRF check passes.
	req.Header.Set("Referer", c.base+"/")
	for _, ck := range c.cookies {
		req.AddCookie(&http.Cookie{Name: ck.Name, Value: ck.Value})
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, resp, err
	}
	return resp.StatusCode, b, resp, nil
}

func statusErr(path string, code int) error {
	return fmt.Errorf("qBittorrent ответил %d на %s", code, path)
}

// login signs in when a user name is set (otherwise qBittorrent is expected
// to let this address in without one).
func (c *qbClient) login(ctx context.Context) error {
	if c.user == "" {
		return nil
	}
	const path = loginPath
	code, b, resp, err := c.send(ctx, http.MethodPost, path, nil, url.Values{"username": {c.user}, "password": {c.pass}})
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return statusErr(path, code)
	}
	if strings.TrimSpace(string(b)) != "Ok." {
		return fmt.Errorf("qBittorrent не принял имя пользователя или пароль")
	}
	c.cookies = resp.Cookies()
	return nil
}

func (c *qbClient) version(ctx context.Context) (string, error) {
	const path = "/api/v2/app/version"
	code, b, _, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", statusErr(path, code)
	}
	return strings.TrimSpace(string(b)), nil
}

// post sends a v5 action, falling back to its v4 name on 404.
func (c *qbClient) post(ctx context.Context, ih, v5, v4 string) error {
	form := url.Values{"hashes": {ih}}
	path := "/api/v2/torrents/" + v5
	code, _, _, err := c.do(ctx, http.MethodPost, path, nil, form)
	if err == nil && code == http.StatusNotFound {
		path = "/api/v2/torrents/" + v4
		code, _, _, err = c.do(ctx, http.MethodPost, path, nil, form)
	}
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return statusErr(path, code)
	}
	return nil
}

// stop stops (v5) or pauses (v4) one torrent.
func (c *qbClient) stop(ctx context.Context, ih string) error {
	return c.post(ctx, ih, "stop", "pause")
}

// start starts (v5) or resumes (v4) one torrent.
func (c *qbClient) start(ctx context.Context, ih string) error {
	return c.post(ctx, ih, "start", "resume")
}

const loginPath = "/api/v2/auth/login"

// errNotInQB: qBittorrent does not have the torrent (any more).
var errNotInQB = errors.New("qBittorrent: раздачи нет")

// qbBusyStates are states in which a torrent is not safe to move: still
// downloading, being checked or moved, or broken (the fastresume on disk
// is saved only now and then and may be older than this).
var qbBusyStates = map[string]bool{
	"downloading": true, "forcedDL": true, "stalledDL": true, "queuedDL": true,
	"metaDL": true, "forcedMetaDL": true, "allocating": true,
	"checkingUP": true, "checkingDL": true, "checkingResumeData": true,
	"moving": true, "error": true, "missingFiles": true, "unknown": true,
}

var qbStoppedStates = map[string]bool{"stoppedUP": true, "pausedUP": true, "stoppedDL": true, "pausedDL": true}

// state returns the torrent's qBittorrent state; found is false when
// qBittorrent does not have it.
func (c *qbClient) state(ctx context.Context, ih string) (state string, found bool, err error) {
	const path = "/api/v2/torrents/info"
	code, b, _, err := c.do(ctx, http.MethodGet, path, url.Values{"hashes": {ih}}, nil)
	if err != nil {
		return "", false, err
	}
	if code != http.StatusOK {
		return "", false, statusErr(path, code)
	}
	var list []struct {
		Hash  string `json:"hash"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return "", false, fmt.Errorf("qBittorrent: непонятный ответ на %s: %w", path, err)
	}
	for _, t := range list {
		if strings.EqualFold(t.Hash, ih) {
			return t.State, true, nil
		}
	}
	return "", false, nil
}

// stopped polls the torrent's state until qBittorrent reports it stopped
// (true) or StopTimeout passes (false). A torrent qBittorrent does not
// have is not stopped (false with errNotInQB).
func (c *qbClient) stopped(ctx context.Context, ih string) (bool, error) {
	deadline := time.Now().Add(c.stopTimeout)
	for {
		st, found, err := c.state(ctx, ih)
		if err != nil {
			return false, err
		}
		if !found {
			return false, errNotInQB
		}
		if qbStoppedStates[st] {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(c.poll):
		}
	}
}
