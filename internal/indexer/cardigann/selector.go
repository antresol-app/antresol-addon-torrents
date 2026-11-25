package cardigann

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"golang.org/x/net/html"
)

var (
	selectorCache sync.Map // string -> cascadia.Selector
	// AngleSharp accepts :contains(\00a0GB); cascadia needs the argument quoted.
	unquotedContains = regexp.MustCompile(`:contains\(([^"')][^)]*)\)`)
	// AngleSharp accepts [border=1]; CSS (and cascadia) want an identifier or a string.
	unquotedAttr = regexp.MustCompile(`\[([\w-]+)\s*([~|^$*]?=)\s*([^\]"'\s]+)\]`)
	scopeAttr    = "data-cardigann-scope"
	errNoMatch   = errors.New("selector matched nothing")
)

func compileSelector(s string) (cascadia.Selector, error) {
	if v, ok := selectorCache.Load(s); ok {
		return v.(cascadia.Selector), nil
	}
	norm := unquotedContains.ReplaceAllStringFunc(s, func(m string) string {
		arg := m[len(":contains(") : len(m)-1]
		return `:contains("` + strings.ReplaceAll(arg, `"`, `\"`) + `")`
	})
	norm = unquotedAttr.ReplaceAllString(norm, `[$1$2"$3"]`)
	norm = strings.ReplaceAll(norm, ":scope", "["+scopeAttr+"]")
	sel, err := cascadia.Compile(norm)
	if err != nil {
		return nil, fmt.Errorf("selector %q: %w", s, err)
	}
	selectorCache.Store(s, sel)
	return sel, nil
}

// rootAware handles the ":root" prefix (AngleSharp lacks it, so Prowlarr
// strips it and searches from the document root).
func rootAware(dom *goquery.Selection, s string) (*goquery.Selection, string) {
	if !strings.HasPrefix(s, ":root") {
		return dom, s
	}
	if p := dom.Parents(); p.Length() > 0 {
		dom = p.Last()
	}
	rest := strings.TrimSpace(s[len(":root"):])
	return dom, rest
}

// queryFirst returns the first descendant of dom matching s.
func queryFirst(dom *goquery.Selection, s string) (*goquery.Selection, error) {
	all, err := queryAll(dom, s)
	if err != nil {
		return nil, err
	}
	return all.First(), nil
}

// queryAll returns the descendants of dom matching s. ":scope" refers to dom
// itself: it is marked with an attribute for the duration of the query.
func queryAll(dom *goquery.Selection, s string) (*goquery.Selection, error) {
	dom, s = rootAware(dom, s)
	if s == "" {
		return dom, nil
	}
	m, err := compileSelector(s)
	if err != nil {
		return nil, err
	}
	if strings.Contains(s, ":scope") {
		dom.SetAttr(scopeAttr, "")
		defer dom.RemoveAttr(scopeAttr)
	}
	return dom.FindMatcher(m), nil
}

func matches(dom *goquery.Selection, s string) bool {
	if strings.HasPrefix(s, ":root") {
		return false
	}
	m, err := compileSelector(s)
	return err == nil && dom.IsMatcher(m)
}

// handleSelector is CardigannBase.HandleSelector for HTML. found is false
// when an optional element/attribute is missing.
func (r *Runner) handleSelector(sel *Selector, dom *goquery.Selection, env *filterEnv, required bool) (value string, found bool, err error) {
	if sel.Text != nil {
		t, err := renderTemplate(*sel.Text, env.vars, nil)
		if err != nil {
			return "", false, err
		}
		v, err := env.apply(t, sel.Filters)
		return v, err == nil, err
	}
	selection := dom
	if sel.Selector != "" {
		s, err := renderTemplate(sel.Selector, env.vars, nil)
		if err != nil {
			return "", false, err
		}
		if !matches(dom, s) {
			if selection, err = queryFirst(dom, s); err != nil {
				return "", false, err
			}
		}
		if selection.Length() == 0 {
			if required {
				return "", false, fmt.Errorf("%w: %s", errNoMatch, s)
			}
			return "", false, nil
		}
	}
	if sel.Remove != "" {
		if rm, err := queryAll(selection, sel.Remove); err == nil {
			rm.Remove()
		}
	}
	switch {
	case len(sel.Case) > 0:
		ok := false
		for _, c := range sel.Case {
			if matches(selection, c.Key) {
				ok = true
			} else if el, err := queryFirst(selection, c.Key); err == nil && el.Length() > 0 {
				ok = true
			}
			if ok {
				if value, err = renderTemplate(c.Value, env.vars, nil); err != nil {
					return "", false, err
				}
				break
			}
		}
		if !ok {
			if required {
				return "", false, fmt.Errorf("%w: none of the case selectors", errNoMatch)
			}
			return "", false, nil
		}
	case sel.Attribute != "":
		v, ok := selection.Attr(sel.Attribute)
		if !ok {
			if required {
				return "", false, fmt.Errorf("attribute %q is not set", sel.Attribute)
			}
			return "", false, nil
		}
		value = v
	default:
		value = selection.Text()
	}
	v, err := env.apply(strings.TrimSpace(value), sel.Filters)
	return v, err == nil, err
}

// handleJSONSelector is CardigannBase.HandleJsonSelector.
func (r *Runner) handleJSONSelector(sel *Selector, obj any, env *filterEnv, required bool) (value string, found bool, err error) {
	if sel.Text != nil {
		t, err := renderTemplate(*sel.Text, env.vars, nil)
		if err != nil {
			return "", false, err
		}
		v, err := env.apply(t, sel.Filters)
		return v, err == nil, err
	}
	isNull := true
	if sel.Selector != "" {
		s, err := renderTemplate(strings.TrimLeft(sel.Selector, "."), env.vars, nil)
		if err != nil {
			return "", false, err
		}
		var tok any
		ok := false
		if path, pok := jsonFieldSelector(obj, s); pok {
			tok, ok = selectToken(obj, path)
		}
		if !ok {
			if required {
				return "", false, fmt.Errorf("%w: %s", errNoMatch, s)
			}
			return "", false, nil
		}
		value, isNull = jsonString(tok), tok == nil
	}
	if len(sel.Case) > 0 {
		matched := false
		for _, c := range sel.Case {
			if (!isNull && value == c.Key) || c.Key == "*" {
				if value, err = renderTemplate(c.Value, env.vars, nil); err != nil {
					return "", false, err
				}
				matched = true
				break
			}
		}
		if !matched {
			if required {
				return "", false, fmt.Errorf("%w: none of the case values", errNoMatch)
			}
			return "", false, nil
		}
	}
	v, err := env.apply(strings.TrimSpace(value), sel.Filters)
	return v, err == nil, err
}

// matchSelector is CardigannRequestGenerator.MatchSelector (download pages).
func (r *Runner) matchSelector(resp *response, sel *SelectorField, vars map[string]any) (string, error) {
	el, err := r.selectElement(resp, sel, vars)
	if err != nil {
		return "", err
	}
	val := el.Text()
	if sel.Attribute != "" {
		v, ok := el.Attr(sel.Attribute)
		if !ok {
			return "", fmt.Errorf("attribute %q is not set", sel.Attribute)
		}
		val = v
	}
	return r.filterEnv(vars).apply(val, sel.Filters)
}

// selectElement returns the first element of resp matching sel.
func (r *Runner) selectElement(resp *response, sel *SelectorField, vars map[string]any) (*goquery.Selection, error) {
	s, err := renderTemplate(sel.Selector, vars, nil)
	if err != nil {
		return nil, err
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(r.text(resp)))
	if err != nil {
		return nil, err
	}
	el, err := queryFirst(doc.Selection, s)
	if err != nil {
		return nil, err
	}
	if el.Length() == 0 {
		return nil, fmt.Errorf("%w: %s", errNoMatch, s)
	}
	return el, nil
}

// maxXMLDepth: a feed nested deeper is not a feed.
const maxXMLDepth = 256

// parseXMLDocument builds a DOM from XML with lower-cased local names so
// CSS selectors work as with AngleSharp's XML parser (and <link> keeps its
// text, which an HTML parser would drop).
func parseXMLDocument(text string) (*goquery.Document, error) {
	dec := xml.NewDecoder(strings.NewReader(text))
	dec.Strict = false
	dec.Entity = xml.HTMLEntity
	dec.CharsetReader = func(_ string, in io.Reader) (io.Reader, error) { return in, nil } // already decoded
	root := &html.Node{Type: html.DocumentNode}
	stack := []*html.Node{root}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("xml: %w", err)
		}
		top := stack[len(stack)-1]
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) > maxXMLDepth {
				return nil, fmt.Errorf("xml: nested more than %d levels", maxXMLDepth)
			}
			n := &html.Node{Type: html.ElementNode, Data: strings.ToLower(t.Name.Local)}
			for _, a := range t.Attr {
				n.Attr = append(n.Attr, html.Attribute{Key: strings.ToLower(a.Name.Local), Val: a.Value})
			}
			top.AppendChild(n)
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			top.AppendChild(&html.Node{Type: html.TextNode, Data: string(t)})
		}
	}
	return goquery.NewDocumentFromNode(root), nil
}
