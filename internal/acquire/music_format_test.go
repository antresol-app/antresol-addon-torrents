package acquire

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
)

func mp3Prefs(bitrate string) *indexer.Prefs {
	p := indexer.DefaultPrefs()
	p.MusicFormat, p.MP3Bitrate = indexer.MusicMP3, bitrate
	return &p
}

func mismatch(r indexer.ScoredRelease) indexer.ScoredRelease { r.FormatMismatch = true; return r }

// «На сервер»: a release of another format is never the automatic pick.
func TestAcquireMusicSkipsOtherFormat(t *testing.T) {
	ctx := context.Background()
	req := AcquireRequest{Kind: KindMusic, ExtID: "rg", Title: "Группа крови", Artist: "Кино", Year: 1988, Mode: ModeDownload, UserID: 1}

	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Prefs: mp3Prefs(indexer.Bitrate320), Releases: []indexer.ScoredRelease{
		relx("Кино - Группа крови (1988) [MP3, 320 kbps]", "https://t/mp3", 1, 40),
		mismatch(relx("Кино - Группа крови (1988) FLAC", "https://t/flac", 1, 80)),
	}}
	if _, err := e.svc.Acquire(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(e.idx.downloads) != 1 || !strings.HasSuffix(e.idx.downloads[0], "https://t/mp3") {
		t.Fatalf("downloads %v", e.idx.downloads)
	}

	// only another format: «Нет варианта в нужном формате», nothing started
	e = newEnv(t)
	e.idx.resp = indexer.SearchResponse{Prefs: mp3Prefs(indexer.Bitrate320), Releases: []indexer.ScoredRelease{
		mismatch(relx("Кино - Группа крови (1988) FLAC", "https://t/flac", 1, 80)),
	}}
	_, err := e.svc.Acquire(ctx, req)
	msg, _ := UserMessage(err)
	if !errors.Is(err, ErrNoFormat) || !errors.Is(err, ErrNoRelease) || msg != "Нет варианта в нужном формате (MP3 320)" {
		t.Fatalf("%v %q", err, msg)
	}
	if len(e.idx.downloads) != 0 {
		t.Fatalf("downloads %v", e.idx.downloads)
	}

	// «Скачать в другом формате»
	req.AnyFormat = true
	if _, err := e.svc.Acquire(ctx, req); err != nil {
		t.Fatal(err)
	}
	if len(e.idx.downloads) != 1 || !strings.HasSuffix(e.idx.downloads[0], "https://t/flac") {
		t.Fatalf("any format: downloads %v", e.idx.downloads)
	}
}

// The import: the release holding the most songs is taken only among
// those of the wanted format; a release whose name names no format is
// judged by its file list.
func TestPlanTracksRespectsFormat(t *testing.T) {
	ctx := context.Background()
	flac := []string{"01 - Группа крови.flac", "02 - Закрой за мной дверь.flac"}
	mp3 := []string{"01 - Группа крови.mp3", "02 - Война.mp3"}

	// FLAC (another format) holds both songs, MP3 one: the MP3
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Prefs: mp3Prefs(indexer.BitrateAny), Releases: []indexer.ScoredRelease{
		mismatch(relx("Кино - Группа крови (1988) FLAC", linkA, 1, 90)), relx("Кино - Группа крови (1988) [MP3]", linkB, 1, 40)}}
	planRelease(t, e, linkA, "Кино - Группа крови FLAC", flac...)
	planRelease(t, e, linkB, "Кино - Группа крови MP3", mp3...)
	p, err := e.svc.PlanTracks(ctx, kinoTracks())
	if err != nil || p.ReleaseLink != linkB {
		t.Fatalf("plan %+v %v", p, err)
	}

	// the name tells nothing, the files are FLAC: another format
	e = newEnv(t)
	e.idx.resp = indexer.SearchResponse{Prefs: mp3Prefs(indexer.BitrateAny), Releases: []indexer.ScoredRelease{
		relx("Кино - Группа крови (1988)", linkA, 1, 90)}}
	planRelease(t, e, linkA, "Кино - Группа крови", flac...)
	_, err = e.svc.PlanTracks(ctx, kinoTracks())
	if !errors.Is(err, ErrNoFormat) {
		t.Fatalf("flac files: %v", err)
	}
	// the folder names the bitrate: «320» is not V0
	e = newEnv(t)
	e.idx.resp = indexer.SearchResponse{Prefs: mp3Prefs(indexer.BitrateV0), Releases: []indexer.ScoredRelease{
		relx("Кино - Группа крови (1988) [MP3]", linkA, 1, 90)}}
	planRelease(t, e, linkA, "Кино - Группа крови (MP3 320)", mp3...)
	if _, err = e.svc.PlanTracks(ctx, kinoTracks()); !errors.Is(err, ErrNoFormat) {
		t.Fatalf("320 folder: %v", err)
	}

	// only other formats: ErrNoFormat; AnyFormat takes them
	e = newEnv(t)
	e.idx.resp = indexer.SearchResponse{Prefs: mp3Prefs(indexer.Bitrate320), Releases: []indexer.ScoredRelease{
		mismatch(relx("Кино - Группа крови (1988) FLAC", linkA, 1, 90))}}
	planRelease(t, e, linkA, "Кино - Группа крови FLAC", flac...)
	_, err = e.svc.PlanTracks(ctx, kinoTracks())
	if msg, _ := UserMessage(err); !errors.Is(err, ErrNoFormat) || msg != "Нет варианта в нужном формате (MP3 320)" {
		t.Fatalf("no format: %v", err)
	}
	if len(e.idx.downloads) != 0 {
		t.Fatalf("opened %v", e.idx.downloads)
	}
	req := kinoTracks()
	req.AnyFormat = true
	if p, err := e.svc.PlanTracks(ctx, req); err != nil || p.ReleaseLink != linkA {
		t.Fatalf("any: %+v %v", p, err)
	}
}

// A release of the format whose file list did not load may hold the
// songs: the album is «a source did not answer» (tried again later), not
// «no release in the format» — Prince's «Purple Rain» had two MP3 320
// releases whose lists timed out and a FLAC one.
func TestPlanTracksUnloadedListIsNotNoFormat(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.idx.resp = indexer.SearchResponse{Prefs: mp3Prefs(indexer.Bitrate256), Releases: []indexer.ScoredRelease{
		relx("Кино - Группа крови (1988) [Mp3 320]", linkA, 1, 90), // no metafile: its list fails
		mismatch(relx("Кино - Группа крови (1988) FLAC", linkB, 1, 80))}}
	planRelease(t, e, linkB, "Кино - Группа крови FLAC", "01 - Группа крови.flac", "02 - Закрой за мной дверь.flac")
	_, err := e.svc.PlanTracks(ctx, kinoTracks())
	if errors.Is(err, ErrNoFormat) || !errors.Is(err, ErrNoRelease) || !errors.Is(err, ErrSourcesFailed) {
		t.Fatalf("unloaded list: %v", err)
	}
}

// Releases the import gave up on (Exclude) are never planned again: by
// the search's infohash before the list is opened, else by the list's.
func TestPlanTracksExcludesGivenUpReleases(t *testing.T) {
	ctx := context.Background()
	both := []string{"01 - Группа крови.mp3", "02 - Закрой за мной дверь.mp3"}
	e := newEnv(t)
	a := relx("Кино - Группа крови (1988) [MP3 320]", linkA, 1, 90)
	a.InfoHash = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	e.idx.resp = indexer.SearchResponse{Prefs: mp3Prefs(indexer.Bitrate256), Releases: []indexer.ScoredRelease{
		a, relx("Кино - Группа крови (1988) [MP3 320] (2)", linkB, 1, 80), relx("Кино - Группа крови [MP3 256]", linkC, 1, 70)}}
	planRelease(t, e, linkA, "Кино - Группа крови A", both...)
	mfB := planRelease(t, e, linkB, "Кино - Группа крови B", both...)
	planRelease(t, e, linkC, "Кино - Группа крови C", both...)
	req := kinoTracks()
	req.Exclude = []string{strings.ToLower(a.InfoHash), metafileIH(t, mfB)}
	p, err := e.svc.PlanTracks(ctx, req)
	if err != nil || p.ReleaseLink != linkC {
		t.Fatalf("plan %+v %v", p, err)
	}
	for _, d := range e.idx.downloads {
		if strings.HasSuffix(d, linkA) {
			t.Fatalf("the excluded release was opened: %v", e.idx.downloads)
		}
	}
}

// A release given up for being slow (Avoid) is taken again only when no
// other release holds the songs.
func TestPlanTracksAvoidsSlowReleasesUnlessAlone(t *testing.T) {
	ctx := context.Background()
	both := []string{"01 - Группа крови.mp3", "02 - Закрой за мной дверь.mp3"}
	e := newEnv(t)
	a := relx("Кино - Группа крови (1988) [MP3 320]", linkA, 1, 90)
	a.InfoHash = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	e.idx.resp = indexer.SearchResponse{Prefs: mp3Prefs(indexer.Bitrate256), Releases: []indexer.ScoredRelease{
		a, relx("Кино - Группа крови [MP3 256]", linkB, 1, 70)}}
	planRelease(t, e, linkA, "Кино - Группа крови A", both...)
	planRelease(t, e, linkB, "Кино - Группа крови B", both...)
	req := kinoTracks()
	req.Avoid = []string{strings.ToLower(a.InfoHash)}
	if p, err := e.svc.PlanTracks(ctx, req); err != nil || p.ReleaseLink != linkB {
		t.Fatalf("another release: %+v %v", p, err)
	}
	// it alone holds them: taken
	e = newEnv(t)
	e.idx.resp = indexer.SearchResponse{Prefs: mp3Prefs(indexer.Bitrate256), Releases: []indexer.ScoredRelease{
		a, relx("Кино - Ночь [MP3 256]", linkB, 1, 70)}}
	planRelease(t, e, linkA, "Кино - Группа крови A", both...)
	planRelease(t, e, linkB, "Кино - Ночь", "01 - Ночь.mp3")
	if p, err := e.svc.PlanTracks(ctx, req); err != nil || p.ReleaseLink != linkA {
		t.Fatalf("alone: %+v %v", p, err)
	}
}
