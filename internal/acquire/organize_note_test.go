package acquire

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// haveLib is a fakeLib that also reports the episodes in the library.
type haveLib struct {
	*fakeLib
	have map[[2]int]bool
}

func (l haveLib) HaveEpisodes(context.Context, int64) (map[[2]int]bool, error) { return l.have, nil }

// A manual «На сервер» of an episode the library has places nothing and
// says so on the torrent.
func TestOrganizeNothingNewIsShown(t *testing.T) {
	e := newEnv(t)
	e.svc.o.Library = haveLib{e.lib, map[[2]int]bool{{1, 2}: true}}
	dir := filepath.Join(e.root, "torrents", "series")
	src := writeFile(t, dir, "Shogun.S01E02.1080p.mkv", "ep")
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Сёгун S01E02 1080p", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "Shogun.S01E02.1080p.mkv", FullPath: src, Size: 2, Progress: 1}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 126308, Kind: KindSeries, Season: 1, Episode: 2, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	for _, v := range e.svc.Torrents() {
		if v.InfoHash == res.InfoHash {
			if !v.Organized || v.Note != "Уже есть в медиатеке" || v.Error != "" {
				t.Fatalf("%+v", v)
			}
			return
		}
	}
	t.Fatal("torrent not listed")
}
