//go:build ignore

// Package connect reads a library from a streaming service's API after the
// admin signed in (musicimport.OAuth): one connector per service.
package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
	"github.com/antresol-app/antresol-addon-torrents/internal/musicimport"
)

// Reader reads a signed-in library into a snapshot.
type Reader interface {
	Read(ctx context.Context, tok musicimport.Token, progress func(done, total int)) (playlistimport.Snapshot, error)
}

// SpotifyScope is what Vault asks Spotify for: read-only access to liked
// songs, saved albums and the listener's playlists.
const SpotifyScope = "user-library-read playlist-read-private playlist-read-collaborative"

// Spotify limits.
const (
	spotifyPage     = 50
	spotifyMaxWait  = 60 * time.Second // a longer Retry-After gives up
	spotifyMaxRetry = 5                // 429s in a row for one request
	spotify5xxRetry = 2                // 5xx answers retried for one request
	spotifyMaxBody  = 8 << 20
)

// Spotify signs in with PKCE (no client secret) and reads liked songs,
// saved albums and playlists through the Web API. Since February 2026
// (Development Mode) Spotify gives the songs only of playlists the
// listener owns or collaborates on: others are listed unreadable.
type Spotify struct {
	ClientID    string
	HTTP        *http.Client
	BaseURL     string            // https://api.spotify.com
	AccountsURL string            // https://accounts.spotify.com
	Limiter     *metadata.Limiter // 100 ms between requests
	// Sleep waits out a 429 (nil: a timer); tests pass a fake clock.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewSpotify is the connector for a client id with Spotify's addresses.
func NewSpotify(clientID string) *Spotify {
	return &Spotify{ClientID: clientID, HTTP: &http.Client{Timeout: 30 * time.Second},
		BaseURL: "https://api.spotify.com", AccountsURL: "https://accounts.spotify.com",
		Limiter: &metadata.Limiter{Interval: 100 * time.Millisecond}}
}

// AuthorizeURL is Spotify's sign-in page (authorization code with PKCE).
func (s *Spotify) AuthorizeURL(redirectURI, state, challenge string) string {
	q := url.Values{
		"client_id":             {s.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"code_challenge_method": {"S256"},
		"code_challenge":        {challenge},
		"state":                 {state},
		"scope":                 {SpotifyScope},
	}
	return s.AccountsURL + "/authorize?" + q.Encode()
}

// Exchange trades the sign-in's code for a token. Errors never carry the
// code, the verifier or a token.
func (s *Spotify) Exchange(ctx context.Context, redirectURI, code, verifier string) (musicimport.Token, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {s.ClientID},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.AccountsURL+"/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return musicimport.Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return musicimport.Token{}, fmt.Errorf("spotify token: %w", scrub(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return musicimport.Token{}, fmt.Errorf("spotify token: %w", err)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return musicimport.Token{}, fmt.Errorf("spotify token: %d %s", resp.StatusCode, safeWord(out.Error))
	}
	tok := musicimport.Token{Access: out.AccessToken, Refresh: out.RefreshToken}
	if out.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// safeWord keeps an OAuth error code («invalid_grant») and drops anything
// else the server sent.
func safeWord(s string) string {
	for _, r := range s {
		if !(r == '_' || r >= 'a' && r <= 'z') {
			return ""
		}
	}
	return s
}

// scrub drops the URL from a transport error (its query may hold a code).
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}
	return err
}

// errForbidden: the API refused one resource (a playlist not the
// listener's); the read goes on without it.
var errForbidden = errors.New("spotify: forbidden")

// statusErr is a refused request; Is ErrConnect: the read cannot go on.
func statusErr(path string, status int, why string) error {
	return fmt.Errorf("%w (spotify %s: %d%s)", musicimport.ErrConnect, path, status, why)
}

// sleepCtx waits d, through fake when set (tests), else a timer that a
// cancelled ctx cuts short.
func sleepCtx(ctx context.Context, fake func(context.Context, time.Duration) error, d time.Duration) error {
	if fake != nil {
		return fake(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// get fetches one API path into out: through the limiter, waiting out a
// 429's Retry-After (up to a minute, a few times) and retrying a 5xx
// twice (1 s, then 2 s); 401 (the token expired or was revoked) and quota
// exhaustion end the read. 403 and 404 are errForbidden.
func (s *Spotify) get(ctx context.Context, tok musicimport.Token, path string, out any) error {
	fails := 0
	for attempt := 0; ; attempt++ {
		if err := s.Limiter.Wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok.Access)
		resp, err := s.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w (spotify %s: %v)", musicimport.ErrConnect, path, scrub(err))
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, spotifyMaxBody))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("%w (spotify %s: %v)", musicimport.ErrConnect, path, err)
		}
		switch resp.StatusCode {
		case http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("%w (spotify %s: bad answer)", musicimport.ErrConnect, path)
			}
			return nil
		case http.StatusForbidden, http.StatusNotFound:
			return errForbidden
		case http.StatusTooManyRequests:
			if bytes.Contains(body, []byte("QUOTA_EXCEEDED")) {
				return statusErr(path, resp.StatusCode, " quota exceeded")
			}
			wait := retryAfter(resp.Header.Get("Retry-After"))
			if wait > spotifyMaxWait || attempt >= spotifyMaxRetry {
				return statusErr(path, resp.StatusCode, fmt.Sprintf(" retry after %s", wait))
			}
			if err := sleepCtx(ctx, s.Sleep, wait); err != nil {
				return err
			}
		default:
			if resp.StatusCode >= 500 && fails < spotify5xxRetry {
				fails++
				if err := sleepCtx(ctx, s.Sleep, time.Duration(fails)*time.Second); err != nil {
					return err
				}
				continue
			}
			return statusErr(path, resp.StatusCode, "")
		}
	}
}

// retryAfter reads Retry-After seconds (missing or unreadable: 1 s).
func retryAfter(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return time.Second
	}
	return time.Duration(n) * time.Second
}

type spArtist struct {
	Name string `json:"name"`
}

type spAlbum struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Artists     []spArtist `json:"artists"`
	ReleaseDate string     `json:"release_date"`
}

type spTrack struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Name        string     `json:"name"`
	DurationMs  int64      `json:"duration_ms"`
	IsLocal     bool       `json:"is_local"`
	Artists     []spArtist `json:"artists"`
	Album       spAlbum    `json:"album"`
	ExternalIDs struct {
		ISRC string `json:"isrc"`
	} `json:"external_ids"`
}

type spPage[T any] struct {
	Total int  `json:"total"`
	Items *[]T `json:"items"` // nil: the field is missing (not given)
}

func first(as []spArtist) string {
	for _, a := range as {
		if n := strings.TrimSpace(a.Name); n != "" {
			return n
		}
	}
	return ""
}

func year(date string) int {
	if len(date) < 4 {
		return 0
	}
	y, _ := strconv.Atoi(date[:4])
	return y
}

func addedAt(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// library collects what a read found.
type library struct {
	snap     playlistimport.Snapshot
	at       map[string]int // track key → index in snap.Tracks
	done     int
	total    int
	progress func(done, total int)
}

func (l *library) track(t spTrack, liked bool, added *time.Time) (string, bool) {
	if t.ID == "" || t.IsLocal || (t.Type != "" && t.Type != "track") {
		return "", false
	}
	key := "spotify:" + t.ID
	if i, ok := l.at[key]; ok {
		if liked {
			l.snap.Tracks[i].Liked = true
			l.snap.Tracks[i].AddedAt = added
		}
		return key, true
	}
	st := playlistimport.SnapTrack{Key: key, Artist: first(t.Artists), Title: t.Name, Album: t.Album.Name,
		AlbumArtist: first(t.Album.Artists), ISRC: t.ExternalIDs.ISRC, DurationMs: t.DurationMs, Liked: liked}
	if liked {
		st.AddedAt = added
	}
	l.at[key] = len(l.snap.Tracks)
	l.snap.Tracks = append(l.snap.Tracks, st)
	return key, true
}

func (l *library) step(n int) {
	l.done += n
	if l.progress != nil {
		l.progress(l.done, max(l.total, l.done))
	}
}

// pages reads every page of a list (limit 50) and hands each to use; the
// first page's total is counted toward the progress. ok false: the API
// refused a page (403, 404) or gave it without items — an unreadable
// playlist; for the library's own lists use required.
func pages[T any](ctx context.Context, s *Spotify, tok musicimport.Token, l *library, path string, use func([]T)) (bool, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	for offset := 0; ; {
		var p spPage[T]
		err := s.get(ctx, tok, fmt.Sprintf("%s%slimit=%d&offset=%d", path, sep, spotifyPage, offset), &p)
		if errors.Is(err, errForbidden) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if p.Items == nil {
			return false, nil
		}
		if offset == 0 {
			l.total += p.Total
		}
		use(*p.Items)
		l.step(len(*p.Items))
		offset += len(*p.Items)
		if len(*p.Items) == 0 || offset >= p.Total {
			return true, nil
		}
		if offset > playlistimport.MaxTracks {
			return false, playlistimport.ErrTooLarge
		}
	}
}

// required reads one of the library's own lists (liked songs, albums,
// playlists): a page refused or without items fails the read (ErrConnect)
// rather than show a partial library; only a playlist's items may be
// unreadable.
func required[T any](ctx context.Context, s *Spotify, tok musicimport.Token, l *library, path string, use func([]T)) error {
	ok, err := pages(ctx, s, tok, l, path, use)
	if err == nil && !ok {
		err = fmt.Errorf("%w (spotify %s: refused or without items)", musicimport.ErrConnect, path)
	}
	return err
}

// Read reads the listener's liked songs (keys spotify:<track id>, with
// the dates they were liked), saved albums and playlists. A playlist that
// is not the listener's own or collaborative comes back without its songs
// (Readable false).
func (s *Spotify) Read(ctx context.Context, tok musicimport.Token, progress func(done, total int)) (playlistimport.Snapshot, error) {
	l := &library{snap: playlistimport.Snapshot{Service: playlistimport.ServiceSpotify}, at: map[string]int{}, progress: progress}
	var me struct {
		ID        string `json:"id"`
		AccountID string `json:"account_id"`
	}
	if err := s.get(ctx, tok, "/v1/me", &me); err != nil {
		if errors.Is(err, errForbidden) {
			err = statusErr("/v1/me", http.StatusForbidden, "") // not on the app's user list
		}
		return l.snap, err
	}
	l.snap.Account = me.AccountID
	if l.snap.Account == "" {
		l.snap.Account = me.ID
	}

	type saved struct {
		AddedAt string  `json:"added_at"`
		Track   spTrack `json:"track"`
	}
	if err := required(ctx, s, tok, l, "/v1/me/tracks", func(items []saved) {
		for _, it := range items {
			l.track(it.Track, true, addedAt(it.AddedAt))
		}
	}); err != nil {
		return l.snap, err
	}

	type savedAlbum struct {
		AddedAt string  `json:"added_at"`
		Album   spAlbum `json:"album"`
	}
	if err := required(ctx, s, tok, l, "/v1/me/albums", func(items []savedAlbum) {
		for _, it := range items {
			if it.Album.ID == "" {
				continue
			}
			l.snap.Albums = append(l.snap.Albums, playlistimport.SnapAlbum{Key: "spotify:album:" + it.Album.ID,
				Artist: first(it.Album.Artists), Title: it.Album.Name, Year: year(it.Album.ReleaseDate), AddedAt: addedAt(it.AddedAt)})
		}
	}); err != nil {
		return l.snap, err
	}

	type count struct {
		Total int `json:"total"`
	}
	type playlist struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		Collaborative bool   `json:"collaborative"`
		Owner         struct {
			ID string `json:"id"`
		} `json:"owner"`
		Items  *count `json:"items"`  // since February 2026
		Tracks *count `json:"tracks"` // before
	}
	var pls []playlist
	if err := required(ctx, s, tok, l, "/v1/me/playlists", func(items []playlist) { pls = append(pls, items...) }); err != nil {
		return l.snap, err
	}
	type entry struct {
		IsLocal bool     `json:"is_local"`
		Item    *spTrack `json:"item"`
		Track   *spTrack `json:"track"` // the deprecated alias
	}
	for _, p := range pls {
		if p.ID == "" {
			continue
		}
		sp := playlistimport.SnapPlaylist{ID: p.ID, Name: p.Name, Entries: []string{}}
		// listed without items: Spotify will not give them (not asked)
		listed := p.Items != nil || p.Tracks != nil
		if listed || p.Owner.ID == me.ID || p.Collaborative {
			// the songs join the library only once every page was read: an
			// unreadable playlist leaves none behind
			var songs []spTrack
			ok, err := pages(ctx, s, tok, l, "/v1/playlists/"+url.PathEscape(p.ID)+"/items?additional_types=track", func(items []entry) {
				for _, e := range items {
					t := e.Item
					if t == nil {
						t = e.Track
					}
					if t != nil && !e.IsLocal {
						songs = append(songs, *t)
					}
				}
			})
			if err != nil {
				return l.snap, err
			}
			sp.Readable = ok
			for _, t := range songs {
				if !ok {
					break
				}
				if key, ok := l.track(t, false, nil); ok {
					sp.Entries = append(sp.Entries, key)
				}
			}
		}
		l.snap.Playlists = append(l.snap.Playlists, sp)
		if len(l.snap.Playlists) > playlistimport.MaxPlaylists {
			return l.snap, playlistimport.ErrTooLarge
		}
	}
	if l.progress != nil {
		l.progress(l.done, l.done)
	}
	return l.snap, nil
}
