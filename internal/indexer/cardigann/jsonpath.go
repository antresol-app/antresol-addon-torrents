package cardigann

import (
	"encoding/json"
	"strconv"
	"strings"
)

// A small subset of Newtonsoft's SelectToken JSONPath, enough for Cardigann:
// "$", "a.b", "$.a[0].b", "['a b']", "$[0].id". Plus the Cardigann row/field
// filters ":has(x)", ":not(x)", ":contains(x)".

// selectToken resolves path against v.
func selectToken(v any, path string) (any, bool) {
	path = strings.TrimSpace(path)
	path = strings.TrimPrefix(path, "$")
	cur := v
	for len(path) > 0 {
		switch path[0] {
		case '.':
			path = path[1:]
		case '[':
			end := strings.IndexByte(path, ']')
			if end < 0 {
				return nil, false
			}
			key := path[1:end]
			path = path[end+1:]
			if len(key) >= 2 && (key[0] == '\'' || key[0] == '"') {
				m, ok := cur.(map[string]any)
				if !ok {
					return nil, false
				}
				if cur, ok = m[key[1:len(key)-1]]; !ok {
					return nil, false
				}
				continue
			}
			idx, err := strconv.Atoi(key)
			arr, ok := cur.([]any)
			if err != nil || !ok {
				return nil, false
			}
			if idx < 0 {
				idx += len(arr)
			}
			if idx < 0 || idx >= len(arr) {
				return nil, false
			}
			cur = arr[idx]
		default:
			end := strings.IndexAny(path, ".[")
			if end < 0 {
				end = len(path)
			}
			name := path[:end]
			path = path[end:]
			m, ok := cur.(map[string]any)
			if !ok {
				return nil, false
			}
			if cur, ok = m[name]; !ok {
				return nil, false
			}
		}
	}
	return cur, true
}

// jsonString converts a JSON value to the string Newtonsoft's Value<string>
// would give: numbers verbatim, booleans "True"/"False", null "".
func jsonString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "True"
		}
		return "False"
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = jsonString(e)
		}
		return strings.Join(parts, ",")
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// jsonFieldSelector mirrors CardigannBase.JsonParseFieldSelector: it returns
// the path part of selector if the token exists and all filters hold.
func jsonFieldSelector(obj any, selector string) (string, bool) {
	path, _, _ := strings.Cut(selector, ":")
	target := obj
	if strings.TrimSpace(path) != "" {
		t, ok := selectToken(obj, path)
		if !ok {
			return "", false
		}
		target = t
	}
	rest := selector[len(path):]
	for len(rest) > 0 {
		name, arg, n := nextJSONFilter(rest)
		if n == 0 {
			break
		}
		rest = rest[n:]
		switch name {
		case "has", "not":
			var found bool
			if strings.Contains(arg, ":") {
				_, found = jsonFieldSelector(target, arg)
			} else {
				_, ok := selectToken(target, arg)
				found = ok
			}
			if found != (name == "has") {
				return "", false
			}
		case "contains":
			s := jsonString(target)
			if !strings.Contains(s, arg) {
				return "", false
			}
		}
	}
	return path, true
}

// nextJSONFilter parses ":name(arg)" at the start of s, honouring nested
// parentheses; it returns the consumed length (0 when s has no filter).
func nextJSONFilter(s string) (name, arg string, n int) {
	if !strings.HasPrefix(s, ":") {
		return "", "", 0
	}
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return "", "", 0
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return s[1:open], s[open+1 : i], i + 1
			}
		}
	}
	return "", "", 0
}
