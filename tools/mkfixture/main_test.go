package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/metainfo"

	"github.com/antresol-app/antresol-addon-torrents/internal/importer"
	"github.com/antresol-app/antresol-addon-torrents/internal/store"
	_ "modernc.org/sqlite"
)

const sqlPath = "../../internal/importer/testdata/vault-main.sql"

func built(t *testing.T) (Summary, options) {
	t.Helper()
	root := t.TempDir()
	o := options{SQL: sqlPath, Out: filepath.Join(root, "out"), Media: filepath.Join(root, "media")}
	s, err := build(o)
	if err != nil {
		t.Fatal(err)
	}
	return s, o
}

// The three downloads get real metafiles: the infohash is the SHA-1 of
// the metafile's info, and it replaces the fixture's fake one everywhere.
func TestBuild_RealInfohashesEverywhere(t *testing.T) {
	s, o := built(t)
	if len(s.Torrents) != 3 {
		t.Fatalf("torrents %+v", s.Torrents)
	}
	d, err := sql.Open("sqlite", "file:"+s.VaultDB+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, tr := range s.Torrents {
		mi, err := metainfo.LoadFromFile(filepath.Join(s.DataDir, "torrents", tr.Infohash+".torrent"))
		if err != nil {
			t.Fatalf("%s: %v", tr.Name, err)
		}
		if got := mi.HashInfoBytes().HexString(); got != tr.Infohash {
			t.Fatalf("%s: metafile %s, summary %s", tr.Name, got, tr.Infohash)
		}
		var save string
		if err := d.QueryRow(`SELECT save_dir FROM torrents WHERE infohash=?`, tr.Infohash).Scan(&save); err != nil {
			t.Fatalf("%s: row: %v", tr.Name, err)
		}
		if save != tr.SaveDir {
			t.Fatalf("%s: save_dir %s, summary %s", tr.Name, save, tr.SaveDir)
		}
	}
	// no fake infohash is left in any table that names one
	for _, fake := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccccccccccccccccccc"} {
		for _, tc := range [][2]string{{"torrents", "infohash"}, {"files", "torrent_infohash"}, {"acquisitions", "infohash"},
			{"release_attempts", "infohash"}, {"pinned_releases", "infohash"}, {"torrent_pieces", "infohash"}} {
			var n int
			if err := d.QueryRow(`SELECT count(*) FROM `+tc[0]+` WHERE lower(`+tc[1]+`)=?`, fake).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%s.%s still holds %s (%d rows)", tc[0], tc[1], fake, n)
			}
		}
	}
	// the library files' paths are under the media dir given
	rows, err := d.Query(`SELECT path FROM files`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(p, o.Media+"/") {
			t.Errorf("files.path %s not under %s", p, o.Media)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// the acquisitions' JSON names library paths under the media dir too
	media, _ := json.Marshal(o.Media + "/")
	arows, err := d.Query(`SELECT paths_json || meta_json FROM acquisitions`)
	if err != nil {
		t.Fatal(err)
	}
	defer arows.Close()
	for arows.Next() {
		var j string
		if err := arows.Scan(&j); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(j, `"/media/`) {
			t.Errorf("acquisitions JSON still names /media/: %s", j)
		}
		if strings.Contains(j, ".mkv") && !strings.Contains(j, strings.Trim(string(media), `"`)) {
			t.Errorf("acquisitions JSON not under %s: %s", o.Media, j)
		}
	}
	if err := arows.Err(); err != nil {
		t.Fatal(err)
	}
}

// The byte counters match the 64 KiB payloads (what is on disk), nothing
// was sent, and the library copy's files row has its real size and mtime.
func TestBuild_CountersAndLibraryRow(t *testing.T) {
	s, o := built(t)
	d, err := sql.Open("sqlite", "file:"+s.VaultDB+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	want := map[string]int64{"complete": payloadSize, "partial": payloadSize / 2, "empty": 0}
	for _, tr := range s.Torrents {
		var up, down int64
		var status string
		if err := d.QueryRow(`SELECT uploaded, downloaded, status FROM torrents WHERE infohash=?`, tr.Infohash).Scan(&up, &down, &status); err != nil {
			t.Fatal(err)
		}
		// the finished one as a finished download is in the live DB
		if (tr.State == "complete") != (status == "seeding") {
			t.Errorf("%s: status %s", tr.Name, status)
		}
		if up != 0 || down != want[tr.State] {
			t.Errorf("%s: uploaded %d downloaded %d, want 0 and %d", tr.Name, up, down, want[tr.State])
		}
	}
	lib := filepath.Join(o.Media, "movies", "Sample Movie (2001)", "sample.mkv")
	fi, err := os.Stat(lib)
	if err != nil {
		t.Fatal(err)
	}
	var size, mtime int64
	if err := d.QueryRow(`SELECT size, mtime FROM files WHERE path=?`, lib).Scan(&size, &mtime); err != nil {
		t.Fatal(err)
	}
	if size != fi.Size() || mtime != fi.ModTime().Unix() {
		t.Errorf("files row size %d mtime %d, disk %d %d", size, mtime, fi.Size(), fi.ModTime().Unix())
	}
}

// pieces checks the data on disk against the metafile: how many of its
// pieces hash right.
func pieces(t *testing.T, s Summary, tr Torrent) (good, total int) {
	t.Helper()
	mi, err := metainfo.LoadFromFile(filepath.Join(s.DataDir, "torrents", tr.Infohash+".torrent"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, f := range info.UpvertedFiles() {
		b, err := os.ReadFile(filepath.Join(append([]string{tr.SaveDir, info.Name}, f.Path...)...))
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if len(b) < int(f.Length) {
			b = append(b, make([]byte, int(f.Length)-len(b))...)
		}
		data = append(data, b...)
	}
	total = info.NumPieces()
	for i := 0; i < total; i++ {
		end := min(int64(i+1)*info.PieceLength, int64(len(data)))
		h := sha1.Sum(data[int64(i)*info.PieceLength : end])
		if bytes.Equal(h[:], info.Piece(i).V1Hash().Value.Bytes()) {
			good++
		}
	}
	return good, total
}

// One download complete, one partial, one with no data; the partial one's
// save dir lies outside the media dir (outside every file root, as the
// live downloads dir is).
func TestBuild_PayloadStates(t *testing.T) {
	s, o := built(t)
	states := map[string]Torrent{}
	for _, tr := range s.Torrents {
		states[tr.State] = tr
		if tr.Size != payloadSize {
			t.Fatalf("%s: size %d", tr.Name, tr.Size)
		}
	}
	if g, n := pieces(t, s, states["complete"]); g != n || n < 2 {
		t.Fatalf("complete: %d/%d pieces", g, n)
	}
	if g, n := pieces(t, s, states["partial"]); g == 0 || g == n {
		t.Fatalf("partial: %d/%d pieces", g, n)
	}
	if g, _ := pieces(t, s, states["empty"]); g != 0 {
		t.Fatalf("empty: %d good pieces", g)
	}
	outside := 0
	for _, tr := range s.Torrents {
		in := strings.HasPrefix(tr.SaveDir, o.Media+"/")
		if tr.Outside == in {
			t.Fatalf("%s: outside=%v but save dir %s", tr.Name, tr.Outside, tr.SaveDir)
		}
		if tr.Outside {
			outside++
		}
	}
	// a download, not a stream: the addon counts only those outside
	if outside != 1 || !states["partial"].Outside {
		t.Fatalf("outside %d, partial %+v", outside, states["partial"])
	}
	// the complete download's library file is a real copy of its payload
	b, err := os.ReadFile(filepath.Join(o.Media, "movies/Sample Movie (2001)/sample.mkv"))
	if err != nil || len(b) != payloadSize {
		t.Fatalf("library file: %d bytes, %v", len(b), err)
	}
}

// The data files the import copies are there, and the summary is JSON.
func TestBuild_DataDirAndSummary(t *testing.T) {
	s, _ := built(t)
	for _, rel := range []string{"definitions", "import-tokens", "migrate/qbt-state"} {
		if fi, err := os.Stat(filepath.Join(s.DataDir, rel)); err != nil || !fi.IsDir() {
			t.Fatalf("%s: %v", rel, err)
		}
	}
	b, err := json.Marshal(s)
	if err != nil || !bytes.Contains(b, []byte(`"vaultDB"`)) {
		t.Fatalf("%s %v", b, err)
	}
	// a second build into the same dir is refused (nothing is overwritten)
	if _, err := build(options{SQL: sqlPath, Out: filepath.Dir(s.VaultDB), Media: t.TempDir()}); err == nil {
		t.Fatal("built over an existing fixture")
	}
}

// The addon's one-time import takes the fixture whole: its three
// downloads and their metafiles arrive in torrents.db.
func TestBuild_ImportsInTheAddon(t *testing.T) {
	s, _ := built(t)
	dataDir := t.TempDir()
	d, err := store.Open(filepath.Join(dataDir, "torrents.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	counts, err := importer.Phase1(context.Background(), importer.Options{From: s.VaultDB, FromDataDir: s.DataDir, DataDir: dataDir, DB: d})
	if err != nil {
		t.Fatal(err)
	}
	if counts["torrents"] != 3 {
		t.Fatalf("counts %v", counts)
	}
	for _, tr := range s.Torrents {
		var save string
		if err := d.QueryRow(`SELECT save_dir FROM torrents WHERE infohash=?`, tr.Infohash).Scan(&save); err != nil || save != tr.SaveDir {
			t.Fatalf("%s: %q %v", tr.Name, save, err)
		}
		if _, err := os.Stat(filepath.Join(dataDir, "torrents", tr.Infohash+".torrent")); err != nil {
			t.Fatalf("%s: metafile not copied: %v", tr.Name, err)
		}
	}
}
