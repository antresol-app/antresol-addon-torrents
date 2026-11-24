package indexer

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// A Cloudflare page with no FlareSolverr address sends the admin to where
// one is entered; one FlareSolverr did not get past asks to check it there.
func TestCloudflareMessagesNameTheSettingsPage(t *testing.T) {
	none := humanError(fmt.Errorf("%w: %w (RuTracker)", cardigann.ErrCloudflare, cardigann.ErrNoSolver))
	failed := humanError(fmt.Errorf("%w: FlareSolverr: timeout", cardigann.ErrCloudflare))
	if !strings.Contains(none, "Нужен FlareSolverr — укажите его адрес в разделе «Настройки → Сервер → Настройки загрузок»") {
		t.Errorf("no solver: %q", none)
	}
	if !strings.Contains(failed, "FlareSolverr её не прошёл") || strings.Contains(failed, "укажите его адрес") {
		t.Errorf("solver failed: %q", failed)
	}
}

// A search that gave up waiting for FlareSolverr's turn says so in both
// languages, though its own timeout ended it too.
func TestSolverBusyMessage(t *testing.T) {
	err := fmt.Errorf("%w: %w: %w", errSourceTimeout, cardigann.ErrCloudflare, fmt.Errorf("%w: %w", cardigann.ErrSolverBusy, context.DeadlineExceeded))
	if got := humanError(err); !strings.Contains(got, "FlareSolverr занят другими запросами") {
		t.Errorf("ru: %q", got)
	}
	en := i18n.WithLanguage(context.Background(), i18n.EN)
	if got := humanErrorIn(en, err); !strings.Contains(got, "FlareSolverr is busy with other requests") {
		t.Errorf("en: %q", got)
	}
}
