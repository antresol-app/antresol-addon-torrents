//go:build ignore

package musicimport

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testVault(t *testing.T) *Vault {
	t.Helper()
	return &Vault{Dir: filepath.Join(t.TempDir(), "import-tokens"), Key: bytes.Repeat([]byte{7}, 32)}
}

func TestTokenVault(t *testing.T) {
	v := testVault(t)
	tok := Token{Access: "BQD-secret-access", Refresh: "AQD-secret-refresh", Expiry: time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC)}
	id := NewTokenID()
	if err := v.Put(id, tok); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(v.Dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir %v %v", fi, err)
	}
	files, _ := filepath.Glob(filepath.Join(v.Dir, "*"))
	if len(files) != 1 {
		t.Fatalf("files %v", files)
	}
	fi, err = os.Stat(files[0])
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", fi.Mode(), err)
	}
	raw, _ := os.ReadFile(files[0])
	if bytes.Contains(raw, []byte("secret")) || bytes.Contains(raw, []byte("BQD")) {
		t.Fatalf("the token is readable at rest: %q", raw)
	}
	got, err := v.Get(id)
	if err != nil || got.Access != tok.Access || got.Refresh != tok.Refresh || !got.Expiry.Equal(tok.Expiry) {
		t.Fatalf("get %+v %v", got, err)
	}
	// another key cannot open it
	other := &Vault{Dir: v.Dir, Key: bytes.Repeat([]byte{8}, 32)}
	if _, err := other.Get(id); err == nil {
		t.Fatal("another key opened the token")
	}
	// ids are not paths
	for _, bad := range []string{"../x", "", "a/b", "ABC"} {
		if err := v.Put(bad, tok); err == nil {
			t.Fatalf("put %q accepted", bad)
		}
	}
	if err := v.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(files[0]); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after delete: %v", err)
	}
	if err := v.Delete(id); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := v.Get(id); err == nil {
		t.Fatal("get after delete")
	}

	// Sweep removes what a stopped read left behind, not a fresh token
	old, fresh := NewTokenID(), NewTokenID()
	if err := v.Put(old, tok); err != nil {
		t.Fatal(err)
	}
	if err := v.Put(fresh, tok); err != nil {
		t.Fatal(err)
	}
	two := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(v.file(old), two, two); err != nil {
		t.Fatal(err)
	}
	n, err := v.Sweep(time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("sweep %d %v", n, err)
	}
	if _, err := v.Get(old); err == nil {
		t.Fatal("old token survived the sweep")
	}
	if _, err := v.Get(fresh); err != nil {
		t.Fatalf("fresh token swept: %v", err)
	}
	// a missing dir sweeps nothing
	if n, err := (&Vault{Dir: filepath.Join(t.TempDir(), "none"), Key: v.Key}).Sweep(0); err != nil || n != 0 {
		t.Fatalf("missing dir %d %v", n, err)
	}
}

func TestVaultKeyPersists(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()
	a, err := VaultKey(ctx, st.DB)
	if err != nil || len(a) != 32 {
		t.Fatalf("%x %v", a, err)
	}
	b, _ := VaultKey(ctx, st.DB)
	if !bytes.Equal(a, b) {
		t.Fatal("the key changed")
	}
}
