package indexer

import (
	"context"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
)

func TestFavouriteStudioReasonEnglish(t *testing.T) {
	en := i18n.WithLanguage(context.Background(), i18n.EN)
	if got := localizeReasons(en, []string{"любимая озвучка: Кубик в Кубе"}); got[0] != "favourite voice-over: Кубик в Кубе" {
		t.Fatalf("%q", got)
	}
	e := newEnv(t)
	e.reload()
	p := DefaultPrefs()
	p.Studios = []string{strings.Repeat("я", 41)}
	if err := e.m.SavePrefs(en, p); err == nil || i18n.Message(en, err) != "Studio name too long (up to 40 characters)" {
		t.Fatalf("%v", err)
	}
}

func TestScoredReleaseJSONStudios(t *testing.T) {
	r := ScoredRelease{Result: Result{Title: "x"}}
	r.Info.Studios = []string{"LostFilm", "Jaskier"}
	b, _ := r.MarshalJSON()
	if !strings.Contains(string(b), `"studios":["LostFilm","Jaskier"]`) {
		t.Fatalf("%s", b)
	}
}
