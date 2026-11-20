package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// The app protocol of «Смотреть сейчас» and «Скачать» (internal/find/app.go):
// the labels, steps and answers a native app shows as the server gives them.
func init() {
	Register(EN, map[string]string{
		// the ways and the search
		"Смотреть сейчас":  "Watch Now",
		"Скачать":          "Download",
		"Найти и добавить": "Find and Add",
		// the steps
		"Сезон %d":           "Season %d",
		"Серия %d":           "Episode %d",
		"Сезон":              "Season",
		"Серия":              "Episode",
		"Сезонов не нашлось": "No seasons found",
		"Серий не нашлось":   "No episodes found",
		"Качество":           "Quality",
		"Любое":              "Any",
		"Любое качество":     "Any quality",
		"Antresol выберет лучшую раздачу в этом качестве, а если такой нет — лучшую в другом.": "Antresol picks the best release in this quality, or the best one in another if there is none.",
		"Раздачи":                       "Releases",
		"Лучшая раздача":                "Best release",
		"Подходящих раздач не нашлось.": "No suitable releases found.",
		"Лучшие — сверху. Antresol учитывает качество, озвучку и сколько людей раздают.": "Best first. Antresol weighs quality, audio and how many people are sharing.",
		"Не ответили: %s. Остальные источники проверены.":                                "No response from: %s. The other sources were checked.",
		// a release's lines
		"Раздают: %d":  "Sharing: %d",
		"весь сезон":   "full season",
		"Субтитры: %s": "Subtitles: %s",
		"Субтитры":     "Subtitles",
		"Б":            "B",
		"КБ":           "KB",
		"МБ":           "MB",
		"ГБ":           "GB",
		"ТБ":           "TB",
		// the answers
		"Загрузка началась":           "Download started",
		"Загрузка сезона %d началась": "Season %d download started",
		"Ждёт места на диске: загрузка начнётся, когда освободится место.": "Waiting for disk space: the download starts when space is freed.",
		"Загрузка отменена": "Download cancelled",
		"Нечего отменять":   "Nothing to cancel",
		// the download's ring
		"Скачано %d%%":   "Downloaded %d%%",
		"Приостановлено": "Paused",
		"Скачано":        "Downloaded",
	})
}
