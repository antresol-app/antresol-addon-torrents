package acquire

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// writeFile creates a file with content under root and returns its path.
func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// torrentFiles writes the files of a torrent under dir and returns their
// FileInfos with in-torrent paths (rel, "/"-separated).
func torrentFiles(t *testing.T, dir string, rels ...string) []torrent.FileInfo {
	t.Helper()
	out := make([]torrent.FileInfo, len(rels))
	for i, rel := range rels {
		p := writeFile(t, dir, rel, "data:"+rel)
		out[i] = torrent.FileInfo{Index: i, Path: rel, FullPath: p, Size: int64(len("data:" + rel)), Progress: 1}
	}
	return out
}

func mediaJob(root, kind string, files []torrent.FileInfo) OrganizeJob {
	return OrganizeJob{Kind: kind, Files: filesOf(files),
		MusicDir: filepath.Join(root, "media/music"), EbooksDir: filepath.Join(root, "media/books/ebooks"),
		AudiobooksDir: filepath.Join(root, "media/books/audiobooks"), ComicsDir: filepath.Join(root, "media/books/manga")}
}
