//go:build ignore

package connect

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
	"github.com/antresol-app/antresol-addon-torrents/internal/musicimport"
)

// Yandex limits. Yandex publishes no rate limit: 2 requests a second, and
// a 429 or 5xx waits 5 s, three times.
const (
	yandexBatch   = 100 // songs per POST /tracks
	yandexBackoff = 5 * time.Second
	yandexRetries = 3
	yandexMaxBody = 32 << 20
	yandexDevice  = "Vault" // the device name Yandex shows
)

// Yandex reads «Мне нравится», liked albums and the listener's own and
// liked playlists from api.music.yandex.net, the (unpublished) API the
// official apps use, as MarshalX/yandex-music-api documents it. It signs
// in with a device code (oauth.yandex.ru) as the Yandex Music Android
// app's public client, or with a token the listener pastes.
type Yandex struct {
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	BaseURL      string            // https://api.music.yandex.net
	OAuthURL     string            // https://oauth.yandex.ru
	Limiter      *metadata.Limiter // 500 ms between requests
	// Sleep waits out a 429 or 5xx (nil: a timer); tests pass a fake clock.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewYandex is the connector for a client with Yandex's addresses.
func NewYandex(clientID, clientSecret string) *Yandex {
	return &Yandex{ClientID: clientID, ClientSecret: clientSecret, HTTP: &http.Client{Timeout: 30 * time.Second},
		BaseURL: "https://api.music.yandex.net", OAuthURL: "https://oauth.yandex.ru",
		Limiter: &metadata.Limiter{Interval: 500 * time.Millisecond}}
}

// oauthPost posts a form to oauth.yandex.ru and decodes the answer.
func (y *Yandex) oauthPost(ctx context.Context, path string, form url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, y.OAuthURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := y.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("yandex %s: %w", path, scrub(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("yandex %s: %w", path, err)
	}
	_ = json.Unmarshal(body, out)
	return resp.StatusCode, nil
}

// DeviceCode starts a device sign-in: the code the listener enters at
// verification_url.
func (y *Yandex) DeviceCode(ctx context.Context) (musicimport.DeviceCode, error) {
	id := make([]byte, 8)
	rand.Read(id)
	var out struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		Interval        int    `json:"interval"`
		ExpiresIn       int    `json:"expires_in"`
		Error           string `json:"error"`
	}
	status, err := y.oauthPost(ctx, "/device/code", url.Values{
		"client_id":   {y.ClientID},
		"device_id":   {hex.EncodeToString(id)},
		"device_name": {yandexDevice},
	}, &out)
	if err != nil {
		return musicimport.DeviceCode{}, err
	}
	if status != http.StatusOK || out.DeviceCode == "" {
		return musicimport.DeviceCode{}, fmt.Errorf("%w (yandex device code: %d %s)", musicimport.ErrConnect, status, safeWord(out.Error))
	}
	return musicimport.DeviceCode{DeviceCode: out.DeviceCode, UserCode: out.UserCode, VerificationURL: out.VerificationURL,
		Interval: out.Interval, ExpiresIn: out.ExpiresIn}, nil
}

// PollToken asks for the token of a device code: ErrPending while the
// listener has not entered it, ErrLinkExpired once it ran out, ErrConnect
// when refused; a network failure or 5xx is a plain error (poll again).
// Errors never carry the code or a token.
func (y *Yandex) PollToken(ctx context.Context, deviceCode string) (musicimport.Token, error) {
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	status, err := y.oauthPost(ctx, "/token", url.Values{
		"grant_type":    {"device_code"},
		"code":          {deviceCode},
		"client_id":     {y.ClientID},
		"client_secret": {y.ClientSecret},
	}, &out)
	switch {
	case err != nil:
		return musicimport.Token{}, err
	case status == http.StatusOK && out.AccessToken != "":
		tok := musicimport.Token{Access: out.AccessToken, Refresh: out.RefreshToken}
		if out.ExpiresIn > 0 {
			tok.Expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
		}
		return tok, nil
	case out.Error == "authorization_pending":
		return musicimport.Token{}, musicimport.ErrPending
	case out.Error == "slow_down" || status == http.StatusTooManyRequests:
		return musicimport.Token{}, musicimport.ErrSlowDown
	case out.Error == "expired_token":
		return musicimport.Token{}, musicimport.ErrLinkExpired
	case status >= 500:
		return musicimport.Token{}, fmt.Errorf("yandex token: %d", status)
	}
	return musicimport.Token{}, fmt.Errorf("%w (yandex token: %d %s)", musicimport.ErrConnect, status, safeWord(out.Error))
}

// errYandexRefused: the API refused one resource (403, 404: a playlist
// deleted or private); only a playlist may be left out for it.
var errYandexRefused = errors.New("yandex: refused")

func yandexErr(path string, status int) error {
	return fmt.Errorf("%w (yandex %s: %d)", musicimport.ErrConnect, path, status)
}

// call sends one API request (GET, or POST with a form) and decodes the
// answer's result into out: through the limiter, waiting 5 s after a 429
// or 5xx, three times. 401 (the token expired or was revoked) ends the
// read; 403 and 404 are errYandexRefused.
func (y *Yandex) call(ctx context.Context, tok musicimport.Token, path string, form url.Values, out any) error {
	for attempt := 0; ; attempt++ {
		if err := y.Limiter.Wait(ctx); err != nil {
			return err
		}
		method, body := http.MethodGet, io.Reader(nil)
		if form != nil {
			method, body = http.MethodPost, strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, y.BaseURL+path, body)
		if err != nil {
			return err
		}
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		req.Header.Set("Authorization", "OAuth "+tok.Access)
		req.Header.Set("Accept", "application/json")
		resp, err := y.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w (yandex %s: %v)", musicimport.ErrConnect, path, scrub(err))
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, yandexMaxBody))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("%w (yandex %s: %v)", musicimport.ErrConnect, path, err)
		}
		switch st := resp.StatusCode; {
		case st == http.StatusOK:
			var env struct {
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(data, &env); err != nil || len(env.Result) == 0 || bytes.Equal(env.Result, []byte("null")) {
				return fmt.Errorf("%w (yandex %s: bad answer)", musicimport.ErrConnect, path)
			}
			if err := json.Unmarshal(env.Result, out); err != nil {
				return fmt.Errorf("%w (yandex %s: bad answer)", musicimport.ErrConnect, path)
			}
			return nil
		case st == http.StatusForbidden || st == http.StatusNotFound:
			return errYandexRefused
		case st == http.StatusTooManyRequests || st >= 500:
			if attempt >= yandexRetries {
				return yandexErr(path, st)
			}
			if err := sleepCtx(ctx, y.Sleep, yandexBackoff); err != nil {
				return err
			}
		default:
			return yandexErr(path, st)
		}
	}
}

// required is call for the library's own lists: a refusal fails the read
// rather than show a partial library.
func (y *Yandex) required(ctx context.Context, tok musicimport.Token, path string, form url.Values, out any) error {
	err := y.call(ctx, tok, path, form, out)
	if errors.Is(err, errYandexRefused) {
		err = fmt.Errorf("%w (yandex %s: refused)", musicimport.ErrConnect, path)
	}
	return err
}

// yID is an id the API gives as a string or as a number.
type yID string

func (v *yID) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*v = yID(s)
		return nil
	}
	if string(b) == "null" {
		*v = ""
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*v = yID(n.String())
	return nil
}

type yArtist struct {
	Name string `json:"name"`
}

type yAlbum struct {
	ID      yID       `json:"id"`
	Title   string    `json:"title"`
	Version string    `json:"version"`
	Year    int       `json:"year"`
	Artists []yArtist `json:"artists"`
}

type yTrack struct {
	ID         yID       `json:"id"`
	Title      string    `json:"title"`
	Version    string    `json:"version"`
	DurationMs int64     `json:"durationMs"`
	Artists    []yArtist `json:"artists"`
	Albums     []yAlbum  `json:"albums"`
	Available  bool      `json:"available"`
}

// yShort is a song of a list: its id, album and when it was added; a
// playlist may carry the song itself.
type yShort struct {
	ID        yID     `json:"id"`
	AlbumID   yID     `json:"albumId"`
	Timestamp string  `json:"timestamp"`
	Track     *yTrack `json:"track"`
}

func (s yShort) ref() string {
	if s.AlbumID != "" {
		return string(s.ID) + ":" + string(s.AlbumID)
	}
	return string(s.ID)
}

type yPlaylist struct {
	UID   yID    `json:"uid"`
	Kind  yID    `json:"kind"`
	Title string `json:"title"`
	Owner struct {
		UID yID `json:"uid"`
	} `json:"owner"`
}

func yFirst(as []yArtist) string {
	for _, a := range as {
		if n := strings.TrimSpace(a.Name); n != "" {
			return n
		}
	}
	return ""
}

// yTitle is a title with its version («Song» + «Live» → «Song (Live)»).
func yTitle(title, version string) string {
	title, version = strings.TrimSpace(title), strings.TrimSpace(version)
	if version == "" {
		return title
	}
	return title + " (" + version + ")"
}

func yTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// yLibrary collects what a read found.
type yLibrary struct {
	snap playlistimport.Snapshot
	at   map[string]int // track key → index in snap.Tracks
}

func (l *yLibrary) add(t yTrack, liked bool, added *time.Time) (string, bool) {
	if t.ID == "" {
		return "", false
	}
	key := "yandex:" + string(t.ID)
	if i, ok := l.at[key]; ok {
		if liked {
			l.snap.Tracks[i].Liked = true
			l.snap.Tracks[i].AddedAt = added
		}
		return key, true
	}
	st := playlistimport.SnapTrack{Key: key, Artist: yFirst(t.Artists), Title: yTitle(t.Title, t.Version), DurationMs: t.DurationMs, Liked: liked}
	if len(t.Albums) > 0 {
		st.Album, st.AlbumArtist = strings.TrimSpace(t.Albums[0].Title), yFirst(t.Albums[0].Artists)
	}
	if liked {
		st.AddedAt = added
	}
	l.at[key] = len(l.snap.Tracks)
	l.snap.Tracks = append(l.snap.Tracks, st)
	return key, true
}

// tracks fetches the songs of refs (id:albumId), at most 100 a request:
// by id.
func (y *Yandex) tracks(ctx context.Context, tok musicimport.Token, refs []string) (map[string]yTrack, error) {
	out := map[string]yTrack{}
	for start := 0; start < len(refs); start += yandexBatch {
		batch := refs[start:min(start+yandexBatch, len(refs))]
		var got []yTrack
		if err := y.call(ctx, tok, "/tracks", url.Values{"track-ids": {strings.Join(batch, ",")}, "with-positions": {"false"}}, &got); err != nil {
			return nil, err
		}
		for _, t := range got {
			out[string(t.ID)] = t
		}
	}
	return out, nil
}

// playlist reads one playlist's songs in order; ok false: Yandex refused
// it (deleted, private) or a song's details.
func (y *Yandex) playlist(ctx context.Context, tok musicimport.Token, owner, kind string) ([]yTrack, bool, error) {
	var p struct {
		Tracks []yShort `json:"tracks"`
	}
	err := y.call(ctx, tok, "/users/"+url.PathEscape(owner)+"/playlists/"+url.PathEscape(kind), nil, &p)
	if errors.Is(err, errYandexRefused) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var missing []string
	for _, s := range p.Tracks {
		if s.Track == nil && s.ID != "" {
			missing = append(missing, s.ref())
		}
	}
	got, err := y.tracks(ctx, tok, missing)
	if errors.Is(err, errYandexRefused) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	songs := make([]yTrack, 0, len(p.Tracks))
	omitted := 0
	for _, s := range p.Tracks {
		switch t, ok := got[string(s.ID)]; {
		case s.Track != nil:
			songs = append(songs, *s.Track)
		case ok:
			songs = append(songs, t)
		default:
			omitted++
		}
	}
	if omitted > 0 {
		slog.Warn("import: yandex gave no details of some playlist songs (left out)", "songs", omitted)
	}
	return songs, true, nil
}

// Read reads «Мне нравится» (keys yandex:<track id>, with the dates
// liked), liked albums, and the listener's own and liked playlists (a
// playlist Yandex refuses is unreadable). Any other refusal fails the
// read.
func (y *Yandex) Read(ctx context.Context, tok musicimport.Token, progress func(done, total int)) (playlistimport.Snapshot, error) {
	l := &yLibrary{snap: playlistimport.Snapshot{Service: playlistimport.ServiceYandex}, at: map[string]int{}}
	var status struct {
		Account struct {
			UID yID `json:"uid"`
		} `json:"account"`
	}
	if err := y.required(ctx, tok, "/account/status", nil, &status); err != nil {
		return l.snap, err
	}
	uid := string(status.Account.UID)
	if uid == "" {
		return l.snap, fmt.Errorf("%w (yandex /account/status: no account)", musicimport.ErrConnect)
	}
	l.snap.Account = uid
	user := "/users/" + url.PathEscape(uid)

	// «Мне нравится»
	var likes struct {
		Library struct {
			Tracks []yShort `json:"tracks"`
		} `json:"library"`
	}
	if err := y.required(ctx, tok, user+"/likes/tracks", nil, &likes); err != nil {
		return l.snap, err
	}
	if len(likes.Library.Tracks) > playlistimport.MaxTracks {
		return l.snap, playlistimport.ErrTooLarge
	}
	refs := make([]string, 0, len(likes.Library.Tracks))
	for _, s := range likes.Library.Tracks {
		if s.ID != "" {
			refs = append(refs, s.ref())
		}
	}
	liked, err := y.tracks(ctx, tok, refs)
	if errors.Is(err, errYandexRefused) {
		err = fmt.Errorf("%w (yandex /tracks: refused)", musicimport.ErrConnect)
	}
	if err != nil {
		return l.snap, err
	}
	omitted := 0
	for _, s := range likes.Library.Tracks {
		if t, ok := liked[string(s.ID)]; ok {
			l.add(t, true, yTime(s.Timestamp))
		} else {
			omitted++
		}
	}
	if omitted > 0 {
		slog.Warn("import: yandex gave no details of some liked songs (left out)", "songs", omitted)
	}
	if progress != nil {
		progress(len(l.snap.Tracks), len(l.snap.Tracks))
	}

	// liked albums
	var albums []struct {
		Timestamp string  `json:"timestamp"`
		Album     *yAlbum `json:"album"`
	}
	if err := y.required(ctx, tok, user+"/likes/albums?rich=true", nil, &albums); err != nil {
		return l.snap, err
	}
	for _, a := range albums {
		if a.Album == nil || a.Album.ID == "" {
			continue
		}
		l.snap.Albums = append(l.snap.Albums, playlistimport.SnapAlbum{Key: "yandex:album:" + string(a.Album.ID),
			Artist: yFirst(a.Album.Artists), Title: strings.TrimSpace(a.Album.Title), Year: a.Album.Year, AddedAt: yTime(a.Timestamp)})
	}

	// own playlists, then the liked ones (read through their owner)
	var own []yPlaylist
	if err := y.required(ctx, tok, user+"/playlists/list", nil, &own); err != nil {
		return l.snap, err
	}
	var likedPls []struct {
		Playlist *yPlaylist `json:"playlist"`
	}
	if err := y.required(ctx, tok, user+"/likes/playlists", nil, &likedPls); err != nil {
		return l.snap, err
	}
	pls := own
	for i := range pls {
		if pls[i].Owner.UID == "" && pls[i].UID == "" {
			pls[i].Owner.UID = yID(uid) // the listener's own list
		}
	}
	for _, lp := range likedPls {
		// another listener's playlist is read through its owner: without
		// one it cannot be (and is never read as the listener's own)
		if lp.Playlist != nil && (lp.Playlist.Owner.UID != "" || lp.Playlist.UID != "") {
			pls = append(pls, *lp.Playlist)
		}
	}
	seen := map[string]bool{}
	for _, p := range pls {
		owner := string(p.Owner.UID)
		if owner == "" {
			owner = string(p.UID)
		}
		if p.Kind == "" {
			continue
		}
		id := owner + ":" + string(p.Kind)
		if seen[id] {
			continue
		}
		seen[id] = true
		songs, ok, err := y.playlist(ctx, tok, owner, string(p.Kind))
		if err != nil {
			return l.snap, err
		}
		sp := playlistimport.SnapPlaylist{ID: id, Name: strings.TrimSpace(p.Title), Readable: ok, Entries: []string{}}
		for _, t := range songs {
			if key, ok := l.add(t, false, nil); ok {
				sp.Entries = append(sp.Entries, key)
			}
		}
		l.snap.Playlists = append(l.snap.Playlists, sp)
		if len(l.snap.Playlists) > playlistimport.MaxPlaylists || len(l.snap.Tracks) > playlistimport.MaxTracks {
			return l.snap, playlistimport.ErrTooLarge
		}
	}
	if progress != nil {
		progress(len(l.snap.Tracks), len(l.snap.Tracks))
	}
	return l.snap, nil
}
