package boot

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// fakeEngine records every SetNoSeeding with the state file as it read at
// that moment.
type fakeEngine struct {
	mu      sync.Mutex
	dataDir string
	calls   []bool
	states  []string // the state file at each call
	closed  bool
}

func (f *fakeEngine) SetNoSeeding(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, on)
	f.states = append(f.states, readState(f.dataDir))
}
func (f *fakeEngine) SetUploadLimit(int64) {}
func (f *fakeEngine) Close() error         { f.closed = true; return nil }

func readState(dataDir string) string {
	b, err := os.ReadFile(filepath.Join(dataDir, StateFile))
	if err != nil {
		return "<none>"
	}
	return string(b)
}

func openStore(t *testing.T, dir string) *sql.DB {
	t.Helper()
	d, err := store.Open(filepath.Join(dir, "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestNoSeed_SetForced(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := openStore(t, dir)
	eng := &fakeEngine{dataDir: dir}
	n := &NoSeed{forced: true, dataDir: dir, settings: store.Settings{DB: d}, eng: eng}
	if err := n.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	if err := n.Set(ctx, false); !errors.Is(err, ErrForced) {
		t.Fatalf("Set(false) forced = %v, want ErrForced", err)
	}
	if err := n.Set(ctx, true); !errors.Is(err, ErrForced) {
		t.Fatalf("Set(true) forced = %v, want ErrForced", err)
	}
	for _, c := range eng.calls {
		if !c {
			t.Fatalf("the engine saw false while forced: %v", eng.calls)
		}
	}
	if s := readState(dir); s != "on forced\n" {
		t.Fatalf("state = %q, want on forced", s)
	}
	if on, err := n.Effective(ctx); err != nil || !on {
		t.Fatalf("Effective = %v, %v", on, err)
	}
}

func TestNoSeed_ApplyWritesStateFirst(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := openStore(t, dir)
	eng := &fakeEngine{dataDir: dir}
	n := &NoSeed{dataDir: dir, settings: store.Settings{DB: d}, eng: eng}
	if err := n.Set(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(eng.calls) != 1 || !eng.calls[0] || eng.states[0] != "on\n" {
		t.Fatalf("calls %v with states %q; want [true] after state on", eng.calls, eng.states)
	}
	if on, _ := (store.Settings{DB: d}).NoSeeding(ctx); !on {
		t.Fatal("Set(true) did not save the setting")
	}
	if err := n.Set(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(eng.calls) != 2 || eng.calls[1] || eng.states[1] != "off\n" {
		t.Fatalf("calls %v with states %q; want false after state off", eng.calls, eng.states)
	}
}

// An unreadable setting is on (parked): Effective answers true with the
// error, Apply still writes on and parks the engine, then returns it.
func TestNoSeed_SettingsErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := openStore(t, dir)
	if _, err := d.Exec(`INSERT INTO settings (key, value) VALUES ('no_seeding', 'x')`); err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{dataDir: dir}
	n := &NoSeed{dataDir: dir, settings: store.Settings{DB: d}, eng: eng}
	if on, err := n.Effective(ctx); err == nil || !on {
		t.Fatalf("Effective = %v, %v; want true and the error", on, err)
	}
	if err := n.Apply(ctx); err == nil {
		t.Fatal("Apply hid the settings error")
	}
	if len(eng.calls) != 1 || !eng.calls[0] || eng.states[0] != "on\n" {
		t.Fatalf("calls %v with states %q; want [true] after state on", eng.calls, eng.states)
	}
	// a closed database is an error too
	d.Close()
	if on, err := n.Effective(ctx); err == nil || !on {
		t.Fatalf("Effective on a closed DB = %v, %v", on, err)
	}
}

// A state file that cannot be written never turns parking off; turning it
// on still reaches the engine.
func TestNoSeed_ApplyStateFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d := openStore(t, t.TempDir())
	if err := (store.Settings{DB: d}).SaveNoSeeding(ctx, false); err != nil {
		t.Fatal(err)
	}
	eng := &fakeEngine{dataDir: dir}
	n := &NoSeed{dataDir: filepath.Join(dir, "missing", "deeper"), settings: store.Settings{DB: d}, eng: eng}
	if err := n.Apply(ctx); err == nil {
		t.Fatal("Apply hid the state write error")
	}
	if len(eng.calls) != 0 {
		t.Fatalf("the engine was told %v without a state file", eng.calls)
	}
	if err := (store.Settings{DB: d}).SaveNoSeeding(ctx, true); err != nil {
		t.Fatal(err)
	}
	if err := n.Apply(ctx); err == nil {
		t.Fatal("Apply hid the state write error")
	}
	if len(eng.calls) != 1 || !eng.calls[0] {
		t.Fatalf("calls %v; want [true]", eng.calls)
	}
}

func TestWriteState_Values(t *testing.T) {
	for _, c := range []struct {
		on, forced bool
		want       string
	}{{true, true, "on forced\n"}, {true, false, "on\n"}, {false, false, "off\n"}, {false, true, "on forced\n"}} {
		dir := t.TempDir()
		if err := WriteState(dir, c.on, c.forced); err != nil {
			t.Fatal(err)
		}
		if got := readState(dir); got != c.want {
			t.Fatalf("WriteState(%v, %v) = %q, want %q", c.on, c.forced, got, c.want)
		}
	}
}

// TestWriteState_Atomic: a reader racing the writes only ever sees a whole
// value, and no temp file stays behind.
func TestWriteState_Atomic(t *testing.T) {
	dir := t.TempDir()
	if err := WriteState(dir, false, false); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	bad := make(chan string, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			switch s := readState(dir); s {
			case "on\n", "off\n", "on forced\n":
			default:
				select {
				case bad <- s:
				default:
				}
				return
			}
		}
	}()
	for i := 0; i < 300; i++ {
		if err := WriteState(dir, i%3 != 0, i%3 == 2); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	select {
	case s := <-bad:
		t.Fatalf("a reader saw %q", s)
	default:
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != StateFile {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
}

// TestWriteState_NotInTheStartSecond: the deploy check wants the state
// newer than the start in whole seconds, so a write in the process's first
// second waits for the next one.
func TestWriteState_NotInTheStartSecond(t *testing.T) {
	start := time.Unix(1_000_000, 200_000_000)
	now := start.Add(300 * time.Millisecond)
	var slept []time.Duration
	restore := setClock(start, func() time.Time { return now }, func(d time.Duration) { slept = append(slept, d); now = now.Add(d) })
	defer restore()
	dir := t.TempDir()
	if err := WriteState(dir, true, false); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 1 || slept[0] != 500*time.Millisecond {
		t.Fatalf("slept %v; want [500ms] up to the next second", slept)
	}
	if now.Unix() == start.Unix() {
		t.Fatal("wrote in the start second")
	}
	// a later second: no wait
	slept = nil
	if err := WriteState(dir, true, false); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 0 {
		t.Fatalf("slept %v in a later second", slept)
	}
	if !strings.HasPrefix(readState(dir), "on") {
		t.Fatal(readState(dir))
	}
}
