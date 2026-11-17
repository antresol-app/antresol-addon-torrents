package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// The stall hints of the adults' progress event (internal/acquire/progress.go;
// the web's comp.stalled.* and comp.tracker.* say the same).
func init() {
	Register(EN, map[string]string{
		"Трекер отклонил раздачу: неверный пасскей": "The tracker rejected the release: invalid passkey",
		"Раздача не зарегистрирована на трекере":    "The release is not registered on the tracker",
		"Нет участников раздачи — ждём…":            "No peers for this release yet — waiting…",
		"Получение данных о раздаче…":               "Getting release details…",
		"Сидов пока не видно":                       "No seeds seen yet",
	})
}
