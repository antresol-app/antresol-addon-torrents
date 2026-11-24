package pinned

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/tracking"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	db "github.com/antresol-app/antresol-addon-torrents/internal/store"
)

// Two trackers, as the tests below set them up.
const (
	trackerA, trackerB = 1, 2
	day                = 24 * time.Hour
)

func relOf(source int64, title, link string) indexer.ScoredRelease {
	return indexer.ScoredRelease{Result: cardigann.Result{Title: title, Link: link}, SourceID: source,
		SourceName: "Tracker " + strconv.FormatInt(source, 10), Score: 1}
}

// round: the core's refresh of a title at now. The scanner is asked, its
// rows replace the title's torrent rows (the fake core stamps them), and
// the rows that came out new to the title come back: those it would tell
// the profiles about.
func (e *env) round(t *testing.T, sc *torrentScanner, ti tracking.Title, eps []tracking.Episode, now time.Time) (got, added []tracking.Source) {
	t.Helper()
	e.now = now
	got, err := sc.Scan(context.Background(), ti, eps)
	if err != nil {
		t.Fatalf("scan at %v: %v", now, err)
	}
	return got, e.track.UpsertSources(ti.ID, "torrent", got, now)
}

// answer says which trackers answer the next search with the releases rs
// and which fail (down); the others are not asked.
func (f *fakeSearcher) answer(rs []indexer.ScoredRelease, answered []int64, down ...int64) {
	f.releases, f.answered, f.answeredIDs, f.failed = rs, len(answered), answered, nil
	for _, id := range down {
		f.failed = append(f.failed, indexer.SourceError{SourceID: id, Name: "Tracker " + strconv.FormatInt(id, 10), Message: "blocked"})
	}
}

// rowsOf are the title's torrent rows by episode and ref, with their
// first seen.
func (e *env) rowsOf(ti tracking.Title) map[string]time.Time {
	out := map[string]time.Time{}
	for _, x := range ofKind(e.track.SourcesOf(ti.ID), "torrent") {
		out[fmt.Sprintf("S%dE%d %s", x.Season, x.Episode, x.Ref)] = x.FirstSeen
	}
	return out
}

func keys(m map[string]time.Time) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// reopen is a restart of the addon: its database is closed and opened
// again, and the scanner is a new one.
func (e *env) reopen(t *testing.T) {
	t.Helper()
	if err := e.svc.DB.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(e.dbFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	e.svc.DB = d
}

func wantRows(t *testing.T, what string, got map[string]time.Time, want ...string) {
	t.Helper()
	slices.Sort(want)
	if g := keys(got); !slices.Equal(g, want) {
		t.Fatalf("%s: rows\n got %v\nwant %v", what, g, want)
	}
}

// A source that fails while another answers keeps the rows it gave: the
// core replaces the title's rows with whatever a scan returns, so without
// them its episodes would lose their source, and be new again (a new first
// seen: «Вышло новое», the letters) when the source answers again. The
// answering source's rows are replaced by what it found, and a source that
// came back replaces its own.
func TestTorrentScannerKeepsTheRowsOfASourceThatFailed(t *testing.T) {
	e := newEnv(t)
	ti := tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}
	eps := season1(2)
	f := &fakeSearcher{}
	sc := newTorrentScanner(e, f, 0)
	t0 := e.now

	// both answer: A has episode 1, B has episode 2 (a baseline)
	f.answer([]indexer.ScoredRelease{
		relOf(trackerA, "Show.S01E01.1080p-GRP", "https://a.test/dl/1"),
		relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2"),
	}, []int64{trackerA, trackerB})
	_, added := e.round(t, sc, ti, eps, t0)
	if len(added) != 2 {
		t.Fatalf("first round added %v, want both episodes", added)
	}
	wantRows(t, "first round", e.rowsOf(ti), "S1E1 https://a.test/dl/1", "S1E2 https://b.test/dl/2")

	// A is down, B answers (with a new release of its own)
	f.answer([]indexer.ScoredRelease{relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2-fresh")}, []int64{trackerB}, trackerA)
	got, added := e.round(t, sc, ti, eps, t0.Add(25*time.Hour))
	if len(added) != 0 {
		t.Fatalf("A down: added %v, want nothing new (B's release replaces its own row of episode 2)", added)
	}
	if len(got) != 2 {
		t.Fatalf("A down: scan returned %v, want B's release and A's earlier one", srcKeys(got))
	}
	rows := e.rowsOf(ti)
	wantRows(t, "A down", rows, "S1E1 https://a.test/dl/1", "S1E2 https://b.test/dl/2-fresh")
	if first := rows["S1E1 https://a.test/dl/1"]; !first.Equal(t0) {
		t.Fatalf("A's episode first seen %v after the outage, want %v", first, t0)
	}

	// A is back, its release under a new link: A's rows are replaced, the
	// episode is not new
	f.answer([]indexer.ScoredRelease{
		relOf(trackerA, "Show.S01E01.1080p-GRP", "https://a.test/dl/1-fresh"),
		relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2-fresh"),
	}, []int64{trackerA, trackerB})
	_, added = e.round(t, sc, ti, eps, t0.Add(50*time.Hour))
	if len(added) != 0 {
		t.Fatalf("A back: added %v, want nothing new (it was told of at first)", added)
	}
	rows = e.rowsOf(ti)
	wantRows(t, "A back", rows, "S1E1 https://a.test/dl/1-fresh", "S1E2 https://b.test/dl/2-fresh")
	if first := rows["S1E1 https://a.test/dl/1-fresh"]; !first.Equal(t0) {
		t.Fatalf("A's episode first seen %v after it came back, want %v: it is no news", first, t0)
	}

	// and an answering source's rows go when it no longer lists them
	f.answer([]indexer.ScoredRelease{relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2-fresh")}, []int64{trackerA, trackerB})
	e.round(t, sc, ti, eps, t0.Add(75*time.Hour))
	wantRows(t, "A answered with nothing", e.rowsOf(ti), "S1E2 https://b.test/dl/2-fresh")
}

// What a source last answered is kept in the addon's database: after a
// restart a source that is still down keeps its rows for a week since it
// last answered, not for a week since the rows were last handed to the
// core (which stamps them with each scan's time), and then they go.
func TestTorrentScannerKeptRowsExpireAWeekAfterTheLastAnswer(t *testing.T) {
	e := newEnv(t)
	ti := tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}
	eps := season1(2)
	f := &fakeSearcher{}
	t0 := e.now

	f.answer([]indexer.ScoredRelease{
		relOf(trackerA, "Show.S01E01.1080p-GRP", "https://a.test/dl/1"),
		relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2"),
	}, []int64{trackerA, trackerB})
	e.round(t, newTorrentScanner(e, f, 0), ti, eps, t0)

	bOnly := []indexer.ScoredRelease{relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2")}
	f.answer(bOnly, []int64{trackerB}, trackerA)
	e.round(t, newTorrentScanner(e, f, 0), ti, eps, t0.Add(25*time.Hour))

	e.reopen(t) // the addon restarts

	// six days on: A is still down, and its row is still there
	sc := newTorrentScanner(e, f, 0)
	e.round(t, sc, ti, eps, t0.Add(6*day))
	wantRows(t, "six days", e.rowsOf(ti), "S1E1 https://a.test/dl/1", "S1E2 https://b.test/dl/2")

	// a week since A answered: its row goes (the row was handed to the core
	// the day before, but that is not an answer)
	e.round(t, sc, ti, eps, t0.Add(7*day+time.Hour))
	wantRows(t, "a week", e.rowsOf(ti), "S1E2 https://b.test/dl/2")

	// and stays gone while A is down (nothing of it left to keep)
	e.round(t, sc, ti, eps, t0.Add(9*day))
	wantRows(t, "later", e.rowsOf(ti), "S1E2 https://b.test/dl/2")
}

// Rows of a source from before its answers were recorded (the addon was
// updated since) are kept by their last_seen, the last time they were in an
// answer of it.
func TestTorrentScannerKeepsRowsFromBeforeTheRecord(t *testing.T) {
	for _, c := range []struct {
		age  time.Duration
		kept bool
	}{{30 * time.Hour, true}, {6 * day, true}, {8 * day, false}} {
		t.Run(c.age.String(), func(t *testing.T) {
			e := newEnv(t)
			ti := tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}
			f := &fakeSearcher{}
			rowA := tracking.Source{Season: 1, Episode: 1, Ref: "https://a.test/dl/1", Provider: "Tracker 1", Meta: tracking.SourceMeta{SourceID: "1"}}
			e.track.UpsertSources(ti.ID, "torrent", []tracking.Source{rowA}, e.now.Add(-c.age))

			f.answer([]indexer.ScoredRelease{relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2")}, []int64{trackerB}, trackerA)
			e.round(t, newTorrentScanner(e, f, 0), ti, season1(2), e.now)
			want := []string{"S1E2 https://b.test/dl/2"}
			if c.kept {
				want = append(want, "S1E1 https://a.test/dl/1")
			}
			wantRows(t, "after the update", e.rowsOf(ti), want...)

			// the rows' age is counted once: the scan stamped the kept row
			// with its own time, which must not start the week anew
			if c.kept {
				e.round(t, newTorrentScanner(e, f, 0), ti, season1(2), e.now.Add(8*day-c.age))
				wantRows(t, "a week after the last sight", e.rowsOf(ti), "S1E2 https://b.test/dl/2")
			}
		})
	}
}

// A source that took no part in the scan (switched off or removed) is not
// kept: its rows go, as before, and so does what the store holds of it.
func TestTorrentScannerDropsTheRowsOfASourceNotAsked(t *testing.T) {
	e := newEnv(t)
	ti := tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}
	f := &fakeSearcher{}
	sc := newTorrentScanner(e, f, 0)
	t0 := e.now

	f.answer([]indexer.ScoredRelease{
		relOf(trackerA, "Show.S01E01.1080p-GRP", "https://a.test/dl/1"),
		relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2"),
	}, []int64{trackerA, trackerB})
	e.round(t, sc, ti, season1(2), t0)
	count := func() (n int) {
		if err := e.svc.DB.QueryRow(`SELECT COUNT(*) FROM torrent_scan_answers WHERE title_id=?`, ti.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(); n != 2 {
		t.Fatalf("%d answers recorded, want 2", n)
	}

	// A is switched off: neither answered nor failed
	f.answer([]indexer.ScoredRelease{relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2")}, []int64{trackerB})
	e.round(t, sc, ti, season1(2), t0.Add(25*time.Hour))
	wantRows(t, "A switched off", e.rowsOf(ti), "S1E2 https://b.test/dl/2")
	if n := count(); n != 1 {
		t.Fatalf("%d answers recorded after A was switched off, want 1", n)
	}
}

// A film's rows are kept in the same way (it has no other season to keep).
func TestTorrentScannerKeepsTheRowsOfAFilmFromASourceThatFailed(t *testing.T) {
	e := newEnv(t)
	film, err := e.track.EnsureTitle(context.Background(), "movie", 300, "Film", "")
	if err != nil {
		t.Fatal(err)
	}
	ti := tracking.Title{ID: film.ID, Kind: "movie", Name: "Film"}
	f := &fakeSearcher{}
	sc := newTorrentScanner(e, f, 0)
	t0 := e.now

	f.answer([]indexer.ScoredRelease{
		relOf(trackerA, "Film.2024.1080p.WEB-DL-GRP", "https://a.test/dl/f"),
		relOf(trackerB, "Film.2024.2160p.WEB-DL-GRP", "https://b.test/dl/f"),
	}, []int64{trackerA, trackerB})
	e.round(t, sc, ti, nil, t0)

	f.answer([]indexer.ScoredRelease{relOf(trackerB, "Film.2024.2160p.WEB-DL-GRP", "https://b.test/dl/f")}, []int64{trackerB}, trackerA)
	e.round(t, sc, ti, nil, t0.Add(25*time.Hour))
	wantRows(t, "A down", e.rowsOf(ti), "S0E0 https://a.test/dl/f", "S0E0 https://b.test/dl/f")
}

// A tracker reached by a backup way in (RuTracker by cookie) is one
// source to the user: a release of it names either way, so the rows of the
// way that answered last time are kept when the tracker fails, whichever
// way is named as failed, and one record is kept for it, by the source the
// user set up.
func TestTorrentScannerKeepsTheRowsOfATrackerReachedByItsBackup(t *testing.T) {
	const primary, backup = 5, 15
	e := newEnv(t)
	ti := tracking.Title{ID: e.title.ID, Kind: "series", Name: "Show"}
	f := &fakeSearcher{}
	sc := newTorrentScanner(e, f, 0)
	t0 := e.now

	// the backup answered for the primary (releases of the backup's id, the
	// answer is the primary's)
	f.answer([]indexer.ScoredRelease{
		relOf(backup, "Show.S01E01.1080p-GRP", "https://rt.test/dl/1"),
		relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2"),
	}, []int64{primary, trackerB})
	e.round(t, sc, ti, season1(2), t0)

	for i, named := range []int64{primary, backup} { // the way that failed is named either
		f.answer([]indexer.ScoredRelease{relOf(trackerB, "Show.S01E02.1080p-GRP", "https://b.test/dl/2")}, []int64{trackerB})
		f.failed = []indexer.SourceError{{SourceID: named, Name: "RuTracker", Message: "blocked", Stands: []int64{primary, backup}}}
		e.round(t, sc, ti, season1(2), t0.Add(time.Duration(25*(i+1))*time.Hour))
		wantRows(t, fmt.Sprintf("%d named", named), e.rowsOf(ti), "S1E1 https://rt.test/dl/1", "S1E2 https://b.test/dl/2")
	}
	var n int
	if err := e.svc.DB.QueryRow(`SELECT COUNT(*) FROM torrent_scan_answers WHERE title_id=? AND source_id IN (?, ?)`, ti.ID, primary, backup).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d records for the tracker, %v; want one", n, err)
	}
}
