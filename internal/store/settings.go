package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// Settings keys of the addon (the settings table of 0013).
const (
	keyNoSeeding     = "no_seeding"
	keyUploadLimited = "upload_limited"
	keyUploadKBs     = "upload_limit_kbs"
	keyStreamCacheGB = "stream_cache_gb"
	keyFlareSolverr  = "flaresolverr_url"
)

// Settings reads and writes the addon's settings rows. Values are stored
// as "1"/"0" and decimal strings; a stored value that does not parse is an
// error, never a default.
type Settings struct{ DB *sql.DB }

// get is the value of key; ok false when there is no row.
func (s Settings) get(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

func (s Settings) set(ctx context.Context, kv ...string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := 0; i+1 < len(kv); i += 2 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, kv[i], kv[i+1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s Settings) flag(ctx context.Context, key string) (bool, error) {
	v, ok, err := s.get(ctx, key)
	if err != nil || !ok {
		return false, err
	}
	switch v {
	case "1":
		return true, nil
	case "0":
		return false, nil
	}
	return false, fmt.Errorf("store: settings %s = %q, not 1 or 0", key, v)
}

func flagValue(on bool) string {
	if on {
		return "1"
	}
	return "0"
}

// NoSeeding is the saved «Не раздавать» setting (false without a row).
// Callers treat an error as on (parked): an unreadable setting never seeds.
func (s Settings) NoSeeding(ctx context.Context) (bool, error) { return s.flag(ctx, keyNoSeeding) }

// SaveNoSeeding saves it (named apart from the engine's SetNoSeeding, which
// only boot.NoSeed calls). Only boot.NoSeed decides what the engine does.
func (s Settings) SaveNoSeeding(ctx context.Context, on bool) error {
	return s.set(ctx, keyNoSeeding, flagValue(on))
}

// Upload is the upload limit: limited and KB/s (false, 0 without rows).
func (s Settings) Upload(ctx context.Context) (limited bool, kbs int64, err error) {
	if limited, err = s.flag(ctx, keyUploadLimited); err != nil {
		return false, 0, err
	}
	v, ok, err := s.get(ctx, keyUploadKBs)
	if err != nil || !ok {
		return limited, 0, err
	}
	kbs, err = strconv.ParseInt(v, 10, 64)
	if err != nil || kbs < 0 {
		return false, 0, fmt.Errorf("store: settings %s = %q, not a whole number", keyUploadKBs, v)
	}
	return limited, kbs, nil
}

// SetUpload saves both upload values in one transaction.
func (s Settings) SetUpload(ctx context.Context, limited bool, kbs int64) error {
	if kbs < 0 {
		return fmt.Errorf("store: upload limit %d below 0", kbs)
	}
	return s.set(ctx, keyUploadLimited, flagValue(limited), keyUploadKBs, strconv.FormatInt(kbs, 10))
}

// StreamCacheGB is the watch cache size in GB; def without a row.
func (s Settings) StreamCacheGB(ctx context.Context, def float64) (float64, error) {
	v, ok, err := s.get(ctx, keyStreamCacheGB)
	if err != nil || !ok {
		return def, err
	}
	gb, err := strconv.ParseFloat(v, 64)
	if err != nil || gb < 0 || math.IsNaN(gb) || math.IsInf(gb, 0) {
		return 0, fmt.Errorf("store: settings %s = %q, not a size", keyStreamCacheGB, v)
	}
	return gb, nil
}

// SetStreamCacheGB saves it.
func (s Settings) SetStreamCacheGB(ctx context.Context, gb float64) error {
	if gb < 0 || math.IsNaN(gb) || math.IsInf(gb, 0) {
		return fmt.Errorf("store: stream cache %v GB is not a size", gb)
	}
	return s.set(ctx, keyStreamCacheGB, strconv.FormatFloat(gb, 'f', -1, 64))
}

// FlareSolverrURL is the FlareSolverr address saved in «Настройки загрузок»
// ("" without a row: the server's ANTRESOL_TORRENTS_FLARESOLVERR_URL, if
// any, applies).
func (s Settings) FlareSolverrURL(ctx context.Context) (string, error) {
	v, _, err := s.get(ctx, keyFlareSolverr)
	return v, err
}

// SetFlareSolverrURL saves it ("" clears it).
func (s Settings) SetFlareSolverrURL(ctx context.Context, url string) error {
	return s.set(ctx, keyFlareSolverr, url)
}
