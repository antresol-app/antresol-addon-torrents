package indexer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// A Cloudflare page with no FlareSolverr address sends the admin to where
// one is entered; one FlareSolverr did not get past asks to check it there.
func TestCloudflareMessagesNameTheSettingsPage(t *testing.T) {
	none := humanError(fmt.Errorf("%w: %w (RuTracker)", cardigann.ErrCloudflare, cardigann.ErrNoSolver))
	failed := humanError(fmt.Errorf("%w: FlareSolverr: timeout", cardigann.ErrCloudflare))
	if !strings.Contains(none, "Нужен FlareSolverr — укажите его адрес в «Управление → Настройки загрузок»") {
		t.Errorf("no solver: %q", none)
	}
	if !strings.Contains(failed, "FlareSolverr её не прошёл") || strings.Contains(failed, "укажите его адрес") {
		t.Errorf("solver failed: %q", failed)
	}
}
