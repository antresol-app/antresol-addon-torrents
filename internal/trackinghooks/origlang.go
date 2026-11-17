package trackinghooks

import (
	"context"
	"encoding/json"

	"github.com/antresol-app/antresol/addonkit"
)

// origLang answers voiceover.origLang: the original language of the title
// of the library file (a film, or an episode's series), "" when unknown;
// the core then fits the voice-over without it.
func (d Deps) origLang(ctx context.Context, _ *addonkit.User, in json.RawMessage) (any, error) {
	var q addonkit.OrigLangRequest
	if err := json.Unmarshal(in, &q); err != nil {
		return nil, err
	}
	out := addonkit.OrigLangAnswer{}
	if q.FileID > 0 && d.OrigLang != nil {
		out.Lang = d.OrigLang(ctx, q.FileID)
	}
	return out, nil
}
