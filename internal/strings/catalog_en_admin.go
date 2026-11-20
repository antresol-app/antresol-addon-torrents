package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// The admin routes (internal/admin).
func init() {
	Register(EN, map[string]string{
		"Задано на сервере":                           "Set on the server",
		"Укажите адрес вида http://flaresolverr:8191": "Enter an address like http://flaresolverr:8191",
		"Неверный адрес":                              "Invalid address",
		"FlareSolverr не ответил за 10 секунд. Проверьте адрес и что он запущен.":   "FlareSolverr did not answer within 10 seconds. Check the address and that it is running.",
		"Не удалось подключиться к FlareSolverr. Проверьте адрес и что он запущен.": "Could not connect to FlareSolverr. Check the address and that it is running.",
		"По этому адресу отвечает не FlareSolverr.":                                 "Something other than FlareSolverr answers at this address.",
		"Неверный запрос":           "Bad request",
		"Внутренняя ошибка сервера": "Internal server error",
		"Неизвестный формат музыки": "Unknown music format",
		"Неизвестный битрейт":       "Unknown bitrate",
		"Подключение к Prowlarr не сохранено: подключите его на странице «Переезд»": "No Prowlarr connection is saved: connect it on the Migrate page",
	})
}
