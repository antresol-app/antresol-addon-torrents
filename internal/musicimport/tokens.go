//go:build ignore

package musicimport

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/auth"
)

// Token is a service's sign-in, kept only while its library is read.
type Token struct {
	Access  string    `json:"access"`
	Refresh string    `json:"refresh"`
	Expiry  time.Time `json:"expiry"`
}

// String keeps a token out of logs (%v, %+v).
func (t Token) String() string { return "musicimport.Token{…}" }

// GoString keeps a token out of logs (%#v).
func (t Token) GoString() string { return t.String() }

// Vault keeps tokens on disk encrypted (AES-256-GCM, the token id as
// additional data) in files only the server's user can read; a token is
// deleted once its library is read, and Sweep removes what a stopped
// server left behind.
type Vault struct {
	Dir string
	Key []byte // 32 bytes (VaultKey)
}

// tokenKeySetting is the settings row of the vault's key.
const tokenKeySetting = "import_token_key"

// VaultKey is the vault's key, made on first use and kept in settings.
func VaultKey(ctx context.Context, db *sql.DB) ([]byte, error) {
	v, err := auth.LoadOrCreateSetting(ctx, db, tokenKeySetting, 32)
	if err != nil {
		return nil, err
	}
	k, err := hex.DecodeString(v)
	if err != nil || len(k) != 32 {
		return nil, errors.New("import token key: malformed setting")
	}
	return k, nil
}

// NewTokenID is a fresh random token id.
func NewTokenID() string {
	b := make([]byte, 16)
	rand.Read(b) // never fails (crypto/rand)
	return hex.EncodeToString(b)
}

var tokenIDRe = regexp.MustCompile(`^[0-9a-f]{32}$`)

const tokenExt = ".tok"

func (v *Vault) file(id string) string { return filepath.Join(v.Dir, id+tokenExt) }

func (v *Vault) aead() (cipher.AEAD, error) {
	if len(v.Key) != 32 {
		return nil, errors.New("token vault: the key is not 32 bytes")
	}
	b, err := aes.NewCipher(v.Key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func checkID(id string) error {
	if !tokenIDRe.MatchString(id) {
		return fmt.Errorf("token vault: bad id %q", id)
	}
	return nil
}

// Put stores a token (the directory 0700, the file 0600, written whole
// or not at all).
func (v *Vault) Put(id string, tok Token) error {
	if err := checkID(id); err != nil {
		return err
	}
	a, err := v.aead()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(v.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(v.Dir, 0o700); err != nil {
		return err
	}
	plain, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	nonce := make([]byte, a.NonceSize())
	rand.Read(nonce)
	sealed := a.Seal(nonce, nonce, plain, []byte(id))
	clear(plain)
	f, err := os.CreateTemp(v.Dir, ".put-*") // 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(sealed)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, v.file(id))
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// Get reads a token back.
func (v *Vault) Get(id string) (Token, error) {
	var tok Token
	if err := checkID(id); err != nil {
		return tok, err
	}
	a, err := v.aead()
	if err != nil {
		return tok, err
	}
	sealed, err := os.ReadFile(v.file(id))
	if err != nil {
		return tok, err
	}
	n := a.NonceSize()
	if len(sealed) < n {
		return tok, errors.New("token vault: truncated token")
	}
	plain, err := a.Open(nil, sealed[:n], sealed[n:], []byte(id))
	if err != nil {
		return tok, errors.New("token vault: the token cannot be opened")
	}
	defer clear(plain)
	err = json.Unmarshal(plain, &tok)
	return tok, err
}

// Delete removes a token (a missing one is no error).
func (v *Vault) Delete(id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	if err := os.Remove(v.file(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Sweep removes tokens (and half-written files) older than olderThan; it
// returns how many it removed. A missing directory has none.
func (v *Vault) Sweep(olderThan time.Duration) (int, error) {
	entries, err := os.ReadDir(v.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-olderThan)
	n := 0
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, tokenExt) || strings.HasPrefix(name, ".put-")) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue // gone meanwhile
		}
		if olderThan > 0 && fi.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(v.Dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}
