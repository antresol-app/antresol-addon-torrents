package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// The admin routes (internal/admin).
func init() {
	Register(EN, map[string]string{
		"Задано на сервере":         "Set on the server",
		"Неверный запрос":           "Bad request",
		"Внутренняя ошибка сервера": "Internal server error",
		"Неизвестный формат музыки": "Unknown music format",
		"Неизвестный битрейт":       "Unknown bitrate",
	})
}
