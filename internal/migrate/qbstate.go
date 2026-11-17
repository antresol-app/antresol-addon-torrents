package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Prev values: the torrent's state in qBittorrent before the move.
const (
	PrevRunning = "running"
	PrevStopped = "stopped"
)

// qbState is the on-disk record of one torrent's move
// (<StateDir>/<ih>.json). It is written before the torrent is added to
// Vault, so after a crash the next run knows (a) that the paused or
// seeding copy in Vault is this migration's and (b) whether qBittorrent
// was running it — once Vault's stop has landed, qBittorrent itself only
// says "stopped". Done is set once the imported line is in a saved report.
type qbState struct {
	Prev   string `json:"prev"`
	Report string `json:"report"`
	Done   bool   `json:"done"`
}

func statePath(dir, ih string) string { return filepath.Join(dir, ih+".json") }

// loadState returns the record of ih; ok is false when there is none.
func loadState(dir, ih string) (qbState, bool, error) {
	if dir == "" || !infohashRe.MatchString(ih) {
		return qbState{}, false, nil
	}
	b, err := os.ReadFile(statePath(dir, ih))
	if errors.Is(err, os.ErrNotExist) {
		return qbState{}, false, nil
	}
	if err != nil {
		return qbState{}, false, err
	}
	var s qbState
	if err := json.Unmarshal(b, &s); err != nil {
		return qbState{}, false, err
	}
	// Fail closed: without a known state nothing may be stopped or started.
	if s.Prev != PrevRunning && s.Prev != PrevStopped {
		return qbState{}, false, fmt.Errorf("migrate: state file %s: unknown prev %q", statePath(dir, ih), s.Prev)
	}
	return s, true, nil
}

// saveState writes the record atomically (temporary file, fsync, rename).
func saveState(dir, ih string, s qbState) error {
	if dir == "" {
		return errors.New("migrate: no state dir")
	}
	if !infohashRe.MatchString(ih) {
		return errors.New("migrate: bad infohash")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, statePath(dir, ih))
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

// syncDir makes a rename in dir durable across a power loss.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = d.Sync()
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	return err
}

func removeState(dir, ih string) error {
	if dir == "" || !infohashRe.MatchString(ih) {
		return nil
	}
	err := os.Remove(statePath(dir, ih))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDir(dir)
}
