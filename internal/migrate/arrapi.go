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

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

// arrClient reads Sonarr, Radarr or Prowlarr over its API (X-Api-Key):
// GETs only.
type arrClient struct {
	app  string // Sonarr | Radarr | Prowlarr
	base string // the address, without a trailing slash
	ver  string // "v3" (Sonarr, Radarr) | "v1" (Prowlarr)
	key  string
	http *http.Client
}

// arrBodyLimit bounds one answer (a big Sonarr's series list is a few MB).
const arrBodyLimit = 64 << 20

func newArrClient(app string, r Remote, hc *http.Client) *arrClient {
	ver := "v3"
	if app == "Prowlarr" {
		ver = "v1"
	}
	hc = sameHostClient(hc, 60*time.Second)
	return &arrClient{app: app, base: strings.TrimRight(strings.TrimSpace(r.URL), "/"), ver: ver, key: r.APIKey, http: hc}
}

// shown is the address for messages: never a login or a query in it.
func (c *arrClient) shown() string { return redactURL(c.base) }

// RemoteError is a connection problem to show as it is (Russian).
type RemoteError struct {
	Msg string
	Err error
}

func (e *RemoteError) Error() string { return e.Msg }
func (e *RemoteError) Unwrap() error { return e.Err }

func remoteErr(err error, format string, a ...any) *RemoteError {
	return &RemoteError{Msg: fmt.Sprintf(format, a...), Err: err}
}

// get fetches /api/<ver>/<path> into v. Its errors are *RemoteError; the
// key never goes into one, nor into the log.
func (c *arrClient) get(ctx context.Context, path string, query url.Values, v any) error {
	u := c.base + "/api/" + c.ver + "/" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return remoteErr(err, "Неверный адрес %s", c.app)
	}
	req.Header.Set("X-Api-Key", c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errCrossHostRedirect) {
			return remoteErr(err, "%s перенаправляет на другой адрес — укажите адрес, который открывается без перенаправления", c.app)
		}
		return remoteErr(err, "%s не отвечает по адресу %s", c.app, c.shown())
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return remoteErr(nil, "%s не принял API-ключ — скопируйте его в %s: «Settings → General → API Key»", c.app, c.app)
	case resp.StatusCode == http.StatusNotFound:
		return remoteErr(nil, "По адресу %s нет API %s — проверьте адрес", c.shown(), c.app)
	case resp.StatusCode != http.StatusOK:
		return remoteErr(nil, "%s ответил %d на %s", c.app, resp.StatusCode, "/api/"+c.ver+"/"+path)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, arrBodyLimit+1))
	if err != nil {
		return remoteErr(err, "%s не отвечает по адресу %s", c.app, c.shown())
	}
	if len(b) > arrBodyLimit {
		return remoteErr(nil, "Ответ %s слишком большой (больше %d МиБ)", c.app, arrBodyLimit>>20)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return remoteErr(err, "По адресу %s отвечает не %s", c.shown(), c.app)
	}
	return nil
}

// check asks /system/status: the address answers, the key is taken and
// it is this app (not Radarr on Sonarr's card).
func (c *arrClient) check(ctx context.Context) error {
	if c.base == "" {
		return remoteErr(nil, "Укажите адрес %s", c.app)
	}
	if err := checkRemoteURL(c.app, c.base); err != nil {
		return err
	}
	if strings.TrimSpace(c.key) == "" {
		return remoteErr(nil, "Укажите API-ключ %s", c.app)
	}
	var st struct {
		AppName string `json:"appName"`
	}
	if err := c.get(ctx, "system/status", nil, &st); err != nil {
		return err
	}
	if st.AppName != "" && !strings.EqualFold(st.AppName, c.app) {
		return remoteErr(nil, "По адресу %s отвечает %s, а не %s", c.shown(), st.AppName, c.app)
	}
	return nil
}

type apiImage struct {
	CoverType string `json:"coverType"`
	URL       string `json:"url"`
	RemoteURL string `json:"remoteUrl"`
}

// imagesJSON is the images as the database's Images column holds them.
func imagesJSON(list []apiImage) string {
	if len(list) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(list)
	return string(b)
}

// apiDownloadClient is a download client in /api/v3/downloadclient; an
// older version without removeCompletedDownloads leaves it nil.
type apiDownloadClient struct {
	Name                     string `json:"name"`
	Enable                   bool   `json:"enable"`
	RemoveCompletedDownloads *bool  `json:"removeCompletedDownloads"`
}

// removingCompleted: the enabled download clients that remove completed
// downloads; an answer without the setting is an error (the warning must
// never vanish silently).
func (c *arrClient) removingCompleted(ctx context.Context) ([]string, error) {
	var list []apiDownloadClient
	if err := c.get(ctx, "downloadclient", nil, &list); err != nil {
		return nil, err
	}
	var names []string
	for _, d := range list {
		if !d.Enable {
			continue
		}
		if d.RemoveCompletedDownloads == nil {
			return nil, errors.New("downloadclient has no removeCompletedDownloads")
		}
		if *d.RemoveCompletedDownloads {
			names = append(names, d.Name)
		}
	}
	return names, nil
}

type apiSeries struct {
	ID        int        `json:"id"`
	Title     string     `json:"title"`
	Year      int        `json:"year"`
	TvdbID    int        `json:"tvdbId"`
	TmdbID    int        `json:"tmdbId"`
	Monitored bool       `json:"monitored"`
	Images    []apiImage `json:"images"`
}

func (c *arrClient) allSeries(ctx context.Context) ([]apiSeries, error) {
	var list []apiSeries
	err := c.get(ctx, "series", nil, &list)
	return list, err
}

type apiEpisode struct {
	SeasonNumber  int  `json:"seasonNumber"`
	EpisodeNumber int  `json:"episodeNumber"`
	Monitored     bool `json:"monitored"`
	HasFile       bool `json:"hasFile"`
	EpisodeFileID int  `json:"episodeFileId"`
}

func (c *arrClient) episodes(ctx context.Context, seriesID int) ([]apiEpisode, error) {
	var list []apiEpisode
	err := c.get(ctx, "episode", url.Values{"seriesId": {fmt.Sprint(seriesID)}}, &list)
	return list, err
}

type apiMovie struct {
	ID          int        `json:"id"`
	Title       string     `json:"title"`
	Year        int        `json:"year"`
	TmdbID      int        `json:"tmdbId"`
	Monitored   bool       `json:"monitored"`
	HasFile     bool       `json:"hasFile"`
	MovieFileID int        `json:"movieFileId"`
	Images      []apiImage `json:"images"`
}

func (c *arrClient) movies(ctx context.Context) ([]apiMovie, error) {
	var list []apiMovie
	err := c.get(ctx, "movie", nil, &list)
	return list, err
}

// prowlarrIndexers reads /api/v1/indexer and /api/v1/tag.
func (c *arrClient) prowlarrIndexers(ctx context.Context) ([]indexer.ProwlarrIndexer, error) {
	var idx, tags json.RawMessage
	if err := c.get(ctx, "indexer", nil, &idx); err != nil {
		return nil, err
	}
	if err := c.get(ctx, "tag", nil, &tags); err != nil {
		return nil, err
	}
	list, err := indexer.ParseProwlarrAPI(idx, tags)
	if err != nil {
		return nil, remoteErr(err, "По адресу %s отвечает не %s", c.shown(), c.app)
	}
	return list, nil
}
