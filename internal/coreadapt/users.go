package coreadapt

import (
	"context"

	"github.com/antresol-app/antresol/addonkit"
)

// Users is the server's profiles through addonkit (users.read).
type Users struct{ C *addonkit.Client }

// Names maps each profile's id to its name.
func (u Users) Names(ctx context.Context) (map[int64]string, error) {
	us, err := u.C.Users(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]string, len(us))
	for _, x := range us {
		out[x.ID] = x.Name
	}
	return out, nil
}

// Profiles are the profiles as GET /users lists them (uid included).
func (u Users) Profiles(ctx context.Context) ([]addonkit.User, error) {
	return u.C.Users(ctx)
}
