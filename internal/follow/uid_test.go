package follow

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/antresol-app/antresol/addonkit"
)

// TestFollow_UIDReusedID: a follow belongs to the profile's uid. The
// core deletes profile (3, A) and creates (3, B): B neither owns the
// follow nor is its author, at once (no profile list read in between),
// with the core down, and after a restart.
func TestFollow_UIDReusedID(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.users.Names[3], e.users.UIDs = "Мама", map[int64]string{3: "A"}
	f, err := e.svc.Add(ctx, e.users.User(3), "series", 42)
	if err != nil {
		t.Fatal(err)
	}
	if f.UserID != 3 || f.UserUID != "A" || f.AddedBy != "Мама" {
		t.Fatalf("added %+v", f)
	}
	var uid string
	if err := e.svc.DB.QueryRow(`SELECT user_uid FROM follows WHERE id=?`, f.ID).Scan(&uid); err != nil || uid != "A" {
		t.Fatalf("stored uid %q %v", uid, err)
	}
	// deleted and created again with the same id
	e.users.Names[3], e.users.UIDs[3] = "Новый", "B"
	check := func(what string, s *Service) {
		t.Helper()
		g, err := s.Get(ctx, f.ID)
		if err != nil || g.UserID != 0 || g.UserUID != "" || g.AddedBy != "Профиль удалён" {
			t.Fatalf("%s: %+v %v", what, g, err)
		}
		if by, on, err := s.Following(ctx, "series", 42); err != nil || !on || by.ID != 0 {
			t.Fatalf("%s: following %+v %v %v", what, by, on, err)
		}
	}
	check("at once", e.svc)
	restarted := &Service{DB: e.svc.DB, Users: e.users.Adapter()}
	check("after a restart", restarted)
	e.users.Err = errors.New("core down")
	check("core down", restarted)
}

// TestFollow_UIDKeptWhileThere: the profile's own follow stays its own
// (by uid) whatever its id is now.
func TestFollow_UIDKeptWhileThere(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.users.Names[3], e.users.UIDs = "Мама", map[int64]string{3: "A"}
	f, _ := e.svc.Add(ctx, e.users.User(3), "series", 42)
	if g, err := e.svc.Get(ctx, f.ID); err != nil || g.UserID != 3 || g.UserUID != "A" || g.AddedBy != "Мама" {
		t.Fatalf("%+v %v", g, err)
	}
	// a profile list whose uids are missing (an older core) forgets nobody
	e.users.UIDs[3] = ""
	if _, err := e.svc.List(ctx); err != nil {
		t.Fatal(err)
	}
	e.users.UIDs[3] = "A"
	if g, _ := e.svc.Get(ctx, f.ID); g.UserUID != "A" || g.UserID != 3 {
		t.Fatalf("a list without uids forgot %+v", g)
	}
	// a request without a uid stores no owner (never an id alone)
	anon, err := e.svc.Add(ctx, addonkit.User{ID: 3}, "movie", 7)
	if err != nil || anon.UserID != 0 || anon.UserUID != "" {
		t.Fatalf("no uid: %+v %v", anon, err)
	}
}

// uidRows are follows' and pins' owner columns, by id.
func uidRows(t *testing.T, d *sql.DB) (follows, pins map[int64][3]any) {
	t.Helper()
	follows, pins = map[int64][3]any{}, map[int64][3]any{}
	for q, m := range map[string]map[int64][3]any{
		`SELECT id, user_id, user_uid, user_gone FROM follows`:        follows,
		`SELECT id, pinned_by, pinned_by_uid, 0 FROM pinned_releases`: pins,
	} {
		rows, err := d.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id int64
			var by sql.NullInt64
			var uid sql.NullString
			var gone int
			if err := rows.Scan(&id, &by, &uid, &gone); err != nil {
				t.Fatal(err)
			}
			m[id] = [3]any{by.Int64, uid.String, gone}
		}
		rows.Close()
	}
	return follows, pins
}

// TestMapUserUIDs: the one-time mapping gives the id-keyed rows the
// uid of their profile (GET /users); an id no profile has is a deleted
// profile's (ForgetGone); a second run changes nothing; a broken list
// (empty, or a profile without a uid) is an error and changes nothing.
func TestMapUserUIDs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d := e.svc.DB
	if _, err := d.Exec(`INSERT INTO follows (id, kind, tmdb_id, title, user_id, created_at) VALUES
		(1, 'series', 1, 'a', 1, 't'), (2, 'series', 2, 'b', 2, 't'), (3, 'series', 3, 'c', 9, 't'), (4, 'movie', 4, 'd', NULL, 't');
		INSERT INTO pinned_releases (id, title_id, infohash, pinned_by, pinned_at) VALUES (1, 1, 'x', 2, 't'), (2, 2, 'y', 9, 't');
		INSERT INTO acquisitions (infohash, kind, tmdb_id, title, added_by) VALUES ('a1', 'movie', 1, 'a', 1), ('a9', 'movie', 2, 'b', 9)`); err != nil {
		t.Fatal(err)
	}
	profiles := []addonkit.User{{ID: 1, UID: "u1"}, {ID: 2, UID: "u2"}}
	before, beforePins := uidRows(t, d)
	for name, bad := range map[string][]addonkit.User{"empty": {}, "no uid": {{ID: 1, UID: "u1"}, {ID: 2}}} {
		if err := MapUserUIDs(ctx, d, list(bad...)); err == nil {
			t.Fatalf("%s: no error", name)
		}
		if f, p := uidRows(t, d); !equalRows(f, before) || !equalRows(p, beforePins) {
			t.Fatalf("%s changed rows: %v %v", name, f, p)
		}
	}
	if err := MapUserUIDs(ctx, d, list(profiles...)); err != nil {
		t.Fatal(err)
	}
	want := map[int64][3]any{1: {int64(1), "u1", 0}, 2: {int64(2), "u2", 0}, 3: {int64(0), "", 1}, 4: {int64(0), "", 0}}
	wantPins := map[int64][3]any{1: {int64(2), "u2", 0}, 2: {int64(0), "", 0}}
	f, p := uidRows(t, d)
	if !equalRows(f, want) || !equalRows(p, wantPins) {
		t.Fatalf("mapped follows %v pins %v", f, p)
	}
	// the downloads: the same (torrents: TestBoot_MapsUserUIDsAfterImport)
	for ih, w := range map[string][2]any{"a1": {int64(1), "u1"}, "a9": {int64(0), ""}} {
		var by sql.NullInt64
		var uid sql.NullString
		if err := d.QueryRow(`SELECT added_by, added_by_uid FROM acquisitions WHERE infohash=?`, ih).Scan(&by, &uid); err != nil ||
			by.Int64 != w[0] || uid.String != w[1] {
			t.Fatalf("acquisition %s: %v %v %v", ih, by, uid, err)
		}
	}
	// a second start (profile 1 since re-created under another uid, a
	// new id-only row as no write path makes it): nothing changes
	if _, err := d.Exec(`INSERT INTO follows (id, kind, tmdb_id, title, user_id, created_at) VALUES (5, 'movie', 5, 'e', 2, 't')`); err != nil {
		t.Fatal(err)
	}
	f, p = uidRows(t, d)
	if err := MapUserUIDs(ctx, d, func(context.Context) ([]addonkit.User, error) { t.Fatal("the core asked again"); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if f2, p2 := uidRows(t, d); !equalRows(f2, f) || !equalRows(p2, p) {
		t.Fatalf("second run changed rows: %v %v", f2, p2)
	}
}

// TestMapUserUIDs_AfterImport: rows an import brings after a first
// mapping (a start without the import before) are mapped too.
func TestMapUserUIDs_AfterImport(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d := e.svc.DB
	// a new database: nothing to map, the core is not asked
	if err := MapUserUIDs(ctx, d, func(context.Context) ([]addonkit.User, error) { return nil, errors.New("asked") }); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO import_state (id, source, stamp, counts_json, phase, fingerprint, updated_at) VALUES (1, '/v/vault.db', 'later', '{}', 1, 'fp', 'later');
		INSERT INTO follows (id, kind, tmdb_id, title, user_id, created_at) VALUES (1, 'series', 1, 'a', 1, 't')`); err != nil {
		t.Fatal(err)
	}
	if err := MapUserUIDs(ctx, d, list(addonkit.User{ID: 1, UID: "u1"})); err != nil {
		t.Fatal(err)
	}
	if f, _ := uidRows(t, d); f[1] != [3]any{int64(1), "u1", 0} {
		t.Fatalf("imported row %v", f[1])
	}
}

// TestForgetGone_UID: ForgetGone keys by uid: a row whose uid the list
// does not have is the deleted profile's, though its id is there; pins
// lose their pinned_by the same way.
func TestForgetGone_UID(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	d := e.svc.DB
	if _, err := d.Exec(`INSERT INTO follows (id, kind, tmdb_id, title, user_id, user_uid, created_at) VALUES
		(1, 'series', 1, 'a', 3, 'A', 't'), (2, 'series', 2, 'b', 4, 'C', 't');
		INSERT INTO pinned_releases (id, title_id, infohash, pinned_by, pinned_by_uid, pinned_at) VALUES (1, 1, 'x', 3, 'A', 't'), (2, 2, 'y', 4, 'C', 't')`); err != nil {
		t.Fatal(err)
	}
	if err := ForgetGone(ctx, d, []addonkit.User{{ID: 3, UID: "B"}, {ID: 4, UID: "C"}}); err != nil {
		t.Fatal(err)
	}
	f, p := uidRows(t, d)
	if !equalRows(f, map[int64][3]any{1: {int64(0), "", 1}, 2: {int64(4), "C", 0}}) ||
		!equalRows(p, map[int64][3]any{1: {int64(0), "", 0}, 2: {int64(4), "C", 0}}) {
		t.Fatalf("follows %v pins %v", f, p)
	}
}

// list is a fixed GET /users.
func list(us ...addonkit.User) func(context.Context) ([]addonkit.User, error) {
	return func(context.Context) ([]addonkit.User, error) { return us, nil }
}

func equalRows(a, b map[int64][3]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
