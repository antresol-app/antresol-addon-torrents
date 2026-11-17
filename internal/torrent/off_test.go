package torrent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// EngineConfig.Off (VAULT_LIBRARY_ONLY): no client, no folders, adding
// fails, the stored torrents stay untouched; Offline still checks the disk.
func TestEngineOff(t *testing.T) {
	dataDir := t.TempDir()
	d, err := db.Open(filepath.Join(dataDir, "vault.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`INSERT INTO torrents (infohash, name, mode, save_dir, status) VALUES ('aa', 'Фильм', 'download', '/x', 'seeding')`); err != nil {
		t.Fatal(err)
	}
	diskErr := error(nil)
	e, err := New(EngineConfig{DataDir: dataDir, StreamDir: filepath.Join(dataDir, "stream"), ListenPort: 1, Off: true,
		StorageCheck: func() error { return diskErr }}, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"stream", "torrents"} {
		if _, err := os.Stat(filepath.Join(dataDir, p)); !os.IsNotExist(err) {
			t.Fatalf("%s created: %v", p, err)
		}
	}
	if l := e.List(); len(l) != 0 {
		t.Fatalf("list: %+v", l)
	}
	fx := newFixture(t, t.TempDir())
	if _, err := e.AddTorrentFile(context.Background(), fx.metafile, AddOptions{Mode: ModeDownload, SaveDir: dataDir}); !errors.Is(err, ErrOff) {
		t.Fatalf("add: %v", err)
	}
	if _, err := e.FetchInfo(context.Background(), "magnet:?xt=urn:btih:"+fx.ih); !errors.Is(err, ErrOff) {
		t.Fatalf("fetch info: %v", err)
	}
	e.SetUploadLimit(1000)
	if e.Offline() != "" {
		t.Fatal("disk there: online")
	}
	diskErr = errors.New("нет папки")
	if e.Offline() == "" {
		t.Fatal("disk gone: Offline must say so on the next call")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM torrents WHERE status='seeding'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stored torrent touched: %d %v", n, err)
	}
}
