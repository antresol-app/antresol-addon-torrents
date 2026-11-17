package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A RuTracker page with a release of «Звуковые дорожки и Переводы» (forum
// 185, Audio in the definition) and one of a series forum (2366, TV/HD).
func rutrackerAudioServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/forum/index.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		w.Write(cp1251(t, `<html><body><a id="logged-in-username" href="profile.php">alex</a></body></html>`))
	})
	mux.HandleFunc("/forum/tracker.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		audio := strings.Replace(strings.Replace(rutrackerRow, "f=2366", "f=185", 1), "%s",
			"Миллиарды / Billions / Сезон: 3 (2018) Звуковая дорожка LostFilm", 1)
		audio = strings.ReplaceAll(audio, "t=6512345", "t=7000001")
		video := strings.Replace(rutrackerRow, "%s", "Миллиарды / Billions / Сезон: 3 [2018, WEB-DL 1080p] MVO (LostFilm)", 1)
		w.Write(cp1251(t, `<html><body><a id="logged-in-username">alex</a><div id="search-results"><table><tbody>`+audio+video+`</tbody></table></div></body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// «Найти озвучку…» searches the title's kind and the Audio category in one
// query: RuTracker's «Звуковые дорожки» forum comes through (by the real
// definition's category mapping); a plain series search drops it.
func TestVoiceOverSearchKeepsAudioForum(t *testing.T) {
	srv := rutrackerAudioServer(t)
	e := newEnv(t)
	e.reload()
	ctx := context.Background()
	if _, err := e.m.Add(ctx, "rutracker-org-cookie", map[string]string{"cookie": "bb_session=0-1-abc", "sitelink": srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	titles := func(r SearchResponse) []string {
		var out []string
		for _, x := range append(r.Releases, r.Rejected...) {
			out = append(out, x.Title)
		}
		return out
	}
	has := func(ts []string, part string) bool {
		for _, s := range ts {
			if strings.Contains(s, part) {
				return true
			}
		}
		return false
	}
	plain, err := e.m.Search(ctx, SearchRequest{Text: "Миллиарды", Kind: "series"})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(plain); has(got, "Звуковая дорожка") || !has(got, "WEB-DL") {
		t.Fatalf("plain series search: %v", got)
	}
	vo, err := e.m.Search(ctx, SearchRequest{Text: "Миллиарды", Kind: "series", VoiceOvers: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := titles(vo); !has(got, "Звуковая дорожка") || !has(got, "WEB-DL") {
		t.Fatalf("voice-over search: %v", got)
	}
}
