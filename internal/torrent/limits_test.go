package torrent

import "testing"

func TestSetUploadLimitLive(t *testing.T) {
	e := openEngine(t, t.TempDir(), engOpts{upLimit: 1000})
	if got := e.UploadLimit(); got != 1000 {
		t.Fatalf("initial %d", got)
	}
	e.SetUploadLimit(2 << 20)
	if got := e.UploadLimit(); got != 2<<20 {
		t.Fatalf("after set %d", got)
	}
	e.SetUploadLimit(0)
	if got := e.UploadLimit(); got != 0 {
		t.Fatalf("unlimited %d", got)
	}
}
