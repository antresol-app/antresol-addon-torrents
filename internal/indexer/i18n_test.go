package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// Source errors read in the request's language: rendered directly, from
// the stored Russian lastError, and with the source's name in front.
func TestHumanErrorEnglish(t *testing.T) {
	en := i18n.WithLanguage(context.Background(), i18n.EN)
	// a sign-in detail as cardigann makes it (ErrLoginFailed + an i18n key)
	err := fmt.Errorf("%w: %w", cardigann.ErrLoginFailed, i18n.Errorf("неверный логин или пароль"))
	if got, want := humanError(err), "Не удалось войти на сайт: неверный логин или пароль."; got != want {
		t.Fatalf("ru: %q", got)
	}
	want := "Couldn’t sign in to the site: wrong username or password."
	if got := humanErrorIn(en, err); got != want {
		t.Fatalf("en: %q", got)
	}
	if got := i18n.Localize(en, humanError(err)); got != want {
		t.Fatalf("Localize: %q", got)
	}
	named := humanize("RuTracker", err)
	if got := named.Error(); got != "RuTracker: Не удалось войти на сайт: неверный логин или пароль." {
		t.Fatalf("named ru: %q", got)
	}
	if got := i18n.Message(en, named); got != "RuTracker: "+want {
		t.Fatalf("named en: %q", got)
	}
	if !errors.Is(named, cardigann.ErrLoginFailed) {
		t.Fatal("cause lost")
	}
}

func TestReasonsEnglish(t *testing.T) {
	en := i18n.WithLanguage(context.Background(), i18n.EN)
	got := localizeReasons(en, []string{"другой год (1999)", "1080p", "дубляж", "раздают: 12"})
	want := []string{"different year (1999)", "1080p", "dub", "seeders: 12"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%q", got)
		}
	}
	if l := opdsEntry.Localized(i18n.EN); l.Name != "OPDS Catalog" || l.Settings[1].Label != "Username" {
		t.Fatalf("%+v", l)
	}
}
