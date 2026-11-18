package migrate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// movedReport is a finished real run that moved one torrent to Vault.
func (e *svcEnv) movedReport() *Report {
	e.t.Helper()
	from := &Report{ID: NewReportID(e.clock()), Sources: []string{"qbittorrent"}, StartedAt: e.clock(), FinishedAt: e.clock(),
		Lines: []Line{{Source: SourceQBittorrent, Kind: "torrent", Name: "Дюна", Status: StatusImported, Ref: strings.Repeat("ab", 20), Prev: PrevStopped}}}
	if err := SaveReport(e.reports, from); err != nil {
		e.t.Fatal(err)
	}
	e.advance(time.Minute)
	return from
}

func rollbackReports(t *testing.T, e *svcEnv, of string) []*Report {
	t.Helper()
	list, err := ListReports(e.reports)
	if err != nil {
		t.Fatal(err)
	}
	var out []*Report
	for _, r := range list {
		if r.RollbackOf == of {
			full, err := LoadReport(e.reports, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, full)
		}
	}
	return out
}

// The connection typed on the page is gone (Forget, a restart): a
// rollback is refused up front and leaves no report behind, so it is not
// burnt; connecting again makes it work.
func TestRollbackWithoutConnectionIsRefusedNotBurnt(t *testing.T) {
	e := newSvcEnv(t, nil)
	ctx := context.Background()
	qb := fakeQB(t)
	if _, err := e.s.Connect(ctx, SourceQBittorrent, Remote{URL: qb.URL}); err != nil {
		t.Fatal(err)
	}
	from := e.movedReport()
	if err := e.s.Forget(SourceQBittorrent); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Rollback(ctx, from.ID); !errors.Is(err, ErrNoQBConnection) {
		t.Fatalf("rollback without a connection: %v", err)
	}
	if got := rollbackReports(t, e, from.ID); len(got) != 0 {
		t.Fatalf("a refused rollback left reports: %+v", got)
	}
	if _, err := e.s.Connect(ctx, SourceQBittorrent, Remote{URL: qb.URL}); err != nil {
		t.Fatal(err)
	}
	id, err := e.s.Rollback(ctx, from.ID)
	if err != nil {
		t.Fatalf("retry after connecting again: %v", err)
	}
	e.s.Wait()
	rep, _ := LoadReport(e.reports, id)
	if rep.FinishedAt.IsZero() || len(rep.Lines) != 1 || !strings.HasPrefix(rep.Lines[0].Reason, "Возвращена в qBittorrent") {
		t.Fatalf("rollback %+v", rep)
	}
}

// A qBittorrent that cannot be reached when the rollback runs did
// nothing: that report is not a finished rollback, the next try is allowed.
func TestRollbackUnreachableCanBeRetried(t *testing.T) {
	e := newSvcEnv(t, nil)
	ctx := context.Background()
	from := e.movedReport()
	e.s.QB.WebURL = "http://127.0.0.1:1"
	id, err := e.s.Rollback(ctx, from.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.s.Wait()
	rep, _ := LoadReport(e.reports, id)
	if !rep.FinishedAt.IsZero() {
		t.Fatalf("an unreached rollback was finished: %+v", rep)
	}
	if l := onlyLine(t, rep); l.Status != StatusFailed || !strings.Contains(l.Reason, "Нет связи с qBittorrent (http://127.0.0.1:1)") || !strings.Contains(l.Reason, "повторите") {
		t.Fatalf("line %+v", l)
	}
	e.advance(time.Minute)
	qb := fakeQB(t)
	e.s.QB.WebURL = qb.URL
	id2, err := e.s.Rollback(ctx, from.ID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	e.s.Wait()
	rep2, _ := LoadReport(e.reports, id2)
	if rep2.FinishedAt.IsZero() || !strings.HasPrefix(rep2.Lines[0].Reason, "Возвращена в qBittorrent") {
		t.Fatalf("retry %+v", rep2)
	}
	e.advance(time.Minute)
	if _, err := e.s.Rollback(ctx, from.ID); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("a finished rollback can be asked again: %v", err)
	}
}

// An address with a login, a password, a query or a fragment is
// refused before any request, and nothing is kept.
func TestConnectRefusesUserinfoAndQuery(t *testing.T) {
	e := newSvcEnv(t, nil)
	ctx := context.Background()
	api := sonarrAPI(t, nil, nil, false)
	qb := fakeQB(t)
	host := strings.TrimPrefix(api.URL, "http://")
	for _, u := range []string{
		"http://admin:pw@" + host,
		"http://admin@" + host,
		api.URL + "/?apikey=" + fakeAPIKey,
		api.URL + "#top",
		api.URL + "?",
	} {
		_, err := e.s.Connect(ctx, SourceSonarr, Remote{URL: u, APIKey: fakeAPIKey})
		var re *RemoteError
		if !errors.As(err, &re) || !strings.Contains(re.Msg, "не должен содержать логин, пароль или параметры") {
			t.Errorf("sonarr %q: %v", u, err)
		}
		if re != nil && (strings.Contains(re.Msg, "pw") || strings.Contains(re.Msg, fakeAPIKey)) {
			t.Errorf("sonarr %q: the message repeats the secret: %q", u, re.Msg)
		}
		_, err = e.s.Connect(ctx, SourceQBittorrent, Remote{URL: strings.Replace(u, host, strings.TrimPrefix(qb.URL, "http://"), 1), Username: "a", Password: "b"})
		if !errors.As(err, &re) || !strings.Contains(re.Msg, "не должен содержать логин, пароль или параметры") {
			t.Errorf("qbittorrent %q: %v", u, err)
		}
	}
	if n := len(api.paths) + len(qb.all()); n != 0 {
		t.Fatalf("%d requests went out for refused addresses", n)
	}
	for _, id := range []string{SourceSonarr, SourceQBittorrent} {
		if v, _ := e.s.Remote(id); v.Connected {
			t.Fatalf("%s kept: %+v", id, v)
		}
	}
}

// Defence in depth: an address that skipped the check (the environment's)
// never shows its login or password in a log line, a report line or a message.
func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://admin:hunter2@qb:8080":       "http://qb:8080",
		"https://qb.example/sub/?apikey=K#x": "https://qb.example/sub",
		"http://qb:8080/":                    "http://qb:8080",
		"::nonsense":                         "",
		"":                                   "",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
	o := QBOptions{WebURL: "http://admin:hunter2@127.0.0.1:1"}
	if r := noAPIReason(o); strings.Contains(r, "hunter2") || strings.Contains(r, "admin") {
		t.Fatalf("noAPIReason: %q", r)
	}
}

func TestRollbackLineHidesUserinfo(t *testing.T) {
	e := newEnv(t)
	e.addTorrent(map[string][]byte{"A/f.mkv": randBytes(64 << 10)}, nil)
	from := e.run(e.opts())
	o := e.opts()
	o.WebURL = "http://admin:hunter2@127.0.0.1:1"
	rep := &Report{ID: NewReportID(time.Now())}
	if err := RollbackQB(context.Background(), o, e.eng, from, rep); !errors.Is(err, ErrQBUnreachable) {
		t.Fatalf("err %v", err)
	}
	if l := onlyLine(t, rep); strings.Contains(l.Reason, "hunter2") || strings.Contains(l.Reason, "admin") || !strings.Contains(l.Reason, "http://127.0.0.1:1") {
		t.Fatalf("line %+v", l)
	}
}

// A redirect to another host is never followed: the key header and the
// login form stay home.
func TestRedirectToAnotherHostNotFollowed(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer other.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	ctx := context.Background()

	err := newArrClient("Sonarr", Remote{URL: redir.URL, APIKey: fakeAPIKey}, nil).check(ctx)
	var re *RemoteError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "перенаправляет на другой адрес") {
		t.Fatalf("arr: %v", err)
	}
	_, err = qbAPICount(ctx, QBOptions{WebURL: redir.URL, Username: "admin", Password: "secret"})
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "перенаправляет на другой адрес") {
		t.Fatalf("qb: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests reached the other host", hits.Load())
	}
	// the same host is fine
	same := sonarrAPI(t, nil, nil, false)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, same.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer front.Close()
	// (front → same is another host too: refused)
	if err := newArrClient("Sonarr", Remote{URL: front.URL, APIKey: fakeAPIKey}, nil).check(ctx); err == nil {
		t.Fatal("a redirect to another port was followed")
	}
	if len(same.paths) != 0 {
		t.Fatalf("requests reached the target: %v", same.paths)
	}
	stay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old/api/v3/system/status" {
			http.Redirect(w, r, "/api/v3/system/status", http.StatusTemporaryRedirect)
			return
		}
		same.Config.Handler.ServeHTTP(w, r)
	}))
	defer stay.Close()
	if err := newArrClient("Sonarr", Remote{URL: stay.URL + "/old", APIKey: fakeAPIKey}, nil).check(ctx); err != nil {
		t.Fatalf("a redirect on the same host: %v", err)
	}
}

// An answer over the limit is said to be too big, not "not Sonarr".
func TestArrBodyOverLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := []byte(strings.Repeat(" ", 1<<20))
		for i := 0; i <= arrBodyLimit>>20; i++ {
			w.Write(chunk)
		}
		w.Write([]byte("[]"))
	}))
	defer srv.Close()
	err := newArrClient("Sonarr", Remote{URL: srv.URL, APIKey: fakeAPIKey}, nil).get(context.Background(), "series", nil, new([]apiSeries))
	var re *RemoteError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "Ответ Sonarr слишком большой") || strings.Contains(re.Msg, "отвечает не") {
		t.Fatalf("err %v", err)
	}
}

func TestQBBodyOverLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()
	qb := newQBClient(QBOptions{WebURL: srv.URL})
	if _, _, _, err := qb.doN(context.Background(), http.MethodGet, "/x", nil, nil, 100); err != nil {
		t.Fatalf("exactly the limit: %v", err)
	}
	if _, _, _, err := qb.doN(context.Background(), http.MethodGet, "/x", nil, nil, 99); !errors.Is(err, errQBTooLarge) {
		t.Fatalf("over the limit: %v", err)
	}
}

// A service that was down when the page looked is asked again by the
// next look; a good answer is still kept.
func TestDiscoverDoesNotCacheAPIError(t *testing.T) {
	e := newSvcEnv(t, nil)
	ctx := context.Background()
	api := sonarrAPI(t, []sonarrRow{{ID: 1, Title: "С", Year: 2001, TmdbID: 1, Monitored: true}}, nil, false)
	inner := api.Config.Handler
	var down atomic.Bool
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer front.Close()
	if _, err := e.s.Connect(ctx, SourceSonarr, Remote{URL: front.URL, APIKey: fakeAPIKey}); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	if st := discovered(t, e, SourceSonarr); st.Error == "" {
		t.Fatalf("no error while down: %+v", st)
	}
	n := e.s.Scans()
	down.Store(false)
	if st := discovered(t, e, SourceSonarr); st.Error != "" || st.Detail != "1 сериал, 1 отслеживается" {
		t.Fatalf("after it came back: %+v", st)
	}
	if e.s.Scans() != n+1 {
		t.Fatalf("the error was cached: %d scans, want %d", e.s.Scans(), n+1)
	}
	discovered(t, e, SourceSonarr)
	if e.s.Scans() != n+1 {
		t.Fatalf("a good answer was not cached: %d scans", e.s.Scans())
	}
}

// A 403 on the login is an address ban, not a wrong password.
func TestQBLoginBanMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	var re *RemoteError
	_, err := qbAPICount(context.Background(), QBOptions{WebURL: srv.URL, Username: "admin", Password: "x"})
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "заблокирован") || strings.Contains(re.Msg, "укажите логин") {
		t.Fatalf("with a login: %v", err)
	}
	// no login typed: the old advice stands
	_, err = qbAPICount(context.Background(), QBOptions{WebURL: srv.URL})
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "укажите логин и пароль") {
		t.Fatalf("without a login: %v", err)
	}
}

// A blank key or password is kept only for the address (and login) it
// was typed for; otherwise it is never sent anywhere.
func TestBlankSecretKeptOnlyForSameAddress(t *testing.T) {
	e := newSvcEnv(t, nil)
	ctx := context.Background()
	one := sonarrAPI(t, nil, nil, false)
	two := sonarrAPI(t, nil, nil, false)
	if _, err := e.s.Connect(ctx, SourceSonarr, Remote{URL: one.URL, APIKey: fakeAPIKey}); err != nil {
		t.Fatal(err)
	}
	before := len(two.paths)
	_, err := e.s.Connect(ctx, SourceSonarr, Remote{URL: two.URL})
	var re *RemoteError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "Укажите API-ключ") {
		t.Fatalf("changed address, blank key: %v", err)
	}
	if len(two.paths) != before {
		t.Fatalf("the kept key was sent to the new address: %v", two.paths)
	}
	if r, _ := e.s.remotes.get(SourceSonarr); r.URL != one.URL || r.APIKey != fakeAPIKey {
		t.Fatalf("a refused connection replaced the kept one: %+v", r)
	}

	qb := fakeQB(t)
	qb.user, qb.pass = "admin", "secret"
	if _, err := e.s.Connect(ctx, SourceQBittorrent, Remote{URL: qb.URL, Username: "admin", Password: "secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Connect(ctx, SourceQBittorrent, Remote{URL: qb.URL, Username: "admin"}); err != nil {
		t.Fatalf("same login, blank password: %v", err)
	}
	logins := qb.logins
	if _, err := e.s.Connect(ctx, SourceQBittorrent, Remote{URL: qb.URL, Username: "other"}); err == nil {
		t.Fatal("another login connected with the kept password")
	}
	if r, _ := e.s.remotes.get(SourceQBittorrent); r.Username != "admin" || r.Password != "secret" || qb.logins != logins {
		t.Fatalf("kept %+v, %d logins", r, qb.logins)
	}
	other := fakeQB(t)
	other.user, other.pass = "admin", "secret"
	if _, err := e.s.Connect(ctx, SourceQBittorrent, Remote{URL: other.URL, Username: "admin"}); err == nil || other.logins != 0 {
		t.Fatalf("the kept password went to another address: %v, %d logins", err, other.logins)
	}
}
