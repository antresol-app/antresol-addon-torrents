package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

const topicPage = `<html><body><a id="logged-in-username">alex</a>
<table><tr><td class="message"><div class="post_body" id="p-1">
<span class="post-b">Сёгун</span><br>
<span class="post-b">Год выпуска</span>: 2024<br>
<span class="post-b">Перевод</span>: Профессиональный (многоголосый закадровый) LostFilm<br>
<span class="post-b">Перевод 2</span>: Авторский (одноголосый закадровый) Пётр Гланц<br>
<span class="post-b">Субтитры</span>: русские<br>
</div></td></tr></table>
<div class="post_body">Комментарий: перевод: дубляж от кого-то ещё</div>
</body></html>`

// A RuTracker release whose name names no voice-over is scored by its
// page: the page's studios and voice-over kinds join its name's.
func TestTopicVoiceOversFromReleasePage(t *testing.T) {
	var pages atomic.Int32
	mux := http.NewServeMux()
	logged := func(r *http.Request) bool {
		c, err := r.Cookie("bb_session")
		return err == nil && c.Value == "0-1-abc"
	}
	page := func(w http.ResponseWriter, s string) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		w.Write(cp1251(t, s))
	}
	mux.HandleFunc("/forum/index.php", func(w http.ResponseWriter, r *http.Request) {
		page(w, `<html><body><a id="logged-in-username" href="profile.php">alex</a></body></html>`)
	})
	mux.HandleFunc("/forum/tracker.php", func(w http.ResponseWriter, r *http.Request) {
		if !logged(r) {
			http.Redirect(w, r, "/forum/login.php", http.StatusFound)
			return
		}
		row := strings.Replace(rutrackerRow, "%s", "Сёгун / Shogun / Сезон: 1 / Серии: 1-10 из 10 [2024, США, драма, WEB-DL 1080p]", 1)
		page(w, `<html><body><a id="logged-in-username">alex</a><div id="search-results"><table><tbody>`+row+`</tbody></table></div></body></html>`)
	})
	mux.HandleFunc("/forum/viewtopic.php", func(w http.ResponseWriter, r *http.Request) {
		if !logged(r) || r.URL.Query().Get("t") != "6512345" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		pages.Add(1)
		page(w, topicPage)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	e := newEnv(t)
	e.reload()
	ctx := context.Background()
	src, err := e.m.Add(ctx, "rutracker-org-cookie", map[string]string{"cookie": "bb_session=0-1-abc", "sitelink": srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if src.Origin != "" || !src.SyncedAt.IsZero() {
		t.Fatalf("a source added by hand: origin %q synced %v", src.Origin, src.SyncedAt)
	}
	for range 2 {
		resp, err := e.m.Search(ctx, SearchRequest{Text: "Сёгун", Kind: "series", Season: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Releases) != 1 {
			t.Fatalf("releases = %+v, failed = %+v", resp.Releases, resp.Failed)
		}
		r := resp.Releases[0]
		if !slices.Contains(r.Info.Studios, "LostFilm") || !slices.Contains(r.Info.Audio, "MVO") || !slices.Contains(r.Info.Audio, "AVO") {
			t.Fatalf("info = %+v: want LostFilm, MVO and AVO from the page", r.Info)
		}
		if slices.Contains(r.Info.Audio, "Dub") {
			t.Fatalf("a comment's words counted: %+v", r.Info.Audio)
		}
		if !slices.Contains(r.Reasons, "многоголосый перевод") {
			t.Fatalf("reasons = %v", r.Reasons)
		}
	}
	if n := pages.Load(); n != 1 {
		t.Fatalf("release page read %d times, want once (cached)", n)
	}
}

func TestParseTopic(t *testing.T) {
	got := parseTopic("Название: X\nАудио 1: Русский (AC3, 6 ch) Дублированный | Кубик в Кубе\n * Звук #2: English\nПеревод:   \n")
	if !slices.Contains(got.Studios, "Кубик в Кубе") || !slices.Equal(got.Audio, []string{"Dub"}) {
		t.Fatalf("got %+v", got)
	}
	if !parseTopic("Описание: про перевод: ничего").empty() {
		t.Fatal("a label mid-line was read")
	}
}

// A studio the household typed (outside the table) counts when the page
// names it; the name's own voice-over kind stays.
func TestWithTopic(t *testing.T) {
	info := release.Parse("Фильм [2024, WEB-DL 1080p] Dub")
	got := withTopic(info, parseTopic("Перевод: Многоголосый закадровый, Студия Пифагор"), []string{"Студия Пифагор"})
	if !slices.Contains(got.Studios, "Студия Пифагор") {
		t.Fatalf("studios = %v", got.Studios)
	}
	if !slices.Equal(got.Audio, info.Audio) {
		t.Fatalf("audio = %v, want the name's %v", got.Audio, info.Audio)
	}
}

func TestTopicCacheExpires(t *testing.T) {
	var c topicCache
	now := time.Now()
	c.put("a", topicInfo{Voice: "x"}, time.Minute, now)
	if _, ok := c.get("a", now.Add(30*time.Second)); !ok {
		t.Fatal("missing")
	}
	if _, ok := c.get("a", now.Add(2*time.Minute)); ok {
		t.Fatal("not expired")
	}
}
