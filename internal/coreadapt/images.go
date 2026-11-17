package coreadapt

import (
	"context"

	"github.com/antresol-app/antresol/addonkit"
)

// Images is the server's image cache through addonkit (images): it
// implements acquire.Images.
type Images struct{ C *addonkit.Client }

// Fetch puts the https image at url into the core's cache and names the
// cache file, which the app shows at /api/images/{name}.
func (i Images) Fetch(ctx context.Context, url string) (string, error) {
	return i.C.CacheImage(ctx, url)
}
