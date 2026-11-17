package pinned

import (
	"context"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
)

func TestPinnedRowsCarryMeta(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	mf := metafile(t, releaseName, "Show.S01E01.1080p.mkv", "Show.S01E02.1080p.mkv")
	e.idx.downloads["https://tracker.test/dl/1"] = cardigann.DownloadResult{Torrent: mf}
	if _, err := e.svc.PinRelease(ctx, e.title.ID, "3", "https://tracker.test/dl/1", "", releaseName, by(e.users[0])); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(time.Hour)
	srcs, err := e.svc.Scanner().Scan(ctx, e.title, nil)
	if err != nil || len(srcs) != 2 {
		t.Fatalf("%+v %v", srcs, err)
	}
	for i, s := range srcs {
		m := s.Meta
		if m.SourceID != "3" || m.FileIndex == nil || *m.FileIndex != i || m.Voice != "HDRezka Studio" || m.Lang != "ru" ||
			len(m.Studios) != 1 || m.Studios[0] != "HDRezka Studio" {
			t.Fatalf("meta %d: %+v", i, m)
		}
	}
}

func TestTorrentRowsCarrySourceID(t *testing.T) {
	e, ctx := newEnv(t), context.Background()
	f := &fakeSearcher{releases: []indexer.ScoredRelease{rel("Show.S01E01.1080p.WEB-DL.MVO.HDRezka-GRP", "https://tracker.test/dl/1", 9)}}
	got, err := newTorrentScanner(e, f, 0).Scan(ctx, tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}, season1(3))
	if err != nil || len(got) == 0 {
		t.Fatalf("%+v %v", got, err)
	}
	m := got[0].Meta
	if m.SourceID != "3" || m.Voice != "HDRezka Studio" || m.Lang != "ru" || m.Audio != "MVO" || len(m.Studios) != 1 || m.FileIndex != nil {
		t.Fatalf("meta %+v", m)
	}
}
