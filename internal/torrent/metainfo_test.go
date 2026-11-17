package torrent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func encodeInfo(t *testing.T, info metainfo.Info) []byte {
	t.Helper()
	ib, err := bencode.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := (&metainfo.MetaInfo{InfoBytes: ib}).Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestParseFiles(t *testing.T) {
	t.Run("single file", func(t *testing.T) {
		fx := newFixture(t, t.TempDir())
		ih, name, files, err := ParseFiles(fx.metafile)
		if err != nil {
			t.Fatal(err)
		}
		if ih != fx.ih || name != fx.name {
			t.Fatalf("ih %s name %q", ih, name)
		}
		want := []FileEntry{{Index: 0, Path: fx.name, Size: testSize}}
		if len(files) != 1 || files[0] != want[0] {
			t.Fatalf("files %+v", files)
		}
	})

	t.Run("multi file", func(t *testing.T) {
		fx := newMultiFixture(t, 16<<10, 30000, 40000, 50000)
		ih, name, files, err := ParseFiles(fx.metafile)
		if err != nil {
			t.Fatal(err)
		}
		if ih != fx.ih || name != albumName {
			t.Fatalf("ih %s name %q", ih, name)
		}
		if len(files) != 3 {
			t.Fatalf("files %+v", files)
		}
		for i, f := range files {
			want := FileEntry{Index: i, Path: albumName + "/" + fx.names[i], Size: int64(len(fx.files[i]))}
			if f != want {
				t.Fatalf("file %d = %+v, want %+v", i, f, want)
			}
		}
	})

	t.Run("v1 with BEP-47 padding", func(t *testing.T) {
		info := metainfo.Info{
			Name:        "Альбом",
			PieceLength: 16 << 10,
			Pieces:      make([]byte, 20*3),
			Files: []metainfo.FileInfo{
				{Length: 10000, Path: []string{"01.flac"}},
				{Length: 16384 - 10000, Path: []string{".pad", "6384"}, ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}},
				{Length: 20000, Path: []string{"CD2", "02.flac"}},
				{Length: 100, Path: []string{".pad", "100"}},
			},
		}
		_, name, files, err := ParseFiles(encodeInfo(t, info))
		if err != nil {
			t.Fatal(err)
		}
		want := []FileEntry{
			{Index: 0, Path: "Альбом/01.flac", Size: 10000},
			{Index: 1, Path: "Альбом/.pad/6384", Size: 6384, Padding: true},
			{Index: 2, Path: "Альбом/CD2/02.flac", Size: 20000},
			{Index: 3, Path: "Альбом/.pad/100", Size: 100, Padding: true},
		}
		if name != "Альбом" || len(files) != len(want) {
			t.Fatalf("name %q files %+v", name, files)
		}
		for i := range want {
			if files[i] != want[i] {
				t.Fatalf("file %d = %+v, want %+v", i, files[i], want[i])
			}
		}
	})

	t.Run("hybrid v1+v2 matches the engine's indexes", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join("testdata", "bittorrent-v2-hybrid-test.torrent"))
		if err != nil {
			t.Fatal(err)
		}
		ih, name, files, err := ParseFiles(data)
		if err != nil {
			t.Fatal(err)
		}
		mi, _ := metainfo.Load(bytes.NewReader(data))
		if ih != mi.HashInfoBytes().HexString() {
			t.Fatalf("infohash %s", ih)
		}
		// The v1 list carries .pad entries; the engine indexes the v2 file tree
		// (no padding), and the preview must use the same indexes.
		if len(files) != 9 {
			t.Fatalf("files %d: %+v", len(files), files)
		}
		e := openEngine(t, t.TempDir(), engOpts{})
		h, err := e.AddTorrentFile(context.Background(), data, AddOptions{Mode: ModeDownload, SaveDir: t.TempDir(), Paused: true})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.WaitInfo(ctx); err != nil {
			t.Fatal(err)
		}
		eng := h.Files()
		if len(eng) != len(files) {
			t.Fatalf("engine %d files, parsed %d", len(eng), len(files))
		}
		for i, f := range files {
			if f.Index != eng[i].Index || f.Path != eng[i].Path || f.Size != eng[i].Size || f.Padding {
				t.Fatalf("file %d parsed %+v engine %+v", i, f, eng[i])
			}
			if !strings.HasPrefix(f.Path, name+"/") {
				t.Fatalf("path %q lacks the torrent dir %q", f.Path, name)
			}
		}
	})

	t.Run("v2-only is an error", func(t *testing.T) {
		data, err := os.ReadFile(filepath.Join("testdata", "bittorrent-v2-test.torrent"))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := ParseFiles(data); err == nil {
			t.Fatal("v2-only metafile accepted")
		}
	})

	t.Run("garbage is an error", func(t *testing.T) {
		if _, _, _, err := ParseFiles([]byte("not a torrent")); err == nil {
			t.Fatal("garbage accepted")
		}
	})
}
