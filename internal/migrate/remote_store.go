package migrate

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
)

// The connections typed on «Переезд» (Connect) are kept across restarts
// and addon updates in the addon's settings table, as the sources' own
// secrets are kept in its database: the whole set as one row, sealed with
// AES-256-GCM under a random key of its own (another settings row, made on
// first use). Only Connect and Forget write it; nothing answers, logs or
// reports a key or a password.
const (
	remotesSetting    = "migrate_remotes"
	remotesKeySetting = "migrate_remotes_key"
	remotesAAD        = "antresol-torrents/migrate_remotes/v1"
)

// storedRemote is one connection as it is sealed.
type storedRemote struct {
	URL      string `json:"url"`
	APIKey   string `json:"apiKey,omitempty"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

func (r *remotes) snapshot() map[string]Remote {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]Remote, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}

func (r *remotes) load(m map[string]Remote) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m = m
	r.gen++
}

// remotesKey is the sealing key, made on first use.
func remotesKey(ctx context.Context, db *sql.DB) ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, remotesKeySetting, hex.EncodeToString(b)); err != nil {
		return nil, err
	}
	var v string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, remotesKeySetting).Scan(&v); err != nil {
		return nil, err
	}
	k, err := hex.DecodeString(v)
	if err != nil || len(k) != 32 {
		return nil, errors.New("migrate remotes: malformed key")
	}
	return k, nil
}

func remotesAEAD(ctx context.Context, db *sql.DB) (cipher.AEAD, error) {
	k, err := remotesKey(ctx, db)
	if err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// saveRemotes seals the kept connections into the settings (none: the row
// goes).
func (s *Service) saveRemotes(ctx context.Context) error {
	if s.DB == nil {
		return nil
	}
	all := s.remotes.snapshot()
	if len(all) == 0 {
		_, err := s.DB.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, remotesSetting)
		return err
	}
	plain := make(map[string]storedRemote, len(all))
	for id, r := range all {
		plain[id] = storedRemote{URL: r.URL, APIKey: r.APIKey, Username: r.Username, Password: r.Password}
	}
	js, err := json.Marshal(plain)
	if err != nil {
		return err
	}
	aead, err := remotesAEAD(ctx, s.DB)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := aead.Seal(nonce, nonce, js, []byte(remotesAAD))
	_, err = s.DB.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		remotesSetting, base64.StdEncoding.EncodeToString(sealed))
	return err
}

// loadRemotes reads the connections kept before (a restart, an addon
// update). A row that does not open (another key, damaged) is dropped: the
// connections are typed again.
func (s *Service) loadRemotes(ctx context.Context) error {
	if s.DB == nil {
		return nil
	}
	var v string
	err := s.DB.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, remotesSetting).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	m, err := s.openRemotes(ctx, v)
	if err != nil {
		_, _ = s.DB.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, remotesSetting)
		return err
	}
	s.remotes.load(m)
	return nil
}

func (s *Service) openRemotes(ctx context.Context, v string) (map[string]Remote, error) {
	sealed, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return nil, errors.New("migrate remotes: not base64")
	}
	aead, err := remotesAEAD(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	if len(sealed) < aead.NonceSize() {
		return nil, errors.New("migrate remotes: too short")
	}
	js, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(remotesAAD))
	if err != nil {
		return nil, errors.New("migrate remotes: does not open")
	}
	var plain map[string]storedRemote
	if err := json.Unmarshal(js, &plain); err != nil {
		return nil, errors.New("migrate remotes: malformed")
	}
	out := make(map[string]Remote, len(plain))
	for id, r := range plain {
		if RemoteKind(id) == "" || r.URL == "" {
			continue // a source this build no longer reaches over the network
		}
		out[id] = Remote(r)
	}
	return out, nil
}

// persistRemotes saves the connections after Connect or Forget; a failure
// is logged (the connection stays for this run) and never names a secret.
func (s *Service) persistRemotes() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.saveRemotes(ctx); err != nil && s.Runner != nil && s.Runner.Log != nil {
		s.Runner.Log.Warn("migrate: the connections were not saved", "err", err)
	}
}
