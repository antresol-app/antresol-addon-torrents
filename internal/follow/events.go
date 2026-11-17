package follow

// The events of follows (the core's events/types_full.go named them before
// the addon split; coreadapt.Forward posts them as addon events).
const (
	// EventFollowQueued: a check queued downloads (admins; data Queued).
	EventFollowQueued = "follow.queued"
	// EventFollowChecked: a check finished (everyone; data Checked).
	EventFollowChecked = "follow.checked"
)
