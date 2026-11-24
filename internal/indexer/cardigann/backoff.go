package cardigann

import "time"

// A source or a sign-in that failed is left alone for a while, not asked
// again by every search: BackoffFirst after the first failure, doubling with
// each one in a row up to BackoffMax. A success ends the run.
const (
	BackoffFirst = 30 * time.Minute
	BackoffMax   = 6 * time.Hour
)

// BackoffAfter is the pause after the fails-th failure in a row (1 is the
// first).
func BackoffAfter(fails int) time.Duration {
	d := BackoffFirst
	for i := 1; i < fails && d < BackoffMax; i++ {
		d *= 2
	}
	return min(d, BackoffMax)
}
