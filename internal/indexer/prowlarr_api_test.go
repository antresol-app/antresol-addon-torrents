package indexer

import (
	"context"
	"reflect"
	"testing"
)

// prowlarrAPIIndexers is GET /api/v1/indexer for the indexers of
// prowlarrFixture (the custom one aside: its definition lives in
// Prowlarr's folder, which the API does not give), as Prowlarr answers it:
// every setting a field, selects as their index, Prowlarr's own
// baseSettings beside them.
const prowlarrAPIIndexers = `[
 {"id":1,"name":"RuTracker.org (Cardigann)","implementation":"Cardigann","enable":true,"priority":10,"tags":[1],"fields":[
   {"name":"definitionFile","value":"rutracker"},{"name":"baseUrl","value":"https://rutracker.org/"},
   {"name":"username","value":"alex"},{"name":"password","value":"p@ss"},{"name":"sort","value":1},{"name":"type","value":0},
   {"name":"baseSettings.limitsUnit","value":0},{"name":"torrentBaseSettings.seedRatio"}]},
 {"id":2,"name":"RuTracker.org","implementation":"RuTracker","enable":false,"priority":25,"tags":[],"fields":[
   {"name":"username","value":"alex2"},{"name":"password","value":"pw2"},{"name":"russianLetters","value":false},{"name":"baseUrl","value":"https://rutracker.net/"}]},
 {"id":4,"name":"Kinozal (old)","implementation":"Cardigann","enable":true,"priority":30,"tags":[],"fields":[
   {"name":"definitionFile","value":"kinozal"},{"name":"username","value":"kz"},{"name":"password","value":"kzpw"}]},
 {"id":5,"name":"Some Usenet","implementation":"Newznab","enable":true,"priority":25,"tags":[],"fields":[
   {"name":"baseUrl","value":"https://nzb.example/"},{"name":"apiKey","value":"k"}]},
 {"id":6,"name":"Lost","implementation":"Cardigann","enable":true,"priority":25,"tags":[],"fields":[{"name":"definitionFile","value":"does-not-exist"}]}
]`

const prowlarrAPITags = `[{"id":1,"label":"FlareSolverr"},{"id":2,"label":"ru"}]`

// The API path adds the same sources the database path adds.
func TestProwlarrAPIMatchesDatabase(t *testing.T) {
	ctx := context.Background()
	fromDB := newEnv(t)
	fromDB.reload()
	dbList, err := fromDB.m.ImportProwlarr(ctx, prowlarrFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	list, err := ParseProwlarrAPI([]byte(prowlarrAPIIndexers), []byte(prowlarrAPITags))
	if err != nil {
		t.Fatal(err)
	}
	if !list[0].FlareSolverr || list[1].FlareSolverr || list[1].Enable || list[0].Priority != 10 {
		t.Fatalf("parsed = %+v", list)
	}
	fromAPI := newEnv(t)
	fromAPI.reload()
	apiList, err := fromAPI.m.ImportProwlarrList(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	type view struct {
		Def      string
		Settings map[string]string
		Enabled  bool
		Priority int
		Reason   string
	}
	see := func(ims []Imported) map[string]view {
		out := map[string]view{}
		for _, im := range ims {
			v := view{Reason: im.Reason}
			if im.Source != nil {
				v = view{Def: im.Source.DefinitionID, Settings: im.Source.Settings, Enabled: im.Source.Enabled, Priority: im.Source.Priority}
			}
			out[im.Name] = v
		}
		return out
	}
	want, got := see(dbList), see(apiList)
	delete(want, "Мой трекер") // a custom definition: only in Prowlarr's folder
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("API import\n%+v\nwant (database)\n%+v", got, want)
	}

	// the preview after it: all already there, as after the database import
	again, err := fromAPI.m.PreviewProwlarrList(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	for _, im := range again {
		if im.Source != nil || (want[im.Name].Def != "" && im.Reason != ReasonAlready) {
			t.Errorf("preview again: %+v", im)
		}
	}
}

// Prowlarr masks passwords in its answers: the source is added without
// one (Hidden names it), and one already imported with it is not added
// again.
func TestProwlarrAPIHiddenPassword(t *testing.T) {
	ctx := context.Background()
	const masked = `[{"id":4,"name":"Kinozal (old)","implementation":"Cardigann","enable":true,"priority":30,"tags":[],"fields":[
	  {"name":"definitionFile","value":"kinozal"},{"name":"username","value":"kz"},{"name":"password","value":"********","privacy":"password"}]}]`
	list, err := ParseProwlarrAPI([]byte(masked), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list[0].Hidden, []string{"password"}) {
		t.Fatalf("hidden = %v", list[0].Hidden)
	}

	fresh := newEnv(t)
	fresh.reload()
	got, err := fresh.m.ImportProwlarrList(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Source == nil || got[0].Source.Settings["username"] != "kz" || got[0].Source.Settings["password"] != "" ||
		!reflect.DeepEqual(got[0].Hidden, []string{"password"}) {
		t.Fatalf("fresh = %+v", got)
	}

	had := newEnv(t)
	had.reload()
	if _, err := had.m.ImportProwlarr(ctx, prowlarrFixture(t)); err != nil {
		t.Fatal(err)
	}
	again, err := had.m.ImportProwlarrList(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Source != nil || again[0].Reason != ReasonAlready {
		t.Fatalf("again = %+v", again)
	}
	srcs, _ := had.m.Sources(ctx)
	for _, s := range srcs {
		if s.DefinitionID == "kinozal" && s.Settings["password"] != "kzpw" {
			t.Fatalf("the source lost its password: %+v", s)
		}
	}
}
