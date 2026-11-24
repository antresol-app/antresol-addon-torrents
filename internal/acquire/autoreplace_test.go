package acquire

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/indexer"
	"github.com/antresol-app/antresol-addon-torrents/internal/indexer/cardigann"
	"github.com/antresol-app/antresol-addon-torrents/internal/torrent"
)

// probingEngine is the fake engine with the liveness seam: alive says
// which infohashes a probe finds peers for.
type probingEngine struct {
	*fakeEngine
	mu      sync.Mutex
	alive   map[string]bool
	checked []string       // CheckLiveness calls
	noted   map[string]int // NoteProbe calls
	probed  []string       // ProbeSwarm calls
	// onProbe runs in ProbeSwarm (a seeder connecting meanwhile…)
	onProbe func()
	// rmErr: what RemoveIfIdle fails with before checking
	rmErr  error
	onPeer []func(string)
}

func (p *probingEngine) NoteProbe(ih string, _ torrent.SwarmProbe) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.noted[ih]++
}

func (p *probingEngine) OnPeer(f func(string)) { p.onPeer = append(p.onPeer, f) }

// RemoveIfIdle is the engine's: only a running torrent with no peer and
// no byte, and never its files.
func (p *probingEngine) RemoveIfIdle(ctx context.Context, ih string) error {
	if p.rmErr != nil {
		return p.rmErr
	}
	p.fakeEngine.mu.Lock()
	st, ok := p.fakeEngine.st[ih]
	p.fakeEngine.mu.Unlock()
	if !ok {
		return torrent.ErrNotFound
	}
	if (st.Status != torrent.StatusQueued && st.Status != torrent.StatusDownloading) || st.HadPeers || st.Done > 0 || st.Downloaded > 0 {
		return torrent.ErrNotIdle
	}
	return p.fakeEngine.Remove(ctx, ih, false)
}

func (p *probingEngine) CheckLiveness(ih string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checked = append(p.checked, ih)
}

func (p *probingEngine) ProbeSwarm(_ context.Context, tg torrent.ProbeTarget) torrent.SwarmProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probed = append(p.probed, tg.InfoHash)
	if p.onProbe != nil {
		p.onProbe()
	}
	if p.alive[tg.InfoHash] {
		return torrent.SwarmProbe{Peers: 1, Answered: true}
	}
	return torrent.SwarmProbe{Answered: true}
}

func withProbe(e *env) *probingEngine {
	p := &probingEngine{fakeEngine: e.eng, alive: map[string]bool{}, noted: map[string]int{}}
	e.svc.o.Engine = p
	e.svc.watchPeers()
	return p
}

func ihOf(c byte) string { return strings.Repeat(string(c), 40) }

func magnetOf(ih string) string {
	return "magnet:?xt=urn:btih:" + ih + "&tr=udp%3A%2F%2Ftracker.example%3A6969%2Fannounce"
}

func magnetRel(title, ih string, score int) indexer.ScoredRelease {
	return indexer.ScoredRelease{Result: cardigann.Result{Title: title, Magnet: magnetOf(ih), Seeders: 30}, SourceID: 1, SourceName: "src", Score: score}
}

// startDead starts a download of the release ih (the best of the search)
// that never gets a peer.
func startDead(t *testing.T, e *env, ih string, req AcquireRequest) {
	t.Helper()
	e.eng.nextIH = ih
	if _, err := e.svc.Acquire(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	e.eng.mu.Lock()
	st := e.eng.st[ih]
	st.Stalled = torrent.StalledNoPeers
	e.eng.st[ih] = st
	e.eng.mu.Unlock()
}

var dune = AcquireRequest{TMDBID: 693134, Kind: KindMovie, Mode: ModeDownload, UserID: 1}

func shortWindow(t *testing.T) {
	old := autoReplaceAfter
	autoReplaceAfter = time.Minute
	t.Cleanup(func() { autoReplaceAfter = old })
}

// checkAt runs the switch's check at now and waits for any switch it starts.
func checkAt(e *env, now time.Time) {
	e.svc.checkAutoReplace(context.Background(), now)
	e.svc.wait()
}

func TestAcquireProbesPickedRelease(t *testing.T) {
	e := newEnv(t)
	p := withProbe(e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{magnetRel("Дюна 1080p", ihOf('a'), 50)}}
	startDead(t, e, ihOf('a'), dune)
	if len(p.checked) != 1 || p.checked[0] != ihOf('a') {
		t.Fatalf("liveness checks %v", p.checked)
	}
	tried, n, err := e.svc.releaseAttempts(context.Background(), "movie:693134")
	if err != nil || !tried[ihOf('a')] || n != 0 {
		t.Fatalf("attempts %v %d %v", tried, n, err)
	}
}

// A download without a peer or a byte for the window is switched to the
// best live release not tried yet; the dead one goes with its data, and
// the new one carries the note.
func TestAutoReplaceSwitchesToLiveRelease(t *testing.T) {
	shortWindow(t)
	e := newEnv(t)
	p := withProbe(e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{
		magnetRel("Дюна 2160p", ihOf('a'), 90), // the dead one
		magnetRel("Дюна 1080p", ihOf('b'), 80), // dead too
		magnetRel("Дюна 720p", ihOf('c'), 70),  // alive
	}}
	startDead(t, e, ihOf('a'), dune)
	p.alive[ihOf('c')] = true
	t0 := time.Now()
	checkAt(e, t0)
	checkAt(e, t0.Add(30*time.Second))
	if e.eng.removed[ihOf('a')] {
		t.Fatal("switched before the window")
	}
	e.eng.nextIH = ihOf('c')
	checkAt(e, t0.Add(time.Minute+time.Second))
	if !e.eng.removed[ihOf('a')] {
		t.Fatal("the dead download stayed")
	}
	if e.eng.removedData[ihOf('a')] {
		t.Fatal("the dead download's files were deleted (it has no byte; its name may be another folder's)")
	}
	// the candidate's probe is the new download's liveness: no second probe
	if len(p.checked) != 1 || p.noted[ihOf('c')] != 1 {
		t.Fatalf("liveness checks %v, noted %v", p.checked, p.noted)
	}
	if _, ok := e.eng.Get(ihOf('c')); !ok {
		t.Fatal("no new download")
	}
	if strings.Join(p.probed, ",") != ihOf('b')+","+ihOf('c') && strings.Join(p.probed, ",") != ihOf('c')+","+ihOf('b') {
		t.Fatalf("probed %v, want the two untried releases", p.probed)
	}
	var view *TorrentView
	for _, v := range e.svc.Torrents() {
		if v.InfoHash == ihOf('c') {
			view = &v
		}
	}
	if view == nil || view.AutoReplace != AutoSwitched || view.TmdbID != 693134 {
		t.Fatalf("view %+v, want the switched note", view)
	}
	a, _ := e.svc.acquisition(context.Background(), ihOf('c'))
	if a.AddedBy != 1 || a.Kind != KindMovie {
		t.Fatalf("acquisition %+v, want the profile and title of the dead one", a)
	}
	tried, n, _ := e.svc.releaseAttempts(context.Background(), "movie:693134")
	if !tried[ihOf('a')] || !tried[ihOf('c')] || n != 1 {
		t.Fatalf("attempts %v %d", tried, n)
	}
	// dismissing hides the note
	if err := e.svc.DismissAutoNote(context.Background(), ihOf('c')); err != nil {
		t.Fatal(err)
	}
	for _, v := range e.svc.Torrents() {
		if v.InfoHash == ihOf('c') && v.AutoReplace != "" {
			t.Fatalf("note %q after dismiss", v.AutoReplace)
		}
	}
}

// A download that ever had a peer, or has a byte, is never switched; nor
// is a paused one, and a pause restarts the window.
func TestAutoReplaceSparesHeadway(t *testing.T) {
	cases := []struct {
		name string
		mod  func(*torrent.Status)
	}{
		{"had a peer", func(s *torrent.Status) { s.HadPeers = true }},
		{"a peer now", func(s *torrent.Status) { s.Peers = 1; s.Stalled = "" }},
		{"bytes", func(s *torrent.Status) { s.Done = 10 }},
		{"paused", func(s *torrent.Status) { s.Status = torrent.StatusPaused }},
		{"not stalled", func(s *torrent.Status) { s.Stalled = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			shortWindow(t)
			e := newEnv(t)
			p := withProbe(e)
			e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{magnetRel("Дюна 2160p", ihOf('a'), 90), magnetRel("Дюна 720p", ihOf('c'), 70)}}
			startDead(t, e, ihOf('a'), dune)
			p.alive[ihOf('c')] = true
			t0 := time.Now()
			checkAt(e, t0)
			e.eng.mu.Lock()
			st := e.eng.st[ihOf('a')]
			c.mod(&st)
			e.eng.st[ihOf('a')] = st
			e.eng.mu.Unlock()
			checkAt(e, t0.Add(time.Hour))
			checkAt(e, t0.Add(2*time.Hour))
			if e.eng.removed[ihOf('a')] || len(p.probed) != 0 {
				t.Fatalf("switched (probed %v)", p.probed)
			}
		})
	}
}

// A peer seen once is remembered across restarts: the download is never
// switched later, even after its peers left.
func TestAutoReplaceRemembersPeers(t *testing.T) {
	shortWindow(t)
	e := newEnv(t)
	p := withProbe(e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{magnetRel("Дюна 2160p", ihOf('a'), 90), magnetRel("Дюна 720p", ihOf('c'), 70)}}
	startDead(t, e, ihOf('a'), dune)
	p.alive[ihOf('c')] = true
	e.eng.mu.Lock()
	st := e.eng.st[ihOf('a')]
	st.HadPeers = true
	e.eng.st[ihOf('a')] = st
	e.eng.mu.Unlock()
	t0 := time.Now()
	checkAt(e, t0)
	// "restart": the engine forgot the peer, a new service the window
	e.eng.mu.Lock()
	st.HadPeers = false
	e.eng.st[ihOf('a')] = st
	e.eng.mu.Unlock()
	e.svc.auto = autoState{}
	checkAt(e, t0.Add(time.Hour))
	checkAt(e, t0.Add(2*time.Hour))
	if e.eng.removed[ihOf('a')] {
		t.Fatal("switched a download that had a peer")
	}
}

// No live alternative: the dead download stays, with the none-alive note,
// and the switch does not try it again.
func TestAutoReplaceNoneAlive(t *testing.T) {
	shortWindow(t)
	e := newEnv(t)
	p := withProbe(e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{magnetRel("Дюна 2160p", ihOf('a'), 90), magnetRel("Дюна 720p", ihOf('c'), 70)}}
	startDead(t, e, ihOf('a'), dune)
	t0 := time.Now()
	checkAt(e, t0)
	checkAt(e, t0.Add(2*time.Minute))
	if e.eng.removed[ihOf('a')] {
		t.Fatal("the dead download was removed")
	}
	var note string
	for _, v := range e.svc.Torrents() {
		if v.InfoHash == ihOf('a') {
			note = v.AutoReplace
		}
	}
	if note != AutoNoneAlive {
		t.Fatalf("note %q", note)
	}
	n := len(p.probed)
	checkAt(e, t0.Add(time.Hour))
	checkAt(e, t0.Add(2*time.Hour))
	if len(p.probed) != n {
		t.Fatal("tried again after none alive")
	}
	if err := e.svc.DismissAutoNote(context.Background(), ihOf('a')); err != nil {
		t.Fatal(err)
	}
	for _, v := range e.svc.Torrents() {
		if v.InfoHash == ihOf('a') && v.AutoReplace != "" {
			t.Fatalf("note %q after dismiss", v.AutoReplace)
		}
	}
}

// At most maxAutoSwitches switches per title: then the dead one stays.
func TestAutoReplaceCap(t *testing.T) {
	shortWindow(t)
	e := newEnv(t)
	p := withProbe(e)
	rels := []indexer.ScoredRelease{magnetRel("r0", ihOf('0'), 100)}
	for i, c := range "12345" {
		rels = append(rels, magnetRel("r"+string(c), ihOf(byte(c)), 90-i))
		p.alive[ihOf(byte(c))] = true
	}
	e.idx.resp = indexer.SearchResponse{Releases: rels}
	startDead(t, e, ihOf('0'), dune)
	t0 := time.Now()
	cur := ihOf('0')
	for i, c := range "123" {
		at := t0.Add(time.Duration(i) * time.Hour)
		checkAt(e, at)
		e.eng.nextIH = ihOf(byte(c))
		checkAt(e, at.Add(2*time.Minute))
		if !e.eng.removed[cur] {
			t.Fatalf("switch %d did not happen", i+1)
		}
		cur = ihOf(byte(c))
		e.eng.mu.Lock()
		st := e.eng.st[cur]
		st.Stalled = torrent.StalledNoPeers
		e.eng.st[cur] = st
		e.eng.mu.Unlock()
	}
	at := t0.Add(5 * time.Hour)
	checkAt(e, at)
	e.eng.nextIH = ihOf('4')
	checkAt(e, at.Add(2*time.Minute))
	if e.eng.removed[cur] {
		t.Fatal("a fourth switch")
	}
	if a, _ := e.svc.acquisition(context.Background(), cur); a.AutoNote != AutoNoneAlive {
		t.Fatalf("note %q at the cap", a.AutoNote)
	}
}

// Music: the album is searched again by its artist and title, and a
// release behind a metafile link is fetched once and started from it.
func TestAutoReplaceAlbum(t *testing.T) {
	shortWindow(t)
	e := newEnv(t)
	p := withProbe(e)
	mf := testMetafile(t, "Кино - Группа крови (1988) FLAC", "01 - Группа крови.flac")
	live := metafileIH(t, mf)
	e.idx.byLink = map[string]cardigann.DownloadResult{"https://t/live": {Torrent: mf}}
	dead := relx("Кино - Группа крови (1988) FLAC", "", 1, 90)
	dead.Magnet = magnetOf(ihOf('a'))
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{dead, relx("Кино - Группа крови (1988) [FLAC]", "https://t/live", 2, 80)}}
	p.alive[live] = true
	startDead(t, e, ihOf('a'), AcquireRequest{Kind: KindMusic, Title: "Группа крови", Artist: "Кино", Year: 1988, ExtID: "rg-1", Mode: ModeDownload, UserID: 2})
	t0 := time.Now()
	checkAt(e, t0)
	e.eng.nextIH = live
	checkAt(e, t0.Add(2*time.Minute))
	if !e.eng.removed[ihOf('a')] {
		t.Fatal("not switched")
	}
	q := e.idx.requests[len(e.idx.requests)-1]
	if q.Kind != KindMusic || !strings.Contains(q.Text, "Группа крови") {
		t.Fatalf("search %+v", q)
	}
	n := 0
	for _, d := range e.idx.downloads {
		if d == "2 https://t/live" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("downloads %v, want the metafile fetched once", e.idx.downloads)
	}
	a, _ := e.svc.acquisition(context.Background(), live)
	if a.Meta.Artist != "Кино" || a.ExtID != "rg-1" || a.AddedBy != 2 || a.Meta.HandPicked {
		t.Fatalf("acquisition %+v", a)
	}
}

// The title page and the pending card get the liveness and the tracker's
// refusal: unseen only when every active torrent is, the refusal only
// with the stall.
func TestTitleLivenessAndTrackerError(t *testing.T) {
	type tor struct {
		ih, liveness, stalled, kind string
	}
	cases := []struct {
		name               string
		tors               []tor
		liveness, wantKind string
	}{
		{"unseen, refused", []tor{{"a", torrent.LivenessUnseen, torrent.StalledNoPeers, torrent.TrackerBadPasskey}}, torrent.LivenessUnseen, torrent.TrackerBadPasskey},
		{"checking", []tor{{"a", torrent.LivenessChecking, "", ""}}, torrent.LivenessChecking, ""},
		{"one alive", []tor{{"a", torrent.LivenessUnseen, torrent.StalledNoPeers, ""}, {"b", torrent.LivenessAlive, "", ""}}, "", ""},
		{"refusal without a stall", []tor{{"a", torrent.LivenessUnseen, "", torrent.TrackerNotRegistered}}, torrent.LivenessUnseen, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			for i, x := range c.tors {
				e.eng.put(torrent.Status{InfoHash: x.ih, Mode: torrent.ModeDownload, Status: torrent.StatusDownloading, Liveness: x.liveness,
					Stalled: x.stalled, TrackerError: "raw", TrackerErrorKind: x.kind, Size: 100, AddedAt: time.Now()})
				if _, err := e.svc.o.DB.Exec(`INSERT INTO acquisitions (infohash, kind, tmdb_id, title, season) VALUES (?,?,?,?,?)`,
					x.ih, "series", 5, "T", i+1); err != nil {
					t.Fatal(err)
				}
			}
			p, ok := e.svc.TitleProgress(context.Background(), "series", 5, 0)
			if !ok || p.Liveness != c.liveness || p.TrackerErrorKind != c.wantKind {
				t.Fatalf("progress %+v, want liveness %q kind %q", p, c.liveness, c.wantKind)
			}
			ds := e.svc.LibraryDownloads(context.Background())
			if len(ds) != 1 || ds[0].Liveness != c.liveness || ds[0].TrackerErrorKind != c.wantKind {
				t.Fatalf("downloads %+v", ds)
			}
		})
	}
}

// A switch fetches metafiles one at a time, only for the release whose turn
// it is, stops at the first live one, and looks at no more than
// probeCandidates releases: indexers and trackers see a few requests.
func TestAutoReplaceProbesLazily(t *testing.T) {
	for _, c := range []struct {
		name               string
		live               int // index of the live release (-1: none)
		wantFetch, wantPrb int
	}{
		{"second is live", 1, 2, 2},
		{"none live", -1, probeCandidates, probeCandidates},
	} {
		t.Run(c.name, func(t *testing.T) {
			shortWindow(t)
			e := newEnv(t)
			p := withProbe(e)
			rels := []indexer.ScoredRelease{magnetRel("dead", ihOf('a'), 100)}
			e.idx.byLink = map[string]cardigann.DownloadResult{}
			for i := range 5 {
				link := "https://t/" + itoa(int64(i))
				ih := ihOf(byte('b' + i))
				e.idx.byLink[link] = cardigann.DownloadResult{Magnet: magnetOf(ih)}
				rels = append(rels, relx("r"+itoa(int64(i)), link, 1, 90-i))
				if i == c.live {
					p.alive[ih] = true
					e.eng.nextIH = ih
				}
			}
			e.idx.resp = indexer.SearchResponse{Releases: rels}
			e.eng.nextIH = ihOf('a')
			startDead(t, e, ihOf('a'), dune)
			if c.live >= 0 {
				e.eng.nextIH = ihOf(byte('b' + c.live))
			}
			e.idx.downloads = nil
			t0 := time.Now()
			checkAt(e, t0)
			checkAt(e, t0.Add(2*time.Minute))
			fetched := 0
			for _, d := range e.idx.downloads {
				if strings.HasPrefix(d, "1 https://t/") {
					fetched++
				}
			}
			// a switch to a release behind a link reuses the fetched metafile
			if fetched != c.wantFetch || len(p.probed) != c.wantPrb {
				t.Fatalf("fetched %d (%v), probed %d, want %d and %d", fetched, e.idx.downloads, len(p.probed), c.wantFetch, c.wantPrb)
			}
			if got := e.eng.removed[ihOf('a')]; got != (c.live >= 0) {
				t.Fatalf("dead removed %v", got)
			}
		})
	}
}

func setStatus(e *env, ih string, f func(*torrent.Status)) {
	e.eng.mu.Lock()
	defer e.eng.mu.Unlock()
	st := e.eng.st[ih]
	f(&st)
	e.eng.st[ih] = st
}

func noteOf(t *testing.T, e *env, ih string) string {
	t.Helper()
	a, err := e.svc.acquisition(context.Background(), ih)
	if err != nil {
		t.Fatal(err)
	}
	return a.AutoNote
}

// A seeder that connects to the old download while the switch runs keeps
// it: it is not removed (the new one stays too), and not switched again.
func TestAutoReplaceKeepsOldThatCameAlive(t *testing.T) {
	shortWindow(t)
	e := newEnv(t)
	p := withProbe(e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{magnetRel("Дюна 2160p", ihOf('a'), 90), magnetRel("Дюна 720p", ihOf('c'), 70)}}
	startDead(t, e, ihOf('a'), dune)
	p.alive[ihOf('c')] = true
	t0 := time.Now()
	checkAt(e, t0)
	e.eng.nextIH = ihOf('c')
	p.onProbe = func() { setStatus(e, ihOf('a'), func(s *torrent.Status) { s.HadPeers = true }) }
	checkAt(e, t0.Add(2*time.Minute))
	if e.eng.removed[ihOf('a')] {
		t.Fatal("removed a download that got a peer during the switch")
	}
	if _, ok := e.eng.Get(ihOf('c')); !ok {
		t.Fatal("the new download is gone")
	}
	if n := noteOf(t, e, ihOf('a')); n != autoQuiet {
		t.Fatalf("old note %q", n)
	}
}

// A removal that fails after the new download started leaves the old one
// noted: it is never switched again (the cap is not burnt).
func TestAutoReplaceFailedRemovalNotSwitchedAgain(t *testing.T) {
	shortWindow(t)
	e := newEnv(t)
	p := withProbe(e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{magnetRel("a", ihOf('a'), 90), magnetRel("c", ihOf('c'), 70), magnetRel("d", ihOf('d'), 60)}}
	startDead(t, e, ihOf('a'), dune)
	p.alive[ihOf('c')], p.alive[ihOf('d')] = true, true
	p.rmErr = errors.New("disk busy")
	t0 := time.Now()
	checkAt(e, t0)
	e.eng.nextIH = ihOf('c')
	checkAt(e, t0.Add(2*time.Minute))
	if n := noteOf(t, e, ihOf('a')); n != autoReplaced {
		t.Fatalf("old note %q", n)
	}
	n := len(p.probed)
	e.eng.nextIH = ihOf('d')
	checkAt(e, t0.Add(time.Hour))
	checkAt(e, t0.Add(2*time.Hour))
	if len(p.probed) != n {
		t.Fatal("the old download was switched again")
	}
}

// A switch that fails (here the search) backs off, persisted, and counts
// toward the title's switches: no endless searches.
func TestAutoReplaceFailureBacksOff(t *testing.T) {
	shortWindow(t)
	e := newEnv(t)
	withProbe(e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{magnetRel("a", ihOf('a'), 90)}}
	startDead(t, e, ihOf('a'), dune)
	e.idx.err = errors.New("indexer down")
	searches := func() int { e.idx.mu.Lock(); defer e.idx.mu.Unlock(); return len(e.idx.requests) }
	base := searches()
	t0 := time.Now()
	checkAt(e, t0)
	checkAt(e, t0.Add(2*time.Minute)) // fails: 1, next in 30 min
	if searches() != base+1 {
		t.Fatalf("searches %d", searches()-base)
	}
	checkAt(e, t0.Add(15*time.Minute))
	if searches() != base+1 {
		t.Fatal("searched again during the backoff")
	}
	if n, next := e.svc.switchFailures(context.Background(), "movie:693134"); n != 1 || !next.After(t0.Add(30*time.Minute)) {
		t.Fatalf("failures %d, next %v", n, next)
	}
	checkAt(e, t0.Add(33*time.Minute)) // fails: 2, next in 60 min
	checkAt(e, t0.Add(2*time.Hour))    // fails: 3 = the cap
	if searches() != base+3 {
		t.Fatalf("searches %d", searches()-base)
	}
	if n := noteOf(t, e, ihOf('a')); n != AutoNoneAlive {
		t.Fatalf("note %q after the cap", n)
	}
	checkAt(e, t0.Add(30*time.Hour))
	if searches() != base+3 {
		t.Fatal("searched after the cap")
	}
}

// The first peer is recorded the moment it connects, not on the next check.
func TestPeerRecordedAtOnce(t *testing.T) {
	e := newEnv(t)
	p := withProbe(e)
	e.idx.resp = indexer.SearchResponse{Releases: []indexer.ScoredRelease{magnetRel("a", ihOf('a'), 90)}}
	startDead(t, e, ihOf('a'), dune)
	for _, f := range p.onPeer {
		f(ihOf('a'))
	}
	if a, _ := e.svc.acquisition(context.Background(), ihOf('a')); !a.HadPeers {
		t.Fatal("had_peers not saved")
	}
}

// The searches nobody waits on are background searches (indexer.Background:
// a source paused for a failed FlareSolverr is left alone for them): the
// automatic switch of a dead download, and a request that says no one waits
// on its answer (the follow's, the channel fetch's). A person's request is not.
func TestSearchesNobodyWaitsOnAreBackground(t *testing.T) {
	shortWindow(t)
	rels := indexer.SearchResponse{Releases: []indexer.ScoredRelease{
		magnetRel("Дюна 2160p", ihOf('a'), 90),
		magnetRel("Дюна 720p", ihOf('c'), 70),
	}}

	// a person's request: a plain context; a background one: marked
	for _, bg := range []bool{false, true} {
		e := newEnv(t)
		e.idx.resp = rels
		e.eng.nextIH = ihOf('a')
		req := dune
		req.Background = bg
		if _, err := e.svc.Acquire(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if len(e.idx.bg) != 1 || e.idx.bg[0] != bg {
			t.Fatalf("request with Background=%v: searched on a background context: %v", bg, e.idx.bg)
		}
	}

	// the switch of a download that went quiet: the person's first search,
	// then the switch's own
	e := newEnv(t)
	p := withProbe(e)
	e.idx.resp = rels
	startDead(t, e, ihOf('a'), dune)
	p.alive[ihOf('c')] = true
	t0 := time.Now()
	checkAt(e, t0)
	e.eng.nextIH = ihOf('c')
	checkAt(e, t0.Add(time.Minute+time.Second))
	if _, ok := e.eng.Get(ihOf('c')); !ok {
		t.Fatal("no switch")
	}
	if len(e.idx.bg) != 2 || e.idx.bg[0] || !e.idx.bg[1] {
		t.Fatalf("searches on a background context: %v, want the person's, then the switch's", e.idx.bg)
	}
}
