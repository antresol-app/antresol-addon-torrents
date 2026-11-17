package coreadapt_test

import (
	"slices"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/coreadapt"
)

// The profiles as the core recorded them: names by id, the list with its
// uids, and a profile's «Языки».
func TestUsers_Replayed(t *testing.T) {
	c := replay(t)
	u := coreadapt.Users{C: c}
	if names, err := u.Names(ctx); err != nil || names[1] != "Папа" || names[2] != "Маша" {
		t.Fatalf("names %v %v", names, err)
	}
	if ps, err := u.Profiles(ctx); err != nil || len(ps) < 3 || ps[0].UID != "{uid}" || !ps[0].IsAdmin {
		t.Fatalf("profiles %+v %v", ps, err)
	}
	p := &coreadapt.Profiles{C: c}
	if l := p.Languages(ctx, 2); !slices.Equal(l, []string{"ru", "en"}) {
		t.Fatalf("languages %v", l)
	}
	if l := p.Languages(ctx, 99); l != nil {
		t.Fatalf("unknown profile %v", l)
	}
}
