package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// Library Import download queue and sign-ins (musicimport).
func init() {
	Register(EN, map[string]string{
		// musicimport: sign-in errors
		"Не удалось подключиться к сервису. Попробуйте ещё раз.": "Couldn’t connect to the service. Try again.",
		"Ссылка устарела. Подключитесь заново.":                  "The link has expired. Connect again.",
		"Сервис не настроен на сервере":                          "The service isn’t set up on the server",
		// musicimport: album failures (stored, localized when served)
		"Ошибка": "Error",
		"Подходящих раздач не нашлось":                                   "No suitable torrents found",
		"Нет раздачи в нужном формате":                                   "No torrent in the right format",
		"Раздачу удалили в «Загрузках»":                                  "The torrent was removed in Downloads",
		"Раздачи альбома не качаются: их никто не раздаёт":               "The album’s torrents don’t download: nobody is seeding them",
		"Подходящих вариантов не нашлось: найденные раздачи не качаются": "No suitable options: the torrents found don’t download",
	})
}
