package boot

import (
	"context"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol/addonkit"
)

// TestStudios_Registered: after Build the addon's own studios are in
// the release tables of this process.
func TestStudios_Registered(t *testing.T) {
	b, c := realBoot(t, "")
	s, err := Build(context.Background(), b, c, goldenClient(t), addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Acq.Close()
	if got := release.StudioKey("hdrezka"); got != "HDRezka Studio" {
		t.Fatalf("StudioKey(hdrezka) = %q", got)
	}
	if got, ok := release.CanonicalStudio("lostfilm.tv"); !ok || got != "LostFilm" {
		t.Fatalf("lostfilm.tv → %q %v", got, ok)
	}
	if s.Studios == nil || s.Studios.Studios == nil {
		t.Fatalf("no core studios for the searches: %+v", s.Studios)
	}
}

// TestBuild_TrackingHooksDeps: the tracking hooks get the pins' and the torrent
// search's scanners, the pin actions' Sync on the run context (with TMDB;
// none without), the followed titles' database, the core's profiles and
// acquire's original language.
func TestBuild_TrackingHooksDeps(t *testing.T) {
	b, c := realBoot(t, "")
	client := (&trackCore{}).client(t)
	run, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, err := Build(run, b, c, client, addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Acq.Close()
	d := s.TrackingHooks()
	if d.Pinned == nil || d.Torrent == nil || d.Sync == nil || d.Run != run || d.DB != s.DB || d.OrigLang == nil {
		t.Fatalf("deps %+v", d)
	}
	if u, ok := d.Users.(coreadapt.Users); !ok || u.C != client {
		t.Fatalf("users %#v", d.Users)
	}
	if d.Torrent != s.TrackingHooks().Torrent {
		t.Fatal("a new torrent scanner per call (its per-title day is lost)")
	}
	b2, c2 := realBoot(t, "")
	s2, err := Build(context.Background(), b2, c2, goldenClient(t), addonkit.Info{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Acq.Close()
	if d2 := s2.TrackingHooks(); d2.Sync != nil || d2.Pinned == nil || d2.Torrent == nil {
		t.Fatalf("without TMDB: %+v", d2)
	}
}
