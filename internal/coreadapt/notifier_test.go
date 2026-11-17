package coreadapt_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	_ "github.com/antresol-app/antresol-addon-torrents/internal/strings"
	"github.com/antresol-app/antresol/addonkit"
)

// notifyCore is a fake core recording POST /notify bodies.
func notifyCore(t *testing.T, status int) (*addonkit.Client, func() []addonkit.NotifyRequest) {
	t.Helper()
	var mu sync.Mutex
	var got []addonkit.NotifyRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/addon-core/v1/notify" {
			http.NotFound(w, r)
			return
		}
		var n addonkit.NotifyRequest
		json.NewDecoder(r.Body).Decode(&n)
		mu.Lock()
		got = append(got, n)
		mu.Unlock()
		w.WriteHeader(status)
		w.Write([]byte(`{"queued":1}`))
	}))
	t.Cleanup(srv.Close)
	return addonkit.NewClient(addonkit.Env{CoreURL: srv.URL + "/", Token: "tok"}), func() []addonkit.NotifyRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]addonkit.NotifyRequest(nil), got...)
	}
}

func TestNotifier_Done(t *testing.T) {
	c, got := notifyCore(t, http.StatusAccepted)
	var asked string
	n := coreadapt.Notifier{C: c, T: i18n.TL, ItemPath: func(_ context.Context, key string) string {
		asked = key
		return "/item/7"
	}}
	if err := n.Finished(context.Background(), coreadapt.Finished{Key: "abc", Kind: "movie", Title: "Фильм", Year: 2020}); err != nil {
		t.Fatal(err)
	}
	rs := got()
	if len(rs) != 1 || asked != "abc" {
		t.Fatalf("%d requests, asked %q", len(rs), asked)
	}
	r := rs[0]
	if r.Topic != "downloads" || r.Audience != "admins" || r.UserIDs != nil || r.Key != "download:abc:done" ||
		r.Subject != (addonkit.Text{RU: "Загрузка завершилась: Фильм (2020)", EN: "Download finished: Фильм (2020)"}) ||
		r.Body != nil || r.Link != "/item/7" || r.Poster != "" {
		t.Fatalf("%+v", r)
	}
	// not in the library (yet): the downloads page; no year: the title alone
	n.ItemPath = func(context.Context, string) string { return "" }
	if err := n.Finished(context.Background(), coreadapt.Finished{Key: "def", Kind: "series", Title: "Сериал"}); err != nil {
		t.Fatal(err)
	}
	if r := got()[1]; r.Link != "/addons/torrents/downloads" || r.Subject.RU != "Загрузка завершилась: Сериал" {
		t.Fatalf("%+v", r)
	}
}

func TestNotifier_Failed(t *testing.T) {
	c, got := notifyCore(t, http.StatusAccepted)
	n := coreadapt.Notifier{C: c, T: i18n.TL, ItemPath: func(context.Context, string) string {
		t.Fatal("ItemPath asked for a failure")
		return ""
	}}
	f := coreadapt.Finished{Key: "abc", Kind: "movie", Title: "Фильм", Year: 2020, Failed: true, Error: "Не удалось сопоставить серии"}
	if err := n.Finished(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	r := got()[0]
	if r.Key != "download:abc:failed" || r.Link != "/addons/torrents/downloads" ||
		r.Subject != (addonkit.Text{RU: "Загрузка не удалась: Фильм (2020)", EN: "Download failed: Фильм (2020)"}) ||
		r.Body == nil || *r.Body != (addonkit.Text{RU: "Не удалось сопоставить серии", EN: "Couldn’t match the files to episodes"}) {
		t.Fatalf("%+v body %+v", r, r.Body)
	}
	// no error text: no body
	f.Key, f.Error = "def", ""
	n.Finished(context.Background(), f)
	if r := got()[1]; r.Body != nil {
		t.Fatalf("body %+v", r.Body)
	}
}

// A refused letter (and a down core) is an error for the caller to log.
func TestNotifier_CoreRefuses(t *testing.T) {
	c, _ := notifyCore(t, http.StatusBadRequest)
	n := coreadapt.Notifier{C: c, T: i18n.TL, ItemPath: func(context.Context, string) string { return "" }}
	if err := n.Finished(context.Background(), coreadapt.Finished{Key: "abc", Title: "Фильм"}); err == nil {
		t.Fatal("400 is not an error")
	}
	if err := n.Finished(context.Background(), coreadapt.Finished{Title: "без ключа"}); err != nil {
		t.Fatalf("no key: %v", err)
	}
}

// The letter's link is the core's page of the title a download brought: a
// movie or series of the acquisitions the library has, by its TMDB id.
func TestAcquisitionItemPath(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`INSERT INTO acquisitions(infohash, kind, tmdb_id, title) VALUES('aaaa', 'movie', 603, 'Матрица')`,
		`INSERT INTO acquisitions(infohash, kind, tmdb_id, title) VALUES('bbbb', 'series', 42, 'Сёгун')`,
		`INSERT INTO acquisitions(infohash, kind, tmdb_id, title) VALUES('cccc', 'movie', 999, 'Нет в медиатеке')`,
		`INSERT INTO acquisitions(infohash, kind, tmdb_id, title) VALUES('dddd', 'music', 0, 'Альбом')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/addon-core/v1/library/tmdb/movie/603":
			w.Write([]byte(`{"id":7,"kind":"movie"}`))
		case "/api/addon-core/v1/library/tmdb/series/42":
			w.Write([]byte(`{"id":8,"kind":"series"}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":"Не найдено"}`))
		}
	}))
	t.Cleanup(srv.Close)
	p := coreadapt.AcquisitionItemPath(db, addonkit.NewClient(addonkit.Env{CoreURL: srv.URL + "/", Token: "tok"}))
	for key, want := range map[string]string{"aaaa": "/item/7", "AAAA": "/item/7", "bbbb": "/item/8", "cccc": "", "dddd": "", "direct-1": ""} {
		if got := p(context.Background(), key); got != want {
			t.Errorf("%s: %q, want %q", key, got, want)
		}
	}
}

// Texts over the core's limits (subject 200, body 2000 characters per
// language) are cut, so the letter is never refused for its length.
func TestNotifier_CutsLongTexts(t *testing.T) {
	c, got := notifyCore(t, http.StatusAccepted)
	n := coreadapt.Notifier{C: c, T: i18n.TL, ItemPath: func(context.Context, string) string { return "" }}
	long := strings.Repeat("я", 3000)
	if err := n.Finished(context.Background(), coreadapt.Finished{Key: "abc", Title: long, Failed: true, Error: long}); err != nil {
		t.Fatal(err)
	}
	r := got()[0]
	for _, s := range []string{r.Subject.RU, r.Subject.EN} {
		if len([]rune(s)) != 200 {
			t.Fatalf("subject %d characters", len([]rune(s)))
		}
	}
	if len([]rune(r.Body.RU)) != 2000 || len([]rune(r.Body.EN)) != 2000 {
		t.Fatalf("body %d / %d characters", len([]rune(r.Body.RU)), len([]rune(r.Body.EN)))
	}
}

// A finished download's letter over the core's recorded answer (one queued).
func TestNotifier_Replayed(t *testing.T) {
	n := coreadapt.Notifier{C: replay(t), T: i18n.TL}
	if err := n.Finished(ctx, coreadapt.Finished{Key: "k-0001", Kind: "movie", Title: "Дюна", Year: 2021}); err != nil {
		t.Fatal(err)
	}
}
