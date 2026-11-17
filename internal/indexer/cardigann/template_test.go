package cardigann

import (
	"net/url"
	"testing"
)

func testVars() map[string]any {
	return map[string]any{
		"Keywords":   "Дюна 2024",
		"Categories": []string{"1", "5"},
		"True":       "True",
		"False":      "",
		"Today":      map[string]any{"Year": "2026"},
		"Config": map[string]any{
			"sitelink":     "https://example.org/",
			"freeleech":    "True",
			"stripcyr":     "",
			"sort":         "2",
			"format-tv":    "TV!",
			"apiurl":       "api.example.org",
			"empty_select": "",
		},
		"Query": map[string]any{
			"Season":   "",
			"Ep":       "",
			"IMDBID":   "tt0111161",
			"Type":     "tvsearch",
			"Keywords": "Дюна 2024",
		},
		"Result": map[string]any{
			"_id":       "42",
			"date_day":  "",
			"date_year": "2024-01-02",
		},
	}
}

func TestTemplateIfElse(t *testing.T) {
	cases := []struct {
		name, tpl, want string
	}{
		{"plain", "browse.php", "browse.php"},
		{"if true", "{{ if .Config.freeleech }}11{{ else }}0{{ end }}", "11"},
		{"if false", "{{ if .Config.stripcyr }}yes{{ else }}no{{ end }}", "no"},
		{"if missing var is false", "{{ if .Query.Season }}S{{ .Query.Season }}{{ else }}none{{ end }}", "none"},
		{"nested and keywords",
			"{{ if .Query.IMDBID }}search/0/0/010/{{ .Config.sort }}/{{ .Query.IMDBID }}{{ else }}{{ end }}{{ if .Keywords }}/k{{ else }}{{ end }}",
			"search/0/0/010/2/tt0111161/k"},
		{"or", "{{ or .Result.date_day .Result.date_year }}", "2024-01-02"},
		{"if or", "{{ if or .Keywords .Query.IMDBID }}{{ else }}/{{ end }}", ""},
		{"eq", `{{ if eq .Query.Type "tvsearch" }}{{ .Config.format-tv }}{{ else }}x{{ end }}`, "TV!"},
		{"ne and", `{{ if and (ne .Result._id "913") (.Config.freeleech) }} RUS{{ else }}{{ end }}`, " RUS"},
		{"range", "{{ range .Categories }}f[]={{.}}&{{end}}", "f[]=1&f[]=5&"},
		{"range index", "{{ range $i, $c := .Categories }}c{{ $i }}={{ $c }};{{ end }}", "c0=1;c1=5;"},
		{"join", `{{ join .Categories "," }}`, "1,5"},
		{"re_replace backslashes", `{{ re_replace .Keywords "\s+" "%" }}`, "Дюна%2024"},
		{"re_replace dotnet groups", `{{ re_replace .Keywords "(\d+)" "y$1y" }}`, "Дюна y2024y"},
		{"today", "{{ .Today.Year }}", "2026"},
		{"result field", "{{ .Config.sitelink }}description.php?id={{ .Result._id }}", "https://example.org/description.php?id=42"},
		{"missing key empty", "a{{ .Config.nope }}b", "ab"},
		{"true var", "{{ .True }}", "True"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := renderTemplate(c.tpl, testVars(), nil)
			if err != nil {
				t.Fatalf("render %q: %v", c.tpl, err)
			}
			if got != c.want {
				t.Fatalf("render %q = %q, want %q", c.tpl, got, c.want)
			}
		})
	}
}

func TestTemplateModifierEncodesOnlyVariables(t *testing.T) {
	got, err := renderTemplate("search/{{ .Keywords }}/{{ if .Keywords }}a b{{ else }}{{ end }}", testVars(), url.QueryEscape)
	if err != nil {
		t.Fatal(err)
	}
	want := "search/" + url.QueryEscape("Дюна 2024") + "/a b"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestTemplateBadSyntaxIsError(t *testing.T) {
	if _, err := renderTemplate("{{ if .Keywords }}x", testVars(), nil); err == nil {
		t.Fatal("expected error for unterminated if")
	}
}
