package opds

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// NewHTTPClient returns the client OPDS sources are fetched with. The
// address check runs on the connected IP (after DNS, on every redirect),
// so a name that resolves into the home network is refused as well; with
// allowPrivate the admin's home network is reachable, link-local and the
// other denied ranges never. Only http and https redirects are followed,
// at most 5; Go drops the Authorization header when a redirect leaves the
// host. Responses must start within 30 s; the caller's context bounds the
// whole request.
func NewHTTPClient(allowPrivate bool) *http.Client {
	tr := addonkit.NewGuardedTransport(allowPrivate)
	if allowPrivate {
		// the admin pointed us at the home network: a proxy of the
		// environment may be what reaches it
		tr.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{
		Transport: privateErr{tr},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("opds: too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return &Error{Msg: ErrBadURL.Msg, kind: ErrBadURL}
			}
			return nil
		},
	}
}

// privateErr says what the guard's refusal means to an OPDS source: the
// address leads into the home network (ErrPrivate, with its message).
type privateErr struct{ http.RoundTripper }

func (t privateErr) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if errors.Is(err, addonkit.ErrBlocked) {
		err = &Error{Msg: ErrPrivate.Msg, Err: err, kind: ErrPrivate}
	}
	return resp, err
}

// withTimeout bounds one feed request.
func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 30*time.Second)
}

// errDowngrade: a redirect from https to http would send the login in the
// clear.
var errDowngrade = errors.New("opds: redirect from https to http")

// SameSite returns c with a stricter redirect policy: never from https to
// http, and the login (Authorization) only on hops to credHost — Go would
// keep it on a subdomain (example.org → www.example.org). When sameHost,
// no hop may leave credHost at all. Every other rule of c stays.
func SameSite(c *http.Client, credHost string, sameHost bool) *http.Client {
	cc := *c
	base := c.CheckRedirect
	cc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if base != nil {
			if err := base(req, via); err != nil {
				return err
			}
		}
		if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errDowngrade
		}
		if !strings.EqualFold(req.URL.Hostname(), credHost) {
			if sameHost {
				return ErrForeignHost
			}
			req.Header.Del("Authorization")
		}
		return nil
	}
	return &cc
}
