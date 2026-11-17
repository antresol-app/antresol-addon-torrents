package organize

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/extaudio"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/media"
)

// A single-video release with its soundtrack: nothing comes into the library.
func TestOrganizeMovieOSTNotLinked(t *testing.T) {
	root := t.TempDir()
	tor := "Film.2020.1080p"
	writeFile(t, root, "torrents/"+tor+"/Film.2020.1080p.mkv", "v")
	writeFile(t, root, "torrents/"+tor+"/OST/01 Main Theme.mp3", "1")
	writeFile(t, root, "torrents/"+tor+"/Soundtrack/02 End.flac", "2")
	writeFile(t, root, "torrents/"+tor+"/Audio/03 Credits.mp3", "3")
	movies := filepath.Join(root, "media/movies")
	if _, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "Фильм", Year: 2020, MoviesDir: movies,
		SeriesDir: filepath.Join(root, "media/series"),
		Files: onDisk(t, root, tor+"/Film.2020.1080p.mkv", tor+"/OST/01 Main Theme.mp3", tor+"/Soundtrack/02 End.flac",
			tor+"/Audio/03 Credits.mp3")}); err != nil {
		t.Fatal(err)
	}
	es, _ := os.ReadDir(filepath.Join(movies, "Фильм (2020)"))
	for _, e := range es {
		if extaudio.IsAudio(e.Name()) {
			t.Fatalf("linked %s", e.Name())
		}
	}
}

// With a prober, a track of another length (another cut) is not linked.
func TestOrganizeChecksTrackDuration(t *testing.T) {
	root := t.TempDir()
	tor := "Film.2020.1080p"
	writeFile(t, root, "torrents/"+tor+"/Film.2020.1080p.mkv", "v")
	writeFile(t, root, "torrents/"+tor+"/Audio/Rus.LostFilm.ac3", "same")
	writeFile(t, root, "torrents/"+tor+"/Audio/Rus.TVShows.ac3", "short")
	probe := func(_ context.Context, p string) (media.ProbeResult, error) {
		switch filepath.Base(p) {
		case "Film.2020.1080p.mkv":
			return media.ProbeResult{DurationMs: 6_000_000}, nil
		case "Rus.LostFilm.ac3":
			return media.ProbeResult{DurationMs: 6_010_000, Audio: []media.AudioStream{{Codec: "ac3"}}}, nil
		}
		return media.ProbeResult{DurationMs: 4_000_000, Audio: []media.AudioStream{{Codec: "ac3"}}}, nil
	}
	movies := filepath.Join(root, "media/movies")
	if _, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "Фильм", Year: 2020, MoviesDir: movies,
		SeriesDir: filepath.Join(root, "media/series"), Probe: probe,
		Files: onDisk(t, root, tor+"/Film.2020.1080p.mkv", tor+"/Audio/Rus.LostFilm.ac3", tor+"/Audio/Rus.TVShows.ac3")}); err != nil {
		t.Fatal(err)
	}
	got := extaudio.Find(filepath.Join(movies, "Фильм (2020)", "Фильм (2020).mkv"))
	if len(got) != 1 || got[0].Studio != "LostFilm" {
		t.Fatalf("%+v", got)
	}
}

// A season pack's «RUS Sound» tracks come into the library beside their
// episodes, named so that the player finds them (extaudio.Find) with
// their language and studio.
func TestOrganizeSeriesPlacesAudioTracks(t *testing.T) {
	root := t.TempDir()
	tor := "Billions.S03.1080p"
	writeFile(t, root, "torrents/"+tor+"/Billions.S03E01.mkv", "1")
	writeFile(t, root, "torrents/"+tor+"/Billions.S03E02.mkv", "2")
	writeFile(t, root, "torrents/"+tor+"/RUS Sound/[LostFilm]/Billions.S03E01.ac3", "a1")
	writeFile(t, root, "torrents/"+tor+"/RUS Sound/[LostFilm]/Billions.S03E02.ac3", "a2")
	writeFile(t, root, "torrents/"+tor+"/RUS Sound/[LostFilm]/Billions.S03E02 (alt).ac3", "a2alt")
	// not this torrent's: a neighbour lying in the same folder is never taken
	writeFile(t, root, "torrents/"+tor+"/RUS Sound/[LostFilm]/Billions.S03E01.eac3", "x")
	series := filepath.Join(root, "media/series")
	_, err := Organize(context.Background(), Job{Kind: library.KindSeries, Title: "Миллиарды", Season: 3, SeriesDir: series,
		MoviesDir: filepath.Join(root, "media/movies"),
		Files: onDisk(t, root, tor+"/Billions.S03E01.mkv", tor+"/Billions.S03E02.mkv", tor+"/RUS Sound/[LostFilm]/Billions.S03E01.ac3",
			tor+"/RUS Sound/[LostFilm]/Billions.S03E02.ac3", tor+"/RUS Sound/[LostFilm]/Billions.S03E02 (alt).ac3")})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(series, "Миллиарды", "Season 03")
	for name, want := range map[string]string{"Миллиарды - S03E01.ru.LostFilm.ac3": "a1", "Миллиарды - S03E02.ru.LostFilm.ac3": "a2alt",
		"Миллиарды - S03E02.ru.LostFilm.2.ac3": "a2"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.*ac3")); len(m) != 3 {
		t.Fatalf("%v", m)
	}
	got := extaudio.Find(filepath.Join(dir, "Миллиарды - S03E01.mkv"))
	if len(got) != 1 || got[0].Lang != "ru" || got[0].Studio != "LostFilm" {
		t.Fatalf("%+v", got)
	}
	// a second run places nothing new
	if _, err := Organize(context.Background(), Job{Kind: library.KindSeries, Title: "Миллиарды", Season: 3, SeriesDir: series,
		MoviesDir: filepath.Join(root, "media/movies"),
		Files: onDisk(t, root, tor+"/Billions.S03E01.mkv", tor+"/Billions.S03E02.mkv", tor+"/RUS Sound/[LostFilm]/Billions.S03E01.ac3",
			tor+"/RUS Sound/[LostFilm]/Billions.S03E02.ac3", tor+"/RUS Sound/[LostFilm]/Billions.S03E02 (alt).ac3")}); err != nil {
		t.Fatal(err)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.*ac3")); len(m) != 3 {
		t.Fatalf("again: %v", m)
	}
}

func TestOrganizeMoviePlacesAudioTracks(t *testing.T) {
	root := t.TempDir()
	tor := "Film.2020.1080p"
	writeFile(t, root, "torrents/"+tor+"/Film.2020.1080p.mkv", "v")
	writeFile(t, root, "torrents/"+tor+"/Audio/Rus/Кубик в Кубе.ac3", "k")
	writeFile(t, root, "torrents/"+tor+"/Film.2020.1080p.eng.mka", "e")
	movies := filepath.Join(root, "media/movies")
	if _, err := Organize(context.Background(), Job{Kind: library.KindMovie, Title: "Фильм", Year: 2020, MoviesDir: movies,
		SeriesDir: filepath.Join(root, "media/series"),
		Files:     onDisk(t, root, tor+"/Film.2020.1080p.mkv", tor+"/Audio/Rus/Кубик в Кубе.ac3", tor+"/Film.2020.1080p.eng.mka")}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(movies, "Фильм (2020)")
	got := extaudio.Find(filepath.Join(dir, "Фильм (2020).mkv"))
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if filepath.Base(got[0].Path) != "Фильм (2020).en.mka" || got[0].Lang != "en" ||
		filepath.Base(got[1].Path) != "Фильм (2020).ru.Кубик в Кубе.ac3" || got[1].Studio != "Кубик в Кубе" {
		t.Fatalf("%+v", got)
	}
}
