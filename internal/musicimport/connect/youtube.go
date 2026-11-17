//go:build ignore

package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/metadata"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
	"github.com/antresol-app/antresol-addon-torrents/internal/musicimport"
)

// YouTubeScope is what Vault asks Google for: read-only access to the
// listener's YouTube account (likes and playlists).
const YouTubeScope = "https://www.googleapis.com/auth/youtube.readonly"

// YouTube limits. The Data API's quota is 10 000 units a day, one a list
// call: a 403 quotaExceeded ends the read; a rate limit (403
// rateLimitExceeded, a 429) waits Retry-After, else 1, 2, 4, 8 s.
const (
	youtubePage     = 50 // items a page, ids a videos.list
	youtubeRetries  = 4  // rate-limit waits for one request
	youtube5xxRetry = 2
	youtubeMaxWait  = 60 * time.Second
	youtubeMaxBody  = 8 << 20
	youtubeMusic    = "10" // the Music category
)

// YouTube signs in with Google OAuth 2.0 (PKCE, and the client secret a
// Google client has even as a Desktop app) and reads the listener's liked
// music (the likes playlist, LL) and playlists through the YouTube Data
// API v3. Only videos of the Music category are songs.
type YouTube struct {
	ClientID     string
	ClientSecret string
	HTTP         *http.Client
	BaseURL      string            // https://www.googleapis.com/youtube/v3
	AuthURL      string            // https://accounts.google.com/o/oauth2/v2/auth
	TokenURL     string            // https://oauth2.googleapis.com/token
	Limiter      *metadata.Limiter // 100 ms between requests
	// Sleep waits out a rate limit or a 5xx (nil: a timer); tests pass a
	// fake clock.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewYouTube is the connector for a Google OAuth client with Google's
// addresses.
func NewYouTube(clientID, clientSecret string) *YouTube {
	return &YouTube{ClientID: clientID, ClientSecret: clientSecret, HTTP: &http.Client{Timeout: 30 * time.Second},
		BaseURL: "https://www.googleapis.com/youtube/v3", AuthURL: "https://accounts.google.com/o/oauth2/v2/auth",
		TokenURL: "https://oauth2.googleapis.com/token", Limiter: &metadata.Limiter{Interval: 100 * time.Millisecond}}
}

// AuthorizeURL is Google's sign-in page (authorization code with PKCE;
// prompt=consent: the consent screen every time).
func (y *YouTube) AuthorizeURL(redirectURI, state, challenge string) string {
	q := url.Values{
		"client_id":             {y.ClientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {YouTubeScope},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"prompt":                {"consent"},
	}
	return y.AuthURL + "?" + q.Encode()
}

// Exchange trades the sign-in's code for a token. Errors never carry the
// code, the verifier, the secret or a token.
func (y *YouTube) Exchange(ctx context.Context, redirectURI, code, verifier string) (musicimport.Token, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {y.ClientID},
		"client_secret": {y.ClientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, y.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return musicimport.Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := y.HTTP.Do(req)
	if err != nil {
		return musicimport.Token{}, fmt.Errorf("google token: %w", scrub(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return musicimport.Token{}, fmt.Errorf("google token: %w", err)
	}
	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return musicimport.Token{}, fmt.Errorf("google token: %d %s", resp.StatusCode, safeWord(out.Error))
	}
	tok := musicimport.Token{Access: out.AccessToken, Refresh: out.RefreshToken}
	if out.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	return tok, nil
}

// errYouTubeRefused: the API refused one resource (a playlist gone or not
// the listener's: 403, 404); only a playlist may be left unreadable.
var errYouTubeRefused = errors.New("youtube: refused")

func youtubeErr(path string, status int, why string) error {
	return fmt.Errorf("%w (youtube %s: %d%s)", musicimport.ErrConnect, path, status, why)
}

// googleReason is the first reason of a Google API error answer
// («quotaExceeded», «rateLimitExceeded»), a safe word or "".
func googleReason(body []byte) string {
	var e struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || len(e.Error.Errors) == 0 {
		return ""
	}
	r := e.Error.Errors[0].Reason
	for _, c := range r {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return ""
		}
	}
	return r
}

// get fetches one API path (with its query) into out: through the
// limiter; a rate limit waits (Retry-After, else 1, 2, 4, 8 s, at most a
// minute) and retries, a 5xx twice; quota exhaustion, 401 and other
// answers end the read (ErrConnect). 403 and 404 are errYouTubeRefused.
func (y *YouTube) get(ctx context.Context, tok musicimport.Token, path string, q url.Values, out any) error {
	fails, limited := 0, 0
	for {
		if err := y.Limiter.Wait(ctx); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, y.BaseURL+path+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok.Access)
		resp, err := y.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w (youtube %s: %v)", musicimport.ErrConnect, path, scrub(err))
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, youtubeMaxBody))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("%w (youtube %s: %v)", musicimport.ErrConnect, path, err)
		}
		reason := googleReason(body)
		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("%w (youtube %s: bad answer)", musicimport.ErrConnect, path)
			}
			return nil
		case reason == "quotaExceeded" || reason == "dailyLimitExceeded":
			return youtubeErr(path, resp.StatusCode, " "+reason) // until midnight Pacific time: no wait helps
		case resp.StatusCode == http.StatusTooManyRequests || reason == "rateLimitExceeded" || reason == "userRateLimitExceeded":
			wait := time.Duration(1<<limited) * time.Second
			if v := resp.Header.Get("Retry-After"); v != "" {
				wait = retryAfter(v)
			}
			if limited >= youtubeRetries || wait > youtubeMaxWait {
				return youtubeErr(path, resp.StatusCode, " rate limited")
			}
			limited++
			if err := sleepCtx(ctx, y.Sleep, wait); err != nil {
				return err
			}
		case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("%w (%s %d %s)", errYouTubeRefused, path, resp.StatusCode, reason)
		case resp.StatusCode >= 500 && fails < youtube5xxRetry:
			fails++
			if err := sleepCtx(ctx, y.Sleep, time.Duration(fails)*time.Second); err != nil {
				return err
			}
		default:
			return youtubeErr(path, resp.StatusCode, "")
		}
	}
}

type ytPageInfo struct {
	TotalResults int `json:"totalResults"`
}

type ytItem struct {
	Snippet struct {
		PublishedAt string `json:"publishedAt"`
		Title       string `json:"title"`
		ResourceID  struct {
			VideoID string `json:"videoId"`
		} `json:"resourceId"`
	} `json:"snippet"`
	ContentDetails struct {
		VideoID string `json:"videoId"`
	} `json:"contentDetails"`
}

func (it ytItem) videoID() string {
	if it.ContentDetails.VideoID != "" {
		return it.ContentDetails.VideoID
	}
	return it.Snippet.ResourceID.VideoID
}

type ytVideo struct {
	ID      string `json:"id"`
	Snippet struct {
		Title        string `json:"title"`
		Description  string `json:"description"`
		ChannelTitle string `json:"channelTitle"`
		CategoryID   string `json:"categoryId"`
	} `json:"snippet"`
	ContentDetails struct {
		Duration string `json:"duration"`
	} `json:"contentDetails"`
}

// ytList walks a list's pages (pageToken) and hands each page's items to
// use; the first page's total counts toward the progress. ok false: the
// API refused it (403, 404) or answered without items.
func ytList[T any](ctx context.Context, y *YouTube, tok musicimport.Token, l *library, path string, q url.Values, use func([]T) error) (bool, error) {
	q.Set("maxResults", strconv.Itoa(youtubePage))
	seen := 0
	for first := true; ; first = false {
		var p struct {
			NextPageToken string     `json:"nextPageToken"`
			PageInfo      ytPageInfo `json:"pageInfo"`
			Items         *[]T       `json:"items"`
		}
		err := y.get(ctx, tok, path, q, &p)
		if errors.Is(err, errYouTubeRefused) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if p.Items == nil {
			return false, nil
		}
		if first {
			l.total += p.PageInfo.TotalResults
		}
		if err := use(*p.Items); err != nil {
			return false, err
		}
		l.step(len(*p.Items))
		if seen += len(*p.Items); seen > playlistimport.MaxTracks {
			return false, playlistimport.ErrTooLarge
		}
		if p.NextPageToken == "" || len(*p.Items) == 0 {
			return true, nil
		}
		q.Set("pageToken", p.NextPageToken)
	}
}

// ytRequired is ytList for the library's own lists: refused or without
// items fails the read rather than show a partial library.
func ytRequired[T any](ctx context.Context, y *YouTube, tok musicimport.Token, l *library, path string, q url.Values, use func([]T) error) error {
	ok, err := ytList(ctx, y, tok, l, path, q, use)
	if err == nil && !ok {
		err = fmt.Errorf("%w (youtube %s: refused or without items)", musicimport.ErrConnect, path)
	}
	return err
}

// song is a music video as a song (nil: not music, or gone).
type song struct {
	artist, title, album string
	durationMs           int64
}

// videos looks up the details of videos not seen yet (≤ 50 ids a call)
// into seen: a song, or nil for a video that is no music or gone.
func (y *YouTube) videos(ctx context.Context, tok musicimport.Token, ids []string, seen map[string]*song) error {
	var want []string
	for _, id := range ids {
		if _, ok := seen[id]; !ok && id != "" {
			seen[id] = nil // gone unless the API names it
			want = append(want, id)
		}
	}
	for len(want) > 0 {
		n := min(len(want), youtubePage)
		var out struct {
			Items *[]ytVideo `json:"items"`
		}
		q := url.Values{"part": {"snippet,contentDetails"}, "id": {strings.Join(want[:n], ",")}, "maxResults": {strconv.Itoa(youtubePage)}}
		err := y.get(ctx, tok, "/videos", q, &out)
		if err == nil && out.Items == nil {
			err = errYouTubeRefused
		}
		if errors.Is(err, errYouTubeRefused) {
			err = fmt.Errorf("%w (youtube /videos: refused)", musicimport.ErrConnect)
		}
		if err != nil {
			return err
		}
		for _, v := range *out.Items {
			if _, asked := seen[v.ID]; !asked || v.Snippet.CategoryID != youtubeMusic {
				continue
			}
			s := songOf(v)
			if s.artist == "" || s.title == "" {
				continue
			}
			seen[v.ID] = &s
		}
		want = want[n:]
	}
	return nil
}

func songOf(v ytVideo) song {
	a, t, al := songName(v.Snippet.Title, v.Snippet.ChannelTitle, v.Snippet.Description)
	return song{artist: a, title: t, album: al, durationMs: isoDuration(v.ContentDetails.Duration)}
}

// Read reads the listener's liked music (keys youtube:<video id>, liked
// at the date the video was liked) and playlists (their music videos in
// order; a playlist gone or refused is unreadable; one of no music is
// left out). Only Music category videos are songs.
func (y *YouTube) Read(ctx context.Context, tok musicimport.Token, progress func(done, total int)) (playlistimport.Snapshot, error) {
	l := &library{snap: playlistimport.Snapshot{Service: playlistimport.ServiceYouTube}, at: map[string]int{}, progress: progress}
	seen := map[string]*song{}
	add := func(id string, s *song, liked bool, at *time.Time) string {
		key := "youtube:" + id
		if i, ok := l.at[key]; ok {
			if liked {
				l.snap.Tracks[i].Liked, l.snap.Tracks[i].AddedAt = true, at
			}
			return key
		}
		t := playlistimport.SnapTrack{Key: key, Artist: s.artist, Title: s.title, Album: s.album, DurationMs: s.durationMs, Liked: liked}
		if liked {
			t.AddedAt = at
		}
		l.at[key] = len(l.snap.Tracks)
		l.snap.Tracks = append(l.snap.Tracks, t)
		return key
	}

	var ch struct {
		Items *[]struct {
			ID             string `json:"id"`
			ContentDetails struct {
				RelatedPlaylists struct {
					Likes string `json:"likes"`
				} `json:"relatedPlaylists"`
			} `json:"contentDetails"`
		} `json:"items"`
	}
	err := y.get(ctx, tok, "/channels", url.Values{"part": {"contentDetails"}, "mine": {"true"}}, &ch)
	if err == nil && ch.Items == nil {
		err = errYouTubeRefused
	}
	if errors.Is(err, errYouTubeRefused) {
		err = youtubeErr("/channels", http.StatusForbidden, " refused or without items")
	}
	if err != nil {
		return l.snap, err
	}
	likes := ""
	if len(*ch.Items) > 0 {
		c := (*ch.Items)[0]
		l.snap.Account, likes = c.ID, c.ContentDetails.RelatedPlaylists.Likes
	}
	if likes == "" && len(*ch.Items) > 0 {
		likes = "LL"
	}
	if likes != "" {
		// a channel-less account has no likes playlist
		skipped := 0
		err := ytRequired(ctx, y, tok, l, "/playlistItems", url.Values{"part": {"snippet,contentDetails"}, "playlistId": {likes}},
			func(items []ytItem) error {
				ids := make([]string, len(items))
				for i, it := range items {
					ids[i] = it.videoID()
				}
				if err := y.videos(ctx, tok, ids, seen); err != nil {
					return err
				}
				for _, it := range items {
					if s := seen[it.videoID()]; s != nil {
						add(it.videoID(), s, true, addedAt(it.Snippet.PublishedAt))
					} else {
						skipped++
					}
				}
				return nil
			})
		if err != nil {
			return l.snap, err
		}
		if skipped > 0 {
			// not Music category, private or gone: a short preview explained
			slog.Default().Info("import: liked YouTube videos that are no songs left out", "count", skipped)
		}
	} else {
		slog.Default().Info("import: the YouTube account has no channel: no likes")
	}

	type ytPlaylist struct {
		ID      string `json:"id"`
		Snippet struct {
			Title string `json:"title"`
		} `json:"snippet"`
	}
	var pls []ytPlaylist
	if err := ytRequired(ctx, y, tok, l, "/playlists", url.Values{"part": {"snippet,contentDetails"}, "mine": {"true"}},
		func(items []ytPlaylist) error { pls = append(pls, items...); return nil }); err != nil {
		return l.snap, err
	}
	for _, p := range pls {
		if p.ID == "" || p.ID == likes {
			continue
		}
		// the songs join the library only once every page was read: an
		// unreadable playlist leaves none behind
		var ids []string
		items := 0
		ok, err := ytList(ctx, y, tok, l, "/playlistItems", url.Values{"part": {"snippet,contentDetails"}, "playlistId": {p.ID}},
			func(page []ytItem) error {
				items += len(page)
				for _, it := range page {
					ids = append(ids, it.videoID())
				}
				return y.videos(ctx, tok, ids[len(ids)-len(page):], seen)
			})
		if err != nil {
			return l.snap, err
		}
		sp := playlistimport.SnapPlaylist{ID: p.ID, Name: p.Snippet.Title, Readable: ok, Entries: []string{}}
		if ok {
			for _, id := range ids {
				if s := seen[id]; s != nil {
					sp.Entries = append(sp.Entries, add(id, s, false, nil))
				}
			}
			if items > 0 && len(sp.Entries) == 0 {
				continue // videos, but no music: not a music playlist
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

// isoDuration reads an ISO 8601 duration as YouTube gives it (PT4M46S,
// P1DT2H) in milliseconds; 0 when unreadable.
func isoDuration(s string) int64 {
	m := isoDurationRe.FindStringSubmatch(s)
	if m == nil || s == "P" || s == "PT" {
		return 0
	}
	var total int64
	for i, unit := range []int64{86400, 3600, 60, 1} {
		if m[i+1] != "" {
			n, _ := strconv.ParseInt(m[i+1], 10, 64)
			total += n * unit
		}
	}
	return total * 1000
}

var isoDurationRe = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// noise is what a music video's title adds to the song's: «(Official
// Video)», «[Official Audio]», «(Lyrics)», «(Клип)», «HD»…
var noise = regexp.MustCompile(`(?i)\s*[\(\[](?:official\s*)?(?:hd\s*|4k\s*)?(?:music\s*)?(?:video|audio|clip|lyrics?(?:\s*video)?|visuali[sz]er)(?:\s*clip)?[\)\]]` +
	`|\s*[\(\[](?:official|hd|hq|4k|клип|официальный клип|премьера клипа|премьера|lyrics?)[\)\]]`)

// songName reads a music video's song: an auto-generated video («Provided
// to YouTube by …» and «Title · Artist · …», then the album) gives the
// title, artist and album; a « - Topic» channel is the artist. Any other
// video is «Artist - Title» by its title, else its channel's song: a
// guess the matcher accepts only on a MusicBrainz match of both artist
// and title.
func songName(title, channel, description string) (artist, song, album string) {
	topic := ""
	if t, ok := strings.CutSuffix(strings.TrimSpace(channel), " - Topic"); ok {
		topic = strings.TrimSpace(t)
	}
	if desc := strings.ReplaceAll(description, "\r\n", "\n"); strings.HasPrefix(strings.TrimSpace(desc), "Provided to YouTube by") {
		var paras []string
		for _, p := range strings.Split(strings.TrimSpace(desc), "\n\n") {
			if p = strings.TrimSpace(p); p != "" {
				paras = append(paras, p)
			}
		}
		if len(paras) > 1 {
			parts := strings.Split(paras[1], " · ")
			if len(parts) >= 2 {
				song, artist = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
				if topic != "" {
					artist = topic
				}
				if len(paras) > 2 && !strings.HasPrefix(paras[2], "℗") && !strings.HasPrefix(paras[2], "©") &&
					!strings.HasPrefix(paras[2], "Released on:") && !strings.HasPrefix(paras[2], "Auto-generated") && !strings.Contains(paras[2], "\n") {
					album = paras[2]
				}
				return artist, song, album
			}
		}
	}
	clean := strings.TrimSpace(noise.ReplaceAllString(title, ""))
	if topic != "" {
		return topic, clean, ""
	}
	for _, sep := range []string{" - ", " – ", " — "} {
		if a, s, ok := strings.Cut(clean, sep); ok && strings.TrimSpace(a) != "" && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(a), strings.TrimSpace(s), ""
		}
	}
	ch := strings.TrimSpace(channel)
	if c, ok := strings.CutSuffix(ch, "VEVO"); ok && c != "" {
		ch = strings.TrimSpace(c)
	}
	return ch, clean, ""
}

// YouTube Takeout resolving limits: oEmbed at 2 requests a second; at
// most youtubeMaxResolve ids an upload (about 20 minutes); a video that
// fails (after oembedRetries) stays unknown, and oembedMaxFails failures
// in a row stop the naming (YouTube is refusing).
const (
	youtubeMaxResolve = 2400
	oembedBackoff     = 5 * time.Second
	oembedRetries     = 3
	oembedMaxFails    = 5
)

// videoIDForm is the form of a YouTube video id; anything else in a
// Takeout is never asked for.
var videoIDForm = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// YouTubeResolver names the videos a Google Takeout gives only by id
// (files.TakeoutResolver): with a token through videos.list (music only),
// else through YouTube's oEmbed, which needs no key and gives no category:
// only a song-shaped video is named (a « - Topic» or VEVO channel, or an
// «Artist - Title» title); a vlog stays unknown.
type YouTubeResolver struct {
	YouTube   *YouTube          // with Token: videos.list (no caller yet: an upload has no token)
	Token     musicimport.Token // empty: oEmbed
	HTTP      *http.Client
	OEmbedURL string            // https://www.youtube.com/oembed
	Limiter   *metadata.Limiter // 500 ms between oEmbed requests
	Sleep     func(ctx context.Context, d time.Duration) error
}

// NewYouTubeResolver is the key-less oEmbed resolver.
func NewYouTubeResolver() *YouTubeResolver {
	return &YouTubeResolver{HTTP: &http.Client{Timeout: 15 * time.Second}, OEmbedURL: "https://www.youtube.com/oembed",
		Limiter: &metadata.Limiter{Interval: 500 * time.Millisecond}}
}

// Resolve names the ids it can (keyed as given): a video private, gone,
// failing or not song-shaped stays unknown. An error (ctx ended, or
// YouTube refusing video after video) comes with what was named before it.
func (r *YouTubeResolver) Resolve(ctx context.Context, ids []string) (map[string]playlistimport.SnapTrack, error) {
	out := map[string]playlistimport.SnapTrack{}
	var want []string
	for _, id := range ids {
		if videoIDForm.MatchString(id) && len(want) < youtubeMaxResolve {
			want = append(want, id)
		}
	}
	if r.YouTube != nil && r.Token.Access != "" {
		seen := map[string]*song{}
		err := r.YouTube.videos(ctx, r.Token, want, seen)
		for id, s := range seen {
			if s != nil {
				out[id] = playlistimport.SnapTrack{Key: id, Artist: s.artist, Title: s.title, Album: s.album, DurationMs: s.durationMs}
			}
		}
		return out, err
	}
	fails := 0
	for _, id := range want {
		t, err := r.oembed(ctx, id)
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if err != nil {
			if fails++; fails >= oembedMaxFails {
				return out, err
			}
			continue // this video stays unknown
		}
		fails = 0
		if t.Title != "" && t.Artist != "" {
			out[id] = t
		}
	}
	return out, nil
}

// songShaped: an oEmbed answer that names a song (oEmbed has no category).
func songShaped(title, author string) bool {
	author = strings.TrimSpace(author)
	if strings.HasSuffix(author, " - Topic") || (strings.HasSuffix(author, "VEVO") && author != "VEVO") {
		return true
	}
	clean := noise.ReplaceAllString(title, "")
	for _, sep := range []string{" - ", " – ", " — "} {
		if a, s, ok := strings.Cut(clean, sep); ok && strings.TrimSpace(a) != "" && strings.TrimSpace(s) != "" {
			return true
		}
	}
	return false
}

// oembed asks for one video: an empty song when it is private, gone or
// not song-shaped; an error when YouTube keeps refusing (429, 5xx, no
// answer) or ctx ends.
func (r *YouTubeResolver) oembed(ctx context.Context, id string) (playlistimport.SnapTrack, error) {
	watch := "https://www.youtube.com/watch?v=" + url.QueryEscape(id)
	u := r.OEmbedURL + "?" + url.Values{"url": {watch}, "format": {"json"}}.Encode()
	for attempt := 0; ; attempt++ {
		if err := r.Limiter.Wait(ctx); err != nil {
			return playlistimport.SnapTrack{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return playlistimport.SnapTrack{}, err
		}
		status, wait := 0, oembedBackoff
		var body []byte
		resp, err := r.HTTP.Do(req)
		if err == nil {
			body, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			status = resp.StatusCode
			if v := resp.Header.Get("Retry-After"); v != "" {
				wait = retryAfter(v)
			}
		}
		if ctx.Err() != nil {
			return playlistimport.SnapTrack{}, ctx.Err()
		}
		switch {
		case err == nil && status == http.StatusOK:
			var o struct {
				Title      string `json:"title"`
				AuthorName string `json:"author_name"`
			}
			if json.Unmarshal(body, &o) != nil || !songShaped(o.Title, o.AuthorName) {
				return playlistimport.SnapTrack{}, nil
			}
			a, s, _ := songName(o.Title, o.AuthorName, "")
			return playlistimport.SnapTrack{Key: id, Artist: a, Title: s}, nil
		case err != nil || status == http.StatusTooManyRequests || status >= 500:
			why := fmt.Sprintf("%d", status)
			if err != nil {
				why = scrub(err).Error()
			}
			if attempt >= oembedRetries || wait > youtubeMaxWait {
				return playlistimport.SnapTrack{}, fmt.Errorf("youtube oembed: %s", why)
			}
			if err := sleepCtx(ctx, r.Sleep, wait); err != nil {
				return playlistimport.SnapTrack{}, err
			}
		default:
			return playlistimport.SnapTrack{}, nil // private (401), gone (404), not embeddable (403)
		}
	}
}
