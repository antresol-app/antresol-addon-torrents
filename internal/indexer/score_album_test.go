package indexer

import (
	"strconv"
	"strings"
	"testing"

	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/release"
)

// albumWant is a music request for one album of a year (0 = any), with
// the catalog's aliases.
func albumWant(artist, album string, year int, aliases ...string) Wanted {
	w := mediaWanted("music", artist, album)
	w.Year, w.Aliases = year, aliases
	return w
}

func TestScoreAlbumTitleInArtist(t *testing.T) {
	cases := []struct {
		name  string
		want  Wanted
		title string
		ok    bool
	}{
		// Metallica, «Metallica» (1991): the Black Album
		{"st twice", albumWant("Metallica", "Metallica", 1991), "Metallica - Metallica - 1991, FLAC (tracks+.cue), lossless", true},
		{"st nickname", albumWant("Metallica", "Metallica", 1991), "Metallica (Black Album) [FLAC]", true},
		{"st the nickname", albumWant("Metallica", "Metallica", 1991), "(Thrash Metal) Metallica - The Black Album (1991) [MP3, 320 kbps]", true},
		{"st mark", albumWant("Metallica", "Metallica", 1991), "Metallica - S/T [FLAC]", true},
		{"st before year", albumWant("Metallica", "Metallica", 1991), "Metallica (1991) [FLAC]", true},
		{"st remaster", albumWant("Metallica", "Metallica", 1991), "Metallica - Metallica (1991) [2021 Remastered Deluxe Box] FLAC", true},
		{"st alias", albumWant("Metallica", "Metallica", 1991, "The Black Album"), "Metallica - Black Album [FLAC]", true},
		{"72 seasons", albumWant("Metallica", "Metallica", 1991), "Metallica - 72 Seasons - 2023, FLAC (tracks+.cue), lossless", false},
		{"72 seasons no year", albumWant("Metallica", "Metallica", 1991), "Metallica - 72 Seasons [FLAC]", false},
		{"master of puppets", albumWant("Metallica", "Metallica", 1991), "Metallica - Master of Puppets (1986) [FLAC]", false},
		{"discography", albumWant("Metallica", "Metallica", 1991), "Metallica - Discography (1983-2016) [FLAC]", false},
		{"st other year", albumWant("Metallica", "Metallica", 1991), "Metallica - Metallica (2023) [FLAC]", false},
		{"st second year, no reissue", albumWant("Metallica", "Metallica", 1991), "Metallica - Metallica (1991) [2008, Vertigo, 24bit] FLAC", true},
		{"st second year first", albumWant("Metallica", "Metallica", 1991), "Metallica - Metallica [2008 Vertigo] - 1991, FLAC (tracks+.cue)", true},
		{"st only other years", albumWant("Metallica", "Metallica", 1991), "Metallica - Metallica [2008, Vertigo, 24bit] FLAC", true}, // 24bit: a reissue, demoted
		{"dsotm vinyl rip", albumWant("Pink Floyd", "The Dark Side of the Moon", 1973), "Pink Floyd - The Dark Side of the Moon (1973) [2016, Vinyl Rip, 24/96] FLAC", true},
		{"dsotm other year only", albumWant("Pink Floyd", "The Dark Side of the Moon", 1973), "Pink Floyd - The Dark Side of the Moon (2003 SACD) FLAC", true}, // SACD: a reissue, demoted
		{"dsotm other year unmarked", albumWant("Pink Floyd", "The Dark Side of the Moon", 1973), "Pink Floyd - The Dark Side of the Moon (2003) FLAC", false},
		{"st only other year unmarked", albumWant("Metallica", "Metallica", 1991), "Metallica - Metallica [2008, Vertigo] FLAC", false},
		{"st remaster without the original", albumWant("Metallica", "Metallica", 1991), "Metallica - Metallica (2021 Remaster) FLAC", true},

		// scene-style separators
		{"scene hyphens", albumWant("Metallica", "Metallica", 1991), "Metallica-Metallica-1991-FLAC", true},
		{"scene underscores", albumWant("Metallica", "Metallica", 1991), "Metallica_-_Metallica_(1991)_FLAC", true},
		{"scene dots", albumWant("Metallica", "Metallica", 1991), "Metallica.-.Metallica.(1991).FLAC", true},
		{"scene other album", albumWant("Metallica", "Metallica", 1991), "Metallica-72_Seasons-2023-FLAC", false},

		// aliases: generic ones are ignored, numbers match roman and arabic,
		// a word part alone is enough
		{"generic alias studio album", albumWant("Metallica", "Metallica", 1991, "studio album"), "Metallica - Metallica [Black Album] (1991) [FLAC]", true},
		{"generic alias remastered", albumWant("Metallica", "Metallica", 1991, "Remastered"), "Metallica - Metallica [Black Album] (1991) [FLAC]", true},
		{"generic alias debut", albumWant("Led Zeppelin", "Led Zeppelin", 1969, "debut"), "Led Zeppelin - Led Zeppelin I (1969)", true},
		{"lz i", albumWant("Led Zeppelin", "Led Zeppelin", 1969), "Led Zeppelin - Led Zeppelin I (1969) [FLAC]", true},
		{"pg security", albumWant("Peter Gabriel", "Peter Gabriel", 1982, "4: Security"), "Peter Gabriel - Security (1982)", true},
		{"pg roman one", albumWant("Peter Gabriel", "Peter Gabriel", 1977, "1: Car"), "Peter Gabriel - Peter Gabriel I (1977)", true},
		{"pg 1 alias car", albumWant("Peter Gabriel", "Peter Gabriel", 1977, "Car"), "Peter Gabriel - Peter Gabriel 1 (1977)", true},
		{"pg 4 for security", albumWant("Peter Gabriel", "Peter Gabriel", 1982, "4: Security"), "Peter Gabriel - Peter Gabriel IV (1982) [FLAC]", true},
		{"pg 3 for security", albumWant("Peter Gabriel", "Peter Gabriel", 1982, "4: Security"), "Peter Gabriel - Peter Gabriel 3 (Melt) (1980)", false},
		{"generic alias live", albumWant("Nirvana", "MTV Unplugged in New York", 1994, "live"), "Nirvana - From the Muddy Banks of the Wishkah (Live) (1996)", false},
		{"generic alias compilation", albumWant("Pixies", "Surfer Rosa", 1988, "compilation"), "Pixies - Wave of Mutilation (compilation) [FLAC]", false},
		{"scene hyphenated artist", albumWant("Blink-182", "Blink-182", 2003), "Blink-182-Blink-182-2003-FLAC", true},
		{"scene hyphenated artist other album", albumWant("Blink-182", "Blink-182", 2003), "Blink-182-Enema_of_the_State-1999-FLAC", false},
		{"beatles 1 pronoun", albumWant("The Beatles", "1", 2000), "The Beatles - I Feel Fine (single) [FLAC]", false},
		{"beatles 1 pronoun 2", albumWant("The Beatles", "1", 2000), "The Beatles - I Want to Hold Your Hand [FLAC]", false},
		{"beatles 1", albumWant("The Beatles", "1", 2000), "The Beatles - 1 (2000) [FLAC]", true},
		{"trailing i", albumWant("Led Zeppelin", "Led Zeppelin I", 1969), "Led Zeppelin - Led Zeppelin 1 (1969) [FLAC]", true},
		{"mfsl", albumWant("Nirvana", "Nevermind", 1991), "Nirvana - Nevermind (MFSL UDCD 666) 1996", true},
		{"shm-cd", albumWant("Nirvana", "Nevermind", 1991), "Nirvana - Nevermind (Japan SHM-CD) 2008", true},
		{"vinyl", albumWant("Nirvana", "Nevermind", 1991), "Nirvana - Nevermind [Vinyl] 2015", true},
		{"lp", albumWant("Nirvana", "Nevermind", 1991), "Nirvana - Nevermind (LP) 2015", true},
		{"melodiya", albumWant("Кино", "Группа крови", 1988), "Кино - Группа крови (Мелодия) 1990", true},
		{"24bit", albumWant("Nirvana", "Nevermind", 1991), "Nirvana - Nevermind [24bit/96kHz] 2011", true},
		{"numeral title arabic", albumWant("Led Zeppelin", "Led Zeppelin II", 1969), "Led Zeppelin - Led Zeppelin 2 (1969) [FLAC]", true},
		{"numeral title roman", albumWant("Peter Gabriel", "Peter Gabriel 4", 1982), "Peter Gabriel - Peter Gabriel IV (1982) [FLAC]", true},

		// Weezer: «Weezer» 1994 (Blue Album) and 2001 (Green Album)
		{"weezer blue", albumWant("Weezer", "Weezer", 1994), "Weezer - Weezer (Blue Album) - 1994, FLAC", true},
		{"weezer green by year", albumWant("Weezer", "Weezer", 1994), "Weezer - Weezer (Green Album) - 2001, FLAC", false},
		{"weezer blue alias", albumWant("Weezer", "Weezer", 0, "Blue Album"), "Weezer (Blue Album) [FLAC]", true},
		{"weezer green alias", albumWant("Weezer", "Weezer", 0, "Blue Album"), "Weezer (Green Album) [FLAC]", false},
		{"weezer green wanted", albumWant("Weezer", "Weezer", 2001, "Green Album"), "Weezer - Weezer (Green Album) - 2001, FLAC", true},

		// Peter Gabriel 1/2/3, all called «Peter Gabriel»
		{"pg1", albumWant("Peter Gabriel", "Peter Gabriel", 1977), "Peter Gabriel - Peter Gabriel 1 (Car) - 1977, FLAC", true},
		{"pg2 for pg1", albumWant("Peter Gabriel", "Peter Gabriel", 1977), "Peter Gabriel - Peter Gabriel 2 (Scratch) - 1978, FLAC", false},
		{"pg3 for pg1", albumWant("Peter Gabriel", "Peter Gabriel", 1977), "Peter Gabriel - Peter Gabriel 3 (Melt) - 1980, FLAC", false},
		{"pg2 by alias", albumWant("Peter Gabriel", "Peter Gabriel", 1978, "2: Scratch"), "Peter Gabriel - Peter Gabriel 2 (Scratch) - 1978, FLAC", true},
		{"pg1 for pg2 alias", albumWant("Peter Gabriel", "Peter Gabriel", 1978, "2: Scratch"), "Peter Gabriel - Peter Gabriel 1 (Car) - 1977, FLAC", false},

		// Led Zeppelin vs Led Zeppelin II (both 1969)
		{"lz", albumWant("Led Zeppelin", "Led Zeppelin", 1969), "Led Zeppelin - Led Zeppelin (1969) [2014 Remaster, Deluxe Edition] FLAC", true},
		{"lz ii for lz", albumWant("Led Zeppelin", "Led Zeppelin", 1969), "Led Zeppelin - Led Zeppelin II (1969) [FLAC]", false},
		{"lz iv for lz", albumWant("Led Zeppelin", "Led Zeppelin", 1969), "Led Zeppelin - Led Zeppelin IV (1971) [FLAC]", false},
		{"lz ii wanted", albumWant("Led Zeppelin", "Led Zeppelin II", 1969), "Led Zeppelin - Led Zeppelin II (1969) [FLAC]", true},

		// Bad Company, Korn
		{"bad company", albumWant("Bad Company", "Bad Company", 1974), "Bad Company - Bad Company - 1974, FLAC (image+.cue)", true},
		{"bad company other", albumWant("Bad Company", "Bad Company", 1974), "Bad Company - Straight Shooter (1975) [FLAC]", false},
		{"bad company discography", albumWant("Bad Company", "Bad Company", 1974), "Bad Company - Discography (1974-2002) MP3", false},
		{"korn", albumWant("Korn", "Korn", 1994), "Korn - Korn (1994) [FLAC]", true},
		{"korn follow the leader", albumWant("Korn", "Korn", 1994), "Korn - Follow the Leader (1998) [FLAC]", false},
		{"korn iii", albumWant("Korn", "Korn", 1994), "Korn - Korn III: Remember Who You Are (2010) [FLAC]", false},

		// not self-titled: unchanged, a live CD of another year is fine
		{"dsotm deluxe", albumWant("Pink Floyd", "The Dark Side of the Moon", 1973), "Pink Floyd - The Dark Side of the Moon (Deluxe Edition, 2CD incl. Live at Wembley 1974) - 1973, FLAC", true},
		{"dsotm remaster", albumWant("Pink Floyd", "The Dark Side of the Moon", 1973), "Pink Floyd - The Dark Side of the Moon [1973/2011 Remaster, Experience Edition] FLAC", true},
		{"dsotm other album", albumWant("Pink Floyd", "The Dark Side of the Moon", 1973), "Pink Floyd - Wish You Were Here (1975) [FLAC]", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, why := Score(Result{Title: c.title, Seeders: 10}, release.Parse(c.title), c.want, DefaultPrefs())
			if got := s != Excluded; got != c.ok {
				t.Fatalf("%q: accepted %v, want %v (%d %v)", c.title, got, c.ok, s, why)
			}
		})
	}
}

func TestScoreAlbumReissuesDemoted(t *testing.T) {
	for _, c := range []struct {
		artist, album string
		year          int
		title         string
	}{
		{"Pink Floyd", "The Wall", 1979, "Pink Floyd - The Wall (2011 Remaster)"},
		{"Pink Floyd", "The Wall", 1979, "Pink Floyd - The Wall [Immersion Box Set] 2012"},
		{"Nirvana", "Nevermind", 1991, "Nirvana - Nevermind (20th Anniversary Super Deluxe Edition) [2011] FLAC"},
		{"Nirvana", "Nevermind", 1991, "Nirvana - Nevermind (Original Recording Remastered) 2021 24/192"},
		{"AC/DC", "Back in Black", 1980, "AC/DC - Back in Black (2003 Remaster)"},
		{"Van Halen", "1984", 1984, "Van Halen - 1984 (2015 Remaster)"},
		{"Prince", "1999", 1982, "Prince - 1999 (Super Deluxe) 2019"},
		{"Кино", "Группа крови", 1988, "Кино - Группа крови (Remastered 2012)"},
		{"Кино", "Группа крови", 1988, "Кино - Группа крови [Переиздание 2019, LP]"},
		{"Кино", "Группа крови", 1988, "Кино - Группа крови (Moroz Records) 1996"},
		{"Metallica", "Metallica", 1991, "Metallica - Metallica (Remastered 2021)"},
		{"Metallica", "Metallica", 1991, "Metallica - Metallica (30th Anniversary Box Set) [2021]"},
		{"Metallica", "Metallica", 1991, "Metallica - Metallica (Re-Release 2021)"},
		{"Led Zeppelin", "Led Zeppelin", 1969, "Led Zeppelin - Led Zeppelin (Remastered Deluxe Edition) 2014"},
		{"Bad Company", "Bad Company", 1974, "Bad Company - Bad Company (Deluxe Edition) 2015"},
		{"The Doors", "The Doors", 1967, "The Doors - The Doors [50th Anniversary Deluxe Edition] 2017"},
		{"Slipknot", "Slipknot", 1999, "Slipknot - Slipknot (10th Anniversary Edition) 2009"},
	} {
		want := albumWant(c.artist, c.album, c.year)
		s, why := Score(Result{Title: c.title, Seeders: 10}, release.Parse(c.title), want, DefaultPrefs())
		if s == Excluded {
			t.Errorf("%q excluded: %v", c.title, why)
			continue
		}
		orig := strings.Replace(c.title, c.album, c.album+" ("+strconv.Itoa(c.year)+")", 1)
		if c.album == c.artist {
			orig = c.title + " " + strconv.Itoa(c.year)
		}
		so, _ := Score(Result{Title: orig, Seeders: 10}, release.Parse(orig), want, DefaultPrefs())
		if so-s != reissuePenalty && c.album != "1984" { // «1984» is the wanted year itself
			t.Errorf("%q: %d, with the original year %q: %d %v", c.title, s, orig, so, why)
		}
	}
}

func TestScoreAlbumOtherYearReason(t *testing.T) {
	title := "Metallica - 72 Seasons - 2023, FLAC"
	// the title rule already rejects it; the year alone does too
	_, why := Score(Result{Title: title, Seeders: 10}, release.Parse(title), albumWant("Metallica", "72 Seasons", 1991), DefaultPrefs())
	if len(why) != 1 || why[0] != "другой год (2023)" {
		t.Fatalf("why %v", why)
	}
}

func TestAudioYears(t *testing.T) {
	years, ranges := release.AudioYears("Metallica - Discography (1983-2016) [24/96, 320 kbps] 1991/2021")
	if len(years) != 2 || years[0] != 1991 || years[1] != 2021 || len(ranges) != 1 || ranges[0] != [2]int{1983, 2016} {
		t.Fatalf("years %v ranges %v", years, ranges)
	}
}
