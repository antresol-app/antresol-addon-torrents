package trackinghooks

import (
	"context"
	"encoding/json"

	"github.com/antresol-app/antresol-addon-torrents/internal/pinned"
	"github.com/antresol-app/antresol/addonkit"
)

// titles answers tracking.titles: the titles with an active follow,
// which the core tracks though nobody marked them.
func (d Deps) titles(ctx context.Context, _ *addonkit.User, _ json.RawMessage) (any, error) {
	ts, err := pinned.FollowedTitles(d.DB)(ctx)
	if err != nil {
		return nil, err
	}
	return addonkit.TitlesAnswer{Titles: ts}, nil
}
