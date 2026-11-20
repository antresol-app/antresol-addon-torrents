package acquire

import (
	"context"
	"errors"
	"testing"
)

// Seasons are TMDB's seasons with their episode counts, the specials
// (season 0) left out.
func TestSeasons(t *testing.T) {
	e := newEnv(t)
	got, err := e.svc.Seasons(context.Background(), 126308)
	if err != nil || len(got) != 2 || got[0].Number != 1 || got[0].Episodes != 10 || got[1].Number != 2 || got[1].Episodes != 8 {
		t.Fatalf("seasons %+v %v", got, err)
	}
	if !e.svc.HasMetadata() {
		t.Fatal("TMDB is set: HasMetadata false")
	}
}

// Without TMDB nothing of a series is known, and the service says so.
func TestSeasons_NoMetadata(t *testing.T) {
	s := &Service{}
	if s.HasMetadata() {
		t.Fatal("no TMDB: HasMetadata true")
	}
	if _, err := s.Seasons(context.Background(), 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no TMDB: %v", err)
	}
}
