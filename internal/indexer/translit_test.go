package indexer

import "testing"

func TestNameSkeleton(t *testing.T) {
	for _, p := range [][2]string{
		{"roberts", "робертс"}, {"tolkien", "толкин"}, {"pratchett", "пратчетт"}, {"rowling", "роулинг"},
		{"king", "кинг"}, {"dickens", "диккенс"}, {"gregory", "грегори"}, {"orwell", "оруэлл"},
	} {
		if a, b := nameSkeleton(p[0]), nameSkeleton(p[1]); a != b {
			t.Errorf("%s %q != %s %q", p[0], a, p[1], b)
		}
	}
	if nameSkeletonsMeet([]string{"иванов"}, []string{"roberts", "shantaram"}) {
		t.Fatal("another author")
	}
	if nameSkeletonsMeet([]string{"ann"}, []string{"анна"}) {
		t.Fatal("a skeleton of two sounds tells nothing")
	}
}

func TestReleaseLang(t *testing.T) {
	for _, c := range []struct {
		r    ScoredRelease
		want string
	}{
		{ScoredRelease{Result: Result{Title: "Gregory David Roberts — Shantaram"}, Direct: true, directTag: "EPUB RUS"}, "ru"},
		{ScoredRelease{Result: Result{Title: "Грегори Робертс — Шантарам"}, Direct: true, directTag: "EPUB ENG"}, "en"},
		{ScoredRelease{Result: Result{Title: "Робертс - Шантарам [FB2]"}}, "ru"},
		{ScoredRelease{Result: Result{Title: "Shantaram [OceanofPDF.com]"}}, ""},
	} {
		if got := releaseLang(c.r); got != c.want {
			t.Errorf("%q: %q", c.r.Title, got)
		}
	}
}
