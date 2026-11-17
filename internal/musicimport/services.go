//go:build ignore

package musicimport

import (
	"context"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
)

// The owner's steps per sign-in service (shown by «Как настроить» until
// its credentials are set).
var (
	spotifySetup = []string{
		"Откройте developer.spotify.com/dashboard и нажмите «Create app».",
		"В «APIs used» отметьте «Web API».",
		"Redirect URI: <адрес Antresol>/api/import/oauth/spotify/callback или http://127.0.0.1:8765/vault-import/spotify.",
		"В Settings → «User Management» добавьте e-mail аккаунта Spotify, из которого переносите медиатеку.",
		"Скопируйте Client ID в переменную VAULT_SPOTIFY_CLIENT_ID на сервере и перезапустите Antresol.",
		"Подключайтесь, открыв Antresol по адресу из VAULT_PUBLIC_URL: после входа Spotify вернёт браузер туда, и вход в Antresol должен быть на том же адресе.",
	}
	yandexSetup = []string{
		"Возьмите client_id и client_secret публичного клиента Яндекс Музыки для Android — они перечислены в библиотеке MarshalX/yandex-music-api (файл device_auth.py).",
		"Впишите их в переменные VAULT_YANDEX_CLIENT_ID и VAULT_YANDEX_CLIENT_SECRET на сервере и перезапустите Antresol.",
	}
	youtubeSetup = []string{
		"Откройте console.cloud.google.com и создайте проект.",
		"В «APIs & Services» включите «YouTube Data API v3».",
		"В «OAuth consent screen» выберите External и режим Testing, добавьте свой аккаунт Google в тестовые пользователи и доступ …/auth/youtube.readonly.",
		"В «Credentials» нажмите «Create OAuth client ID»: без VAULT_PUBLIC_URL — тип Desktop app; с VAULT_PUBLIC_URL — Web application с Authorized redirect URI <адрес Antresol>/api/import/oauth/youtube/callback.",
		"Впишите Client ID и Client secret в переменные VAULT_GOOGLE_CLIENT_ID и VAULT_GOOGLE_CLIENT_SECRET на сервере и перезапустите Antresol.",
		"При входе Google предупредит «Google hasn’t verified this app»: это ваше приложение — нажмите «Continue».",
	}
	deezerSetup = []string{
		"Откройте developers.deezer.com/myapps и нажмите «Create a new Application».",
		"Домен — адрес Antresol или 127.0.0.1; «Redirect URL after authentication» — <адрес Antresol>/api/import/oauth/deezer/callback или http://127.0.0.1:8765/vault-import/deezer.",
		"Впишите Application ID в переменную VAULT_DEEZER_APP_ID, Secret Key — в VAULT_DEEZER_SECRET на сервере и перезапустите Antresol.",
	}
)

// localizeSteps is the owner's setup steps in the request language.
func localizeSteps(ctx context.Context, steps []string) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = i18n.Localize(ctx, s)
	}
	return out
}

// Services lists the tiles of «Импорт», in the order the page shows them
// (playlistimport.ConnectorStatus).
func (o *OAuth) Services(ctx context.Context) []playlistimport.ServiceInfo {
	spotify := playlistimport.ServiceInfo{ID: "spotify", Connect: ModeRedirect, Setup: localizeSteps(ctx, spotifySetup), Files: []string{"csv"}}
	if o != nil {
		spotify.Connect = o.Mode("spotify")
	}
	if o.Configured("spotify") {
		spotify.Configured, spotify.Setup = true, []string{}
	}
	youtube := playlistimport.ServiceInfo{ID: "youtube", Connect: ModeRedirect, Setup: localizeSteps(ctx, youtubeSetup), Files: []string{"zip", "csv"}}
	if o != nil {
		youtube.Connect = o.Mode("youtube")
	}
	if o.Configured("youtube") && !o.IsDevice("youtube") {
		youtube.Configured, youtube.Setup = true, []string{}
	}
	yandex := playlistimport.ServiceInfo{ID: "yandex", Connect: ModeDevice, Setup: localizeSteps(ctx, yandexSetup), Files: []string{"csv"}}
	if o.IsDevice("yandex") {
		yandex.Configured, yandex.Setup = true, []string{}
	}
	// a public playlist link needs no sign-in: it works either way
	deezer := playlistimport.ServiceInfo{ID: "deezer", Connect: ModeRedirect, Setup: localizeSteps(ctx, deezerSetup), Files: []string{"csv"}}
	if o != nil {
		deezer.Connect = o.Mode("deezer")
	}
	if o.Configured("deezer") && !o.IsDevice("deezer") {
		deezer.Configured, deezer.Setup = true, []string{}
	}
	out := []playlistimport.ServiceInfo{
		spotify,
		{ID: "apple", Configured: true, Connect: "file", Setup: []string{}, Files: []string{"xml"}},
		yandex,
		youtube,
		deezer,
		{ID: "file", Configured: true, Connect: "file", Setup: []string{}, Files: []string{"csv", "m3u", "xml", "zip"}},
	}
	return out
}

var (
	_ playlistimport.ConnectorStatus = (*OAuth)(nil)
	_ playlistimport.Stopper         = (*OAuth)(nil)
	_ playlistimport.Downloader      = (*Queue)(nil)
)
