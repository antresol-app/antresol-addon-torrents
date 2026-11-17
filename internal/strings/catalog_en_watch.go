package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// «Смотреть сейчас» (internal/watch) and the routes before the start ends (main.go).
func init() {
	Register(EN, map[string]string{
		"Не удалось открыть видео": "Couldn’t open the video",
		"Расширение запускается":   "The extension is starting",
	})
}
