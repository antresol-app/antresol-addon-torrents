package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// «ТВ» channels with fetching (internal/channelfetch).
func init() {
	Register(EN, map[string]string{
		"Загрузка пропала":           "The download is gone",
		"Источники недоступны":       "The sources are unavailable",
		"Не удалось начать загрузку": "Could not start the download",
	})
}
