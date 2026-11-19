package cardigann

import (
	"net/url"
	"strings"
	"testing"
)

func TestTopicSameSite(t *testing.T) {
	base, _ := url.Parse("https://rutracker.org/forum/")
	for link, want := range map[string]bool{
		"https://rutracker.org/forum/viewtopic.php?t=1": true,
		"https://m.rutracker.org/forum/viewtopic.php":   true,
		"https://evil.example/rutracker.org":            false,
		"https://notrutracker.org/x":                    false,
	} {
		u, _ := url.Parse(link)
		if got := sameSite(base, u); got != want {
			t.Errorf("%s: %v, want %v", link, got, want)
		}
	}
}

func TestTopicTextFirstPost(t *testing.T) {
	got, err := topicText(`<body><div class="post_body">Перевод: LostFilm<br>Звук: Dub</div><div class="post_body">Перевод: Чужой</div></body>`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Перевод: LostFilm\nЗвук: Dub") || strings.Contains(got, "Чужой") {
		t.Fatalf("got %q", got)
	}
}
