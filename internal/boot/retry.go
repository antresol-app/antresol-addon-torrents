package boot

import (
	"context"
	"errors"
	"time"
)

// finalError is a start error a retry cannot cure: the import's refusals (the
// source changed, the addon database not empty), a bad app_settings, a bad
// configuration, the no-seeding state not written. Anything else (a busy
// vault.db the snapshot cannot catch still, a core that does not answer
// yet, a locked database) is worth another try: no engine exists until a
// whole Boot succeeds.
type finalError struct{ err error }

func (f *finalError) Error() string { return f.err.Error() }
func (f *finalError) Unwrap() error { return f.err }

// Final marks err final (nil stays nil).
func Final(err error) error {
	if err == nil || IsFinal(err) {
		return err
	}
	return &finalError{err}
}

// IsFinal reports whether err was marked Final.
func IsFinal(err error) bool {
	var f *finalError
	return errors.As(err, &f)
}

// Backoff is the pause before try i+2 (i from 0): 2 s, doubling, at most a
// minute.
func Backoff(i int) time.Duration {
	d := 2 * time.Second
	for ; i > 0 && d < time.Minute; i-- {
		d *= 2
	}
	return min(d, time.Minute)
}

// Retry runs try until it succeeds, fails with a Final error (returned) or
// ctx ends (ctx.Err() returned). Every failure goes to report first.
func Retry(ctx context.Context, try func() error, report func(error), pause func(i int) time.Duration) error {
	for i := 0; ; i++ {
		err := try()
		if err == nil {
			return nil
		}
		report(err)
		if IsFinal(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause(i)):
		}
	}
}
