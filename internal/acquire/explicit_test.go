package acquire

import "testing"

func TestExplicitSubjects(t *testing.T) {
	for _, c := range []struct {
		texts []string
		want  bool
	}{
		{[]string{"Fiction", "Erotica"}, true},
		{[]string{"Erotic stories"}, true},
		{[]string{"Sex instruction"}, true},
		{[]string{"Sexual behavior"}, true},
		{[]string{"Pornography"}, true},
		{[]string{"Fiction, romance, erotic"}, true},
		{[]string{"Эротическая проза"}, true},
		{[]string{"Книги для взрослых"}, true},
		{[]string{"explicit"}, true},
		{[]string{"Essex (England)", "Sextet", "Fantasy fiction", "Magic", "Детская литература"}, false},
		{[]string{"", "Live", "Compilation"}, false},
		{nil, false},
	} {
		if got := ExplicitSubjects(c.texts); got != c.want {
			t.Errorf("%q: %v", c.texts, got)
		}
	}
}
