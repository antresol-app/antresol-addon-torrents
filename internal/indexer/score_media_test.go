package indexer

import (
	"slices"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// rank scores releases for want and returns the accepted titles best
// first and the reasons of the rejected ones.
func rank(t *testing.T, want Wanted, p Prefs, titles ...string) (accepted []string, rejected map[string]string) {
	t.Helper()
	type sc struct {
		title string
		score int
	}
	var ok []sc
	rejected = map[string]string{}
	for _, title := range titles {
		r := Result{Title: title, Seeders: 10}
		s, why := Score(r, release.Parse(title), want, p)
		if s == Excluded {
			rejected[title] = strings.Join(why, "; ")
			continue
		}
		ok = append(ok, sc{title, s})
	}
	slices.SortStableFunc(ok, func(a, b sc) int { return b.score - a.score })
	for _, s := range ok {
		accepted = append(accepted, s.title)
	}
	return accepted, rejected
}

// mediaWanted is what Manager.Search builds from acquire's media request.
func mediaWanted(kind, artist, work string) Wanted {
	text := strings.TrimSpace(artist + " " + work)
	return Wanted{Kind: kind, Titles: []string{text, work}, Artist: artist, Work: work}
}

func musicWant(artist, album string) Wanted { return mediaWanted("music", artist, album) }

func TestScoreMusicPrefersFLAC(t *testing.T) {
	flac := "Кино - Группа крови - 1988, FLAC (tracks+.cue), lossless"
	mp3 := "Кино - Группа крови (1988) [MP3, 320 kbps]"
	low := "Кино - Группа крови (1988) [MP3, 128 kbps]"
	disco := "Кино - Дискография (1982-1990), FLAC, Группа крови и другие"
	other := "Кино - Дискография (1982-1990), FLAC"
	got, rej := rank(t, musicWant("Кино", "Группа крови"), DefaultPrefs(), low, disco, mp3, flac, other)
	// the discography that names the album stays, 20 below the same format
	// alone; MP3 is another format than the default lossless: last
	if !slices.Equal(got, []string{flac, disco, mp3, low}) {
		t.Fatalf("order %q", got)
	}
	if rej[other] != "другое название" || len(rej) != 1 {
		t.Fatalf("rejected %v", rej)
	}
	sf, _ := Score(Result{Title: flac, Seeders: 10}, release.Parse(flac), musicWant("Кино", "Группа крови"), DefaultPrefs())
	sd, why := Score(Result{Title: disco, Seeders: 10}, release.Parse(disco), musicWant("Кино", "Группа крови"), DefaultPrefs())
	if sf-sd != 20 || !slices.Contains(why, "дискография — альбом в её составе") {
		t.Fatalf("flac %d disco %d %v", sf, sd, why)
	}
}

func TestScoreMusicArtistWantsDiscography(t *testing.T) {
	want := mediaWanted("music", "Ария", "")
	disco := "Ария - Дискография (1985-2014) [MP3, 320 kbps]"
	album := "Ария - Герой асфальта (1987) [MP3, 320 kbps]"
	got, _ := rank(t, want, DefaultPrefs(), album, disco, "Кино - Дискография [MP3, 320 kbps]")
	if !slices.Equal(got, []string{disco, album}) {
		t.Fatalf("order %q", got)
	}
}

func TestScoreMusicCollectorsEditionIsOneAlbum(t *testing.T) {
	for _, title := range []string{
		"Кино - Группа крови (Коллекционное издание) - 1988, FLAC",
		"Кино - Группа крови (Collector's Edition) - 1988, FLAC",
		"Кино - Группа крови (Collection Edition) - 1988, FLAC",
	} {
		s, why := Score(Result{Title: title, Seeders: 5}, release.Parse(title), musicWant("Кино", "Группа крови"), DefaultPrefs())
		if s == Excluded || slices.ContainsFunc(why, func(r string) bool { return strings.Contains(r, "дискография") }) {
			t.Errorf("%q: %d %v", title, s, why)
		}
	}
}

func TestScoreMusicArtistStopwords(t *testing.T) {
	_, rej := rank(t, musicWant("The Beatles", "Abbey Road"), DefaultPrefs(), "The Rolling Stones - Abbey Road Sessions FLAC")
	if len(rej) != 1 {
		t.Fatal("The Rolling Stones accepted for The Beatles")
	}
	if got, _ := rank(t, musicWant("The The", "Soul Mining"), DefaultPrefs(), "The The - Soul Mining (1983) FLAC"); len(got) != 1 {
		t.Fatal("The The rejected")
	}
}

func TestScoreMusicLossyPreference(t *testing.T) {
	flac := "Кино - Группа крови - 1988, FLAC (tracks+.cue)"
	mp3 := "Кино - Группа крови (1988) [MP3, 320 kbps]"
	low := "Кино - Группа крови (1988) [MP3, 128 kbps]"
	p := DefaultPrefs()
	if !p.PreferLossless {
		t.Fatal("PreferLossless must default to true")
	}
	p.PreferLossless = false
	got, _ := rank(t, musicWant("Кино", "Группа крови"), p, flac, low, mp3)
	if !slices.Equal(got, []string{mp3, flac, low}) {
		t.Fatalf("order %q", got)
	}
}

func TestScoreMusicOtherArtistOrAlbum(t *testing.T) {
	_, rej := rank(t, musicWant("Кино", "Группа крови"), DefaultPrefs(),
		"Алиса - Группа крови (2020) FLAC", "Кино - Звезда по имени Солнце (1989) FLAC")
	if len(rej) != 2 || rej["Алиса - Группа крови (2020) FLAC"] != "другое название" {
		t.Fatalf("rejected %v", rej)
	}
}

func TestScoreMusicTitleWithCollectionWord(t *testing.T) {
	// the album itself is called «The Collection»: not a discography
	got, _ := rank(t, musicWant("Alan Parsons", "The Collection"), DefaultPrefs(), "Alan Parsons - The Collection (1990) FLAC")
	if len(got) != 1 {
		t.Fatalf("accepted %q", got)
	}
}

func TestScoreBooksEPUBOverFB2OverPDF(t *testing.T) {
	want := mediaWanted("book", "Булгаков", "Мастер и Маргарита")
	epub := "Булгаков М. - Мастер и Маргарита [1967, EPUB]"
	fb2 := "Булгаков М. - Мастер и Маргарита [1967, FB2]"
	pdf := "Булгаков М. - Мастер и Маргарита [1967, PDF]"
	fb2ru := "Булгаков М. - Мастер и Маргарита [1967, FB2, RUS]"
	got, _ := rank(t, want, DefaultPrefs(), pdf, fb2, epub)
	if !slices.Equal(got, []string{epub, fb2, pdf}) {
		t.Fatalf("order %q", got)
	}
	// the language is the badge's: a tag, else Cyrillic letters (Russian)
	fb2en := "Булгаков М. - Мастер и Маргарита [1967, FB2, ENG]"
	got, _ = rank(t, want, DefaultPrefs(), fb2en, fb2ru)
	if got[0] != fb2ru {
		t.Fatalf("Russian not preferred: %q", got)
	}
}

func TestScoreDJVUExcluded(t *testing.T) {
	want := mediaWanted("book", "Булгаков", "Мастер и Маргарита")
	djvu := "Булгаков М. - Мастер и Маргарита [1967, DJVU]"
	both := "Булгаков М. - Мастер и Маргарита [1967, DJVU, PDF]"
	doc := "Булгаков М. - Мастер и Маргарита [DOC, RTF]"
	got, rej := rank(t, want, DefaultPrefs(), djvu, both, doc)
	if rej[djvu] != "Формат не открывается в Antresol" || rej[doc] != "Формат не открывается в Antresol" || !slices.Equal(got, []string{both}) {
		t.Fatalf("accepted %q rejected %v", got, rej)
	}
}

func TestScoreAudiobookM4B(t *testing.T) {
	want := mediaWanted("audiobook", "Лем", "Солярис")
	m4b := "Лем Станислав - Солярис [Иванов, 2019, M4B]"
	mp3 := "Лем Станислав - Солярис [Иванов, 2019, 128 kbps, MP3]"
	got, _ := rank(t, want, DefaultPrefs(), mp3, m4b)
	if !slices.Equal(got, []string{m4b, mp3}) {
		t.Fatalf("order %q", got)
	}
}

func TestScoreMangaLanguage(t *testing.T) {
	want := mediaWanted("comic", "", "Берсерк")
	rus := "Берсерк / Berserk [RUS] Том 1-41 (CBZ)"
	eng := "Берсерк / Berserk [ENG] Том 1-41 (CBZ)"
	plain := "Берсерк / Berserk Том 1-41 (CBZ)"
	raw := "Берсерк / Berserk [JAP] RAW (CBZ)"
	cbr := "Берсерк / Berserk Том 1-41 (CBR)"
	pdf := "Берсерк / Berserk Том 1-41 (PDF)"
	got, rej := rank(t, want, DefaultPrefs(), pdf, cbr, plain, raw, eng, rus) // plain and raw tie: input order
	if !slices.Equal(got, []string{rus, eng, plain, raw, cbr}) {
		t.Fatalf("order %q", got)
	}
	if rej[pdf] != "Формат не открывается в читалке комиксов" {
		t.Fatalf("rejected %v", rej)
	}
	// on a Russian tracker an unlabeled release is the Russian translation
	want.DefaultLang = "ru"
	got, _ = rank(t, want, DefaultPrefs(), raw, eng, plain)
	if !slices.Equal(got, []string{plain, eng, raw}) {
		t.Fatalf("russian tracker order %q", got)
	}
}

func TestScoreMediaIgnoresMaxSizeAndCountsSeeders(t *testing.T) {
	p := DefaultPrefs()
	p.MaxSizeGB = 1
	want := musicWant("Pink Floyd", "The Wall")
	big := Result{Title: "Pink Floyd - The Wall (1979) FLAC 24bit", Size: 5 << 30, Seeders: 100}
	small := Result{Title: "Pink Floyd - The Wall (1979) FLAC", Size: 1 << 20, Seeders: 1}
	sb, _ := Score(big, release.Parse(big.Title), want, p)
	ss, _ := Score(small, release.Parse(small.Title), want, p)
	if sb == Excluded || ss == Excluded || sb <= ss {
		t.Fatalf("big %d small %d", sb, ss)
	}
	none := Result{Title: small.Title}
	if s, why := Score(none, release.Parse(none.Title), want, p); s != Excluded || why[0] != "нет раздающих" {
		t.Fatalf("no seeders: %d %v", s, why)
	}
}

func TestMediaMatchesShortArtist(t *testing.T) {
	// "AC/DC" has no word of 3 letters: all its words must be in the name
	want := musicWant("AC/DC", "Back in Black")
	for title, ok := range map[string]bool{
		"AC/DC - Back in Black (1980) FLAC": true,
		"Back in Black (1980) Tribute FLAC": false,
	} {
		s, _ := Score(Result{Title: title, Seeders: 5}, release.Parse(title), want, DefaultPrefs())
		if (s != Excluded) != ok {
			t.Errorf("%q: score %d", title, s)
		}
	}
}

// Release names recorded from the preview's sources (October 2026).
func TestScoreAudiobookRecorded(t *testing.T) {
	type c struct {
		want   Wanted
		accept []string
		reject map[string]string // title -> a part of the reason
	}
	mm := mediaWanted("audiobook", "Михаил Афанасьевич Булгаков", "Мастер и Маргарита")
	phm := mediaWanted("audiobook", "Andy Weir", "Project Hail Mary")
	phm.Aliases = []string{"Проект «Аве Мария»"}
	hp := mediaWanted("audiobook", "Джоан Роулинг", "Гарри Поттер и философский камень")
	hp.Aliases = []string{"Harry Potter and the Philosopher's Stone"}
	metro := mediaWanted("audiobook", "Дмитрий Глуховский", "Метро 2033")
	metro.Aliases = []string{"Metro 2033"}
	dune := mediaWanted("audiobook", "Frank Herbert", "Dune")
	dune.Aliases = []string{"Дюна"}
	for _, tc := range []c{
		{mm, []string{
			"Булгаков Михаил - Мастер и Маргарита [Леонид Ярмольник, 2024, 320 kbps, MP3]",
			"Булгаков Михаил - Мастер и Маргарита (Аудиоспектакль) [2022, режиссер Алексей Багдасаров, разные исполнители]",
		}, map[string]string{
			"Мастер и Маргарита (Михаил Локшин) [2023, Россия, драма, фэнтези, WEB-DL 1080p] Original (Rus)": "видео",
		}},
		{phm, []string{
			"Вейер Энди – Проект «Аве Мария» [Игорь Князев, 2022, 128 kbps, MP3]",
			"Энди Вейер - Проект «Аве Мария» (2022) MP3",
			"[Английский] Weir Andy / Вейер Энди - Project Hail Mary / Проект «Аве Мария» [Ray Porter, 2021, MP3, 128 kbps]",
			"Project Hail Mary - Andy Weir [M4B 128 Kbps] [English]",
		}, map[string]string{
			"Проект «Конец света» / Project Hail Mary (2026) WEB-DL 1080p-MovieDalen": "",
		}},
		{hp, []string{
			"Harry Potter and the Philosopher's Stone [Audio Book Greek]",
			"Harry Potter And The Philosopher's Stone Audiobook Jim Dale amey",
			"Джоан Роулинг | Гарри Поттер и философский камень [Книга 1] (2006) [m4b|AAC]",
		}, map[string]string{
			"Гарри Поттер и философский камень / Harry Potter and the Philosopher's Stone (Крис Коламбус / Chris Columbus) [2001, WEB-DL 1080p]": "",
		}},
		{metro, []string{
			"Глуховский Дмитрий - Метро 2033 [Алексей Данков, 2020, 80 kbps, MP3]",
		}, map[string]string{
			"Метро 2033 / Metro 2033 (2010) PC | Steam-Rip от R.G. Origins": "",
			"Метро 2033 [Дмитрий Глуховский] (2007) FB2":                    "электронная книга",
		}},
		{dune, []string{
			"Герберт Фрэнк - Дюна 1, Дюна [Сергей Чонишвили, 2021, 128 kbps, MP3]",
		}, map[string]string{
			"Дюна / Dune (Дени Вильнев / Denis Villeneuve) [2021, США, фантастика, BDRip 1080p]": "",
		}},
	} {
		var all []string
		all = append(all, tc.accept...)
		for k := range tc.reject {
			all = append(all, k)
		}
		got, rej := rank(t, tc.want, DefaultPrefs(), all...)
		for _, a := range tc.accept {
			if !slices.Contains(got, a) {
				t.Errorf("%s: rejected %q: %s", tc.want.Work, a, rej[a])
			}
		}
		for title, why := range tc.reject {
			if r, ok := rej[title]; !ok || !strings.Contains(r, why) {
				t.Errorf("%s: %q: want rejected (%s), got %q", tc.want.Work, title, why, r)
			}
		}
	}
}

func TestShortSkeletons(t *testing.T) {
	if !shortSkeletonsMeet([]string{"weir"}, []string{"вейер", "энди"}) {
		t.Fatal("Weir = Вейер")
	}
	if nameSkeleton("rowling") != nameSkeleton("роулинг") {
		t.Fatal("w is still dropped for the long skeleton")
	}
	if shortSkeletonsMeet([]string{"weir"}, []string{"мир"}) {
		t.Fatal("another name")
	}
}

// «Языки» of books: the viewer's order wins; a language not chosen is
// still offered, below.
func TestScoreBooksFollowTheViewersLanguages(t *testing.T) {
	want := mediaWanted("book", "Bulgakov", "Master and Margarita")
	ru := "Bulgakov - Master and Margarita [1967, FB2, RUS]"
	en := "Bulgakov - Master and Margarita [1967, FB2, ENG]"
	ja := "Bulgakov - Master and Margarita [1967, FB2, JPN]"
	p := DefaultPrefs()
	p.BookLangs = []string{"en", "ru"}
	got, rej := rank(t, want, p, ru, ja, en)
	if !slices.Equal(got, []string{en, ru, ja}) || len(rej) != 0 {
		t.Fatalf("order %q, rejected %v", got, rej)
	}
}

// «Озвучка» in order: original first, then the dub.
func TestScoreVoicesInOrder(t *testing.T) {
	want := Wanted{Titles: []string{"Дюна", "Dune"}, Year: 2021, Kind: "movie"}
	dub := "Дюна / Dune (2021) WEB-DL 1080p | Дубляж"
	orig := "Dune (2021) WEB-DL 1080p Original"
	p := DefaultPrefs()
	p.Voices = []string{"original", "dub"}
	got, _ := rank(t, want, p, dub, orig)
	if !slices.Equal(got, []string{orig, dub}) {
		t.Fatalf("original first: %q", got)
	}
	p.Voices = []string{"dub", "original"}
	if got, _ = rank(t, want, p, orig, dub); !slices.Equal(got, []string{dub, orig}) {
		t.Fatalf("dub first: %q", got)
	}
}
