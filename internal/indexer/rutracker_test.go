package indexer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/text/encoding/charmap"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

// cp1251 encodes a test page like RuTracker does (non-cp1251 letters are
// HTML entities on the real site, e.g. ō = &#333;).
func cp1251(t *testing.T, s string) []byte {
	t.Helper()
	b, err := charmap.Windows1251.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("cp1251: %v", err)
	}
	return b
}

const rutrackerRow = `<tr class="tCenter hl-tr">
<td class="row1 f-name-col"><a class="gen f" href="tracker.php?f=2366">HD Video</a></td>
<td class="row4 med tLeft t-title-col tt"><a class="med tLink tt-text ts-text hl-tags bold" href="viewtopic.php?t=6512345">%s</a></td>
<td class="row4 small nowrap tor-size"><a class="small tr-dl dl-stub" href="dl.php?t=6512345">20.5 GB &#8595;</a></td>
<td class="row4 nowrap"><b class="seedmed">120</b></td>
<td class="row4 leechmed bold">5</td>
<td class="row4 small number-format">1500</td>
<td class="row4 small nowrap"><p>14-Ноя-24</p></td>
</tr>`

const rutrackerFullSeason = "Сёгун / Sh&#333;gun / Сезон: 1 / Серии: 1-10 из 10 (Джонатан ван Тюлекен) [2024, США, драма, WEB-DL 1080p] MVO (HDRezka Studio) + Original"

func rutrackerServer(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	logged := func(r *http.Request) bool {
		c, err := r.Cookie("bb_session")
		return err == nil && c.Value == "0-1-abc"
	}
	mux.HandleFunc("/forum/index.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		if logged(r) {
			w.Write(cp1251(t, `<html><body><a id="logged-in-username" href="profile.php">alex</a></body></html>`))
			return
		}
		w.Write(cp1251(t, `<html><body><a href="login.php">Вход</a></body></html>`))
	})
	mux.HandleFunc("/forum/tracker.php", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-1251")
		if !logged(r) {
			http.Redirect(w, r, "/forum/login.php", http.StatusFound)
			return
		}
		row := strings.Replace(rutrackerRow, "%s", rutrackerFullSeason, 1)
		w.Write(cp1251(t, `<html><body><a id="logged-in-username">alex</a><div id="search-results"><table><tbody>`+row+`</tbody></table></div></body></html>`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The Vault RuTracker definitions keep the site's own title: a full season
// «Серии: 1-10 из 10» is scored as a full season and the Russian name matches.
func TestRutrackerKeepsTitleAndFullSeason(t *testing.T) {
	srv := rutrackerServer(t)
	e := newEnv(t)
	e.reload()
	ctx := context.Background()
	s, err := e.m.Add(ctx, "rutracker-org-cookie", map[string]string{"cookie": "bb_session=0-1-abc", "sitelink": srv.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != StatusOK {
		t.Fatalf("status %q: %s", s.Status, s.LastError)
	}
	resp, err := e.m.Search(ctx, SearchRequest{Text: "Сёгун", Kind: "series", Season: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Releases) != 1 {
		t.Fatalf("releases = %+v, rejected = %+v, failed = %+v", resp.Releases, resp.Rejected, resp.Failed)
	}
	r := resp.Releases[0]
	if !strings.HasPrefix(r.Title, "Сёгун / Shōgun / Сезон: 1 / Серии: 1-10 из 10") {
		t.Fatalf("title rewritten: %q", r.Title)
	}
	if !r.Info.IsSeasonPack || r.Seeders != 120 || r.Link == "" {
		t.Fatalf("release = %+v", r)
	}
	for _, why := range r.Reasons {
		if strings.Contains(why, "не весь сезон") {
			t.Fatalf("full season scored as partial: %v", r.Reasons)
		}
	}
}

// RuTracker shows an image captcha after failed logins: the user gets the
// hint to use the cookie variant, not "wrong password".
func TestRutrackerCaptchaHint(t *testing.T) {
	for _, when := range []string{"login page", "after post"} {
		t.Run(when, func(t *testing.T) {
			var posted url.Values
			const captcha = `<img src="https://static.rutracker.cc/captcha/0/abc.jpg"><input type="hidden" name="cap_sid" value="x"><input type="text" name="cap_code_x">`
			mux := http.NewServeMux()
			mux.HandleFunc("/forum/login.php", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=windows-1251")
				extra := ""
				if r.Method == http.MethodPost {
					r.ParseForm()
					posted = r.PostForm
					extra = `<h4 class="warnColor1 tCenter mrg_16">Вы ввели неверный код подтверждения</h4>` + captcha
				} else if when == "login page" {
					extra = captcha
				}
				w.Write(cp1251(t, `<html><body><div id="login-form-full"><form action="login.php" method="post">`+extra+
					`<input name="login_username"><input type="password" name="login_password"><input type="submit" name="login" value="Вход"></form></div></body></html>`))
			})
			srv := httptest.NewServer(mux)
			defer srv.Close()

			e := newEnv(t)
			e.reload()
			s, err := e.m.Add(context.Background(), "rutracker", map[string]string{"username": "alex", "password": "pw", "sitelink": srv.URL + "/"})
			if err != nil {
				t.Fatal(err)
			}
			if s.Status != StatusError || !strings.Contains(s.LastError, "капч") || !strings.Contains(s.LastError, "cookie") {
				t.Fatalf("status %q, error %q: want the captcha hint", s.Status, s.LastError)
			}
			err = e.m.Test(context.Background(), s.ID)
			if !errors.Is(err, cardigann.ErrCaptcha) {
				t.Fatalf("Test err = %v, want ErrCaptcha", err)
			}
			if when == "after post" {
				got, _ := charmap.Windows1251.NewDecoder().String(posted.Get("login"))
				if got != "Вход" {
					t.Fatalf("login button value = %q, want Вход", got)
				}
			}
		})
	}
}
