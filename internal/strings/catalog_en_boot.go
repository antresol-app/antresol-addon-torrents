package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// The start (internal/boot).
func init() {
	Register(EN, map[string]string{
		"Источник изменился после импорта: очистите данные расширения и импортируйте заново": "The source changed after the import: clear the extension’s data and import again",
	})
}
