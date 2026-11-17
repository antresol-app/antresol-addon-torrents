package coretest

import (
	"context"
	"maps"
	"slices"
	"strconv"

	"github.com/antresol-app/antresol/addonkit"
)

// Users is the fake core's profiles: Names by id (nil: none); UIDs the
// profiles' uids by id (a profile without one has "uid-<id>"); Err, when
// set, is what a lookup answers instead (the core unreachable). A field
// cannot share the method's name, so the addon's Users interface
// (coreadapt.Users' Names and Profiles) is u.Adapter().
type Users struct {
	Names map[int64]string
	UIDs  map[int64]string
	Err   error
}

// UID is profile id's uid.
func (u *Users) UID(id int64) string {
	if s, ok := u.UIDs[id]; ok {
		return s
	}
	return "uid-" + strconv.FormatInt(id, 10)
}

// User is profile id as the core sends it (X-Antresol-User).
func (u *Users) User(id int64) addonkit.User {
	return addonkit.User{ID: id, UID: u.UID(id), Name: u.Names[id]}
}

// Adapter is u as coreadapt.Users: Names(ctx) answers a copy of u.Names
// (read at each call, so a test may change them in between).
func (u *Users) Adapter() UsersAdapter { return UsersAdapter{u} }

// UsersAdapter is the Names and Profiles methods of a *Users.
type UsersAdapter struct{ u *Users }

func (a UsersAdapter) Names(context.Context) (map[int64]string, error) {
	if a.u.Err != nil {
		return nil, a.u.Err
	}
	return maps.Clone(a.u.Names), nil
}

// Profiles is GET /users: the profiles of Names, by id.
func (a UsersAdapter) Profiles(context.Context) ([]addonkit.User, error) {
	if a.u.Err != nil {
		return nil, a.u.Err
	}
	out := []addonkit.User{}
	for _, id := range slices.Sorted(maps.Keys(a.u.Names)) {
		out = append(out, a.u.User(id))
	}
	return out, nil
}
