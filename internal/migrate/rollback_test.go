package migrate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

func TestRollbackRestartsInQB(t *testing.T) {
	e := newEnv(t)
	ih1, _ := e.addTorrent(map[string][]byte{"A/f.mkv": randBytes(100 << 10)}, nil)
	ih2, _ := e.addTorrent(map[string][]byte{"B/f.mkv": randBytes(100 << 10)}, map[string]any{"mapped_files": []string{"B/g.mkv"}})
	os.Rename(filepath.Join(e.save, "B", "f.mkv"), filepath.Join(e.save, "B", "g.mkv"))
	from := e.run(e.opts())
	if c := from.Tally(); c[StatusImported] != 2 {
		t.Fatalf("import %+v", from.Lines)
	}
	failedIH := "0123456789abcdef0123456789abcdef01234567"
	from.Add(Line{Source: SourceQBittorrent, Kind: "torrent", Name: "C", Status: StatusFailed, Ref: failedIH, Reason: "Нет папки /x"})
	dataFiles := []string{filepath.Join(e.save, "A", "f.mkv"), filepath.Join(e.save, "B", "g.mkv")}
	var before []string
	for _, p := range dataFiles {
		before = append(before, fileHash(t, p))
	}
	n := len(e.qb.posts())

	rep := &Report{ID: NewReportID(time.Now())}
	if err := RollbackQB(context.Background(), e.opts(), e.eng, from, rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Lines) != 2 {
		t.Fatalf("lines %+v", rep.Lines)
	}
	for _, l := range rep.Lines {
		if l.Status != StatusImported || l.Reason != "Возвращена в qBittorrent" {
			t.Fatalf("line %+v", l)
		}
	}
	for _, ih := range []string{ih1, ih2} {
		notInVault(t, e, ih)
	}
	starts := e.qb.posts()[n:]
	if len(starts) != 2 {
		t.Fatalf("posts %+v", starts)
	}
	got := map[string]bool{}
	for _, c := range starts {
		if c.Path != "/api/v2/torrents/start" {
			t.Fatalf("post %+v", c)
		}
		if _, ok := inEngine(c.Engine, c.Hashes); ok {
			t.Fatalf("%s started in qBittorrent while still in Vault", c.Hashes)
		}
		got[c.Hashes] = true
	}
	if !got[ih1] || !got[ih2] || got[failedIH] {
		t.Fatalf("started %v", got)
	}
	for i, p := range dataFiles {
		if fileHash(t, p) != before[i] {
			t.Fatalf("%s changed", p)
		}
	}
	if _, err := os.Stat(filepath.Join(e.link, ih2)); !os.IsNotExist(err) {
		t.Fatal("link tree left behind")
	}
}

// The real server: 4 of 6 torrents are paused in qBittorrent. They are
// moved without a stop, and a rollback leaves them paused.
func TestRollbackKeepsPausedPaused(t *testing.T) {
	e := newEnv(t)
	var running, paused []string
	for i, n := range []string{"A", "B", "C", "D", "E", "F"} {
		fr := map[string]any{}
		if i >= 2 {
			fr["paused"] = 1
		}
		ih, _ := e.addTorrent(map[string][]byte{n + "/f.mkv": randBytes(64 << 10)}, fr)
		if i >= 2 {
			e.qb.setState(ih, "stoppedUP")
			paused = append(paused, ih)
		} else {
			running = append(running, ih)
		}
	}
	from := e.run(e.opts())
	if c := from.Tally(); c[StatusImported] != 6 {
		t.Fatalf("import %+v", from.Lines)
	}
	stops := map[string]bool{}
	for _, c := range e.qb.posts() {
		if c.Path != "/api/v2/torrents/stop" {
			t.Fatalf("post %+v", c)
		}
		stops[c.Hashes] = true
	}
	if len(stops) != 2 || !stops[running[0]] || !stops[running[1]] {
		t.Fatalf("stopped %v", stops)
	}
	n := len(e.qb.posts())
	// The rolled-back report comes from disk, as in the app.
	dir := t.TempDir()
	if err := SaveReport(dir, from); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadReport(dir, from.ID)
	if err != nil {
		t.Fatal(err)
	}
	rep := &Report{ID: NewReportID(time.Now())}
	if err := RollbackQB(context.Background(), e.opts(), e.eng, loaded, rep); err != nil {
		t.Fatal(err)
	}
	if len(e.eng.List()) != 0 {
		t.Fatalf("engine %+v", e.eng.List())
	}
	starts := map[string]bool{}
	for _, c := range e.qb.posts()[n:] {
		if c.Path != "/api/v2/torrents/start" {
			t.Fatalf("post %+v", c)
		}
		starts[c.Hashes] = true
	}
	if len(starts) != 2 || !starts[running[0]] || !starts[running[1]] {
		t.Fatalf("started %v", starts)
	}
	for _, ih := range paused {
		if s, _ := e.qb.stateOf(ih); s != "stoppedUP" {
			t.Fatalf("%s is %q in qBittorrent", ih, s)
		}
	}
	kept := 0
	for _, l := range rep.Lines {
		if l.Status != StatusImported {
			t.Fatalf("line %+v", l)
		}
		if l.Reason == "Возвращена в qBittorrent (на паузе, как была)" {
			kept++
		}
	}
	if kept != 4 {
		t.Fatalf("lines %+v", rep.Lines)
	}
	if entries, _ := os.ReadDir(e.state); len(entries) != 0 {
		t.Fatalf("state files left: %v", entries)
	}
}

func TestRollbackNoAPIRemovesNothing(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"A/f.mkv": randBytes(64 << 10)}, nil)
	from := e.run(e.opts())
	o := e.opts()
	o.WebURL = ""
	rep := &Report{ID: NewReportID(time.Now())}
	if err := RollbackQB(context.Background(), o, e.eng, from, rep); err != nil {
		t.Fatal(err)
	}
	if l := onlyLine(t, rep); l.Status != StatusFailed || !strings.HasPrefix(l.Reason, "Нет связи с qBittorrent") {
		t.Fatalf("line %+v", l)
	}
	if st := status(t, e, ih); st.Status != torrent.StatusSeeding {
		t.Fatalf("status %+v", st)
	}
}

// a torrent removed from Vault together with its data is not started
// in qBittorrent (it would download everything again).
func TestRollbackMissingDataNotStarted(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"A/f.mkv": randBytes(64 << 10)}, nil)
	from := e.run(e.opts())
	if err := e.eng.Remove(context.Background(), ih, true); err != nil {
		t.Fatal(err)
	}
	n := len(e.qb.posts())
	rep := &Report{ID: NewReportID(time.Now())}
	if err := RollbackQB(context.Background(), e.opts(), e.eng, from, rep); err != nil {
		t.Fatal(err)
	}
	if l := onlyLine(t, rep); l.Status != StatusFailed || l.Reason != "Файлов раздачи нет на диске — в qBittorrent не запускаю" {
		t.Fatalf("line %+v", l)
	}
	if len(e.qb.posts()) != n {
		t.Fatal("started")
	}
}

// a line without Prev and no valid state file is removed
// from Vault but not started in qBittorrent.
func TestRollbackUnknownPrevNotStarted(t *testing.T) {
	for _, body := range []string{"", `{}`, `broken`} {
		t.Run("file "+body, func(t *testing.T) {
			e := newEnv(t)
			ih, _ := e.addTorrent(map[string][]byte{"A/f.mkv": randBytes(64 << 10)}, nil)
			from := e.run(e.opts())
			from.Lines[0].Prev = ""
			removeState(e.state, ih)
			if body != "" {
				os.WriteFile(statePath(e.state, ih), []byte(body), 0o644)
			}
			n := len(e.qb.posts())
			rep := &Report{ID: NewReportID(time.Now())}
			if err := RollbackQB(context.Background(), e.opts(), e.eng, from, rep); err != nil {
				t.Fatal(err)
			}
			l := onlyLine(t, rep)
			if l.Status != StatusFailed || !strings.HasPrefix(l.Reason, "Неизвестно, раздавалась ли она в qBittorrent") {
				t.Fatalf("line %+v", l)
			}
			if len(e.qb.posts()) != n {
				t.Fatalf("posts %+v", e.qb.posts()[n:])
			}
			notInVault(t, e, ih)
		})
	}
}

// A rollback whose caller set no KeepPaused fails closed: the torrent goes
// back paused, never started in qBittorrent, and the reason does not claim
// no-seeding is on (reasonNoRule, not reasonKeptPaused).
func TestRollback_NilKeepPausedKeepsPaused(t *testing.T) {
	e := newEnv(t)
	ih, _ := e.addTorrent(map[string][]byte{"A/f.mkv": randBytes(100 << 10)}, nil)
	from := e.run(e.opts())
	if l := onlyLine(t, from); l.Status != StatusImported || l.Prev != PrevRunning {
		t.Fatalf("import %+v", l)
	}
	n := len(e.qb.posts())
	o := e.opts()
	o.KeepPaused = nil
	rep := &Report{ID: NewReportID(time.Now())}
	if err := RollbackQB(context.Background(), o, e.eng, from, rep); err != nil {
		t.Fatal(err)
	}
	if l := onlyLine(t, rep); l.Status != StatusImported || l.Reason != reasonNoRule || l.Reason == reasonKeptPaused {
		t.Fatalf("line %+v", l)
	}
	notInVault(t, e, ih)
	if posts := e.qb.posts()[n:]; len(posts) != 0 {
		t.Fatalf("qBittorrent was sent %+v", posts)
	}
}
