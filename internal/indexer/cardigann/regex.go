package cardigann

import (
	"regexp"
	"sync"
	"time"

	"github.com/dlclark/regexp2"
)

// Definitions are written for .NET Regex: lookarounds, \p{IsCyrillic},
// "$1E$2" replacements (group 1 then "E"), ${name} groups. RE2 supports none
// of these faithfully, so we use regexp2 (a port of the .NET engine).
// regexp2 lacks .NET named Unicode blocks; they are mapped to scripts.

type dotnetRegex struct{ re *regexp2.Regexp }

var (
	regexCache sync.Map // pattern -> *dotnetRegex
	blockRe    = regexp.MustCompile(`\\[pP]\{Is([A-Za-z]+)\}`)
	blockMap   = map[string]string{
		"CJKUnifiedIdeographs": "Han", "CJKUnifiedIdeographsExtensionA": "Han",
		"HangulSyllables": "Hangul", "GreekandCoptic": "Greek",
	}
)

const regexTimeout = 2 * time.Second

func compileRegex(pattern string) (*dotnetRegex, error) {
	if v, ok := regexCache.Load(pattern); ok {
		return v.(*dotnetRegex), nil
	}
	p := blockRe.ReplaceAllStringFunc(pattern, func(m string) string {
		name := m[5 : len(m)-1] // \p{IsCyrillic} -> Cyrillic
		if s, ok := blockMap[name]; ok {
			name = s
		}
		return `\` + string(m[1]) + "{" + name + "}"
	})
	re, err := regexp2.Compile(p, regexp2.None)
	if err != nil {
		return nil, err
	}
	re.MatchTimeout = regexTimeout
	r := &dotnetRegex{re: re}
	regexCache.Store(pattern, r)
	return r, nil
}

// replace replaces all matches; repl uses .NET syntax ($1, ${name}, $$, $0).
func (r *dotnetRegex) replace(s, repl string) (string, error) {
	return r.re.Replace(s, repl, -1, -1)
}

// group1 returns the first capture group of the first match ("" if none).
func (r *dotnetRegex) group1(s string) (string, error) {
	m, err := r.re.FindStringMatch(s)
	if err != nil || m == nil {
		return "", err
	}
	g := m.GroupByNumber(1)
	if g == nil {
		return "", nil
	}
	return g.String(), nil
}
