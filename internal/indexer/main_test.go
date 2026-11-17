package indexer

import (
	"os"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/studios"
)

// The tests read release names with the addon's studios (as the addon
// does from boot.Build).
func TestMain(m *testing.M) {
	studios.Register()
	os.Exit(m.Run())
}
