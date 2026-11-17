package coreadapt_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol/addonkit"
)

// The tracking adapter maps the core's recorded answers onto the copied
// model; an id the core does not track has no episodes or sources, and
// its kick is the SDK's 404.
func TestTrackingAdapter(t *testing.T) {
	tr := coreadapt.Tracking{C: lib(t).C}
	ti, err := tr.EnsureTitle(ctx, "series", 126308, "Сёгун", "/shogun.jpg")
	want := tracking.Title{ID: 1, Kind: "series", TMDBID: 126308, Name: "Сёгун", Poster: "/shogun.jpg", Status: "ended", Cert: "18+"}
	if err != nil || !reflect.DeepEqual(ti, want) {
		t.Fatalf("ensure %+v %v, want %+v", ti, err, want)
	}
	eps, err := tr.Episodes(ctx, 1)
	if err != nil || len(eps) != 2 || eps[1] != (tracking.Episode{TitleID: 1, Season: 1, Number: 2, Name: "Слуги двух господ", AirDate: "2024-02-27"}) {
		t.Fatalf("episodes %+v %v", eps, err)
	}
	srcs, err := tr.Sources(ctx, 1)
	if err != nil || len(srcs) != 3 {
		t.Fatalf("sources %+v %v", srcs, err)
	}
	seen := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	three := 3
	wantSrc := tracking.Source{TitleID: 1, Season: 1, Episode: 2, Kind: "alpha", Ref: "alpha:1", Provider: "a1", Quality: "1080p",
		Labels: []string{"HDR"}, Meta: tracking.SourceMeta{SourceID: "s-1", Voice: "Студия", Lang: "ru", Audio: "MVO",
			Studios: []string{"Студия"}, Subs: []string{"en"}, FileIndex: &three}, FirstSeen: seen, LastSeen: seen}
	if !reflect.DeepEqual(srcs[1], wantSrc) || srcs[0].Meta.ItemID != 4 || srcs[0].Meta.FileID != 2 || srcs[2].Meta.Provider != "b-host" {
		t.Fatalf("sources %+v", srcs)
	}
	if back := coreadapt.ToSource(coreadapt.FromSource(srcs[1])); !reflect.DeepEqual(back, wantSrc) {
		t.Fatalf("round trip %+v", back)
	}
	if _, ok, err := tr.TitleByID(ctx, 9); ok || err != nil {
		t.Fatalf("unknown title: %v %v", ok, err)
	}
	if eps, err := tr.Episodes(ctx, 9); err != nil || len(eps) != 0 {
		t.Fatalf("unknown episodes %+v %v", eps, err)
	}
	if srcs, err := tr.Sources(ctx, 9); err != nil || len(srcs) != 0 {
		t.Fatalf("unknown sources %+v %v", srcs, err)
	}
	if err := tr.Kick(ctx, 9); !addonkit.IsNotFound(err) {
		t.Fatalf("kick of an unknown title: %v", err)
	}
}

// A tracked title read back by its id and by its TMDB id, and its kick,
// as the core recorded them.
func TestTrackingAdapter_Lookups(t *testing.T) {
	tr := coreadapt.Tracking{C: replay(t)}
	want := tracking.Title{ID: 1, Kind: "series", TMDBID: 126308, Name: "Сёгун", Poster: "/shogun.jpg", Status: "ended", Cert: "18+"}
	if ti, ok, err := tr.TitleByID(ctx, 1); err != nil || !ok || !reflect.DeepEqual(ti, want) {
		t.Fatalf("by id %+v %v %v", ti, ok, err)
	}
	if ti, ok, err := tr.TitleByTMDB(ctx, "series", 126308); err != nil || !ok || !reflect.DeepEqual(ti, want) {
		t.Fatalf("by tmdb %+v %v %v", ti, ok, err)
	}
	if err := tr.Kick(ctx, 1); err != nil {
		t.Fatalf("kick %v", err)
	}
}
