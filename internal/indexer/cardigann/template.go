package cardigann

import (
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

// Cardigann templates are Go-template-like ({{ if }}, {{ range }}, {{ .X }})
// but Prowlarr evaluates them with regexes and .NET semantics. We run them on
// text/template after rewriting each action:
//   - "…" literals become raw `…` literals: Cardigann does no escape
//     processing, so "\s+" is a regex, not an invalid Go escape;
//   - dotted names that are not Go identifiers (.Config.format-tv,
//     .Config.2facode) become index calls;
//   - output actions get "| __m" so a modifier (URL encoding of the search
//     path) applies to substituted values only, never to literal text —
//     exactly what Prowlarr's ApplyGoTemplateText(modifier) does.
// Variables are nested maps of strings; nil/missing values render as "".

var (
	dashedField = regexp.MustCompile(`(\.[A-Za-z_]\w*)((?:\.[\w-]*\w)+)`)
	keywordRe   = regexp.MustCompile(`^(if|else|end|range|with|define|template|block|break|continue)\b`)
	declRe      = regexp.MustCompile(`^\$\w*\s*(:=|=)`)
)

// renderTemplate executes a Cardigann template. mod, if not nil, is applied
// to every substituted value.
func renderTemplate(tpl string, vars map[string]any, mod func(string) string) (string, error) {
	if !strings.Contains(tpl, "{{") {
		return tpl, nil
	}
	src := rewriteTemplate(tpl)
	if mod == nil {
		mod = func(s string) string { return s }
	}
	t, err := template.New("").Option("missingkey=zero").Funcs(template.FuncMap{
		"__m":        func(v any) string { return mod(toStr(v)) },
		"re_replace": tmplReReplace,
		"join":       tmplJoin,
	}).Parse(src)
	if err != nil {
		return "", fmt.Errorf("template %q: %w", tpl, err)
	}
	var b strings.Builder
	if err := t.Execute(&b, vars); err != nil {
		return "", fmt.Errorf("template %q: %w", tpl, err)
	}
	return strings.ReplaceAll(b.String(), "<no value>", ""), nil
}

func toStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []string:
		return strings.Join(x, ",")
	default:
		return fmt.Sprint(x)
	}
}

func tmplReReplace(input any, pattern, repl string) (string, error) {
	re, err := compileRegex(pattern)
	if err != nil {
		return "", err
	}
	return re.replace(toStr(input), repl)
}

func tmplJoin(list any, sep string) string {
	switch x := list.(type) {
	case []string:
		return strings.Join(x, sep)
	case nil:
		return ""
	default:
		return toStr(x)
	}
}

// rewriteTemplate rewrites every {{ action }} as described above.
func rewriteTemplate(tpl string) string {
	var out strings.Builder
	for {
		i := strings.Index(tpl, "{{")
		if i < 0 {
			out.WriteString(tpl)
			break
		}
		out.WriteString(tpl[:i])
		j := findActionEnd(tpl, i+2)
		if j < 0 {
			out.WriteString(tpl[i:]) // unterminated: let the parser report it
			break
		}
		out.WriteString(rewriteAction(tpl[i+2 : j]))
		tpl = tpl[j+2:]
	}
	return out.String()
}

// findActionEnd returns the index of the "}}" closing an action that starts
// at from, skipping quoted literals.
func findActionEnd(s string, from int) int {
	for k := from; k < len(s)-1; k++ {
		switch s[k] {
		case '"', '`':
			if e := strings.IndexByte(s[k+1:], s[k]); e >= 0 {
				k += e + 1
			}
		case '}':
			if s[k+1] == '}' {
				return k
			}
		}
	}
	return -1
}

func rewriteAction(inner string) string {
	ltrim, rtrim := "", ""
	if strings.HasPrefix(inner, "- ") {
		ltrim, inner = "- ", inner[2:]
	}
	if strings.HasSuffix(inner, " -") {
		rtrim, inner = " -", inner[:len(inner)-2]
	}
	body := strings.TrimSpace(inner)
	if strings.HasPrefix(body, "/*") {
		return "{{" + ltrim + inner + rtrim + "}}"
	}
	// split into literal / code segments, rewrite code and literals
	var b strings.Builder
	for len(body) > 0 {
		q := strings.IndexAny(body, "\"`")
		if q < 0 {
			b.WriteString(dashedField.ReplaceAllStringFunc(body, dashToIndex))
			break
		}
		b.WriteString(dashedField.ReplaceAllStringFunc(body[:q], dashToIndex))
		e := strings.IndexByte(body[q+1:], body[q])
		if e < 0 {
			b.WriteString(body[q:])
			break
		}
		lit := body[q+1 : q+1+e]
		if body[q] == '"' && strings.Contains(lit, `\`) && !strings.Contains(lit, "`") {
			b.WriteString("`" + lit + "`")
		} else {
			b.WriteString(body[q : q+e+2])
		}
		body = body[q+e+2:]
	}
	code := b.String()
	if !keywordRe.MatchString(code) && !declRe.MatchString(code) {
		code = "(" + code + ") | __m"
	}
	return "{{" + ltrim + " " + code + " " + rtrim + "}}"
}

// dashToIndex turns .Config.format-tv into (index .Config "format-tv").
func dashToIndex(m string) string {
	parts := strings.Split(m[1:], ".")
	// keep the prefix of valid identifiers as a field chain
	k := 0
	for k < len(parts) && isIdent(parts[k]) {
		k++
	}
	if k == len(parts) {
		return m
	}
	if k == 0 {
		return m // the regex guarantees an identifier first
	}
	var b strings.Builder
	b.WriteString("(index ." + strings.Join(parts[:k], "."))
	for _, p := range parts[k:] {
		b.WriteString(` "` + p + `"`)
	}
	b.WriteString(")")
	return b.String()
}

func isIdent(s string) bool {
	if s == "" || strings.Contains(s, "-") {
		return false
	}
	return s[0] == '_' || (s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z') || s[0] >= 0x80
}
