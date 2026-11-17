package acquire

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

type finishedLog struct {
	mu  sync.Mutex
	got []Finished
}

func (f *finishedLog) add(x Finished) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, x)
}

func (f *finishedLog) all() []Finished {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Finished(nil), f.got...)
}

// Options.Finished hears of an organized download (notification letters).
func TestFinishedOnOrganize(t *testing.T) {
	e := newEnv(t)
	log := &finishedLog{}
	e.svc.SetFinished(log.add)
	dir := filepath.Join(e.root, "torrents", "movies")
	src := writeFile(t, dir, "Dune.2024/Dune.2024.mkv", "movie")
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "Dune.2024/Dune.2024.mkv", FullPath: src, Size: 5, Progress: 1}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	got := log.all()
	if len(got) != 1 || got[0].Key != res.InfoHash || got[0].Failed || got[0].Kind != KindMovie || got[0].Title == "" {
		t.Fatalf("finished %+v", got)
	}
}

// …and of one that could not be put into the library.
func TestFinishedOnOrganizeFailure(t *testing.T) {
	e := newEnv(t)
	log := &finishedLog{}
	e.svc.SetFinished(log.add)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{relx("Дюна 2024", "https://t/1", 1, 5)}}
	e.eng.nextFiles = []torrent.FileInfo{{Index: 0, Path: "readme.txt", FullPath: filepath.Join(e.root, "nope.txt"), Size: 5, Progress: 1}}
	res, err := e.svc.Acquire(context.Background(), AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload})
	if err != nil {
		t.Fatal(err)
	}
	e.eng.complete(res.InfoHash)
	e.svc.wait()
	got := log.all()
	if len(got) == 0 || !got[0].Failed || got[0].Error == "" || got[0].Key != res.InfoHash {
		t.Fatalf("finished %+v", got)
	}
}
