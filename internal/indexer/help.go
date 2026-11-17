package indexer

import (
	"strings"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// Human Russian labels and hints for typical definition settings.

const cookieHelp = `Нужна, если сайт при входе просит капчу. Как получить:
1. Откройте сайт на компьютере в Chrome, Safari или Firefox и войдите в свой аккаунт.
2. Откройте инструменты разработчика: F12 (на Mac — ⌥⌘I; в Safari сначала включите меню «Разработка» в настройках).
3. Перейдите на вкладку «Сеть» (Network) и обновите страницу.
4. Нажмите на самый первый запрос к сайту и найдите в разделе «Заголовки запроса» (Request Headers) строку «cookie».
5. Скопируйте её значение целиком и вставьте сюда.
Не берите cookie через document.cookie — в нём нет главной части. Обычно такой вход работает около года; если перестанет — повторите.`

type fieldText struct {
	label, help string
	required    bool
	secret      bool
}

var knownFields = map[string]fieldText{
	"username":          {"Логин", "Логин на сайте", true, false},
	"password":          {"Пароль", "Пароль от вашего аккаунта на этом сайте. Хранится только на вашем сервере.", true, true},
	"cookie":            {"Cookie", cookieHelp, true, true},
	"apikey":            {"API-ключ", "Ключ из настроек профиля на сайте (обычно раздел «Профиль» или «API»).", true, true},
	"passkey":           {"Passkey", "Личный ключ из профиля на сайте — он есть в ссылках на скачивание торрентов.", true, true},
	"rsskey":            {"RSS-ключ", "Личный ключ для RSS из профиля на сайте.", true, true},
	"token":             {"Токен", "Личный токен из профиля на сайте.", true, true},
	"pid":               {"PID", "Личный идентификатор из профиля на сайте.", true, true},
	"email":             {"Почта", "Почта, с которой вы входите на сайт.", true, false},
	"2facode":           {"Код двухфакторной защиты", "Если на сайте включена двухэтапная проверка — секрет из приложения-аутентификатора.", false, true},
	"freeleech":         {"Только бесплатные раздачи", "Показывать только раздачи, скачивание которых не учитывается в рейтинге.", false, false},
	"sort":              {"Сортировка на сайте", "Как сайт сортирует результаты до того, как их оценит Antresol.", false, false},
	"type":              {"Порядок сортировки", "", false, false},
	"stripcyrillic":     {"Убирать кириллицу из названий", "Не включайте: Antresol сам понимает русские названия.", false, false},
	"addrussiantotitle": {"Добавлять «RUS» к названиям", "Нужно только для Sonarr и Radarr, Antresol это не нужно.", false, false},
	"addrussian":        {"Добавлять «RUS» к названиям", "Нужно только для Sonarr и Radarr, Antresol это не нужно.", false, false},
	"useragent":         {"User-Agent браузера", "Нужен, только если сайт пускает лишь с того браузера, из которого взят cookie.", false, false},
}

// settingSpec turns a definition setting into a form field; false for
// informational settings that are not fields.
func settingSpec(d *cardigann.Definition, s cardigann.Setting) (SettingSpec, bool) {
	t := strings.ToLower(s.Type)
	if strings.HasPrefix(t, "info") || t == "cardiganncaptcha" {
		return SettingSpec{}, false
	}
	spec := SettingSpec{Name: s.Name, Type: s.Type, Label: s.Label, Default: s.Default}
	if spec.Type == "" {
		spec.Type = "text"
	}
	if spec.Label == "" {
		spec.Label = s.Name
	}
	known, ok := knownFields[strings.ToLower(s.Name)]
	if ok {
		spec.Label, spec.Help, spec.Secret = known.label, known.help, known.secret
		// definitions without a login get Prowlarr's default username and
		// password settings: they are not used then
		credential := known.required && (d.Login != nil || (s.Name != "username" && s.Name != "password"))
		spec.Required = credential && s.Default == ""
	}
	if t == "password" {
		spec.Secret = true
	}
	for _, o := range s.Options {
		spec.Options = append(spec.Options, Option{Value: o.Key, Label: o.Value})
	}
	spec.Advanced = !spec.Required
	return spec, true
}

func siteLinkSpec(links []string) SettingSpec {
	spec := SettingSpec{
		Name: "sitelink", Type: "text", Label: "Адрес сайта",
		Help:     "Оставьте пустым — Antresol возьмёт основной адрес. Укажите зеркало, если основной сайт не открывается.",
		Advanced: true,
	}
	for _, l := range links {
		spec.Options = append(spec.Options, Option{Value: l, Label: l})
	}
	return spec
}

// isSecret reports whether a stored setting must be hidden from the API.
func isSecret(def *cardigann.Definition, name string) bool {
	if k, ok := knownFields[strings.ToLower(name)]; ok && k.secret {
		return true
	}
	if def != nil {
		for _, s := range def.Settings {
			if s.Name == name && strings.EqualFold(s.Type, "password") {
				return true
			}
		}
	}
	return false
}
