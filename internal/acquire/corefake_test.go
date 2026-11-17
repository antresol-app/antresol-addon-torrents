package acquire

import (
	"context"
	"maps"
	"slices"
	"strconv"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/library"
	"github.com/antresol-app/antresol/addonkit"
)

// addItem puts it into the fake core's library under a new id.
func (l *fakeLib) addItem(it library.Item) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	it.ID = int64(len(l.Library.Items) + 1)
	l.Library.Items = append(l.Library.Items, it)
	return it.ID
}

// addFile puts a file of item into the fake core's library under a new id.
func (l *fakeLib) addFile(f library.File) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	f.ID = int64(len(l.Library.Files) + 1)
	l.Library.Files = append(l.Library.Files, f)
	return f.ID
}

// fileID is the fake core's library file at path (0: none).
func (l *fakeLib) fileID(path string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.Library.Files {
		if f.Path == path {
			return f.ID
		}
	}
	return 0
}

// testUsers are the test services' profiles: the ids the tests ask as.
var testUsers = fakeUsers{1: "Папа", 2: "Мама", 7: "Сын"}

// fakeUsers is the server's profiles by id, each with the uid
// "uid-<id>".
type fakeUsers map[int64]string

func (u fakeUsers) Profiles(context.Context) ([]addonkit.User, error) {
	out := []addonkit.User{}
	for _, id := range slices.Sorted(maps.Keys(u)) {
		out = append(out, addonkit.User{ID: id, UID: "uid-" + strconv.FormatInt(id, 10), Name: u[id]})
	}
	return out, nil
}
