package indexer

import (
	"slices"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

const (
	fFLAC = "Кино - Группа крови - 1988, FLAC (tracks+.cue), lossless"
	f320  = "Кино - Группа крови (1988) [MP3, 320 kbps]"
	fV0   = "Кино - Группа крови (1988) [MP3 V0]"
	f256  = "Кино - Группа крови (1988) [MP3, 256 kbps]"
	f128  = "Кино - Группа крови (1988) [MP3, 128 kbps]"
	fMP3  = "Кино - Группа крови (1988) [MP3]"
	fNone = "Кино - Группа крови (1988)"
)

func formatPrefs(format, bitrate string) Prefs {
	p := DefaultPrefs()
	p.MusicFormat, p.MP3Bitrate = format, bitrate
	return p
}

func TestMusicFit(t *testing.T) {
	cases := []struct {
		format, bitrate  string
		yes, no, unknown []string
	}{
		{MusicLossless, "", []string{fFLAC}, []string{f320, fV0, f128, fMP3}, []string{fNone}},
		{MusicMP3, Bitrate320, []string{f320}, []string{fFLAC, fV0, f256, f128}, []string{fMP3, fNone}},
		{MusicMP3, BitrateV0, []string{fV0}, []string{fFLAC, f320, f256, f128}, []string{fMP3, fNone}},
		{MusicMP3, Bitrate256, []string{f320, fV0, f256}, []string{fFLAC, f128}, []string{fMP3, fNone}},
		{MusicMP3, BitrateAny, []string{f320, fV0, f128, fMP3}, []string{fFLAC}, []string{fNone}},
		{MusicAny, "", []string{fFLAC, f320, f128, fNone}, nil, nil},
	}
	for _, c := range cases {
		p := formatPrefs(c.format, c.bitrate)
		for want, titles := range map[Fit][]string{FitYes: c.yes, FitNo: c.no, FitUnknown: c.unknown} {
			for _, title := range titles {
				if got := MusicFit(release.ParseAudio(title), p); got != want {
					t.Errorf("%s/%s %q: %v, want %v", c.format, c.bitrate, title, got, want)
				}
			}
		}
	}
}

// The reported bug: MP3 chosen, a well seeded FLAC still came first.
func TestScoreMusicFormatBeatsSeeders(t *testing.T) {
	want := musicWant("Кино", "Группа крови")
	for _, c := range []struct {
		p    Prefs
		best string
	}{
		{formatPrefs(MusicMP3, Bitrate320), f320},
		{formatPrefs(MusicMP3, BitrateV0), fV0},
		{formatPrefs(MusicLossless, ""), fFLAC},
	} {
		rels := []ScoredRelease{}
		for _, title := range []string{fFLAC, f320, fV0} {
			seeders := 2
			if title != c.best {
				seeders = 5000
			}
			r := Result{Title: title, Seeders: seeders}
			s, why := Score(r, release.Parse(title), want, c.p)
			rels = append(rels, ScoredRelease{Result: r, Score: s, Reasons: why, FormatMismatch: MusicFit(release.ParseAudio(title), c.p) == FitNo})
		}
		sortReleases(rels)
		if rels[0].Title != c.best || rels[0].FormatMismatch {
			t.Errorf("%s/%s: best %q", c.p.MusicFormat, c.p.MP3Bitrate, rels[0].Title)
		}
		if !rels[1].FormatMismatch || !rels[2].FormatMismatch || !slices.ContainsFunc(rels[2].Reasons, func(r string) bool { return r == "не тот формат" }) {
			t.Errorf("%s/%s: not marked %+v", c.p.MusicFormat, c.p.MP3Bitrate, rels)
		}
	}
}

// The legacy «Загружать музыку без потерь» off is MP3 320; on is lossless.
func TestMusicFormatLegacy(t *testing.T) {
	p := Prefs{PreferLossless: false}
	if p.MusicFormatOrDefault() != MusicMP3 || p.MP3BitrateOrDefault() != Bitrate320 {
		t.Fatalf("%q %q", p.MusicFormatOrDefault(), p.MP3BitrateOrDefault())
	}
	if DefaultPrefs().MusicFormatOrDefault() != MusicLossless {
		t.Fatal("default must be lossless")
	}
}

func TestScoredReleaseJSONAudio(t *testing.T) {
	r := ScoredRelease{Result: cardigann.Result{Title: f320}, AudioFormat: "MP3 320", FormatMismatch: true}
	b, _ := r.MarshalJSON()
	if !strings.Contains(string(b), `"audioFormat":"MP3 320"`) || !strings.Contains(string(b), `"formatMismatch":true`) {
		t.Fatalf("%s", b)
	}
}

func TestSavePrefsMusicFormat(t *testing.T) {
	e := newEnv(t)
	e.reload()
	ctx := t.Context()
	p := formatPrefs(MusicMP3, BitrateV0)
	if err := e.m.SavePrefs(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, _ := e.m.Prefs(ctx)
	if got.MusicFormat != MusicMP3 || got.MP3Bitrate != BitrateV0 {
		t.Fatalf("%+v", got)
	}
	if err := e.m.SavePrefs(ctx, formatPrefs("ogg", "")); err == nil {
		t.Fatal("unknown format saved")
	}
	if err := e.m.SavePrefs(ctx, formatPrefs(MusicMP3, "999")); err == nil {
		t.Fatal("unknown bitrate saved")
	}
}

func TestScoredReleaseJSONSubs(t *testing.T) {
	title := "Dune Part Two 2024 2160p WEB-DL Dub + Sub Rus, Eng"
	r := ScoredRelease{Result: cardigann.Result{Title: title}, Info: release.Parse(title)}
	b, _ := r.MarshalJSON()
	if !strings.Contains(string(b), `"audio":["Dub"]`) || !strings.Contains(string(b), `"subs":["ru","en"]`) {
		t.Fatalf("%s", b)
	}
	r = ScoredRelease{Result: cardigann.Result{Title: "Movie 2023 1080p"}, Info: release.Parse("Movie 2023 1080p")}
	b, _ = r.MarshalJSON()
	if strings.Contains(string(b), `"subs"`) {
		t.Fatalf("subs must be omitted when none: %s", b)
	}
}
