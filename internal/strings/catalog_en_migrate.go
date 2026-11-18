package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// «Управление → Переезд» (internal/migrate): the full build's part, beside the
// media servers' (catalog_en_mediaimport.go): source statuses, report lines
// (saved in Russian, translated with Localize when served), leftovers.
func init() {
	Register(EN, map[string]string{
		// the addon's own run (migrate.Runner; copied from the core's
		// catalog_en_mediaimport.go, whose run it is written from)
		"Не найден — смонтируйте %s":   "Not found — mount %s",
		"Не удалось прочитать базу %s": "Couldn’t read the %s database",
		"Перенос из %s прервался: что-то пошло не так (подробности в журнале Antresol)": "Moving from %s stopped: something went wrong (details in the Antresol log)",
		"Не удалось создать временную папку переноса — ничего не перенесено":            "Couldn’t create the temporary migration folder — nothing was moved",
		"Не удалось проверить %s — посмотрите там вручную, что не переносится":          "Couldn’t check %s — look there yourself for what isn’t moved",
		"Уже в Antresol": "Already in Antresol",
		// the addon's /api/admin/migrate answers (internal/migrateapi;
		// the core's /api/migrate texts)
		"Сначала проверьте, что будет перенесено":                    "First check what will be moved",
		"Сервер не назвал профиль администратора — перенос не начат": "The server did not name the admin's profile, so the move did not start",
		"Перенос уже идёт":        "A migration is already running",
		"Неизвестный источник":    "Unknown source",
		"Выберите, что перенести": "Choose what to move",
		"Отчёт не найден":         "Report not found",

		// sources (Discover)
		"Не удалось прочитать папку BT_backup":                                                  "Couldn’t read the BT_backup folder",
		"Не задан адрес qBittorrent (ANTRESOL_TORRENTS_QBITTORRENT_URL) — раздачи не перенести": "The qBittorrent address isn’t set (ANTRESOL_TORRENTS_QBITTORRENT_URL) — torrents can’t be moved",
		"%s, %d отслеживается":                                 "%s, %d monitored",
		"Источники Antresol недоступны":                        "Antresol sources are unavailable",
		"Торренты Antresol недоступны — раздачи не перенесены": "Antresol torrents are unavailable — torrents weren’t moved",
		"Торренты Antresol недоступны — ничего не возвращено":  "Antresol torrents are unavailable — nothing was returned",

		// Prowlarr, Sonarr, Radarr
		"Уже отслеживается":                                                              "Already followed",
		"Уже в медиатеке":                                                                "Already in the library",
		"Не удалось найти в TMDB (нет ключа или сети)":                                   "Couldn’t find it on TMDB (no key or no network)",
		"В Radarr у фильма нет TMDB id":                                                  "The movie has no TMDB id in Radarr",
		"Включится кнопкой «Перейти на Antresol»":                                        "Turns on with “Switch to Antresol”",
		"Включится кнопкой «Перейти на Antresol» · отслеживается %s":                     "Turns on with “Switch to Antresol” · following %s",
		"Добавлен — проверьте его в «Источниках»":                                        "Added — check it in “Sources”",
		"%s: нужен FlareSolverr — укажите его адрес в «Управление → Настройки загрузок»": "%s: needs FlareSolverr — enter its address in «Manage → Download settings»",
		"База %s не найдена: %s":                                                         "%s database not found: %s",
		"Не удалось прочитать серии в базе Sonarr":                                       "Couldn’t read the episodes in the Sonarr database",
		"%s: не удалось проверить, удаляет ли он завершённые загрузки — проверьте в настройках клиента загрузки до переноса раздач":                    "%s: couldn’t check whether it removes completed downloads — check its download client settings before moving the torrents",
		"%s удаляет завершённые загрузки (%s) — остановите его до переноса раздач, иначе он удалит остановленные раздачи вместе с файлами в torrents/": "%s removes completed downloads (%s) — stop it before moving the torrents, or it will delete the stopped torrents along with their files in torrents/",

		// progress (Jellyfin, Kavita, Audiobookshelf)

		// leftovers
		"Не переносится: книги из списков Librarr (%d) — добавьте вручную": "Not moved: books from Librarr lists (%d) — add them yourself",
		"Не переносится: неодобренные запросы Seerr (%d)":                  "Not moved: unapproved Seerr requests (%d)",
		"правила RSS qBittorrent": "qBittorrent RSS rules",
		"Правила RSS":             "RSS Rules",
		"Не переносится: правила RSS qBittorrent (%d)":    "Not moved: qBittorrent RSS rules (%d)",
		"следимые папки qBittorrent":                      "qBittorrent watched folders",
		"Следимые папки":                                  "Watched Folders",
		"Не переносится: следимые папки qBittorrent (%d)": "Not moved: qBittorrent watched folders (%d)",

		// qBittorrent
		"адрес qBittorrent не задан": "the qBittorrent address isn’t set",
		"адрес не задан":             "address not set",
		"Нет связи с qBittorrent (%s) — раздачи не перенесены":              "Can’t reach qBittorrent (%s) — torrents weren’t moved",
		"Медиадиск недоступен — раздачи не перенесены":                      "The media disk is unavailable — torrents weren’t moved",
		"Не задана папка состояния переноса — раздачи не перенесены":        "The migration state folder isn’t set — torrents weren’t moved",
		"Не удалось прочитать папку BT_backup (%s) — раздачи не перенесены": "Couldn’t read the BT_backup folder (%s) — torrents weren’t moved",
		"Не удалось прочитать файл торрента":                                "Couldn’t read the torrent file",
		"Файл торрента повреждён":                                           "The torrent file is damaged",
		"Торрент v2 — перенос не поддерживается":                            "v2 torrent — moving isn’t supported",
		"Нет файла состояния qBittorrent":                                   "No qBittorrent state file",
		"Файл состояния qBittorrent не читается":                            "The qBittorrent state file can’t be read",
		"Файл состояния переноса не читается: %s":                           "The migration state file can’t be read: %s",
		"Перенос был прерван — будет завершён":                              "The move was interrupted — it will be finished",
		"Не удалось остановить раздачу в qBittorrent: %s":                   "Couldn’t stop the torrent in qBittorrent: %s",
		"Перенос завершён после перезапуска":                                "Move finished after a restart",
		"Уже в Antresol (на паузе)":                                         "Already in Antresol (paused)",
		"Уже в Antresol (%s)":                                               "Already in Antresol (%s)",
		"Недокачано (%d %%) — докачайте в qBittorrent и повторите перенос":  "Incomplete (%d %%) — finish downloading in qBittorrent and move again",
		"Не удалось узнать состояние раздачи в qBittorrent: %s":             "Couldn’t get the torrent’s state in qBittorrent: %s",
		"В qBittorrent раздача сейчас в состоянии «%s» — дождитесь, пока она раздаётся или остановлена, и повторите перенос": "The torrent is “%s” in qBittorrent now — wait until it’s seeding or stopped, then move again",
		"В файле состояния qBittorrent не указана папка раздачи":                                                             "The qBittorrent state file has no torrent folder",
		"Нет папки %s": "No folder %s",
		"Файлы переименованы в qBittorrent, а папка для ссылок не задана": "The files were renamed in qBittorrent, and the links folder isn’t set",
		"Файлы переименованы и лежат на другом диске":                     "The files were renamed and are on another disk",
		"Не удалось подготовить переименованные файлы: %s":                "Couldn’t prepare the renamed files: %s",
		"недопустимый путь в торренте: %q":                                "invalid path in the torrent: %q",
		"Не удалось записать состояние переноса: %s":                      "Couldn’t save the migration state: %s",
		"%s — раздача осталась в qBittorrent":                             "%s — the torrent stayed in qBittorrent",
		"%s (в Antresol она осталась на паузе)":                           "%s (it stayed paused in Antresol)",
		"Antresol не принял торрент: %s":                                  "Antresol didn’t accept the torrent: %s",
		"Не удалось проверить данные: %s":                                 "Couldn’t check the data: %s",
		"Проверка данных не закончилась за %s":                            "The data check didn’t finish in %s",
		"Данные на диске не совпадают с торрентом (%d %%)":                "The data on disk doesn’t match the torrent (%d %%)",
		"Не удалось запустить раздачу в Antresol: %s":                     "Couldn’t start the torrent in Antresol: %s",
		"В qBittorrent была на паузе — теперь раздаёт Antresol":           "Was paused in qBittorrent — Antresol seeds it now",
		"qBittorrent не подтвердил остановку за %s":                       "qBittorrent didn’t confirm the stop within %s",
		"она поставлена на паузу":                                         "it was paused",
		"и поставить её на паузу тоже не удалось":                         "and pausing it failed too",
		"%s — Antresol не смог убрать свою копию раздачи (%s), поэтому в qBittorrent её не запускаю: проверьте раздачу в Antresol и qBittorrent вручную": "%s — Antresol couldn’t remove its copy of the torrent (%s), so it isn’t started in qBittorrent: check the torrent in Antresol and qBittorrent yourself",
		"%s — в qBittorrent этой раздачи больше нет":                                           "%s — qBittorrent no longer has this torrent",
		"%s — раздача осталась в qBittorrent на паузе, как была":                               "%s — the torrent stayed paused in qBittorrent, as it was",
		"%s — проверьте раздачу в qBittorrent и при необходимости запустите её там":            "%s — check the torrent in qBittorrent and start it there if needed",
		"%s — запустить раздачу в qBittorrent снова не удалось (%s): запустите её там вручную": "%s — couldn’t start the torrent in qBittorrent again (%s): start it there yourself",
		"%s — раздача возвращена в qBittorrent":                                                "%s — the torrent was returned to qBittorrent",
		"%s — раздача осталась в qBittorrent на паузе: включён запрет раздачи":                 "%s — the torrent stayed paused in qBittorrent: sharing is turned off",
		"%s — раздача осталась в qBittorrent на паузе: запустите её там вручную, если нужно":   "%s — the torrent stayed paused in qBittorrent: start it there yourself if needed",
		"qBittorrent не принял имя пользователя или пароль":                                    "qBittorrent didn’t accept the user name or password",
		"qBittorrent: раздачи нет":                "qBittorrent: no such torrent",
		"qBittorrent: непонятный ответ на %s: %w": "qBittorrent: unexpected answer to %s: %w",
		"qBittorrent ответил %d на %s":            "qBittorrent answered %d to %s",

		// rollback
		"Нет связи с qBittorrent (%s) — ничего не возвращено":                                                           "Can’t reach qBittorrent (%s) — nothing was returned",
		"Будет возвращена в qBittorrent":                                                                                "Will be returned to qBittorrent",
		"В Antresol эта раздача уже не в режиме раздачи — не трогаю":                                                    "This torrent is no longer seeding in Antresol — left alone",
		"Не удалось убрать раздачу из Antresol: %s":                                                                     "Couldn’t remove the torrent from Antresol: %s",
		"Неизвестно, раздавалась ли она в qBittorrent — запустите там вручную, если нужно":                              "Unknown whether it was seeding in qBittorrent — start it there yourself if needed",
		"Раздачи возвращены на паузе: включён запрет раздачи":                                                           "Torrents returned paused: sharing is turned off",
		"Раздачи возвращены на паузе: запустите их в qBittorrent вручную, если нужно":                                   "Torrents returned paused: start them in qBittorrent yourself if needed",
		"Возвращена в qBittorrent (на паузе, как была)":                                                                 "Returned to qBittorrent (paused, as it was)",
		"Файлов раздачи нет на диске — в qBittorrent не запускаю":                                                       "The torrent’s files aren’t on disk — not starting it in qBittorrent",
		"Antresol больше не раздаёт её, но запустить в qBittorrent не удалось: %s — запустите её в qBittorrent вручную": "Antresol no longer seeds it, but starting it in qBittorrent failed: %s — start it in qBittorrent yourself",
		"Возвращена в qBittorrent": "Returned to qBittorrent",
	})
	RegisterPlural(EN, "раздача", "раздачи", "раздач", "torrent", "torrents")
	RegisterPlural(EN, "индексатор", "индексатора", "индексаторов", "indexer", "indexers")
	RegisterPlural(EN, "сериал", "сериала", "сериалов", "TV show", "TV shows")
	RegisterPlural(EN, "фильм", "фильма", "фильмов", "movie", "movies")
	RegisterPlural(EN, "серия", "серии", "серий", "episode", "episodes")
}
