package cardigann

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// secretParams are query parameters that carry tracker credentials in
// Cardigann definitions (search and download links).
var secretParams = map[string]bool{
	"apikey": true, "api_key": true, "api-key": true, "key": true,
	"passkey": true, "pass_key": true, "torrent_pass": true, "authkey": true, "auth_key": true,
	"rsskey": true, "token": true, "secret": true, "password": true, "pass": true,
	"cookie": true, "cookies": true, "sid": true, "session": true, "uid": true,
}

// tokenSegment matches a path segment that looks like a passkey (long hex
// or base64-ish), as in /rss/download/<id>/<passkey>.
var tokenSegment = regexp.MustCompile(`^[A-Za-z0-9_\-]{24,}$`)

const redacted = "REDACTED"

// RedactURL hides credentials in a URL for logs and messages: userinfo,
// secret query values and passkey-looking path segments.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if i := strings.IndexAny(raw, "?#"); i >= 0 {
			return raw[:i] + "?" + redacted // cannot tell secret parameters apart
		}
		return raw
	}
	if u.Host == "" {
		return raw
	}
	if u.User != nil {
		u.User = url.User(redacted)
	}
	if u.RawQuery != "" {
		q := u.Query()
		for k := range q {
			if secretParams[strings.ToLower(k)] {
				q[k] = []string{redacted}
			}
		}
		u.RawQuery = q.Encode()
	}
	if u.Path != "" {
		segs := strings.Split(u.Path, "/")
		changed := false
		for i, s := range segs {
			if tokenSegment.MatchString(s) && strings.ContainsAny(s, "0123456789") {
				segs[i] = redacted
				changed = true
			}
		}
		if changed {
			u.Path = strings.Join(segs, "/")
			u.RawPath = ""
		}
	}
	return u.String()
}

var urlInText = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"'<>]+`)

// RedactText applies RedactURL to every URL in s (error messages).
func RedactText(s string) string {
	return urlInText.ReplaceAllStringFunc(s, RedactURL)
}

// redactErr hides credentials in the URL of a *url.Error inside err (what
// http.Client.Do returns: its text contains the full request URL).
func redactErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = RedactURL(ue.URL)
	}
	return err
}
