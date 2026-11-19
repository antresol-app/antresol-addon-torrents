package strings

import (
	"regexp"
	gostrings "strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
)

// The product's names settle once and the texts follow (the core's
// internal/i18n/names_test.go): «Управление» is not in the menu any more
// (it is «Настройки → Сервер»; only «Управление каналами» is a real label)
// and English says «Shows», not «TV Shows».
var (
	retiredRU = regexp.MustCompile(`Управлени[еяию]`)
	retiredEN = regexp.MustCompile(`Manage\s*→|→\s*Manage\b|\bTV [Ss]hows?\b`)
)

func TestNoRetiredNames(t *testing.T) {
	for _, l := range i18n.Languages[1:] {
		for _, k := range i18n.Keys(l) {
			if retiredRU.MatchString(gostrings.ReplaceAll(k, "Управление каналами", "")) {
				t.Errorf("%q: «Управление» is not in the menu any more (use «Настройки → Сервер → …»)", k)
			}
			if tr, _ := i18n.Translation(l, k); retiredEN.MatchString(tr) {
				t.Errorf("%s translation %q: retired name (use «Settings → Server → …», «Shows»)", l, tr)
			}
		}
	}
}
