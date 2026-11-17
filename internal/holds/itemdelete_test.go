package holds

import (
	"strings"
	"testing"
)

// The hold texts are today's, in both languages, never empty, with the
// core's {name} placeholder.
func TestHoldTexts(t *testing.T) {
	for _, x := range []struct{ ru, en string }{
		{seedWarningText("{name}").RU, seedWarningText("{name}").EN},
		{keptWarningText("{name}").RU, keptWarningText("{name}").EN},
		{labelText("A").RU, labelText("A").EN},
	} {
		if strings.TrimSpace(x.ru) == "" || strings.TrimSpace(x.en) == "" || x.ru == x.en {
			t.Fatalf("text %q / %q", x.ru, x.en)
		}
	}
	if got := keptWarningText("{name}").RU; got != "Раздача «{name}» продолжится: в ней есть и другое из медиатеки" {
		t.Fatalf("kept %q", got)
	}
	if got := seedWarningText("{name}").RU; got != "Раздача «{name}» прекратится" {
		t.Fatalf("move %q", got)
	}
}
