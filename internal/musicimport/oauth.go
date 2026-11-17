//go:build ignore

package musicimport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/playlistimport"
)

// Sign-in errors (their text is shown as is).
var (
	ErrConnect       = errors.New("Не удалось подключиться к сервису. Попробуйте ещё раз.")
	ErrLinkExpired   = errors.New("Ссылка устарела. Подключитесь заново.")
	ErrNotConfigured = errors.New("Сервис не настроен на сервере")
)

// Reader reads a signed-in library with its token.
type Reader interface {
	// Read reads the library with the token; progress may be nil.
	Read(ctx context.Context, tok Token, progress func(done, total int)) (playlistimport.Snapshot, error)
}

// Connector is a streaming service Vault signs in to (OAuth 2.0 with PKCE)
// and reads a library from (connect.Spotify and the later services).
type Connector interface {
	Reader
	// AuthorizeURL is the service's sign-in page for this redirect, state
	// and PKCE challenge.
	AuthorizeURL(redirectURI, state, challenge string) string
	// Exchange trades the code the sign-in sent back for a token.
	Exchange(ctx context.Context, redirectURI, code, verifier string) (Token, error)
}

// StateOptional is a Connector whose sign-in may send the browser back
// without the state (Deezer): the profile's only sign-in of that service
// in progress, if it has exactly one (within pendingTTL), is the one.
type StateOptional interface {
	StateOptional() bool
}

// stateOptional reports whether a service may come back without the state.
func (o *OAuth) stateOptional(service string) bool {
	so, ok := o.Connectors[service].(StateOptional)
	return ok && so.StateOptional()
}

// ErrPending: the listener has not entered the device code yet (poll
// again after the interval).
var ErrPending = errors.New("musicimport: authorization pending")

// ErrSlowDown is ErrPending with the service asking to poll less often
// (RFC 8628 slow_down, or a 429): the interval grows by slowDownStep for
// the rest of the sign-in.
var ErrSlowDown = fmt.Errorf("%w (slow down)", ErrPending)

// slowDownStep is what RFC 8628 §3.5 adds to the interval on slow_down.
const slowDownStep = 5 * time.Second

// DeviceCode is a service's answer to a device sign-in (RFC 8628): the
// listener opens VerificationURL and enters UserCode; DeviceCode is what
// the server polls with (never shown).
type DeviceCode struct {
	DeviceCode      string
	UserCode        string
	VerificationURL string
	Interval        int // seconds between polls
	ExpiresIn       int // seconds the code is valid
}

// DeviceConnector is a service signed in to with a device code
// (connect.Yandex), or with a token the listener pastes.
type DeviceConnector interface {
	Reader
	// DeviceCode starts a sign-in.
	DeviceCode(ctx context.Context) (DeviceCode, error)
	// PollToken asks whether the code was entered: a token, ErrPending
	// (not yet), ErrSlowDown (not yet, and poll less often),
	// ErrLinkExpired (the code ran out), ErrConnect (refused);
	// any other error is a passing failure and the sign-in goes on.
	PollToken(ctx context.Context, deviceCode string) (Token, error)
}

// ModeDevice: the listener enters a code on the service's page.
const ModeDevice = "device"

// DeviceDTO is what «Подключить» of a device sign-in shows: «Откройте
// <verificationUrl> и введите код <userCode>»; the page polls with pollId
// every interval seconds.
type DeviceDTO struct {
	PollID          string `json:"pollId"`
	UserCode        string `json:"userCode"`
	VerificationURL string `json:"verificationUrl"`
	ExpiresIn       int    `json:"expiresIn"`
	Interval        int    `json:"interval"`
}

// devicePending is a device sign-in in progress (memory only).
type devicePending struct {
	PollID   string
	Service  string
	Code     string // the device code
	UserID   int64
	Expires  time.Time
	Interval time.Duration
	next     time.Time // no poll reaches the service before
	polling  bool      // a poll is asking the service
}

// Device sign-in bounds.
const (
	deviceMinInterval = time.Second
	deviceMaxTTL      = 30 * time.Minute
	pollTimeout       = 30 * time.Second
	maxPastedToken    = 1024
)

// How the browser comes back from a service's sign-in page.
const (
	// ModeRedirect: to this server (VAULT_PUBLIC_URL is https).
	ModeRedirect = "redirect"
	// ModePaste: to a loopback address that does not load; the admin
	// pastes it into «Импорт» («Вставить адрес из браузера»).
	ModePaste = "paste"
)

// Sign-ins in progress: at most pendingMax (the profile's oldest makes
// room, else the oldest of all), each
// usable once within pendingTTL.
const (
	pendingMax = 16
	pendingTTL = 10 * time.Minute
)

// readTimeout bounds one library read.
const readTimeout = 30 * time.Minute

// PasteRedirect is the loopback redirect URI of the paste mode (Spotify
// accepts http only for 127.0.0.1, not localhost).
func PasteRedirect(service string) string { return "http://127.0.0.1:8765/vault-import/" + service }

// CallbackPath is this server's redirect path for a service.
func CallbackPath(service string) string { return "/api/import/oauth/" + service + "/callback" }

// Pending is a sign-in started by Begin and not finished yet (memory only).
type Pending struct {
	ID          string // the token's id in the Vault
	Service     string
	State       string
	Verifier    string
	RedirectURI string
	UserID      int64
	Created     time.Time
}

// BeginDTO is what «Подключить» opens.
type BeginDTO struct {
	AuthorizeURL string `json:"authorizeUrl"`
	Mode         string `json:"mode"`
	RedirectURI  string `json:"redirectUri"`
}

// OAuth signs an admin in to a service and reads its library into a job:
// Begin gives the sign-in page; Finish (the callback) or FinishPasted (the
// address pasted back) trades the code for a token, which is kept in the
// Vault only while a background read fills a reading job (→ preview, or
// failed with the reason).
type OAuth struct {
	Svc        *playlistimport.Service
	Vault      *Vault
	Connectors map[string]Connector       // the configured services
	Devices    map[string]DeviceConnector // the configured device sign-in services
	PublicURL  string                     // VAULT_PUBLIC_URL (no trailing slash)
	Now        func() time.Time           // nil: time.Now
	Base       context.Context            // the reads' lifetime; nil: Background

	mu        sync.Mutex
	pending   []Pending // oldest first
	devices   []devicePending
	stops     map[int64]*readStop // a reading job's read, by job id
	resolving map[string]bool     // profile/service pairs with a ReadInBackground running
	wg        sync.WaitGroup
}

func (o *OAuth) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Configured reports whether the server has credentials for a service.
func (o *OAuth) Configured(service string) bool {
	return o != nil && (o.Connectors[service] != nil || o.Devices[service] != nil)
}

// IsDevice reports whether a configured service signs in with a device code.
func (o *OAuth) IsDevice(service string) bool { return o != nil && o.Devices[service] != nil }

// Mode is how the browser comes back from the sign-in: redirect with a
// public https address, paste otherwise.
func (o *OAuth) Mode(service string) string {
	if strings.HasPrefix(o.PublicURL, "https://") {
		return ModeRedirect
	}
	return ModePaste
}

func (o *OAuth) redirectURI(service string) string {
	if o.Mode(service) == ModeRedirect {
		return o.PublicURL + CallbackPath(service)
	}
	return PasteRedirect(service)
}

// Challenge is the PKCE S256 challenge of a verifier (RFC 7636).
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func randString(n int) string {
	b := make([]byte, n)
	rand.Read(b) // never fails (crypto/rand)
	return base64.RawURLEncoding.EncodeToString(b)
}

// newVerifier is a PKCE verifier: 64 random bytes, 86 characters.
func newVerifier() string { return randString(64) }

// Begin starts a sign-in (ErrUnknownService, ErrNotConfigured).
func (o *OAuth) Begin(userID int64, service string) (BeginDTO, error) {
	if !slices.Contains(playlistimport.Services, service) {
		return BeginDTO{}, playlistimport.ErrUnknownService
	}
	c := o.Connectors[service]
	if c == nil {
		return BeginDTO{}, ErrNotConfigured
	}
	p := Pending{ID: NewTokenID(), Service: service, State: randString(32), Verifier: newVerifier(),
		RedirectURI: o.redirectURI(service), UserID: userID, Created: o.now()}
	o.mu.Lock()
	o.prune()
	for len(o.pending) >= pendingMax {
		// the profile's own oldest sign-in makes room first: one admin's
		// clicks never push out another admin's sign-in
		i := slices.IndexFunc(o.pending, func(q Pending) bool { return q.UserID == userID })
		o.pending = slices.Delete(o.pending, max(i, 0), max(i, 0)+1)
	}
	o.pending = append(o.pending, p)
	o.mu.Unlock()
	return BeginDTO{AuthorizeURL: c.AuthorizeURL(p.RedirectURI, p.State, Challenge(p.Verifier)), Mode: o.Mode(service),
		RedirectURI: p.RedirectURI}, nil
}

// prune drops expired sign-ins (o.mu held).
func (o *OAuth) prune() {
	now := o.now()
	o.pending = slices.DeleteFunc(o.pending, func(p Pending) bool { return now.Sub(p.Created) > pendingTTL })
}

func (o *OAuth) pendingCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.pending)
}

// take finds the profile's sign-in of a state; check (nil: none) may
// refuse it while keeping it. Unless refused, it is used up. Another
// profile's state reads as expired and stays its owner's.
func (o *OAuth) take(userID int64, state string, check func(Pending) error) (Pending, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.prune()
	for i, p := range o.pending {
		if subtle.ConstantTimeCompare([]byte(p.State), []byte(state)) != 1 {
			continue
		}
		if p.UserID != userID {
			return Pending{}, ErrLinkExpired
		}
		if check != nil {
			if err := check(p); err != nil {
				return Pending{}, err
			}
		}
		o.pending = slices.Delete(o.pending, i, i+1)
		return p, nil
	}
	return Pending{}, ErrLinkExpired
}

// takeOnly is take for a sign-in that came back without its state: the
// profile's one sign-in of a service that may do so and that match
// accepts; none or several (which one is unknown) read as expired.
func (o *OAuth) takeOnly(userID int64, match func(Pending) bool) (Pending, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.prune()
	at := -1
	for i, p := range o.pending {
		if p.UserID != userID || !o.stateOptional(p.Service) || !match(p) {
			continue
		}
		if at >= 0 {
			return Pending{}, ErrLinkExpired
		}
		at = i
	}
	if at < 0 {
		return Pending{}, ErrLinkExpired
	}
	p := o.pending[at]
	o.pending = slices.Delete(o.pending, at, at+1)
	return p, nil
}

// takeFor finds the profile's sign-in of a service by its state, or —
// without one, for a service that may drop it — its only sign-in of that
// service ("": any service, and the state is needed).
func (o *OAuth) takeFor(userID int64, service, state string) (Pending, error) {
	if state == "" && service != "" {
		return o.takeOnly(userID, func(p Pending) bool { return p.Service == service })
	}
	return o.take(userID, state, func(p Pending) error {
		if service != "" && p.Service != service {
			return ErrLinkExpired
		}
		return nil
	})
}

// Finish completes a sign-in with the code the service sent back: the
// token goes to the Vault and a reading job is made, which a background
// read fills (ErrLinkExpired, ErrConnect).
func (o *OAuth) Finish(ctx context.Context, userID int64, state, code string) (int64, error) {
	return o.FinishFor(ctx, userID, "", state, code)
}

// FinishFor is Finish for the callback of one service: a state of another
// service's sign-in reads as expired and stays its own ("": any service).
func (o *OAuth) FinishFor(ctx context.Context, userID int64, service, state, code string) (int64, error) {
	p, err := o.takeFor(userID, service, state)
	if err != nil {
		return 0, err
	}
	return o.finish(ctx, p, code)
}

// FinishPasted completes a sign-in from the address the browser was sent
// back to (the paste mode): it must be the sign-in's redirect URI with its
// state (a service that may drop the state: its only sign-in of that
// address); error=access_denied or error_reason=user_denied (the listener
// said no) is ErrConnect.
func (o *OAuth) FinishPasted(ctx context.Context, userID int64, raw string) (int64, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return 0, ErrConnect
	}
	q := u.Query()
	state := q.Get("state")
	back := u.Scheme + "://" + u.Host + u.Path
	var p Pending
	if state == "" {
		if p, err = o.takeOnly(userID, func(p Pending) bool { return back == p.RedirectURI }); err != nil {
			return 0, ErrConnect
		}
	} else if p, err = o.take(userID, state, func(p Pending) error {
		if back != p.RedirectURI {
			return ErrConnect // not where the service sends back to: kept for the right address
		}
		return nil
	}); err != nil {
		return 0, err
	}
	if Refused(q) {
		return 0, ErrConnect
	}
	return o.finish(ctx, p, q.Get("code"))
}

// Denied drops a sign-in the service reports refused (the callback's
// error=…); it is ErrConnect, or ErrLinkExpired for an unknown state.
func (o *OAuth) Denied(userID int64, state string) error {
	return o.DeniedFor(userID, "", state)
}

// DeniedFor is Denied for the callback of one service (a service that may
// drop the state: its only sign-in when the state is missing).
func (o *OAuth) DeniedFor(userID int64, service, state string) error {
	if _, err := o.takeFor(userID, service, state); err != nil {
		return err
	}
	return ErrConnect
}

// Refused reports whether a sign-in's answer says the listener refused:
// error=… (OAuth 2.0), or error_reason=… (Deezer).
func Refused(q url.Values) bool { return q.Has("error") || q.Has("error_reason") }

func (o *OAuth) finish(ctx context.Context, p Pending, code string) (int64, error) {
	c := o.Connectors[p.Service]
	if c == nil {
		return 0, ErrNotConfigured
	}
	if code == "" {
		return 0, ErrConnect
	}
	tok, err := c.Exchange(ctx, p.RedirectURI, code, p.Verifier)
	if err != nil {
		if ctx.Err() == nil {
			o.Svc.Logger().Warn("import: sign-in refused", "service", p.Service, "err", err)
		}
		return 0, ErrConnect
	}
	return o.startRead(ctx, p.ID, p.UserID, p.Service, tok, c)
}

// startRead keeps the token in the Vault under tokenID and makes a
// reading job that a background read fills; the token is deleted either
// way.
func (o *OAuth) startRead(ctx context.Context, tokenID string, userID int64, service string, tok Token, c Reader) (int64, error) {
	if err := o.Vault.Put(tokenID, tok); err != nil {
		return 0, err
	}
	return o.background(ctx, userID, service, func(ctx context.Context, _ int64) (playlistimport.Snapshot, error) {
		tok, err := o.Vault.Get(tokenID)
		if err != nil {
			return playlistimport.Snapshot{}, err
		}
		snap, err := c.Read(ctx, tok, nil)
		tok = Token{}
		return snap, err
	}, func() { o.deleteToken(tokenID) })
}

// ReadInBackground makes a reading job of a service whose snapshot read
// makes in the background (an uploaded Takeout whose videos are being
// named): the job moves to preview, or fails with the reason; shutdown
// and Recover treat it as any library read, and a cancel or delete of the
// job stops it (Stop). One runs at a time per profile and service:
// another is ErrBusy.
func (o *OAuth) ReadInBackground(ctx context.Context, userID int64, service string, read func(ctx context.Context, jobID int64) (playlistimport.Snapshot, error)) (int64, error) {
	key := strconv.FormatInt(userID, 10) + "/" + service
	o.mu.Lock()
	if o.resolving[key] {
		o.mu.Unlock()
		return 0, playlistimport.ErrBusy
	}
	if o.resolving == nil {
		o.resolving = map[string]bool{}
	}
	o.resolving[key] = true
	o.mu.Unlock()
	return o.background(ctx, userID, service, read, func() {
		o.mu.Lock()
		delete(o.resolving, key)
		o.mu.Unlock()
	})
}

// readStop cancels one read (compared by pointer: a job id given again
// is another read).
type readStop struct{ cancel context.CancelFunc }

// Stop ends a reading job's read, if one runs (the job was cancelled or
// deleted): the service is asked no more.
func (o *OAuth) Stop(jobID int64) {
	if o == nil {
		return
	}
	o.mu.Lock()
	stop := o.stops[jobID]
	o.mu.Unlock()
	if stop != nil {
		stop.cancel()
	}
}

// background makes a reading job that read fills in a goroutine; done
// (nil: none) runs once either way.
func (o *OAuth) background(ctx context.Context, userID int64, service string, read func(context.Context, int64) (playlistimport.Snapshot, error), done func()) (int64, error) {
	id, err := o.Svc.Store.CreateReadingJob(ctx, userID, service)
	if err != nil {
		if done != nil {
			done()
		}
		return 0, err
	}
	base := o.Base
	if base == nil {
		base = context.Background()
	}
	rctx, cancel := context.WithTimeout(base, readTimeout)
	stop := &readStop{cancel: cancel}
	o.mu.Lock()
	if o.stops == nil {
		o.stops = map[int64]*readStop{}
	}
	o.stops[id] = stop
	o.mu.Unlock()
	o.Svc.Publish(ctx, id, true)
	o.wg.Add(1)
	go o.read(rctx, base, id, service, read, func() {
		o.mu.Lock()
		if o.stops[id] == stop {
			delete(o.stops, id)
		}
		o.mu.Unlock()
		cancel()
		if done != nil {
			done()
		}
	})
	return id, nil
}

// read fills a reading job (a cancel or delete meanwhile wins).
func (o *OAuth) read(ctx, base context.Context, jobID int64, service string, read func(context.Context, int64) (playlistimport.Snapshot, error), done func()) {
	defer o.wg.Done()
	defer done()
	log := o.Svc.Logger().With("job", jobID, "service", service)
	snap, err := read(ctx, jobID)
	if err == nil {
		snap.Service = service
		err = o.Svc.Fill(ctx, jobID, snap)
	}
	if err == nil {
		return
	}
	switch {
	case base.Err() != nil:
		log.Warn("import: library read stopped by shutdown")
	case errors.Is(ctx.Err(), context.Canceled):
		log.Info("import: library read stopped: the job was cancelled or deleted")
		return
	default:
		log.Warn("import: library read failed", "err", err)
	}
	if _, aerr := o.Svc.Advance(context.WithoutCancel(ctx), jobID, playlistimport.StatusReading, playlistimport.StatusFailed, readFailure(err)); aerr != nil &&
		!errors.Is(aerr, playlistimport.ErrNotFound) {
		log.Error("import: fail reading job", "err", aerr)
	}
}

// BeginDevice starts a device sign-in (ErrUnknownService,
// ErrNotConfigured, ErrConnect): the code to enter and where.
func (o *OAuth) BeginDevice(ctx context.Context, userID int64, service string) (DeviceDTO, error) {
	if !slices.Contains(playlistimport.Services, service) {
		return DeviceDTO{}, playlistimport.ErrUnknownService
	}
	c := o.Devices[service]
	if c == nil {
		return DeviceDTO{}, ErrNotConfigured
	}
	dc, err := c.DeviceCode(ctx)
	if err != nil {
		if ctx.Err() == nil {
			o.Svc.Logger().Warn("import: device code refused", "service", service, "err", err)
		}
		return DeviceDTO{}, ErrConnect
	}
	if dc.DeviceCode == "" || dc.UserCode == "" || !strings.HasPrefix(dc.VerificationURL, "https://") {
		return DeviceDTO{}, ErrConnect // the page is shown as a link: https only
	}
	interval := max(time.Duration(dc.Interval)*time.Second, deviceMinInterval)
	ttl := time.Duration(dc.ExpiresIn) * time.Second
	if ttl <= 0 || ttl > deviceMaxTTL {
		ttl = min(pendingTTL, deviceMaxTTL)
	}
	now := o.now()
	p := devicePending{PollID: randString(32), Service: service, Code: dc.DeviceCode, UserID: userID,
		Expires: now.Add(ttl), Interval: interval, next: now}
	o.mu.Lock()
	o.pruneDevices()
	for len(o.devices) >= pendingMax {
		// the profile's own oldest makes room first; a sign-in whose poll
		// is asking the service is never dropped (its token would be lost)
		i := slices.IndexFunc(o.devices, func(q devicePending) bool { return q.UserID == userID && !q.polling })
		if i < 0 {
			i = slices.IndexFunc(o.devices, func(q devicePending) bool { return !q.polling })
		}
		if i < 0 {
			break
		}
		o.devices = slices.Delete(o.devices, i, i+1)
	}
	o.devices = append(o.devices, p)
	o.mu.Unlock()
	return DeviceDTO{PollID: p.PollID, UserCode: dc.UserCode, VerificationURL: dc.VerificationURL,
		ExpiresIn: int(ttl / time.Second), Interval: int(interval / time.Second)}, nil
}

// pruneDevices drops expired device sign-ins (o.mu held); one whose poll
// is asking the service stays until the answer.
func (o *OAuth) pruneDevices() {
	now := o.now()
	o.devices = slices.DeleteFunc(o.devices, func(p devicePending) bool { return !p.polling && !now.Before(p.Expires) })
}

// device finds the profile's device sign-in of a poll id (o.mu held): -1
// when unknown, expired, another profile's or another service's.
func (o *OAuth) device(userID int64, service, pollID string) int {
	o.pruneDevices()
	now := o.now()
	for i, p := range o.devices {
		if subtle.ConstantTimeCompare([]byte(p.PollID), []byte(pollID)) == 1 {
			if p.UserID != userID || p.Service != service || !now.Before(p.Expires) {
				return -1
			}
			return i
		}
	}
	return -1
}

// Poll asks whether the listener entered the code: ErrPending (poll again
// later; a poll sooner than the interval does not ask the service), the
// reading job once signed in — the sign-in is then used up —,
// ErrLinkExpired (the code ran out, or unknown) or ErrConnect (refused).
func (o *OAuth) Poll(ctx context.Context, userID int64, service, pollID string) (int64, error) {
	o.mu.Lock()
	i := o.device(userID, service, pollID)
	if i < 0 {
		o.mu.Unlock()
		return 0, ErrLinkExpired
	}
	p := &o.devices[i]
	now := o.now()
	if p.polling || now.Before(p.next) {
		o.mu.Unlock()
		return 0, ErrPending
	}
	p.polling, p.next = true, now.Add(p.Interval)
	code, id := p.Code, p.PollID
	c := o.Devices[service]
	o.mu.Unlock()

	// a token Yandex hands out is not lost when the page goes away
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pollTimeout)
	tok, err := c.PollToken(pctx, code)
	cancel()

	o.mu.Lock()
	j := slices.IndexFunc(o.devices, func(q devicePending) bool { return q.PollID == id })
	keep := errors.Is(err, ErrPending) || (err != nil && !errors.Is(err, ErrLinkExpired) && !errors.Is(err, ErrConnect))
	if j >= 0 {
		if keep {
			d := &o.devices[j]
			d.polling = false
			if errors.Is(err, ErrSlowDown) {
				// for this and every later poll (RFC 8628 §3.5)
				d.Interval += slowDownStep
				d.next = o.now().Add(d.Interval)
			}
		} else {
			o.devices = slices.Delete(o.devices, j, j+1) // used up
		}
	}
	o.mu.Unlock()
	switch {
	case errors.Is(err, ErrPending):
		return 0, ErrPending
	case errors.Is(err, ErrLinkExpired):
		return 0, ErrLinkExpired
	case err != nil:
		o.Svc.Logger().Warn("import: device sign-in poll", "service", service, "err", err)
		if keep {
			return 0, ErrPending // a passing failure: the next poll asks again
		}
		return 0, ErrConnect
	case j < 0:
		return 0, ErrLinkExpired // dropped meanwhile
	}
	return o.startRead(context.WithoutCancel(ctx), NewTokenID(), userID, service, tok, c)
}

// FinishToken reads a library with a token the listener pasted
// («Вставить токен»; "OAuth " before it is dropped): the reading job
// (ErrNotConfigured, ErrConnect).
func (o *OAuth) FinishToken(ctx context.Context, userID int64, service, token string) (int64, error) {
	c := o.Devices[service]
	if c == nil {
		return 0, ErrNotConfigured
	}
	token = strings.TrimSpace(token)
	if len(token) > 6 && strings.EqualFold(token[:6], "oauth ") {
		token = strings.TrimSpace(token[6:])
	}
	if token == "" || len(token) > maxPastedToken || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r > '~' }) {
		return 0, ErrConnect
	}
	return o.startRead(ctx, NewTokenID(), userID, service, Token{Access: token}, c)
}

// knownErrors are the import's errors whose text may be shown as is.
var knownErrors = []error{ErrConnect, ErrLinkExpired, ErrNotConfigured, playlistimport.ErrUnknownService, playlistimport.ErrBusy, playlistimport.ErrTooLarge}

// KnownError is the shown text of a sign-in or read failure: the known
// error err wraps, or ErrConnect (ok false) for anything else, which is
// logged, never shown.
func KnownError(err error) (error, bool) {
	for _, known := range knownErrors {
		if errors.Is(err, known) {
			return known, true
		}
	}
	return ErrConnect, false
}

// readFailure is the reason a failed read shows.
func readFailure(err error) string {
	e, _ := KnownError(err)
	return e.Error()
}

func (o *OAuth) deleteToken(id string) {
	if err := o.Vault.Delete(id); err != nil {
		o.Svc.Logger().Error("import: delete token", "err", err)
	}
}

// Wait waits for the reads in progress (tests, shutdown).
func (o *OAuth) Wait() { o.wg.Wait() }

// Recover runs at startup: tokens of reads a stop cut short are removed,
// and their reading jobs fail (a read does not resume).
func (o *OAuth) Recover(ctx context.Context) error {
	_, err := o.Vault.Sweep(0)
	ids, ferr := o.Svc.Store.FailReading(ctx, ErrConnect.Error())
	for _, id := range ids {
		o.Svc.Publish(ctx, id, true)
	}
	return errors.Join(err, ferr)
}
