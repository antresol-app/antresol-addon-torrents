package cardigann

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/encoding"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// filterEnv carries what filters need: the definition charset (urlencode,
// urldecode), template variables (replace/append/prepend/re_replace
// arguments are templates) and the clock (relative dates).
type filterEnv struct {
	enc  encoding.Encoding // nil = UTF-8
	vars map[string]any
	now  func() time.Time
	log  *slog.Logger
}

func (e *filterEnv) tmpl(s string) (string, error) { return renderTemplate(s, e.vars, nil) }

func (e *filterEnv) clock() time.Time {
	if e.now == nil {
		return time.Now()
	}
	return e.now()
}

// apply runs filters in order (Prowlarr CardigannBase.ApplyFilters).
func (e *filterEnv) apply(data string, filters []Filter) (string, error) {
	for _, fl := range filters {
		var err error
		data, err = e.one(data, fl)
		if err != nil {
			return data, fmt.Errorf("filter %s: %w", fl.Name, err)
		}
	}
	return data, nil
}

func (e *filterEnv) one(data string, fl Filter) (string, error) {
	a := fl.Args
	switch fl.Name {
	case "querystring":
		return queryArg(data, a.arg(0)), nil
	case "timeparse", "dateparse":
		t, err := parseDateLayout(data, a.arg(0))
		if err != nil {
			e.debug("dateparse", "err", err)
			return data, nil // Prowlarr logs and keeps the value
		}
		return formatDate(t), nil
	case "regexp":
		re, err := compileRegex(a.arg(0))
		if err != nil {
			return data, err
		}
		return re.group1(data)
	case "re_replace":
		re, err := compileRegex(a.arg(0))
		if err != nil {
			return data, err
		}
		repl, err := e.tmpl(a.arg(1))
		if err != nil {
			return data, err
		}
		return re.replace(data, repl)
	case "split":
		sep := a.arg(0)
		if sep == "" {
			return data, fmt.Errorf("empty separator")
		}
		pos, err := strconv.Atoi(a.arg(1))
		if err != nil {
			return data, err
		}
		parts := strings.Split(data, string([]rune(sep)[0]))
		if pos < 0 {
			pos += len(parts)
		}
		if pos < 0 || pos >= len(parts) {
			return data, fmt.Errorf("index %s out of range for %q", a.arg(1), data)
		}
		return parts[pos], nil
	case "replace":
		to, err := e.tmpl(a.arg(1))
		if err != nil {
			return data, err
		}
		if a.arg(0) == "" {
			return data, nil
		}
		return strings.ReplaceAll(data, a.arg(0), to), nil
	case "trim":
		if len(a) == 0 || a.arg(0) == "" {
			return strings.TrimSpace(data), nil
		}
		return strings.Trim(data, string([]rune(a.arg(0))[0])), nil
	case "prepend":
		s, err := e.tmpl(a.arg(0))
		return s + data, err
	case "append":
		s, err := e.tmpl(a.arg(0))
		return data + s, err
	case "tolower":
		return strings.ToLower(data), nil
	case "toupper":
		return strings.ToUpper(data), nil
	case "base64decode":
		b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.NewReplacer("+", "-", "/", "_").Replace(data), "="))
		return string(b), err
	case "base64encode":
		return base64.RawURLEncoding.EncodeToString([]byte(data)), nil
	case "urldecode":
		return urlDecode(e.enc, data)
	case "urlencode":
		return urlEncode(e.enc, data), nil
	case "htmldecode":
		return html.UnescapeString(data), nil
	case "htmlencode":
		return html.EscapeString(data), nil
	case "timeago", "reltime":
		t, err := parseTimeAgo(data, e.clock())
		if err != nil {
			return data, err
		}
		return formatDate(t), nil
	case "fuzzytime":
		t, err := parseUnknown(data, e.clock())
		if err != nil {
			return data, err
		}
		return formatDate(t), nil
	case "validfilename":
		return validFilename(data), nil
	case "diacritics":
		if a.arg(0) != "replace" {
			return data, fmt.Errorf("unsupported diacritics argument %q", a.arg(0))
		}
		s, _, err := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), data)
		return s, err
	case "jsonjoinarray":
		var v any
		if err := json.Unmarshal([]byte(data), &v); err != nil {
			return data, err
		}
		sel, ok := selectToken(v, a.arg(0))
		if !ok {
			return data, fmt.Errorf("path %q not found", a.arg(0))
		}
		arr, ok := sel.([]any)
		if !ok {
			return data, fmt.Errorf("path %q is not an array", a.arg(0))
		}
		parts := make([]string, len(arr))
		for i, x := range arr {
			parts[i] = jsonString(x)
		}
		return strings.Join(parts, a.arg(1)), nil
	case "hexdump", "strdump":
		e.debug(fl.Name, "data", data) // debugging aids only
		return data, nil
	case "validate":
		return validateList(data, a.arg(0)), nil
	case "andmatch":
		return data, nil // row filter; query matching is the manager's job
	default:
		e.debug("unsupported filter", "name", fl.Name)
		return data, nil
	}
}

func (e *filterEnv) debug(msg string, args ...any) {
	if e.log != nil {
		e.log.Debug("cardigann: "+msg, args...)
	}
}

// queryArg returns a query-string argument of a (possibly relative) URL.
func queryArg(link, name string) string {
	_, qs, ok := strings.Cut(link, "?")
	if !ok {
		return ""
	}
	qs, _, _ = strings.Cut(qs, "#")
	v, err := url.ParseQuery(qs)
	if err != nil && v == nil {
		return ""
	}
	return v.Get(name)
}

func validFilename(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`\/:*?"<>|`, r) {
			return '_'
		}
		return r
	}, s)
}

var validateDelims = ",  /)(.;[]\"|:"

// validateList keeps the words of data that appear in the allowed list
// (Prowlarr's "validate", used for genres).
func validateList(data, allowed string) string {
	split := func(s string) []string {
		return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return strings.ContainsRune(validateDelims, r) })
	}
	in := map[string]bool{}
	for _, w := range split(data) {
		in[strings.TrimSpace(w)] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, w := range split(allowed) {
		w = strings.TrimSpace(w)
		if w != "" && in[w] && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return strings.Join(out, ", ")
}
