package cardigann

import (
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
)

var fixedNow = time.Date(2026, 10, 25, 15, 0, 0, 0, time.Local)

func f(name string, args ...string) Filter { return Filter{Name: name, Args: StrList(args)} }

func TestFilters(t *testing.T) {
	local := func(y int, m time.Month, d, h, min int) string {
		return formatDate(time.Date(y, m, d, h, min, 0, 0, time.Local))
	}
	msk := time.FixedZone("", 3*3600)
	cases := []struct {
		name   string
		in     string
		filter Filter
		want   string
	}{
		{"replace", "1,5 ГБ", f("replace", "ГБ", "GB"), "1,5 GB"},
		{"replace templated", "x", f("replace", "x", "{{ .Today.Year }}"), "2026"},
		{"re_replace", "Сезон 2 серия 5", f("re_replace", `(?i)[CС]езоны?[\s:]*(\d+)`, "S$1E"), "S2E серия 5"},
		{"re_replace cyrillic block", "Дюна / Dune", f("re_replace", `[\p{IsCyrillic}]+`, ""), " / Dune"},
		{"re_replace lookahead", "ab ac", f("re_replace", `a(?!b)`, "X"), "ab Xc"},
		{"re_replace templated", "abc", f("re_replace", "b", "{{ if .Config.freeleech }}B{{ else }}{{ end }}"), "aBc"},
		{"regexp", "magnet:?xt=urn:btih:0123456789ABCDEF0123456789abcdef01234567&dn=x", f("regexp", "([A-F|a-f|0-9]{40})"), "0123456789ABCDEF0123456789abcdef01234567"},
		{"regexp no match", "abc", f("regexp", `(\d+)`), ""},
		{"split", "a/b/c", f("split", "/", "1"), "b"},
		{"split negative", "a/b/c", f("split", "/", "-1"), "c"},
		{"trim default", "  x  ", f("trim"), "x"},
		{"trim cutset", "--x--", f("trim", "-"), "x"},
		{"append", "Title", f("append", "{{ if .Config.freeleech }} RUS{{ else }}{{ end }}"), "Title RUS"},
		{"prepend", "x", f("prepend", "Uploader: {{ .Result._id }} "), "Uploader: 42 x"},
		{"tolower", "АБВ ABC", f("tolower"), "абв abc"},
		{"toupper", "абв abc", f("toupper"), "АБВ ABC"},
		{"urldecode", "a%20b%2Bc", f("urldecode"), "a b+c"},
		{"urlencode", "a b&c", f("urlencode"), "a+b%26c"},
		{"querystring", "/forum/tracker.php?f=913&x=1", f("querystring", "f"), "913"},
		{"querystring missing", "/viewtopic.php?t=1", f("querystring", "f"), ""},
		{"htmldecode", "a &amp; b", f("htmldecode"), "a & b"},
		{"timeago", "2 hours ago", f("timeago"), formatDate(fixedNow.Add(-2 * time.Hour))},
		{"timeago ru", "5 минут назад", f("timeago"), formatDate(fixedNow.Add(-5 * time.Minute))},
		{"reltime", "1 day, 3 hours", f("reltime"), formatDate(fixedNow.Add(-27 * time.Hour))},
		{"fuzzytime yesterday ru", "Вчера 10:00", f("fuzzytime"), local(2026, 10, 24, 10, 0)},
		{"fuzzytime today ru", "сегодня 12:30", f("fuzzytime"), local(2026, 10, 25, 12, 30)},
		{"fuzzytime today ru at", "сегодня в 12:30", f("fuzzytime"), local(2026, 10, 25, 12, 30)},
		{"fuzzytime ru month", "5 мар 24", f("fuzzytime"), local(2024, 3, 5, 0, 0)},
		{"fuzzytime ru genitive", "12 октября 2023 18:45", f("fuzzytime"), local(2023, 10, 12, 18, 45)},
		{"fuzzytime iso", "2024-01-02 03:04:05", f("fuzzytime"), formatDate(time.Date(2024, 1, 2, 3, 4, 5, 0, time.Local))},
		{"fuzzytime unix", "1700000000", f("fuzzytime"), formatDate(time.Unix(1700000000, 0))},
		{"fuzzytime dotted", "24.10.2017 23:44", f("fuzzytime"), local(2017, 10, 24, 23, 44)},
		{"fuzzytime now", "now", f("fuzzytime"), formatDate(fixedNow)},
		{"dateparse dotnet", "27 Oct 17 00:00:00 +03:00", f("dateparse", "dd MMM yy HH:mm:ss zzz"), formatDate(time.Date(2017, 10, 27, 0, 0, 0, 0, msk))},
		{"dateparse dotnet ru month", "5 мар 24", f("dateparse", "d MMM yy"), local(2024, 3, 5, 0, 0)},
		{"dateparse go layout", "2024-03-05 10:11", f("dateparse", "2006-01-02 15:04"), local(2024, 3, 5, 10, 11)},
		{"dateparse dotted", "24.10.2017 23:44", f("dateparse", "dd.MM.yyyy HH:mm"), local(2017, 10, 24, 23, 44)},
		{"dateparse rutracker", "5-3-24", f("dateparse", "d-M-yy"), local(2024, 3, 5, 0, 0)},
		{"dateparse bad keeps data", "garbage", f("dateparse", "dd.MM.yyyy"), "garbage"},
		{"timeparse", "2024-03-05", f("timeparse", "2006-01-02"), local(2024, 3, 5, 0, 0)},
		{"validfilename", `a/b:c*d?"e"<f>|g`, f("validfilename"), "a_b_c_d__e__f__g"},
		{"diacritics", "Amélie Pokémon", f("diacritics", "replace"), "Amelie Pokemon"},
		{"jsonjoinarray", `{"genres":["Drama","Crime"]}`, f("jsonjoinarray", "$.genres", ", "), "Drama, Crime"},
		{"hexdump noop", "abc", f("hexdump"), "abc"},
		{"strdump noop", "abc", f("strdump", "tag"), "abc"},
		{"validate", "Action, Drama, Foo", f("validate", "action, drama, comedy"), "action, drama"},
		{"andmatch noop", "abc", f("andmatch"), "abc"},
		{"base64decode", "aGVsbG8", f("base64decode"), "hello"},
		{"unknown filter passthrough", "abc", f("no_such_filter"), "abc"},
	}
	env := &filterEnv{vars: testVars(), now: func() time.Time { return fixedNow }}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := env.apply(c.in, []Filter{c.filter})
			if err != nil {
				t.Fatalf("%s(%q): %v", c.filter.Name, c.in, err)
			}
			if got != c.want {
				t.Fatalf("%s(%q) = %q, want %q", c.filter.Name, c.in, got, c.want)
			}
		})
	}
}

func TestFilterURLEncodingUsesDefinitionCharset(t *testing.T) {
	env := &filterEnv{enc: charmap.Windows1251, vars: testVars(), now: func() time.Time { return fixedNow }}
	got, err := env.apply("Брат", []Filter{f("urlencode")})
	if err != nil {
		t.Fatal(err)
	}
	if got != "%C1%F0%E0%F2" {
		t.Fatalf("urlencode cp1251 = %q", got)
	}
	back, err := env.apply(got, []Filter{f("urldecode")})
	if err != nil || back != "Брат" {
		t.Fatalf("urldecode cp1251 = %q, %v", back, err)
	}
}

func TestFilterSplitOutOfRangeIsError(t *testing.T) {
	env := &filterEnv{vars: testVars(), now: time.Now}
	if _, err := env.apply("a/b", []Filter{f("split", "/", "5")}); err == nil {
		t.Fatal("expected error")
	}
}
