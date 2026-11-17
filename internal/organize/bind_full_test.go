package organize

import (
	"context"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coretest"
)

// Every Bindable path has a file row after the scan, so the full edition
// can bind it to its download.
func init() {
	bindToDownload = func(t *testing.T, repo *coretest.Library, p string) {
		t.Helper()
		if _, _, _, err := repo.Import(context.Background(), []string{p}, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", 0); err != nil {
			t.Errorf("bind %s: %v", p, err)
		}
	}
}
