package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// The keys of pinned releases and follows (internal/{pinned,follow,followapi}).
func init() {
	Register(EN, map[string]string{
		"Этот трекер не подключён в «Источниках»":                              "This tracker isn’t connected in “Sources”",
		"Трекер прислал не торрент-файл — возможно, нужно снова войти на сайт": "The tracker didn’t send a torrent file — you may need to sign in to the site again",
		"В обновлённой раздаче нет части серий — оставлена прежняя":            "The updated torrent is missing some episodes — the previous one is kept",
		"В обновлённой раздаче нет части файлов — оставлена прежняя":           "The updated torrent is missing some files — the previous one is kept",
		"Новой раздачи не предложено":                                          "No newer torrent has been suggested",
		"Укажите серии для загрузки":                                           "Choose the episodes to download",
		"В раздаче нет этих серий":                                             "The torrent has none of these episodes",
		"Раздача не закреплена":                                                "No torrent is pinned",
		"Укажите magnet-ссылку, ссылку на раздачу или торрент-файл":            "Enter a magnet link, a torrent page link or a torrent file",
		"Этот фильм или сериал не отслеживается":                               "This film or series isn’t tracked",
		"Не удалось получить раздачу — проверьте ссылку":                       "Couldn’t get the torrent — check the link",
		"Это не торрент-файл":                                                  "Not a torrent file",
		"Неизвестное качество":                                                 "Unknown quality",
		// a follow whose profile is gone (internal/follow)
		"Профиль удалён": "Profile deleted",
	})
}
