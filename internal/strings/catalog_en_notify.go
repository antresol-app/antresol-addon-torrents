package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// The download letters (internal/coreadapt/notifier.go).
func init() {
	Register(EN, map[string]string{
		"Загрузка завершилась: %s": "Download finished: %s",
		"Загрузка не удалась: %s":  "Download failed: %s",
	})
}
