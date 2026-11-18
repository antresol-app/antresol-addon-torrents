package migrate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	mediaimport "github.com/antresol-app/antresol-addon-torrents/internal/corecopy/mediaimport/runkit"
)

func discovered(t *testing.T, e *svcEnv, id string) SourceStatus {
	t.Helper()
	for _, st := range e.s.Discover(context.Background()) {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("no source %s", id)
	return SourceStatus{}
}

// A card whose database is not mounted: «Проверить» with an address and
// a key keeps the connection (in memory), the card is found over the
// API, and the run reads it as it reads the database.
func TestConnectSonarrOverAPI(t *testing.T) {
	e := newSvcEnv(t, nil)
	ctx := context.Background()
	api := sonarrAPI(t, []sonarrRow{
		{ID: 1, Title: "Сериал 1", Year: 2001, TmdbID: 1001, Monitored: true},
		{ID: 2, Title: "Сериал 2", Year: 2002, TmdbID: 1002, Monitored: false},
	}, nil, false)

	if v, ok := e.s.Remote(SourceSonarr); !ok || v.Kind != "apikey" || v.Connected {
		t.Fatalf("before: %+v %v", v, ok)
	}
	if st := discovered(t, e, SourceSonarr); st.Found {
		t.Fatalf("before: %+v", st)
	}

	// a wrong key: a clear reason, nothing kept
	_, err := e.s.Connect(ctx, SourceSonarr, Remote{URL: api.URL, APIKey: "bad"})
	var re *RemoteError
	if !errors.As(err, &re) || !strings.Contains(re.Msg, "Sonarr не принял API-ключ") {
		t.Fatalf("wrong key: %v", err)
	}
	if v, _ := e.s.Remote(SourceSonarr); v.Connected {
		t.Fatalf("a refused connection was kept: %+v", v)
	}

	detail, err := e.s.Connect(ctx, SourceSonarr, Remote{URL: " " + api.URL + "/ ", APIKey: fakeAPIKey})
	if err != nil || detail != "2 сериала, 1 отслеживается" {
		t.Fatalf("connect: %q %v", detail, err)
	}
	v, ok := e.s.Remote(SourceSonarr)
	if !ok || !v.Connected || v.URL != api.URL || !v.HasSecret {
		t.Fatalf("view %+v", v)
	}
	if st := discovered(t, e, SourceSonarr); !st.Found || st.Detail != "2 сериала, 1 отслеживается" || st.Error != "" {
		t.Fatalf("discover %+v", st)
	}

	// again with the key left blank: the kept one is used
	if _, err := e.s.Connect(ctx, SourceSonarr, Remote{URL: api.URL}); err != nil {
		t.Fatalf("blank key: %v", err)
	}

	plan := e.start(mediaimport.RunRequest{Sources: []string{SourceSonarr}, DryRun: true})
	if l := lineByName(t, plan, "Сериал 1 (2001)"); l.Status != StatusPlanned {
		t.Fatalf("plan %+v", plan.Lines)
	}
	rep := e.start(mediaimport.RunRequest{Sources: []string{SourceSonarr}, PlanID: plan.ID})
	if l := lineByName(t, rep, "Сериал 1 (2001)"); l.Status != StatusImported || l.Ref != "series:1001" {
		t.Fatalf("run %+v", rep.Lines)
	}
	if fs := e.follows(); len(fs) != 1 || fs[0].tmdb != 1001 || fs[0].active {
		t.Fatalf("follows %+v", fs)
	}

	// «Забыть» after the move
	if err := e.s.Forget(SourceSonarr); err != nil {
		t.Fatal(err)
	}
	if v, _ := e.s.Remote(SourceSonarr); v.Connected || v.URL != "" {
		t.Fatalf("forgotten: %+v", v)
	}
	if st := discovered(t, e, SourceSonarr); st.Found {
		t.Fatalf("forgotten: %+v", st)
	}
}

// Mounted files win: no connection form, Connect refused.
func TestConnectMountedRefused(t *testing.T) {
	e := newSvcEnv(t, nil)
	e.sonarr(2)
	if _, ok := e.s.Remote(SourceSonarr); ok {
		t.Fatal("a form for a mounted database")
	}
	api := sonarrAPI(t, nil, nil, false)
	if _, err := e.s.Connect(context.Background(), SourceSonarr, Remote{URL: api.URL, APIKey: fakeAPIKey}); !errors.Is(err, ErrMounted) {
		t.Fatalf("connect: %v", err)
	}
	if _, err := e.s.Connect(context.Background(), "jellyfin", Remote{}); !errors.Is(err, ErrUnknownRemote) {
		t.Fatalf("unknown: %v", err)
	}
}

// qBittorrent over its Web API: Connect counts its torrents, the run
// takes the connection's address and login.
func TestConnectQBittorrent(t *testing.T) {
	e := newSvcEnv(t, nil)
	qb := fakeQB(t)
	qb.user, qb.pass, qb.mirror = "admin", "secret", t.TempDir()
	meta, ih := makeTorrent(t, t.TempDir(), map[string][]byte{"A/a.mkv": randBytes(40 << 10)})
	writeFile(t, filepath.Join(qb.mirror, ih+".torrent"), string(meta))
	writeFastresume(t, qb.mirror, ih, map[string]any{"save_path": "/data"})
	qb.setState(ih, "uploading")
	if v, ok := e.s.Remote(SourceQBittorrent); !ok || v.Kind != "login" {
		t.Fatalf("view %+v %v", v, ok)
	}
	detail, err := e.s.Connect(context.Background(), SourceQBittorrent, Remote{URL: qb.URL, Username: "admin", Password: "secret"})
	if err != nil || detail != "1 раздача" {
		t.Fatalf("connect %q %v", detail, err)
	}
	o := e.s.qbOpts()
	if !o.FromAPI || o.WebURL != qb.URL || o.Username != "admin" || o.Password != "secret" || o.KeepPaused == nil {
		t.Fatalf("run options %+v", o)
	}
	st := discovered(t, e, SourceQBittorrent)
	if !st.Found || st.Detail != "1 раздача" || st.Error != "" {
		t.Fatalf("discover %+v", st)
	}
	v, _ := e.s.Remote(SourceQBittorrent)
	if v.Username != "admin" || !v.HasSecret {
		t.Fatalf("view %+v", v)
	}
}
