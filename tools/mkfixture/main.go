// mkfixture builds the old build's database and data for the torrents
// e2e in a fresh directory: nothing is checked in under web/.
//
//	go run ./tools/mkfixture -out DIR -media MEDIA [-sql internal/importer/testdata/vault-main.sql]
//	go run ./tools/mkfixture -releases DIR -metafiles DIR
//
// The second form is the releases mode (releases.go): the fake site's
// releases with real payloads, for the search e2e.
//
// DIR/vault.db is internal/importer/testdata/vault-main.sql (the old
// schema and its small data set, WAL mode). Its three downloads get real
// data: a 64 KiB payload each and a real metafile in DIR/data/torrents/
// <infohash>.torrent, whose infohash replaces the fake one in every table
// that names it. The finished one is complete on disk under
// MEDIA/torrents, the empty one (a stream) has no data there, and the
// partial one has only its first half, in a save dir outside MEDIA
// (outside every file root, as the live downloads dir is: a download, so
// the addon counts it). The byte counters say what is on disk, and
// the finished one is seeding, as a finished download is live. The
// library files' paths move under MEDIA, in the files rows and in the
// acquisitions' JSON; the finished download's library file is a copy of
// its payload, and its files row has the copy's size and mtime. The
// summary (JSON) goes to stdout. DIR must not exist yet.
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	_ "modernc.org/sqlite"
)

// payloadSize is each download's data: four pieces of 16 KiB.
const (
	payloadSize = 64 << 10
	pieceLength = 16 << 10
)

type options struct {
	SQL   string // the snapshot schema and data (vault-main.sql)
	Out   string // the new fixture dir
	Media string // the media root the e2e server uses
}

// Torrent is one download of the fixture.
type Torrent struct {
	Infohash string `json:"infohash"`
	Name     string `json:"name"`
	SaveDir  string `json:"saveDir"`
	State    string `json:"state"` // complete | partial | empty
	Size     int64  `json:"size"`
	Outside  bool   `json:"outside"` // the save dir lies outside Media
}

// Summary is what the e2e setup reads.
type Summary struct {
	VaultDB  string    `json:"vaultDB"`
	DataDir  string    `json:"dataDir"`
	Torrents []Torrent `json:"torrents"`
}

// download is a fixture download: its fake infohash in vault-main.sql, the
// state its data gets, and its folder and file names.
type download struct {
	fake, state, folder, file string
}

var downloads = []download{
	{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "complete", "Sample Movie 2001", "sample.mkv"},
	{"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "partial", "Sample Series S01", "S01E01.mkv"},
	{"cccccccccccccccccccccccccccccccccccccccc", "empty", "Sample Stream", "stream.mkv"},
}

func main() {
	var o options
	flag.StringVar(&o.SQL, "sql", "internal/importer/testdata/vault-main.sql", "the snapshot schema and data")
	flag.StringVar(&o.Out, "out", "", "the new fixture dir (must not exist)")
	flag.StringVar(&o.Media, "media", "", "the e2e server's media root")
	releases := flag.String("releases", "", "releases mode: the download dir the payloads go to")
	metafiles := flag.String("metafiles", "", "releases mode: the dir the metafiles go to")
	flag.Parse()
	if *releases != "" || *metafiles != "" {
		if *releases == "" || *metafiles == "" {
			fmt.Fprintln(os.Stderr, "usage: mkfixture -releases DIR -metafiles DIR")
			os.Exit(2)
		}
		m, err := buildReleases(*releases, *metafiles)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mkfixture:", err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(m); err != nil {
			fmt.Fprintln(os.Stderr, "mkfixture:", err)
			os.Exit(1)
		}
		return
	}
	if o.Out == "" || o.Media == "" {
		fmt.Fprintln(os.Stderr, "usage: mkfixture -out DIR -media MEDIA [-sql FILE]")
		os.Exit(2)
	}
	s, err := build(o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkfixture:", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(s); err != nil {
		fmt.Fprintln(os.Stderr, "mkfixture:", err)
		os.Exit(1)
	}
}

// payload is a download's data: deterministic bytes from its name.
func payload(name string) []byte {
	out := make([]byte, 0, payloadSize)
	h := sha256.Sum256([]byte(name))
	for len(out) < payloadSize {
		out = append(out, h[:]...)
		h = sha256.Sum256(h[:])
	}
	return out[:payloadSize]
}

// build makes the fixture; the staging dir of the metafiles' sources is
// removed whatever happens.
func build(o options) (s Summary, err error) {
	if o.Out, err = filepath.Abs(o.Out); err != nil {
		return Summary{}, err
	}
	if o.Media, err = filepath.Abs(o.Media); err != nil {
		return Summary{}, err
	}
	if _, err := os.Stat(o.Out); err == nil {
		return Summary{}, fmt.Errorf("%s exists: a fixture is built into a new dir", o.Out)
	}
	text, err := os.ReadFile(o.SQL)
	if err != nil {
		return Summary{}, err
	}
	data := filepath.Join(o.Out, "data")
	for _, d := range []string{"torrents", "definitions", "import-tokens", "migrate/qbt-state"} {
		if err := os.MkdirAll(filepath.Join(data, d), 0o755); err != nil {
			return Summary{}, err
		}
	}
	staging := filepath.Join(o.Out, "src")
	defer func() {
		if rerr := os.RemoveAll(staging); err == nil {
			err = rerr
		}
	}()
	s = Summary{VaultDB: filepath.Join(o.Out, "vault.db"), DataDir: data}
	db, err := sql.Open("sqlite", "file:"+s.VaultDB+"?_pragma=journal_mode(WAL)")
	if err != nil {
		return Summary{}, err
	}
	defer db.Close()
	if _, err := db.Exec(string(text)); err != nil {
		return Summary{}, fmt.Errorf("schema: %w", err)
	}
	outside := filepath.Join(o.Out, "outside")
	for _, d := range downloads {
		saveDir := filepath.Join(o.Media, "torrents")
		if d.state == "partial" {
			saveDir = outside
		}
		t, err := makeDownload(d, saveDir, staging, data)
		if err != nil {
			return Summary{}, fmt.Errorf("%s: %w", d.folder, err)
		}
		t.Outside = d.state == "partial"
		if err := rewrite(db, d.fake, t); err != nil {
			return Summary{}, fmt.Errorf("%s: %w", d.folder, err)
		}
		s.Torrents = append(s.Torrents, t)
	}
	// the library paths move under the media root; the pieces table is the
	// engine's (it checks the data again)
	if _, err := db.Exec(`UPDATE files SET path = ? || substr(path, length('/media') + 1) WHERE path LIKE '/media/%'`, o.Media); err != nil {
		return Summary{}, err
	}
	// and so do the ones the acquisitions' JSON names ("/media/… inside a
	// JSON string; the media root JSON-escaped the same way)
	jm, err := json.Marshal(o.Media + "/")
	if err != nil {
		return Summary{}, err
	}
	for _, col := range []string{"paths_json", "meta_json"} {
		if _, err := db.Exec(`UPDATE acquisitions SET `+col+` = replace(`+col+`, '"/media/', ?)`, strings.TrimSuffix(string(jm), `"`)); err != nil {
			return Summary{}, fmt.Errorf("acquisitions.%s: %w", col, err)
		}
	}
	if _, err := db.Exec(`DELETE FROM torrent_pieces`); err != nil {
		return Summary{}, err
	}
	// the finished download's library file: a copy of its payload
	lib := filepath.Join(o.Media, "movies", "Sample Movie (2001)", "sample.mkv")
	if err := writeFile(lib, payload(downloads[0].folder)); err != nil {
		return Summary{}, err
	}
	// its files row says what is on disk, so a scan sees it unchanged
	fi, err := os.Stat(lib)
	if err != nil {
		return Summary{}, err
	}
	if _, err := db.Exec(`UPDATE files SET size=?, mtime=? WHERE path=?`, fi.Size(), fi.ModTime().Unix(), lib); err != nil {
		return Summary{}, err
	}
	if err := db.Close(); err != nil {
		return Summary{}, err
	}
	return s, nil
}

// makeDownload writes d's metafile (built from its full payload in a
// staging dir) and the part of its data its state keeps under saveDir.
func makeDownload(d download, saveDir, staging, data string) (Torrent, error) {
	body := payload(d.folder)
	src := filepath.Join(staging, d.folder)
	if err := writeFile(filepath.Join(src, d.file), body); err != nil {
		return Torrent{}, err
	}
	info := metainfo.Info{PieceLength: pieceLength}
	if err := info.BuildFromFilePath(src); err != nil {
		return Torrent{}, err
	}
	ib, err := bencode.Marshal(info)
	if err != nil {
		return Torrent{}, err
	}
	mi := metainfo.MetaInfo{InfoBytes: ib}
	ih := mi.HashInfoBytes().HexString()
	f, err := os.Create(filepath.Join(data, "torrents", ih+".torrent"))
	if err != nil {
		return Torrent{}, err
	}
	if err := mi.Write(f); err != nil {
		f.Close()
		return Torrent{}, err
	}
	if err := f.Close(); err != nil {
		return Torrent{}, err
	}
	switch d.state {
	case "complete":
		err = writeFile(filepath.Join(saveDir, d.folder, d.file), body)
	case "partial":
		err = writeFile(filepath.Join(saveDir, d.folder, d.file), body[:payloadSize/2])
	case "empty":
		err = os.MkdirAll(saveDir, 0o755)
	default:
		err = errors.New("unknown state " + d.state)
	}
	if err != nil {
		return Torrent{}, err
	}
	return Torrent{Infohash: ih, Name: d.folder, SaveDir: saveDir, State: d.state, Size: payloadSize}, nil
}

// rewrite puts t's infohash and save dir in place of the fake infohash, in
// every table that names one (files keep any case).
func rewrite(db *sql.DB, fake string, t Torrent) error {
	// the byte counters say what is on disk; nothing was ever sent
	// and the finished one is what a finished download is in the live DB
	var have int64
	status := "downloading"
	switch t.State {
	case "complete":
		have, status = payloadSize, "seeding"
	case "partial":
		have = payloadSize / 2
	}
	if _, err := db.Exec(`UPDATE torrents SET infohash=?, save_dir=?, name=?, magnet=NULL, uploaded=0, downloaded=?, status=? WHERE infohash=?`, t.Infohash, t.SaveDir, t.Name, have, status, fake); err != nil {
		return err
	}
	for _, q := range []string{
		`UPDATE files SET torrent_infohash=? WHERE lower(torrent_infohash)=?`,
		`UPDATE acquisitions SET infohash=? WHERE infohash=?`,
		`UPDATE release_attempts SET infohash=? WHERE infohash=?`,
		`UPDATE pinned_releases SET infohash=? WHERE infohash=?`,
	} {
		if _, err := db.Exec(q, t.Infohash, fake); err != nil {
			return fmt.Errorf("%s: %w", strings.Fields(q)[1], err)
		}
	}
	return nil
}

func writeFile(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
