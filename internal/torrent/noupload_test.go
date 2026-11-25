package torrent

import (
	"testing"
	"time"
)

// wrote: the bytes of file data the client has sent to anyone.
func wrote(e *Engine) int64 {
	st := e.client.Stats()
	return st.BytesWrittenData.Int64()
}

// EngineConfig.NoUpload (the server forces no-seeding): the client itself
// never sends file data, with no per-torrent switch involved (NoSeeding is
// off here, so nothing is parked or disallowed), and downloading is not
// touched. The control run shows the same setup without it serves a peer.
func TestNoUploadClientDownloadsButNeverUploads(t *testing.T) {
	fx := newMultiFixture(t, 16<<10, 32<<10, 32<<10)

	t.Run("control", func(t *testing.T) {
		e := partial(t, engOpts{}, fx)
		l := leech(t, e, fx)
		waitFor(t, l, fx.ih, 15*time.Second, func(s Status) bool { return s.Done > 0 })
		if wrote(e) == 0 {
			t.Fatal("the control engine uploaded nothing: the test proves nothing")
		}
	})

	t.Run("in progress", func(t *testing.T) {
		e := partial(t, engOpts{noUpload: true}, fx)
		l := leech(t, e, fx)
		getsNothing(t, e, l, fx.ih)
		if n := wrote(e); n != 0 {
			t.Fatalf("the client wrote %d bytes of file data", n)
		}
	})

	t.Run("finished", func(t *testing.T) {
		// control: a finished engine without NoUpload serves the very same
		// leech (so a leech that gets nothing below is not just one that
		// never connected)
		ctl, _ := downloaded(t, t.TempDir(), t.TempDir(), engOpts{}, fx, nil)
		waitFor(t, leech(t, ctl, fx), fx.ih, 15*time.Second, func(s Status) bool { return s.Done > 0 })
		if wrote(ctl) == 0 {
			t.Fatal("the finished control engine uploaded nothing: the test proves nothing")
		}

		e, _ := downloaded(t, t.TempDir(), t.TempDir(), engOpts{noUpload: true}, fx, nil)
		if s, _ := status(e, fx.ih); s.Done != 64<<10 || s.Downloaded == 0 {
			t.Fatalf("a no-upload engine must still download: %+v", s)
		}
		// the client may not even take the peer in: nothing may reach it
		l := leech(t, e, fx)
		for end := time.Now().Add(1500 * time.Millisecond); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if s, _ := status(l, fx.ih); s.Done != 0 {
				t.Fatalf("the leech got %d bytes", s.Done)
			}
		}
		if n := wrote(e); n != 0 {
			t.Fatalf("the client wrote %d bytes of file data", n)
		}
	})
}
