package torrent

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// FileEntry is one file of a metafile, indexed like FileInfo.Index (the
// order of anacrolix's UpvertedFiles, which AddOptions.Files refers to).
type FileEntry struct {
	Index int
	Path  string // "/"-separated, includes the torrent dir like FileInfo.Path
	Size  int64
	// Padding marks a BEP-47 padding file. It is listed, not dropped, so
	// the indexes stay those of the engine.
	Padding bool
}

// ParseFiles reads a metafile's v1 infohash, name and file list. A v2-only
// metafile (no v1 infohash) is an error: AddTorrentFile cannot add it.
func ParseFiles(metafile []byte) (infoHash, name string, files []FileEntry, err error) {
	mi, err := metainfo.Load(bytes.NewReader(metafile))
	if err != nil {
		return "", "", nil, fmt.Errorf("torrent file: %w", err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return "", "", nil, fmt.Errorf("torrent file: %w", err)
	}
	if !info.HasV1() {
		return "", "", nil, errors.New("torrent: v2-only torrents are not supported")
	}
	name = info.BestName()
	for i, fi := range info.UpvertedFiles() {
		// Same path as anacrolix's File.Path(): the name, then the path.
		p := strings.Join(append([]string{name}, fi.BestPath()...), "/")
		files = append(files, FileEntry{Index: i, Path: p, Size: fi.Length, Padding: isPadding(fi)})
	}
	return mi.HashInfoBytes().HexString(), name, files, nil
}

// isPadding reports a BEP-47 padding file: attr "p", or the conventional
// ".pad/<n>" path some clients write without the attr.
func isPadding(fi metainfo.FileInfo) bool {
	if strings.Contains(fi.Attr, "p") {
		return true
	}
	p := fi.BestPath()
	return len(p) == 2 && p[0] == ".pad"
}
