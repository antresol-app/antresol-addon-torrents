package strings

import . "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"

// The holds sent to the core (internal/holds); its warnings are in
// catalog_en_apiadmin_full.go.
func init() {
	Register(EN, map[string]string{
		"Загрузка «%s»": "Download “%s”",
		"Список защищённых файлов стал пустым при %d загрузках — отправка пропущена": "The list of protected files became empty with %d downloads — not sent",
	})
}
