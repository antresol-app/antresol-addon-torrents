package indexer

import "context"

type backgroundKey struct{}

// Background marks ctx as the context of a search nobody waits on: the
// core's tracking scans, the follow, auto-switch and import loops. Such a
// search leaves a source that is paused for a failed FlareSolverr alone,
// and its failure is what pauses it (see solverOutcome). A search a person
// started (the release picker, «Смотреть сейчас», a manual search, an
// app's call) carries no mark: it tries a paused source all the same (the
// gate of FlareSolverr already keeps it to one request at a time), an
// answer ends the pause, and its failure never makes the pause longer.
func Background(ctx context.Context) context.Context {
	if IsBackground(ctx) {
		return ctx
	}
	return context.WithValue(ctx, backgroundKey{}, true)
}

// IsBackground reports whether ctx was made by Background (the context of
// a request, or of a search a person started, is not).
func IsBackground(ctx context.Context) bool {
	_, ok := ctx.Value(backgroundKey{}).(bool)
	return ok
}
