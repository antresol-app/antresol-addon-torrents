package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// «Следить», the pinned releases and «Найти озвучку…» (internal/followapi).
func init() {
	Register(EN, map[string]string{
		"Не найдено":  "Not found",
		"Неверный id": "Invalid id",
		"Торрент-файл слишком большой": "Torrent file is too large",
	})
}
