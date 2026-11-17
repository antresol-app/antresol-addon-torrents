//go:build ignore

package musicimport

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/auth"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// newTestStore opens a fresh database with one admin profile.
func newTestStore(t *testing.T) (*playlistimport.Store, int64) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	u, err := auth.NewStore(d).CreateFirstAdmin(context.Background(), "Папа", "secret")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	return &playlistimport.Store{DB: d, Now: func() time.Time { return now }}, u.ID
}

func mustExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}
