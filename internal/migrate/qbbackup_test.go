package migrate

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// The BT_backup tests again, with BT_backup not mounted: the .torrent
// files and their state come from qBittorrent's Web API (the fake answers
// from the folder the test fills, as qBittorrent answers from its own
// state). Each must pass unchanged: the move, the renamed files, the
// checks, the stop, the give-back under no-seeding and the rollback are
// the same code.
func TestQBOverAPI(t *testing.T) {
	tests := map[string]func(*testing.T){
		"SeedsWithoutRedownload":            TestQBImportSeedsWithoutRedownload,
		"MappedFiles":                       TestQBImportMappedFiles,
		"CyrillicName":                      TestQBImportCyrillicName,
		"CorruptDataKeepsQB":                TestQBImportCorruptDataKeepsQB,
		"IncompleteWarns":                   TestQBImportIncompleteWarns,
		"StopFailsRollsBack":                TestQBImportStopFailsRollsBack,
		"StopTimeoutRestarts":               TestQBImportStopTimeoutRestarts,
		"Idempotent":                        TestQBImportIdempotent,
		"DryRunTouchesNothing":              TestQBImportDryRunTouchesNothing,
		"LoginWhenUserSet":                  TestQBImportLoginWhenUserSet,
		"PausedInQB":                        TestQBImportPausedInQB,
		"StoppedInQBSendsNoStop":            TestQBImportStoppedInQBSendsNoStop,
		"BusyInQBWarns":                     TestQBImportBusyInQBWarns,
		"StartedMeanwhileIsStopped":         TestQBImportStartedMeanwhileIsStopped,
		"GiveBackKeepsPaused":               TestQBImportGiveBackKeepsPaused,
		"RollbackRestartsInQB":              TestRollbackRestartsInQB,
		"RollbackKeepsPausedPaused":         TestRollbackKeepsPausedPaused,
		"RollbackMissingDataNotStarted":     TestRollbackMissingDataNotStarted,
		"Rollback_NilKeepPausedKeepsPaused": TestRollback_NilKeepPausedKeepsPaused,
	}
	for name, f := range tests {
		t.Run(name, func(t *testing.T) {
			qbOverAPI = true
			defer func() { qbOverAPI = false }()
			f(t)
		})
	}
}

// A dry run over the API plans exactly what the dry run over BT_backup
// plans, torrent by torrent.
func TestQBOverAPIDryRunMatchesBTBackup(t *testing.T) {
	e := newEnv(t)
	e.qb.mirror = e.bt
	e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10), "Show/ep2.mkv": randBytes(60 << 10)}, nil)
	e.addTorrent(map[string][]byte{"Фильм (2020).mkv": randBytes(90 << 10)}, nil)
	paused, _ := e.addTorrent(map[string][]byte{"Paused/a.mkv": randBytes(70 << 10)}, nil)
	e.qb.setState(paused, "stoppedUP")
	busy, _ := e.addTorrent(map[string][]byte{"Busy/a.mkv": randBytes(70 << 10)}, nil)
	e.qb.setState(busy, "checkingUP")
	e.addTorrent(map[string][]byte{"Half/a.mkv": randBytes(200 << 10)}, map[string]any{"pieces": "\x01\x01\x00\x00\x00\x00\x00"})
	e.addTorrent(map[string][]byte{"Renamed/ep1.mkv": randBytes(50 << 10)}, map[string]any{"mapped_files": []string{"Renamed/Серия 1.mkv"}})

	o := e.opts()
	o.DryRun = true
	file := e.run(o)
	o.FromAPI, o.BTBackupDir = true, ""
	api := e.run(o)
	if len(file.Lines) != 6 || !reflect.DeepEqual(api.Lines, file.Lines) {
		t.Fatalf("over the API\n%+v\nwant (BT_backup)\n%+v", api.Lines, file.Lines)
	}
}

// A qBittorrent older than 4.5 has no /torrents/export: «Проверить» says
// so, and a run moves nothing and says why.
func TestQBOverAPINoExport(t *testing.T) {
	e := newEnv(t)
	e.qb.mirror, e.qb.noExport = e.bt, true
	ih, _ := e.addTorrent(map[string][]byte{"Show/ep1.mkv": randBytes(100 << 10)}, nil)
	o := e.opts()
	o.FromAPI, o.BTBackupDir = true, ""
	_, err := qbAPICount(context.Background(), o)
	var re *RemoteError
	if !errors.As(err, &re) || re.Msg != reasonNoExport {
		t.Fatalf("check: %v", err)
	}
	l := onlyLine(t, e.run(o))
	if l.Status != StatusFailed || l.Reason != reasonNoExport || l.Name != ih+".torrent" {
		t.Fatalf("line %+v", l)
	}
	if posts := e.qb.posts(); len(posts) != 0 {
		t.Fatalf("qBittorrent was sent %+v", posts)
	}
	notInVault(t, e, ih)
}

// «Проверить» of a qBittorrent connection: the login, the count.
func TestQBAPICount(t *testing.T) {
	e := newEnv(t)
	e.qb.mirror, e.qb.user, e.qb.pass = e.bt, "admin", "secret"
	e.addTorrent(map[string][]byte{"A/a.mkv": randBytes(40 << 10)}, nil)
	e.addTorrent(map[string][]byte{"B/b.mkv": randBytes(40 << 10)}, nil)
	ctx := context.Background()
	o := e.opts()
	o.Username, o.Password = "admin", "secret"
	if n, err := qbAPICount(ctx, o); err != nil || n != 2 {
		t.Fatalf("count %d %v", n, err)
	}
	for _, c := range []struct {
		url, user, pass, want string
	}{
		{e.qb.URL, "admin", "wrong", "qBittorrent не принял логин или пароль"},
		{e.qb.URL, "", "", "qBittorrent не пустил: укажите логин и пароль его веб-интерфейса"},
		{"", "admin", "secret", "Укажите адрес qBittorrent"},
		{"qb.local:8080", "admin", "secret", "Адрес qBittorrent должен начинаться с http:// или https://"},
		{"http://127.0.0.1:1", "admin", "secret", "qBittorrent не отвечает по адресу http://127.0.0.1:1"},
	} {
		o := e.opts()
		o.WebURL, o.Username, o.Password = c.url, c.user, c.pass
		_, err := qbAPICount(ctx, o)
		var re *RemoteError
		if !errors.As(err, &re) || re.Msg != c.want {
			t.Errorf("%+v: %v, want %q", c, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "wrong") {
			t.Errorf("the password is in %q", err)
		}
	}
	// only reads: no POST but the login
	if posts := e.qb.posts(); len(posts) != 0 {
		t.Fatalf("qBittorrent was sent %+v", posts)
	}
}
