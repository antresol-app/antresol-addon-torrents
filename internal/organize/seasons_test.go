package organize

import "testing"

func TestFolderSeason(t *testing.T) {
	for p, want := range map[string]int{
		"Pack/Season 3/01.mkv":            3,
		"Pack/Сезон 3/01.mkv":             3,
		"Pack/S03/Disc 1/01.mkv":          3,
		"Pack/3 сезон/01.mkv":             3,
		"Suits.S03.1080p/01.mkv":          3,
		"Suits S01-S03/01.mkv":            0,
		"Suits S01-S03/Season 2/01.mkv":   2,
		"Suits/01.mkv":                    0,
		"Suits S01-S03/Extras/making.mkv": 0,
	} {
		if got := FolderSeason(p); got != want {
			t.Errorf("folderSeason(%q) = %d, want %d", p, got, want)
		}
	}
}
