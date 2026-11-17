package indexer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// lastError (humanError) never carries tracker credentials, even when the
// Russian detail after a sentinel quotes a URL.
func TestHumanErrorRedactsSecrets(t *testing.T) {
	err := fmt.Errorf("%w: сайт вернул https://t.example/login?passkey=SECRET1", cardigann.ErrLoginFailed)
	if got := humanError(err); strings.Contains(got, "SECRET1") || !strings.Contains(got, "Не удалось войти") {
		t.Fatalf("%s", got)
	}
}
