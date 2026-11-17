package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// English for internal/organize: placing a finished file set into the
// library.
func init() {
	Register(EN, map[string]string{
		"Среди файлов нет видео":             "No video among the files",
		"Не удалось понять, какие это серии": "Couldn’t tell which episodes these are",
		"Неизвестный тип %q":                 "Unknown type %q",
		"исходный файл не найден: %w":        "source file not found: %w",
		"%s уже существует":                  "%s already exists",
		"Нет доступа на запись в папку медиатеки %s. Проверьте права или что диск не подключён только для чтения": "No write access to the library folder %s. Check the permissions, or that the disk isn’t mounted read-only",
		"На диске закончилось место — не удалось добавить файл в %s":                                              "The disk is out of space — couldn’t add the file to %s",
		"Не удалось добавить в медиатеку: %s":                                                                     "Couldn’t add to the library: %s",
		"Не удалось добавить в медиатеку: %w":                                                                     "Couldn’t add to the library: %w",
		"Среди файлов нет музыки":                                                                                 "No music among the files",
		"Скачан другой альбом: %s":                                                                                "A different album was downloaded: %s",
		"Среди файлов нет альбома «%s»":                                                                           "No album “%s” among the files",
		"Среди файлов нет книг в форматах, которые открывает Antresol":                                            "No books in formats Antresol can open among the files",
		"Среди файлов нет аудиокниги":                                                                             "No audiobook among the files",
		"Среди файлов нет томов манги или комиксов (архивов или папок с картинками)":                              "No manga or comic volumes (archives or folders of images) among the files",
	})
}
