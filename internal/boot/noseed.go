package boot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// StateFile is the no-seeding state in the data dir: "on forced\n", "on\n"
// or "off\n", written before the engine exists and on every change. The
// deploy check reads it (it must be newer than the container's start).
const StateFile = "no-seeding.state"

// Engine is what boot needs of torrent.Engine.
type Engine interface {
	SetNoSeeding(bool)
	SetUploadLimit(int64)
	Close() error
}

// ErrForced: no-seeding is forced by ANTRESOL_TORRENTS_NO_SEEDING=on and
// cannot be changed here (409 «Задано на сервере»).
var ErrForced = errors.New("no-seeding forced by the server")

// NoSeed is the only owner of the effective value and the only caller of
// Engine.SetNoSeeding: every settings save goes through Apply.
type NoSeed struct {
	forced   bool
	dataDir  string
	settings store.Settings
	eng      Engine
	mu       sync.Mutex // one Apply at a time: the state file and the engine agree
}

// NewNoSeed is the owner of the effective value over settings and eng
// (Boot makes the one the program uses; tests of the admin routes make
// their own over a fake engine).
func NewNoSeed(forced bool, dataDir string, settings store.Settings, eng Engine) *NoSeed {
	return &NoSeed{forced: forced, dataDir: dataDir, settings: settings, eng: eng}
}

// Forced reports whether the server forces no-seeding.
func (n *NoSeed) Forced() bool { return n.forced }

// Effective is forced || the saved setting. An unreadable setting is on,
// returned with its error: it never seeds.
func (n *NoSeed) Effective(ctx context.Context) (bool, error) {
	return effective(ctx, n.forced, n.settings)
}

func effective(ctx context.Context, forced bool, s store.Settings) (bool, error) {
	if forced {
		return true, nil
	}
	on, err := s.NoSeeding(ctx)
	if err != nil {
		return true, err
	}
	return on, nil
}

// Apply writes the effective value to the state file, then gives it to the
// engine. On a settings error it still writes on and parks the engine,
// then returns the error; a state file that cannot be written keeps the
// engine as it is unless the value is on (parking is never held back).
func (n *NoSeed) Apply(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	on, err := n.Effective(ctx)
	if werr := WriteState(n.dataDir, on, n.forced); werr != nil {
		if on {
			n.eng.SetNoSeeding(true)
		}
		return errors.Join(err, werr)
	}
	n.eng.SetNoSeeding(on)
	return err
}

// Set saves the setting and applies it; forced: ErrForced, nothing changes.
func (n *NoSeed) Set(ctx context.Context, on bool) error {
	if n.forced {
		return ErrForced
	}
	if err := n.settings.SaveNoSeeding(ctx, on); err != nil {
		return err
	}
	return n.Apply(ctx)
}

// The clock of WriteState (tests replace it). processStart is the
// program's start (SetProcessStart, from main).
var (
	stateMu      sync.Mutex
	processStart time.Time
	clockNow     = time.Now
	clockSleep   = time.Sleep
)

// SetProcessStart records the program's start; main calls it first thing.
func SetProcessStart(t time.Time) {
	stateMu.Lock()
	processStart = t
	stateMu.Unlock()
}

// setClock replaces the start and the clock (tests); it returns the undo.
func setClock(start time.Time, now func() time.Time, sleep func(time.Duration)) func() {
	stateMu.Lock()
	ps, cn, cs := processStart, clockNow, clockSleep
	processStart, clockNow, clockSleep = start, now, sleep
	stateMu.Unlock()
	return func() {
		stateMu.Lock()
		processStart, clockNow, clockSleep = ps, cn, cs
		stateMu.Unlock()
	}
}

// WriteState writes dataDir/no-seeding.state: a temp file, fsync, rename
// into place, then the folder fsynced; a reader only ever sees a whole
// value. The deploy check (deploy/torrents-addon-check.sh post) judges
// freshness to the nanosecond, not by this process's start second: on a
// deploy the old file is moved aside while the container is stopped, so a
// file there is this process's (docker's StartedAt may come after it). The
// start-second wait in writeAfterStart is kept, but nothing relies on it.
func WriteState(dataDir string, on, forced bool) error {
	val := "off\n"
	switch {
	case forced:
		val = "on forced\n"
	case on:
		val = "on\n"
	}
	if err := writeAfterStart(dataDir, StateFile, val); err != nil {
		return fmt.Errorf("boot: no-seeding state: %w", err)
	}
	return nil
}

// writeAfterStart writes dataDir/name atomically (temp file, fsync, rename,
// the folder fsynced), never within the process's first whole second. That
// wait predates the deploy check's current rule and is no guarantee to it:
// docker's StartedAt is set after the container's processes already run,
// so this process's start second can lie before StartedAt. The check
// compares to the nanosecond against the marker of the move-aside (see
// WriteState), with StartedAt only as a 5 s sanity line. One write at a
// time (stateMu): no-seeding.state and ready.state share the clock.
func writeAfterStart(dataDir, name, val string) error {
	stateMu.Lock()
	defer stateMu.Unlock()
	if !processStart.IsZero() {
		if now := clockNow(); now.Unix() == processStart.Unix() {
			clockSleep(time.Unix(processStart.Unix()+1, 0).Sub(now))
		}
	}
	tmp, err := os.CreateTemp(dataDir, name+".tmp-")
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.WriteString(val); err != nil {
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dataDir, name)); err != nil {
		os.Remove(tmp.Name())
		done = true
		return err
	}
	done = true
	if d, err := os.Open(dataDir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
