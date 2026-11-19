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
		"Уже отслеживается":                                          "Already followed",
		"Уже в медиатеке":                                            "Already in the library",
		"Не удалось найти в TMDB (нет ключа или сети)":               "Couldn’t find it on TMDB (no key or no network)",
		"В Radarr у фильма нет TMDB id":                              "The movie has no TMDB id in Radarr",
		"Включится кнопкой «Перейти на Antresol»":                    "Turns on with “Switch to Antresol”",
		"Включится кнопкой «Перейти на Antresol» · отслеживается %s": "Turns on with “Switch to Antresol” · following %s",
		"Добавлен — проверьте его в «Источниках»":                    "Added — check it in “Sources”",
		"%s: нужен FlareSolverr — укажите его адрес в разделе «Настройки → Сервер → Настройки загрузок»": "%s: needs FlareSolverr — enter its address in Settings → Server → Download settings",
		"База %s не найдена: %s":               "%s database not found: %s",
		"Не удалось прочитать серии в базе %s": "Couldn’t read the episodes in the %s database",
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
		"Нет связи с qBittorrent (%s) — ничего не возвращено; проверьте подключение и повторите":          "Can’t reach qBittorrent (%s) — nothing was returned; check the connection and try again",
		"Нет связи с qBittorrent — ничего не возвращено; подключите его на странице переноса и повторите": "Can’t reach qBittorrent — nothing was returned; connect it on the migration page and try again",
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

		// a service reached over its API (arrapi.go, qbbackup.go, remote.go):
		// «Проверить» on a card whose files are not mounted, and the run
		"Не подключён — укажите адрес или смонтируйте %s": "Not connected — enter its address or mount %s",
		"Неверный адрес %s":           "Invalid %s address",
		"%s не отвечает по адресу %s": "%s doesn’t answer at %s",
		"%s не принял API-ключ — скопируйте его в %s: «Settings → General → API Key»": "%s didn’t accept the API key — copy it from %s: “Settings → General → API Key”",
		"По адресу %s нет API %s — проверьте адрес":                                   "There’s no %[2]s API at %[1]s — check the address",
		"%s ответил %d на %s":                               "%s answered %d to %s",
		"По адресу %s отвечает не %s":                       "What answers at %s isn’t %s",
		"Укажите адрес %s":                                  "Enter the %s address",
		"Адрес %s должен начинаться с http:// или https://": "The %s address must start with http:// or https://",
		"Укажите API-ключ %s":                               "Enter the %s API key",
		"По адресу %s отвечает %s, а не %s":                 "%[2]s answers at %[1]s, not %[3]s",
		"%s: Prowlarr не отдаёт пароли по API (%s) — введите их в «Источниках» после переноса":                                         "%s: Prowlarr doesn’t give passwords over its API (%s) — enter them in “Sources” after the move",
		"Источники Antresol не умеют читать Prowlarr по API":                                                                           "Antresol’s sources can’t read Prowlarr over its API",
		"Не удалось получить серии из %s: %s":                                                                                          "Couldn’t get the episodes from %s: %s",
		"Не удалось прочитать ответ %s":                                                                                                "Couldn’t read the %s answer",
		"Не удалось получить список раздач из qBittorrent (%s) — раздачи не перенесены":                                                "Couldn’t get the torrent list from qBittorrent (%s) — torrents weren’t moved",
		"qBittorrent не отдаёт файлы .torrent по API (нужен qBittorrent 4.5 или новее) — обновите его или смонтируйте папку BT_backup": "qBittorrent doesn’t give .torrent files over its API (qBittorrent 4.5 or newer is needed) — update it or mount the BT_backup folder",
		"Не удалось получить файл торрента из qBittorrent: %s":                                                                         "Couldn’t get the torrent file from qBittorrent: %s",
		"Не удалось получить состояние раздачи из qBittorrent: %s":                                                                     "Couldn’t get the torrent’s state from qBittorrent: %s",
		"Раздачи больше нет в qBittorrent":                                                                                             "The torrent is no longer in qBittorrent",
		"qBittorrent: пустой ответ на %s":                                                                                              "qBittorrent: empty answer to %s",
		"qBittorrent: файл с номером %d из %d":                                                                                         "qBittorrent: file number %d of %d",
		"Укажите адрес qBittorrent":                                                                                                    "Enter the qBittorrent address",
		"qBittorrent не принял логин или пароль":                                                                                       "qBittorrent didn’t accept the login or password",
		"qBittorrent не отвечает по адресу %s":                                                                                         "qBittorrent doesn’t answer at %s",
		"qBittorrent не пустил: укажите логин и пароль его веб-интерфейса":                                                             "qBittorrent refused: enter its web interface login and password",
		"Адрес %s не должен содержать логин, пароль или параметры — их вводят в отдельных полях":                                       "The %s address must not contain a login, a password or parameters — they go in their own fields",
		"%s перенаправляет на другой адрес — укажите адрес, который открывается без перенаправления":                                   "%s redirects to another address — enter the address that opens without a redirect",
		"qBittorrent перенаправляет на другой адрес — укажите адрес, который открывается без перенаправления":                          "qBittorrent redirects to another address — enter the address that opens without a redirect",
		"Ответ %s слишком большой (больше %d МиБ)":                                                                                     "The answer from %s is too big (over %d MiB)",
		"qBittorrent отказал во входе (403): адрес, вероятно, временно заблокирован":                                                   "qBittorrent refused the login (403): the address is probably blocked for a while",
		"qBittorrent: ответ слишком большой":                                                                                           "qBittorrent: the answer is too big",
		"Ответ qBittorrent слишком большой":                                                                                            "The answer from qBittorrent is too big",
		"qBittorrent отказал во входе — возможно, адрес временно заблокирован после неудачных попыток; подождите и не пробуйте снова сразу": "qBittorrent refused the login — the address may be temporarily blocked after failed attempts; wait and don’t retry right away",
		"Чтобы вернуть раздачи, подключите qBittorrent заново: «Подключить» на его карточке (связь не переживает «Забыть» и перезапуск)":    "To give the torrents back, connect qBittorrent again: “Connect” on its card (the connection does not survive “Forget” or a restart)",
		"Файлы этого сервиса смонтированы — они и используются":                                                                             "This service’s files are mounted — they are used",
	})
	RegisterPlural(EN, "раздача", "раздачи", "раздач", "torrent", "torrents")
	RegisterPlural(EN, "индексатор", "индексатора", "индексаторов", "indexer", "indexers")
	RegisterPlural(EN, "сериал", "сериала", "сериалов", "show", "shows")
	RegisterPlural(EN, "фильм", "фильма", "фильмов", "movie", "movies")
	RegisterPlural(EN, "серия", "серии", "серий", "episode", "episodes")
}
