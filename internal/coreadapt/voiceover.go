package coreadapt

import (
	"context"

	"github.com/antresol-app/antresol/addonkit"
)

// The states of a placed voice-over track as acquire names them
// (acquire.VoicePlaced, VoiceFitted, VoiceMisfit, VoiceUnmeasured):
// acquire imports this package, so they are mirrored here.
const (
	VoicePlaced     = "placed"
	VoiceFitted     = "fitted"
	VoiceMisfit     = "misfit"
	VoiceUnmeasured = "unmeasured"
)

// VoiceOver is the core's voice-over fitting through addonkit (permission
// voiceover): the two funcs acquire.SetVoiceOverFit takes.
type VoiceOver struct{ C *addonkit.Client }

// Placed queues the fit of the track at path, placed beside the library
// file fileID, before it is linked (POST /voiceover/fit).
func (v VoiceOver) Placed(ctx context.Context, path string, fileID int64, origLang string) error {
	return v.C.QueueFit(ctx, path, fileID, origLang)
}

// State is a placed track's state on the sheet (GET /voiceover/fit),
// mapped from the core's neutral states: pending → placed (being
// fitted), ok → fitted with its offset (the worker's sign: a track that
// starts 1.5 s late fits at -1500 ms) and tempo, misfit, unmeasured. ok is
// false when the core has none queued for path, or does not answer.
func (v VoiceOver) State(ctx context.Context, path string) (state string, offsetMs, tempo float64, ok bool) {
	f, found, err := v.C.FitState(ctx, path)
	if err != nil || !found {
		return "", 0, 0, false
	}
	switch f.State {
	case addonkit.FitPending:
		return VoicePlaced, 0, 0, true
	case addonkit.FitOK:
		return VoiceFitted, f.OffsetMs, f.Tempo, true
	case addonkit.FitMisfit:
		return VoiceMisfit, 0, 0, true
	}
	return VoiceUnmeasured, 0, 0, true
}
