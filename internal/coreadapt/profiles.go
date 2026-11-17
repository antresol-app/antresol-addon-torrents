package coreadapt

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/antresol-app/antresol/addonkit"
)

// profilesTTL is how long Profiles keeps the core's list.
const profilesTTL = 60 * time.Second

// Profiles is the server's profiles through addonkit (users.read), the
// list kept profilesTTL: one cache for every reader of a profile's
// «Языки» (find, the admin settings). Use a *Profiles.
type Profiles struct {
	C *addonkit.Client
	// now is the clock (tests); nil: time.Now.
	now func() time.Time

	mu       sync.Mutex
	at       time.Time
	list     []addonkit.User
	failedAt time.Time // the last failed ask (failWait)
	err      error     // its error
}

// Languages is the profile's «Языки» (User.Languages: its own audio
// languages, else the household's), else its interface language alone;
// nil when the core does not know the profile or cannot be asked (logged).
func (p *Profiles) Languages(ctx context.Context, userID int64) []string {
	list, err := p.users(ctx)
	if err != nil {
		slog.Warn("coreadapt: profiles", "err", err)
		return nil
	}
	for _, u := range list {
		if u.ID != userID {
			continue
		}
		if len(u.Languages) > 0 {
			return slices.Clone(u.Languages)
		}
		if u.Lang != "" {
			return []string{u.Lang}
		}
		return nil
	}
	return nil
}

func (p *Profiles) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// failWait is how long a failed ask is not repeated: a 403 or a down
// core does not make every request wait on it.
const failWait = 10 * time.Second

// users is the cached list, asked again past profilesTTL (never under
// the lock: readers meanwhile get the list known before). A failed ask
// keeps a list known before and is not repeated for failWait.
func (p *Profiles) users(ctx context.Context) ([]addonkit.User, error) {
	now := p.clock()
	p.mu.Lock()
	list, at, failed, lastErr := p.list, p.at, p.failedAt, p.err
	p.mu.Unlock()
	if list != nil && now.Sub(at) < profilesTTL {
		return list, nil
	}
	if !failed.IsZero() && now.Sub(failed) < failWait {
		if list != nil {
			return list, nil
		}
		return nil, lastErr
	}
	fresh, err := p.C.Users(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.failedAt, p.err = p.clock(), err
		if p.list != nil {
			return p.list, nil
		}
		return nil, err
	}
	if fresh == nil {
		fresh = []addonkit.User{}
	}
	p.list, p.at, p.failedAt, p.err = fresh, p.clock(), time.Time{}, nil
	return fresh, nil
}
