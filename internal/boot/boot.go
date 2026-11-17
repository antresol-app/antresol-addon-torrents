package boot

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
	"github.com/antresol-app/antresol-addon-torrents/internal/importer"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// Deps are Boot's steps (main passes the real ones, tests fakes).
type Deps struct {
	OpenDB      func(path string) (*sql.DB, error) // store.Open
	Import      func(ctx context.Context, o importer.Options) (importer.Counts, error)
	Fingerprint func(ctx context.Context, from, dataDir string) (string, error) // importer.Fingerprint
	NewEngine   func(cfg torrent.EngineConfig, db *sql.DB) (Engine, error)      // torrent.New
	// MapUsers (main: follow.MapUserUIDs with the core's GET /users) keys
	// the id-keyed rows by the profiles' uids, after the import and
	// before anything serves; nil: no mapping (tests).
	MapUsers func(ctx context.Context, db *sql.DB) error
}

// Booted is what Boot started.
type Booted struct {
	DB     *sql.DB
	Engine Engine
	NoSeed *NoSeed
}

// errSourceChanged: the old build wrote its tables after the import
// (a rollback, then a forward deploy).
const errSourceChanged = "Источник изменился после импорта: очистите данные расширения и импортируйте заново"

// Boot opens torrents.db, imports (once) or checks the import's source,
// keys the rows by the profiles' uids (MapUsers), decides no-seeding (forced: saved as on, sticky), writes the state
// file and only then builds the engine with that value. Any
// error before the engine returns without one (and closes the database);
// the ones a retry cannot cure are Final (see Retry).
func Boot(ctx context.Context, c Conf, d Deps) (b Booted, err error) {
	if d.OpenDB == nil || d.Import == nil || d.Fingerprint == nil || d.NewEngine == nil {
		return Booted{}, Final(errors.New("boot: missing deps"))
	}
	if err := importer.RemoveLeftovers(c.DataDir); err != nil {
		return Booted{}, fmt.Errorf("boot: remove the snapshots a crash left: %w", err)
	}
	db, err := d.OpenDB(filepath.Join(c.DataDir, "torrents.db"))
	if err != nil {
		return Booted{}, fmt.Errorf("boot: open torrents.db: %w", err)
	}
	defer func() {
		if err != nil {
			db.Close()
		}
	}()
	if c.ImportFrom != "" {
		st, ok, err := importer.LoadState(ctx, db)
		if err != nil {
			return Booted{}, err
		}
		if !ok {
			if _, err := d.Import(ctx, importer.Options{From: c.ImportFrom, FromDataDir: c.ImportDataDir, DataDir: c.DataDir, DB: db}); err != nil {
				err = fmt.Errorf("boot: import: %w", err)
				if errors.Is(err, importer.ErrNotEmpty) || errors.Is(err, importer.ErrBadSettings) {
					err = Final(err)
				}
				return Booted{}, err
			}
		} else {
			fp, err := d.Fingerprint(ctx, c.ImportFrom, c.DataDir)
			if err != nil {
				return Booted{}, fmt.Errorf("boot: import source: %w", err)
			}
			if fp != st.Fingerprint {
				return Booted{}, Final(i18n.Errorf(errSourceChanged))
			}
		}
	}
	if d.MapUsers != nil {
		if err := d.MapUsers(ctx, db); err != nil {
			return Booted{}, fmt.Errorf("boot: profile uids: %w", err)
		}
	}
	settings := store.Settings{DB: db}
	if c.ForcedNoSeeding {
		if err := settings.SaveNoSeeding(ctx, true); err != nil {
			return Booted{}, fmt.Errorf("boot: save the forced no-seeding: %w", err)
		}
	}
	noSeeding, err := effective(ctx, c.ForcedNoSeeding, settings)
	if err != nil {
		// fail closed: the state says on, and no engine
		err = fmt.Errorf("boot: no-seeding setting: %w", err)
		if werr := WriteState(c.DataDir, true, c.ForcedNoSeeding); werr != nil {
			return Booted{}, Final(errors.Join(err, werr))
		}
		return Booted{}, err
	}
	limited, kbs, err := settings.Upload(ctx)
	if err != nil {
		return Booted{}, fmt.Errorf("boot: upload limit: %w", err)
	}
	var upload int64
	if limited && kbs > 0 {
		upload = kbs * 1024
	}
	if err := WriteState(c.DataDir, noSeeding, c.ForcedNoSeeding); err != nil {
		return Booted{}, Final(err)
	}
	var check func() error
	if len(c.StorageDirs) > 0 {
		check = torrent.DirsPresent(c.StorageDirs...)
	}
	// The downloads folder exists from the start (the core's disk
	// check refuses «Перейти на Antresol» without it), mode as the engine's
	// folders; never while the media disk is not mounted, so nothing lands
	// on the empty mount point. A failure is left to that disk check.
	if c.DownloadDir != "" && (check == nil || check() == nil) {
		_ = os.MkdirAll(c.DownloadDir, 0o755)
	}
	eng, err := d.NewEngine(torrent.EngineConfig{
		NoSeeding:    noSeeding,
		UploadLimit:  upload,
		DataDir:      c.DataDir,
		StreamDir:    c.StreamDir,
		DownloadDir:  c.DownloadDir,
		ListenPort:   c.Port,
		NoUPnP:       !c.UPnP,
		LocalOnly:    c.LocalOnly,
		StorageCheck: check,
	}, db)
	if err != nil {
		return Booted{}, fmt.Errorf("boot: engine: %w", err)
	}
	return Booted{DB: db, Engine: eng, NoSeed: NewNoSeed(c.ForcedNoSeeding, c.DataDir, settings, eng)}, nil
}
