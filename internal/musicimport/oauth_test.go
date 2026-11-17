//go:build ignore

package musicimport

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/auth"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
)

func TestPKCEChallenge(t *testing.T) {
	// RFC 7636, appendix B (the brief misquotes the challenge's tail; this is the RFC's)
	if got := Challenge("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatalf("challenge %q", got)
	}
	v := newVerifier()
	if len(v) < 43 || len(v) > 128 || strings.ContainsAny(v, "+/=") {
		t.Fatalf("verifier %q", v)
	}
	if newVerifier() == v {
		t.Fatal("verifiers repeat")
	}
}

// fakeConn is a service that signs in and reads a fixed library.
type fakeConn struct {
	mu        sync.Mutex
	exchanges []string // code|verifier|redirect
	readTok   Token
	exErr     error
	readErr   error
	snap      playlistimport.Snapshot
}

func (c *fakeConn) AuthorizeURL(redirectURI, state, challenge string) string {
	return "https://accounts.example/authorize?" + url.Values{"redirect_uri": {redirectURI}, "state": {state},
		"code_challenge": {challenge}}.Encode()
}

func (c *fakeConn) Exchange(_ context.Context, redirectURI, code, verifier string) (Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.exchanges = append(c.exchanges, code+"|"+verifier+"|"+redirectURI)
	if c.exErr != nil {
		return Token{}, c.exErr
	}
	return Token{Access: "access-" + code, Refresh: "refresh"}, nil
}

func (c *fakeConn) Read(_ context.Context, tok Token, progress func(done, total int)) (playlistimport.Snapshot, error) {
	c.mu.Lock()
	c.readTok = tok
	c.mu.Unlock()
	if progress != nil {
		progress(1, 1)
	}
	return c.snap, c.readErr
}

func newTestOAuth(t *testing.T, public string) (*OAuth, *fakeConn, *time.Time, int64) {
	t.Helper()
	st, uid := newTestStore(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	conn := &fakeConn{snap: playlistimport.Snapshot{Service: playlistimport.ServiceSpotify, Account: "acc", Tracks: []playlistimport.SnapTrack{{Key: "spotify:1", Artist: "Кино", Title: "Кукушка", Liked: true}}}}
	o := &OAuth{Svc: &playlistimport.Service{Store: st}, Vault: testVault(t), Connectors: map[string]Connector{playlistimport.ServiceSpotify: conn},
		PublicURL: public, Now: func() time.Time { return now }}
	t.Cleanup(o.Wait)
	return o, conn, &now, uid
}

func stateOf(t *testing.T, b BeginDTO) string {
	t.Helper()
	u, err := url.Parse(b.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("state")
}

func TestOAuthBeginModes(t *testing.T) {
	o, _, _, uid := newTestOAuth(t, "https://vault.example.org")
	b, err := o.Begin(uid, playlistimport.ServiceSpotify)
	if err != nil || b.Mode != ModeRedirect || b.RedirectURI != "https://vault.example.org/api/import/oauth/spotify/callback" {
		t.Fatalf("%+v %v", b, err)
	}
	u, _ := url.Parse(b.AuthorizeURL)
	if u.Query().Get("redirect_uri") != b.RedirectURI || len(stateOf(t, b)) < 32 || u.Query().Get("code_challenge") == "" {
		t.Fatalf("authorize %s", b.AuthorizeURL)
	}
	for _, public := range []string{"", "http://192.168.1.5:8080"} {
		o.PublicURL = public
		b, err := o.Begin(uid, playlistimport.ServiceSpotify)
		if err != nil || b.Mode != ModePaste || b.RedirectURI != "http://127.0.0.1:8765/vault-import/spotify" {
			t.Fatalf("%q: %+v %v", public, b, err)
		}
	}
	if _, err := o.Begin(uid, "napster"); !errors.Is(err, playlistimport.ErrUnknownService) {
		t.Fatalf("unknown %v", err)
	}
	if _, err := o.Begin(uid, playlistimport.ServiceDeezer); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("not configured %v", err)
	}
	if o.Configured(playlistimport.ServiceDeezer) || !o.Configured(playlistimport.ServiceSpotify) {
		t.Fatal("configured")
	}
}

func TestOAuthStateExpired(t *testing.T) {
	ctx := context.Background()
	o, conn, now, uid := newTestOAuth(t, "https://vault.example.org")
	b, err := o.Begin(uid, playlistimport.ServiceSpotify)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(11 * time.Minute)
	if _, err := o.Finish(ctx, uid, stateOf(t, b), "code"); !errors.Is(err, ErrLinkExpired) || err.Error() != "Ссылка устарела. Подключитесь заново." {
		t.Fatalf("expired: %v", err)
	}

	// another admin's state: the same answer, and the owner can still use it
	b, _ = o.Begin(uid, playlistimport.ServiceSpotify)
	if _, err := o.Finish(ctx, uid+1, stateOf(t, b), "code"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("other admin: %v", err)
	}
	if _, err := o.Finish(ctx, uid, "made-up", "code"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("unknown state: %v", err)
	}
	if len(conn.exchanges) != 0 {
		t.Fatalf("exchanged %v", conn.exchanges)
	}
	id, err := o.Finish(ctx, uid, stateOf(t, b), "code")
	if err != nil || id == 0 {
		t.Fatalf("owner: %d %v", id, err)
	}
	// single use
	if _, err := o.Finish(ctx, uid, stateOf(t, b), "code"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("reuse: %v", err)
	}
	if len(conn.exchanges) != 1 || !strings.HasSuffix(conn.exchanges[0], "|https://vault.example.org/api/import/oauth/spotify/callback") {
		t.Fatalf("exchanges %v", conn.exchanges)
	}
}

func TestOAuthPendingBounded(t *testing.T) {
	ctx := context.Background()
	o, _, _, uid := newTestOAuth(t, "")
	first, _ := o.Begin(uid, playlistimport.ServiceSpotify)
	for range pendingMax {
		if _, err := o.Begin(uid, playlistimport.ServiceSpotify); err != nil {
			t.Fatal(err)
		}
	}
	if n := o.pendingCount(); n != pendingMax {
		t.Fatalf("pending %d", n)
	}
	// the oldest sign-in made room for the newest
	if _, err := o.Finish(ctx, uid, stateOf(t, first), "code"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("oldest: %v", err)
	}
}

func TestOAuthFinishReadsIntoPreview(t *testing.T) {
	ctx := context.Background()
	o, conn, _, uid := newTestOAuth(t, "https://vault.example.org")
	b, _ := o.Begin(uid, playlistimport.ServiceSpotify)
	id, err := o.Finish(ctx, uid, stateOf(t, b), "c1")
	if err != nil {
		t.Fatal(err)
	}
	o.Wait()
	j, err := o.Svc.Store.Job(ctx, id)
	if err != nil || j.Status != playlistimport.StatusPreview || j.Service != playlistimport.ServiceSpotify || j.Account != "acc" {
		t.Fatalf("%+v %v", j, err)
	}
	if conn.readTok.Access != "access-c1" {
		t.Fatalf("read with %+v", conn.readTok)
	}
	if n, _ := o.Vault.Sweep(0); n != 0 {
		t.Fatalf("token left: %d", n)
	}

	// the code is refused: no job, the sign-in is used up
	conn.exErr = errors.New("invalid_grant")
	b, _ = o.Begin(uid, playlistimport.ServiceSpotify)
	if _, err := o.Finish(ctx, uid, stateOf(t, b), "bad"); !errors.Is(err, ErrConnect) {
		t.Fatalf("refused code: %v", err)
	}
	if _, err := o.Finish(ctx, uid, stateOf(t, b), "bad"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("reuse after refusal: %v", err)
	}
}

func TestOAuthFinishPasted(t *testing.T) {
	ctx := context.Background()
	o, conn, _, uid := newTestOAuth(t, "")
	b, _ := o.Begin(uid, playlistimport.ServiceSpotify)
	st := stateOf(t, b)

	// not the address Spotify sends back to: refused, the sign-in stays
	for _, bad := range []string{"https://evil.example/vault-import/spotify?code=x&state=" + st,
		"http://127.0.0.1:8765/other?code=x&state=" + st, "not a url %%", "http://127.0.0.1:8765/vault-import/spotify?code=x"} {
		if _, err := o.FinishPasted(ctx, uid, bad); !errors.Is(err, ErrConnect) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
	if len(conn.exchanges) != 0 {
		t.Fatalf("exchanged %v", conn.exchanges)
	}
	id, err := o.FinishPasted(ctx, uid, "  http://127.0.0.1:8765/vault-import/spotify?code=pasted&state="+st+"  ")
	if err != nil || id == 0 {
		t.Fatalf("%d %v", id, err)
	}
	if len(conn.exchanges) != 1 || !strings.HasPrefix(conn.exchanges[0], "pasted|") {
		t.Fatalf("exchanges %v", conn.exchanges)
	}

	// the listener said no
	b, _ = o.Begin(uid, playlistimport.ServiceSpotify)
	_, err = o.FinishPasted(ctx, uid, "http://127.0.0.1:8765/vault-import/spotify?error=access_denied&state="+stateOf(t, b))
	if !errors.Is(err, ErrConnect) || err.Error() != "Не удалось подключиться к сервису. Попробуйте ещё раз." {
		t.Fatalf("denied: %v", err)
	}
	if _, err := o.Finish(ctx, uid, stateOf(t, b), "late"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("after denial: %v", err)
	}
}

func TestOAuthReadFailureFailsJob(t *testing.T) {
	ctx := context.Background()
	o, conn, _, uid := newTestOAuth(t, "https://vault.example.org")
	conn.readErr = errors.New("spotify: /v1/me/tracks: 500")
	b, _ := o.Begin(uid, playlistimport.ServiceSpotify)
	id, err := o.Finish(ctx, uid, stateOf(t, b), "c")
	if err != nil {
		t.Fatal(err)
	}
	o.Wait()
	j, _ := o.Svc.Store.Job(ctx, id)
	if j.Status != playlistimport.StatusFailed || j.Error != ErrConnect.Error() {
		t.Fatalf("%+v", j)
	}
	if n, _ := o.Vault.Sweep(0); n != 0 {
		t.Fatalf("token left: %d", n)
	}
}

func TestRecoverFailsStaleReading(t *testing.T) {
	ctx := context.Background()
	o, _, _, uid := newTestOAuth(t, "")
	id, err := o.Svc.Store.CreateReadingJob(ctx, uid, playlistimport.ServiceSpotify)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Vault.Put(NewTokenID(), Token{Access: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := o.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	j, _ := o.Svc.Store.Job(ctx, id)
	if j.Status != playlistimport.StatusFailed || j.Error != ErrConnect.Error() {
		t.Fatalf("%+v", j)
	}
	if n, _ := o.Vault.Sweep(0); n != 0 {
		t.Fatalf("token left: %d", n)
	}
}

func TestOAuthPendingCapEvictsOwnFirst(t *testing.T) {
	ctx := context.Background()
	o, _, _, uid := newTestOAuth(t, "")
	u, err := auth.NewStore(o.Svc.Store.DB).CreateUser(ctx, auth.NewUser{Name: "Мама", Password: "secret", IsAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	other := u.ID
	theirs, _ := o.Begin(other, playlistimport.ServiceSpotify)
	for range pendingMax + 3 {
		if _, err := o.Begin(uid, playlistimport.ServiceSpotify); err != nil {
			t.Fatal(err)
		}
	}
	if n := o.pendingCount(); n != pendingMax {
		t.Fatalf("pending %d", n)
	}
	// one admin's clicks do not push out another admin's sign-in
	if _, err := o.Finish(ctx, other, stateOf(t, theirs), "code"); err != nil {
		t.Fatalf("other admin's sign-in: %v", err)
	}
}

func TestOAuthFinishForService(t *testing.T) {
	ctx := context.Background()
	o, _, _, uid := newTestOAuth(t, "https://vault.example.org")
	b, _ := o.Begin(uid, playlistimport.ServiceSpotify)
	// another service's callback: not this sign-in, which stays
	if _, err := o.FinishFor(ctx, uid, playlistimport.ServiceDeezer, stateOf(t, b), "code"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("deezer: %v", err)
	}
	if _, err := o.FinishFor(ctx, uid, playlistimport.ServiceSpotify, stateOf(t, b), "code"); err != nil {
		t.Fatalf("spotify: %v", err)
	}
}

func TestKnownError(t *testing.T) {
	for _, e := range []error{ErrConnect, ErrLinkExpired, ErrNotConfigured, playlistimport.ErrUnknownService, playlistimport.ErrBusy, playlistimport.ErrTooLarge} {
		if got, ok := KnownError(fmt.Errorf("wrapped: %w", e)); !ok || got != e {
			t.Fatalf("%v → %v %v", e, got, ok)
		}
	}
	if got, ok := KnownError(errors.New("disk full")); ok || got != ErrConnect {
		t.Fatalf("unknown → %v %v", got, ok)
	}
}

// statelessConn is a service that may send the browser back without the
// state (Deezer).
type statelessConn struct{ fakeConn }

func (*statelessConn) StateOptional() bool { return true }

func TestOAuthStateOptional(t *testing.T) {
	ctx := context.Background()
	o, conn, now, uid := newTestOAuth(t, "https://vault.example.org")
	dz := &statelessConn{fakeConn{snap: playlistimport.Snapshot{Service: playlistimport.ServiceDeezer, Tracks: []playlistimport.SnapTrack{{Key: "deezer:1", Artist: "Кино", Title: "Кукушка", Liked: true}}}}}
	o.Connectors[playlistimport.ServiceDeezer] = dz

	// a service that always sends the state back: none is none
	o.Begin(uid, playlistimport.ServiceSpotify)
	if _, err := o.FinishFor(ctx, uid, playlistimport.ServiceSpotify, "", "c"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("spotify without state: %v", err)
	}
	if len(conn.exchanges) != 0 {
		t.Fatalf("exchanged %v", conn.exchanges)
	}

	// no Deezer sign-in of this profile (another admin's is not taken)
	o.Begin(uid+100, playlistimport.ServiceDeezer)
	if _, err := o.FinishFor(ctx, uid, playlistimport.ServiceDeezer, "", "c"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("another admin's: %v", err)
	}
	// the profile's one Deezer sign-in: taken, used up
	o.Begin(uid, playlistimport.ServiceDeezer)
	id, err := o.FinishFor(ctx, uid, playlistimport.ServiceDeezer, "", "fr1")
	if err != nil || id == 0 {
		t.Fatalf("one sign-in: %d %v", id, err)
	}
	o.Wait()
	if _, err := o.FinishFor(ctx, uid, playlistimport.ServiceDeezer, "", "fr1"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("used twice: %v", err)
	}
	o.Svc.Store.CancelJob(ctx, id)

	// two at once: which one is unknown, neither is taken
	b1, _ := o.Begin(uid, playlistimport.ServiceDeezer)
	o.Begin(uid, playlistimport.ServiceDeezer)
	if _, err := o.FinishFor(ctx, uid, playlistimport.ServiceDeezer, "", "fr2"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("two sign-ins: %v", err)
	}
	// with its state, one of them still works
	id, err = o.FinishFor(ctx, uid, playlistimport.ServiceDeezer, stateOf(t, b1), "fr3")
	if err != nil {
		t.Fatalf("with state: %v", err)
	}
	o.Wait()
	o.Svc.Store.CancelJob(ctx, id)
	// the other one ran out: no longer than 10 minutes
	*now = now.Add(11 * time.Minute)
	if _, err := o.FinishFor(ctx, uid, playlistimport.ServiceDeezer, "", "fr4"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("after 11 min: %v", err)
	}

	// refused without the state (error_reason=user_denied): used up
	o.Begin(uid, playlistimport.ServiceDeezer)
	if err := o.DeniedFor(uid, playlistimport.ServiceDeezer, ""); !errors.Is(err, ErrConnect) {
		t.Fatalf("denied: %v", err)
	}
	if _, err := o.FinishFor(ctx, uid, playlistimport.ServiceDeezer, "", "fr5"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("after denial: %v", err)
	}

	// pasted without the state: the address names the service
	o.PublicURL = ""
	o.Begin(uid, playlistimport.ServiceSpotify)
	o.Begin(uid, playlistimport.ServiceDeezer)
	if _, err := o.FinishPasted(ctx, uid, "http://127.0.0.1:8765/vault-import/spotify?code=x"); !errors.Is(err, ErrConnect) {
		t.Fatalf("spotify pasted without state: %v", err)
	}
	if _, err := o.FinishPasted(ctx, uid, "http://127.0.0.1:8765/vault-import/deezer?error_reason=user_denied"); !errors.Is(err, ErrConnect) {
		t.Fatalf("deezer denied pasted: %v", err)
	}
	o.Begin(uid, playlistimport.ServiceDeezer)
	if id, err := o.FinishPasted(ctx, uid, "http://127.0.0.1:8765/vault-import/deezer?code=fr6"); err != nil || id == 0 {
		t.Fatalf("deezer pasted: %d %v", id, err)
	}
	o.Wait()
	dz.mu.Lock()
	defer dz.mu.Unlock()
	if got := dz.exchanges; len(got) != 3 || !strings.HasPrefix(got[0], "fr1|") || !strings.HasPrefix(got[1], "fr3|") || !strings.HasPrefix(got[2], "fr6|") {
		t.Fatalf("exchanges %v", got)
	}
}

func TestOAuthStateMustMatchWhenGiven(t *testing.T) {
	ctx := context.Background()
	o, _, _, uid := newTestOAuth(t, "")
	dz := &statelessConn{fakeConn{snap: playlistimport.Snapshot{Service: playlistimport.ServiceDeezer, Tracks: []playlistimport.SnapTrack{{Key: "deezer:1", Artist: "Кино", Title: "Кукушка"}}}}}
	o.Connectors[playlistimport.ServiceDeezer] = dz
	b, _ := o.Begin(uid, playlistimport.ServiceDeezer)
	// a state that is not the sign-in's never falls back to «the only one»
	if _, err := o.FinishFor(ctx, uid, playlistimport.ServiceDeezer, "forged-state", "fr1"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("callback with a wrong state: %v", err)
	}
	if err := o.DeniedFor(uid, playlistimport.ServiceDeezer, "forged-state"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("denial with a wrong state: %v", err)
	}
	if _, err := o.FinishPasted(ctx, uid, b.RedirectURI+"?code=fr1&state=forged-state"); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("pasted with a wrong state: %v", err)
	}
	if len(dz.exchanges) != 0 || o.pendingCount() != 1 {
		t.Fatalf("exchanged %v, pending %d", dz.exchanges, o.pendingCount())
	}
	// the sign-in is still there for its own state
	if id, err := o.FinishFor(ctx, uid, playlistimport.ServiceDeezer, stateOf(t, b), "fr2"); err != nil || id == 0 {
		t.Fatalf("its own state: %d %v", id, err)
	}
}

// One background read per profile and service: a Takeout being named
// does not hold up a Deezer link; a second Takeout waits its turn. A
// deleted job's read is stopped before the job goes.
func TestReadInBackgroundPerService(t *testing.T) {
	ctx := context.Background()
	o, _, _, uid := newTestOAuth(t, "")
	o.Svc.Reads = o
	release := make(chan struct{})
	defer close(release)
	slow := func(ctx context.Context, _ int64) (playlistimport.Snapshot, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return playlistimport.Snapshot{}, ctx.Err()
		}
		return playlistimport.Snapshot{Service: playlistimport.ServiceYouTube, Tracks: []playlistimport.SnapTrack{{Key: "a", Artist: "Кино", Title: "Кукушка"}}}, nil
	}
	yt, err := o.ReadInBackground(ctx, uid, playlistimport.ServiceYouTube, slow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.ReadInBackground(ctx, uid, playlistimport.ServiceYouTube, slow); !errors.Is(err, playlistimport.ErrBusy) {
		t.Fatalf("second takeout: %v", err)
	}
	dz, err := o.ReadInBackground(ctx, uid, playlistimport.ServiceDeezer, func(context.Context, int64) (playlistimport.Snapshot, error) {
		return playlistimport.Snapshot{Service: playlistimport.ServiceDeezer, Tracks: []playlistimport.SnapTrack{{Key: "b", Artist: "Кино", Title: "Звезда"}}}, nil
	})
	if err != nil || dz == yt {
		t.Fatalf("deezer while a takeout is named: %d %v", dz, err)
	}
	if err := o.Svc.Delete(ctx, yt); err != nil {
		t.Fatal(err)
	}
	o.Wait()
	if _, err := o.Svc.Store.Job(ctx, yt); !errors.Is(err, playlistimport.ErrNotFound) {
		t.Fatalf("deleted job: %v", err)
	}
}
