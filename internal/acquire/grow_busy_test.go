package acquire

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// A selection that cannot grow because the torrent's files are being moved
// says so (400), not «failed» from the source (502).
func TestGrowErrBusyIsMoving(t *testing.T) {
	err := growErr(fmt.Errorf("x: %w", torrent.ErrBusy))
	if msg, _ := UserMessage(err); !errors.Is(err, ErrInvalid) || errors.Is(err, ErrSource) || !strings.Contains(msg, "переносятся") {
		t.Fatalf("busy: %v (%q)", err, msg)
	}
	err = growErr(errors.New("boom"))
	if msg, _ := UserMessage(err); !errors.Is(err, ErrSource) || msg != msgGrowFailed {
		t.Fatalf("other: %v (%q)", err, msg)
	}
}
