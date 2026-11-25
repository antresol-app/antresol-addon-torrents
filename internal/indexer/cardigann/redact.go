package cardigann

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// secretParams are query parameters and setting names that carry tracker
// credentials in Cardigann definitions (search and download links), by their
// whole name; secretParts are what a longer name may contain (login_password,
// vb_login_password, staffpass, csrf_token, 2facode).
var secretParams = map[string]bool{
	"apikey": true, "api_key": true, "api-key": true, "key": true,
	"passkey": true, "pass_key": true, "torrent_pass": true, "authkey": true, "auth_key": true,
	"rsskey": true, "token": true, "secret": true, "password": true, "pass": true,
	"cookie": true, "cookies": true, "sid": true, "session": true, "uid": true,
	"pid": true, "pin": true, "pwd": true,
}

var secretParts = []string{"pass", "pwd", "token", "secret", "apikey", "api_key", "api-key", "authkey", "auth_key", "cookie", "2fa"}

// IsSecretName reports whether a query parameter or a setting of this name
// holds a credential (whatever its declared type).
func IsSecretName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if secretParams[n] {
		return true
	}
	for _, p := range secretParts {
		if strings.Contains(n, p) {
			return true
		}
	}
	return false
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
			if IsSecretName(k) {
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

// maxPageMessage is the most of a tracker page's own words an error keeps
// (they reach logs and the stored last error of a source).
const maxPageMessage = 200

// pageMessage is msg on one line, cut to maxPageMessage runes.
func pageMessage(msg string) string {
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); len(r) > maxPageMessage {
		return string(r[:maxPageMessage]) + "…"
	}
	return msg
}
