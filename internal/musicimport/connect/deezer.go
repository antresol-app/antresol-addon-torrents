//go:build ignore

package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
	"github.com/antresol-app/antresol-addon-torrents/internal/musicimport"
)

// DeezerPerms is what Vault asks Deezer for: the listener's library, and a
// token that does not expire during the read (offline_access: expires 0).
const DeezerPerms = "basic_access,offline_access"

// Deezer limits. The API allows 50 requests in 5 s per user (or per
// address without a token); over it, it answers error code 4 and Vault
// waits 5 s.
const (
	deezerPage         = 100
	deezerQuotaWait    = 5 * time.Second
	deezerQuotaRetries = 5 // code 4 answers in a row for one request
	deezer5xxRetry     = 2
	deezerMaxBody      = 8 << 20
	deezerMaxHops      = 3 // redirects of a short link
	deezerMaxLink      = 2048
)

// Deezer error codes (HTTP 200 {"error":{"type","message","code"}}).
const (
	deezerQuota      = 4
	deezerPermission = 200
	deezerBadToken   = 300
	deezerBusy       = 700
	deezerNoData     = 800
)

// Deezer signs in with OAuth 2.0 (no PKCE; the app's secret trades the
// code) and reads favourite tracks, albums and playlists through
// api.deezer.com. A public playlist is read by its link with no sign-in
// (Public), which needs neither the app id nor the secret.
type Deezer struct {
	AppID      string
	Secret     string
	HTTP       *http.Client
	BaseURL    string // https://api.deezer.com
	ConnectURL string // https://connect.deezer.com
	// LinkHTTP resolves short links (link.deezer.com);
	// redirects are never followed by it: Public checks every hop.
	LinkHTTP *http.Client
	Limiter  *metadata.Limiter // 100 ms between requests
	// Sleep waits out a quota or a 5xx (nil: a timer); tests pass a fake
	// clock.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewDeezer is the connector for an app with Deezer's addresses (an empty
// app id and secret: public playlists only).
func NewDeezer(appID, secret string) *Deezer {
	return &Deezer{AppID: appID, Secret: secret, HTTP: &http.Client{Timeout: 30 * time.Second},
		BaseURL: "https://api.deezer.com", ConnectURL: "https://connect.deezer.com",
		LinkHTTP: &http.Client{Timeout: 10 * time.Second},
		Limiter:  &metadata.Limiter{Interval: 100 * time.Millisecond}}
}

// StateOptional: Deezer's sign-in may send the browser back without the
// state; the admin's only Deezer sign-in in progress is taken then.
func (d *Deezer) StateOptional() bool { return true }

// AuthorizeURL is Deezer's sign-in page (the challenge is not used: no
// PKCE).
func (d *Deezer) AuthorizeURL(redirectURI, state, _ string) string {
	q := url.Values{
		"app_id":       {d.AppID},
		"redirect_uri": {redirectURI},
		"perms":        {DeezerPerms},
		"state":        {state},
	}
	return d.ConnectURL + "/oauth/auth.php?" + q.Encode()
}

// Exchange trades the sign-in's code for a token: JSON with output=json,
// else a query string (both are read). Errors never carry the code, the
// secret or a token.
func (d *Deezer) Exchange(ctx context.Context, _, code, _ string) (musicimport.Token, error) {
	q := url.Values{"app_id": {d.AppID}, "secret": {d.Secret}, "code": {code}, "output": {"json"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.ConnectURL+"/oauth/access_token.php?"+q.Encode(), nil)
	if err != nil {
		return musicimport.Token{}, errors.New("deezer token: bad request")
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return musicimport.Token{}, fmt.Errorf("deezer token: %w", scrub(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return musicimport.Token{}, fmt.Errorf("deezer token: %w", err)
	}
	var out struct {
		AccessToken string          `json:"access_token"`
		Expires     json.RawMessage `json:"expires"`
	}
	if json.Unmarshal(body, &out) != nil {
		// access_token=…&expires=0; «wrong code» reads as no token
		if v, err := url.ParseQuery(strings.TrimSpace(string(body))); err == nil {
			out.AccessToken, out.Expires = v.Get("access_token"), json.RawMessage(v.Get("expires"))
		}
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return musicimport.Token{}, fmt.Errorf("deezer token: %d no token", resp.StatusCode)
	}
	tok := musicimport.Token{Access: out.AccessToken}
	if n, _ := strconv.ParseInt(strings.Trim(string(out.Expires), `"`), 10, 64); n > 0 {
		tok.Expiry = time.Now().Add(time.Duration(n) * time.Second)
	}
	return tok, nil
}

// errDeezerRefused: the API refused one resource (a playlist gone, private
// or not the listener's: no data, permission, 403, 404).
var errDeezerRefused = errors.New("deezer: refused")

func deezerErr(path string, why string) error {
	return fmt.Errorf("%w (deezer %s: %s)", musicimport.ErrConnect, path, why)
}

type deezerError struct {
	Type string `json:"type"`
	Code int    `json:"code"`
}

// get fetches one API path into out (the token, if any, as access_token;
// errors name only the path): through the limiter; code 4 (the quota)
// waits 5 s and asks again, a few times; a 5xx or code 700 (busy) is
// retried twice (1 s, then 2 s); no data, permission, 403 and 404 are
// errDeezerRefused; an invalid token (300) and anything else end the read.
func (d *Deezer) get(ctx context.Context, token, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	if token != "" {
		q.Set("access_token", token)
	}
	fails, quota := 0, 0
	for {
		if err := d.Limiter.Wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.BaseURL+path+"?"+q.Encode(), nil)
		if err != nil {
			return deezerErr(path, "bad request")
		}
		resp, err := d.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return deezerErr(path, scrub(err).Error())
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, deezerMaxBody))
		resp.Body.Close()
		if err != nil {
			return deezerErr(path, "reading the answer")
		}
		var e struct {
			Error *deezerError `json:"error"`
		}
		code := 0
		switch {
		case resp.StatusCode == http.StatusOK && json.Unmarshal(body, &e) != nil:
			return deezerErr(path, "bad answer")
		case resp.StatusCode == http.StatusOK && e.Error == nil:
			if err := json.Unmarshal(body, out); err != nil {
				return deezerErr(path, "bad answer")
			}
			return nil
		case resp.StatusCode == http.StatusOK:
			code = e.Error.Code
		case resp.StatusCode == http.StatusTooManyRequests:
			code = deezerQuota
		}
		switch {
		case code == deezerQuota:
			if quota >= deezerQuotaRetries {
				return deezerErr(path, "quota exceeded")
			}
			quota++
			if err := sleepCtx(ctx, d.Sleep, deezerQuotaWait); err != nil {
				return err
			}
		case code == deezerNoData || code == deezerPermission ||
			resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w (%s %d/%d)", errDeezerRefused, path, resp.StatusCode, code)
		case (code == deezerBusy || resp.StatusCode >= 500) && fails < deezer5xxRetry:
			fails++
			if err := sleepCtx(ctx, d.Sleep, time.Duration(fails)*time.Second); err != nil {
				return err
			}
		case code == deezerBadToken:
			return deezerErr(path, "invalid token")
		default:
			return deezerErr(path, fmt.Sprintf("%d/%d", resp.StatusCode, code))
		}
	}
}

type dzArtist struct {
	Name string `json:"name"`
}

type dzTrack struct {
	ID           int64    `json:"id"`
	Title        string   `json:"title"`
	TitleVersion string   `json:"title_version"`
	Duration     int64    `json:"duration"` // seconds
	TimeAdd      int64    `json:"time_add"` // unix
	Artist       dzArtist `json:"artist"`
	Album        struct {
		Title string `json:"title"`
	} `json:"album"`
}

// title is the title with its version («Sad But True (Live)»), which
// Deezer often has in the title already.
func (t dzTrack) title() string {
	title, v := strings.TrimSpace(t.Title), strings.TrimSpace(t.TitleVersion)
	if v == "" || strings.Contains(strings.ToLower(title), strings.ToLower(v)) {
		return title
	}
	return title + " " + v
}

func unixTime(s int64) *time.Time {
	if s <= 0 {
		return nil
	}
	t := time.Unix(s, 0).UTC()
	return &t
}

// dzTrackOf is a Deezer track as a song (ok false: no id).
func dzTrackOf(t dzTrack, liked bool) (playlistimport.SnapTrack, bool) {
	if t.ID <= 0 {
		return playlistimport.SnapTrack{}, false
	}
	st := playlistimport.SnapTrack{Key: "deezer:" + strconv.FormatInt(t.ID, 10), Artist: strings.TrimSpace(t.Artist.Name), Title: t.title(),
		Album: strings.TrimSpace(t.Album.Title), DurationMs: t.Duration * 1000, Liked: liked}
	if liked {
		st.AddedAt = unixTime(t.TimeAdd)
	}
	return st, true
}

func (l *library) add(st playlistimport.SnapTrack) string {
	if i, ok := l.at[st.Key]; ok {
		if st.Liked {
			l.snap.Tracks[i].Liked, l.snap.Tracks[i].AddedAt = true, st.AddedAt
		}
		return st.Key
	}
	l.at[st.Key] = len(l.snap.Tracks)
	l.snap.Tracks = append(l.snap.Tracks, st)
	return st.Key
}

// dzList walks a list's pages (index, limit 100), each at the index its
// «next» names, and hands each page's data to use; the first page's total
// counts toward the progress. ok false: the API refused it or answered
// without data.
func dzList[T any](ctx context.Context, d *Deezer, token string, l *library, path string, use func([]T)) (bool, error) {
	seen := 0
	for index := 0; ; {
		var p struct {
			Data  *[]T   `json:"data"`
			Total int    `json:"total"`
			Next  string `json:"next"`
		}
		q := url.Values{"index": {strconv.Itoa(index)}, "limit": {strconv.Itoa(deezerPage)}}
		err := d.get(ctx, token, path, q, &p)
		if errors.Is(err, errDeezerRefused) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if p.Data == nil {
			return false, nil
		}
		if index == 0 {
			l.total += p.Total
		}
		use(*p.Data)
		l.step(len(*p.Data))
		if seen += len(*p.Data); seen > playlistimport.MaxTracks {
			return false, playlistimport.ErrTooLarge
		}
		if p.Next == "" {
			return true, nil
		}
		// only the index is taken from «next»: the address is not followed
		next, err := url.Parse(p.Next)
		n := 0
		if err == nil {
			n, err = strconv.Atoi(next.Query().Get("index"))
		}
		if err != nil || n <= index {
			return false, deezerErr(path, "bad next page")
		}
		index = n
	}
}

// dzRequired is dzList for the library's own lists: refused or without
// data fails the read rather than show a partial library.
func dzRequired[T any](ctx context.Context, d *Deezer, token string, l *library, path string, use func([]T)) error {
	ok, err := dzList(ctx, d, token, l, path, use)
	if err == nil && !ok {
		err = deezerErr(path, "refused or without data")
	}
	return err
}

// playlist reads a playlist's songs in order (with the token: the
// listener's private ones too); they join the library only once every page
// was read. ok false: refused (gone, private): no entries.
func (d *Deezer) playlist(ctx context.Context, token string, l *library, id string) ([]string, bool, error) {
	var songs []playlistimport.SnapTrack
	ok, err := dzList(ctx, d, token, l, "/playlist/"+url.PathEscape(id)+"/tracks", func(items []dzTrack) {
		for _, t := range items {
			if st, ok := dzTrackOf(t, false); ok {
				songs = append(songs, st)
			}
		}
	})
	if err != nil || !ok {
		return []string{}, false, err
	}
	entries := make([]string, 0, len(songs))
	for _, st := range songs {
		entries = append(entries, l.add(st))
	}
	return entries, true, nil
}

// Read reads the listener's favourite tracks (keys deezer:<id>, with the
// dates they were added), favourite albums and playlists. «Любимые
// треки» (is_loved_track) is the favourites list itself, not a playlist.
// ISRCs are not read (one request per track).
func (d *Deezer) Read(ctx context.Context, tok musicimport.Token, progress func(done, total int)) (playlistimport.Snapshot, error) {
	l := &library{snap: playlistimport.Snapshot{Service: playlistimport.ServiceDeezer}, at: map[string]int{}, progress: progress}
	var me struct {
		ID int64 `json:"id"`
	}
	if err := d.get(ctx, tok.Access, "/user/me", nil, &me); err != nil || me.ID <= 0 {
		if err == nil || errors.Is(err, errDeezerRefused) {
			err = deezerErr("/user/me", "refused")
		}
		return l.snap, err
	}
	l.snap.Account = strconv.FormatInt(me.ID, 10)

	if err := dzRequired(ctx, d, tok.Access, l, "/user/me/tracks", func(items []dzTrack) {
		for _, t := range items {
			if st, ok := dzTrackOf(t, true); ok {
				l.add(st)
			}
		}
	}); err != nil {
		return l.snap, err
	}

	type album struct {
		ID          int64    `json:"id"`
		Title       string   `json:"title"`
		ReleaseDate string   `json:"release_date"`
		TimeAdd     int64    `json:"time_add"`
		Artist      dzArtist `json:"artist"`
	}
	if err := dzRequired(ctx, d, tok.Access, l, "/user/me/albums", func(items []album) {
		for _, a := range items {
			if a.ID <= 0 {
				continue
			}
			l.snap.Albums = append(l.snap.Albums, playlistimport.SnapAlbum{Key: "deezer:album:" + strconv.FormatInt(a.ID, 10),
				Artist: strings.TrimSpace(a.Artist.Name), Title: strings.TrimSpace(a.Title), Year: year(a.ReleaseDate), AddedAt: unixTime(a.TimeAdd)})
		}
	}); err != nil {
		return l.snap, err
	}

	type playlist struct {
		ID           int64  `json:"id"`
		Title        string `json:"title"`
		IsLovedTrack bool   `json:"is_loved_track"`
	}
	var pls []playlist
	if err := dzRequired(ctx, d, tok.Access, l, "/user/me/playlists", func(items []playlist) { pls = append(pls, items...) }); err != nil {
		return l.snap, err
	}
	for _, p := range pls {
		if p.ID <= 0 || p.IsLovedTrack {
			continue // the favourites, read above
		}
		id := strconv.FormatInt(p.ID, 10)
		entries, ok, err := d.playlist(ctx, tok.Access, l, id)
		if err != nil {
			return l.snap, err
		}
		l.snap.Playlists = append(l.snap.Playlists, playlistimport.SnapPlaylist{ID: id, Name: p.Title, Readable: ok, Entries: entries})
		if len(l.snap.Playlists) > playlistimport.MaxPlaylists {
			return l.snap, playlistimport.ErrTooLarge
		}
	}
	if l.progress != nil {
		l.progress(l.done, l.done)
	}
	return l.snap, nil
}

// The hosts a playlist link may name (the input is untrusted): the site,
// whose path is read and never fetched, and the short-link hosts, which
// are fetched (https only) without following redirects: every hop is
// checked again.
var (
	deezerSiteHosts = map[string]bool{"deezer.com": true, "www.deezer.com": true}
	// deezer.page.link went with Firebase Dynamic Links (August 2025)
	deezerShortHosts = map[string]bool{"link.deezer.com": true}
	// /<lang>/playlist/<id> or /playlist/<id>
	deezerPlaylistPath = regexp.MustCompile(`^/(?:[a-z]{2}(?:-[a-z]{2})?/)?playlist/([0-9]{1,19})/?$`)
	// a page's <meta …> and <link …> tags, and their attributes
	htmlTag  = regexp.MustCompile(`(?i)<(meta|link)\b[^>]*>`)
	htmlAttr = regexp.MustCompile(`([A-Za-z][\w:-]*)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// deezerPageMax bounds the page of a short link that is not a redirect.
const deezerPageMax = 256 << 10

// canonicalOf is where a page says it is: its og:url, else its canonical
// link ("": neither). Nothing else on the page is read: a link to some
// other playlist in it is never taken.
func canonicalOf(page []byte) string {
	var og, canonical string
	for _, m := range htmlTag.FindAllSubmatch(page, -1) {
		attrs := map[string]string{}
		for _, a := range htmlAttr.FindAllSubmatch(m[0], -1) {
			attrs[strings.ToLower(string(a[1]))] = html.UnescapeString(string(a[2]) + string(a[3]))
		}
		switch strings.ToLower(string(m[1])) {
		case "meta":
			if og == "" && (strings.EqualFold(attrs["property"], "og:url") || strings.EqualFold(attrs["name"], "og:url")) {
				og = strings.TrimSpace(attrs["content"])
			}
		case "link":
			if canonical == "" && slices.Contains(strings.Fields(strings.ToLower(attrs["rel"])), "canonical") {
				canonical = strings.TrimSpace(attrs["href"])
			}
		}
	}
	if og != "" {
		return og
	}
	return canonical
}

// playlistID is the id of the playlist a link opens (ErrBadLink; a short
// link that cannot be reached: ErrConnect): a site link is read as is, a
// short link is resolved — its redirect, or the canonical address of its
// page — at most deezerMaxHops times, each address checked again.
func (d *Deezer) playlistID(ctx context.Context, link string) (string, error) {
	link = strings.TrimSpace(link)
	if link == "" || len(link) > deezerMaxLink {
		return "", playlistimport.ErrBadLink
	}
	u, err := url.Parse(link)
	if err != nil {
		return "", playlistimport.ErrBadLink
	}
	c := *d.LinkHTTP
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for hop := 0; ; hop++ {
		host := strings.ToLower(u.Host)
		if u.User != nil || u.Port() != "" || u.Opaque != "" {
			return "", playlistimport.ErrBadLink
		}
		switch {
		case deezerSiteHosts[host] && (u.Scheme == "https" || u.Scheme == "http"):
			m := deezerPlaylistPath.FindStringSubmatch(u.Path)
			if m == nil {
				return "", playlistimport.ErrBadLink
			}
			return m[1], nil
		case !deezerShortHosts[host] || u.Scheme != "https" || hop >= deezerMaxHops:
			return "", playlistimport.ErrBadLink
		}
		next, err := d.hop(ctx, &c, u)
		if err != nil {
			return "", err
		}
		u = next
	}
}

// hop asks a short link where it goes: its redirect target, or the
// canonical address of its page (og:url, the canonical link), resolved
// against it; the caller checks that address like any other.
func (d *Deezer) hop(ctx context.Context, c *http.Client, u *url.URL) (*url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, playlistimport.ErrBadLink
	}
	resp, err := c.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// the network, not the link: «try again», not «not found»
		return nil, fmt.Errorf("%w (deezer link: %v)", musicimport.ErrConnect, scrub(err))
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		next, err := resp.Location() // a relative one is resolved against u
		if err != nil {
			return nil, playlistimport.ErrBadLink
		}
		return next, nil
	case resp.StatusCode == http.StatusOK:
		page, err := io.ReadAll(io.LimitReader(resp.Body, deezerPageMax))
		if err != nil {
			return nil, fmt.Errorf("%w (deezer link: reading the page)", musicimport.ErrConnect)
		}
		if at := canonicalOf(page); at != "" {
			if next, err := u.Parse(at); err == nil {
				return next, nil
			}
		}
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w (deezer link: %d)", musicimport.ErrConnect, resp.StatusCode)
	}
	return nil, playlistimport.ErrBadLink
}

// FindPublic is the public playlist a link opens — www.deezer.com/<lang>/
// playlist/<id>, or a short link (link.deezer.com) — with no sign-in:
// ErrBadLink, ErrNotFound for a playlist that is gone or private, or
// ErrConnect.
func (d *Deezer) FindPublic(ctx context.Context, link string) (playlistimport.PublicPlaylist, error) {
	id, err := d.playlistID(ctx, link)
	if err != nil {
		return playlistimport.PublicPlaylist{}, err
	}
	var p struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}
	err = d.get(ctx, "", "/playlist/"+id, nil, &p)
	switch {
	case errors.Is(err, errDeezerRefused) || err == nil && p.ID <= 0:
		return playlistimport.PublicPlaylist{}, playlistimport.ErrNotFound
	case err != nil:
		return playlistimport.PublicPlaylist{}, err
	}
	return playlistimport.PublicPlaylist{ID: strconv.FormatInt(p.ID, 10), Name: strings.TrimSpace(p.Title)}, nil
}

// ReadPublic reads a public playlist's songs with no sign-in: a snapshot
// of that one playlist, nothing liked (ErrNotFound: it went private or
// away meanwhile).
func (d *Deezer) ReadPublic(ctx context.Context, p playlistimport.PublicPlaylist) (playlistimport.Snapshot, error) {
	l := &library{snap: playlistimport.Snapshot{Service: playlistimport.ServiceDeezer}, at: map[string]int{}}
	entries, ok, err := d.playlist(ctx, "", l, p.ID)
	if err != nil {
		return l.snap, err
	}
	if !ok {
		return l.snap, playlistimport.ErrNotFound
	}
	l.snap.Playlists = []playlistimport.SnapPlaylist{{ID: p.ID, Name: p.Name, Readable: true, Entries: entries}}
	return l.snap, nil
}

// Public reads the public playlist a link opens (FindPublic, then
// ReadPublic).
func (d *Deezer) Public(ctx context.Context, link string) (playlistimport.Snapshot, error) {
	p, err := d.FindPublic(ctx, link)
	if err != nil {
		return playlistimport.Snapshot{}, err
	}
	return d.ReadPublic(ctx, p)
}
