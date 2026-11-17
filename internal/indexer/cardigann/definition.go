// Package cardigann executes Prowlarr/Jackett Cardigann YAML indexer
// definitions (format of github.com/Prowlarr/Indexers, v10/v11) directly:
// login, search, result parsing and torrent download.
//
// Semantics follow Prowlarr's C# implementation (CardigannBase.cs,
// CardigannParser.cs, CardigannRequestGenerator.cs). Templates run on Go
// text/template after a small rewrite (see template.go), regular expressions
// use a .NET-compatible engine (see regex.go) because definitions are written
// for .NET Regex.
package cardigann

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Definition is a parsed Cardigann YAML definition. YAML keys follow
// Prowlarr's camelCase naming convention.
type Definition struct {
	ID              string    `yaml:"id"`
	Name            string    `yaml:"name"`
	Description     string    `yaml:"description"`
	Language        string    `yaml:"language"`
	Type            string    `yaml:"type"` // public | semi-private | private
	Encoding        string    `yaml:"encoding"`
	RequestDelay    float64   `yaml:"requestDelay"`
	Links           []string  `yaml:"links"`
	LegacyLinks     []string  `yaml:"legacylinks"`
	FollowRedirect  bool      `yaml:"followredirect"`
	TestLinkTorrent *bool     `yaml:"testlinktorrent"`
	Caps            Caps      `yaml:"caps"`
	Settings        []Setting `yaml:"settings"`
	Login           *Login    `yaml:"login"`
	Search          Search    `yaml:"search"`
	Download        *Download `yaml:"download"`
}

// Caps describes categories and search modes.
type Caps struct {
	Categories       OrderedMap          `yaml:"categories"`
	CategoryMappings []CategoryMapping   `yaml:"categorymappings"`
	Modes            map[string][]string `yaml:"modes"`
	AllowRawSearch   bool                `yaml:"allowrawsearch"`
}

// CategoryMapping maps a tracker category id to a Newznab category name.
type CategoryMapping struct {
	ID      string `yaml:"id"`
	Cat     string `yaml:"cat"`
	Desc    string `yaml:"desc"`
	Default bool   `yaml:"default"`
}

// Setting is a user-configurable field (username, password, cookie, select…).
type Setting struct {
	Name     string     `yaml:"name"`
	Type     string     `yaml:"type"`
	Label    string     `yaml:"label"`
	Default  string     `yaml:"default"`
	Defaults []string   `yaml:"defaults"`
	Options  OrderedMap `yaml:"options"`
}

// SortedOptionKeys returns option keys in the order Prowlarr uses to store a
// select value as an index (keys sorted as strings).
func (s Setting) SortedOptionKeys() []string {
	keys := make([]string, 0, len(s.Options))
	for _, kv := range s.Options {
		keys = append(keys, kv.Key)
	}
	sort.Strings(keys)
	return keys
}

// OptionKeyByIndex converts a Prowlarr-stored select index into an option key.
func (s Setting) OptionKeyByIndex(i int) (string, bool) {
	keys := s.SortedOptionKeys()
	if i < 0 || i >= len(keys) {
		return "", false
	}
	return keys[i], true
}

// Login describes how to authenticate.
type Login struct {
	Path              string             `yaml:"path"`
	SubmitPath        string             `yaml:"submitpath"`
	Cookies           []string           `yaml:"cookies"`
	Method            string             `yaml:"method"` // post | form | cookie | get | oneurl
	Form              string             `yaml:"form"`
	Selectors         bool               `yaml:"selectors"`
	Inputs            OrderedMap         `yaml:"inputs"`
	SelectorInputs    Fields             `yaml:"selectorinputs"`
	GetSelectorInputs Fields             `yaml:"getselectorinputs"`
	Error             []ErrorBlock       `yaml:"error"`
	Test              *PageTest          `yaml:"test"`
	Captcha           *Captcha           `yaml:"captcha"`
	Headers           map[string]StrList `yaml:"headers"`
}

// Captcha block of a login.
type Captcha struct {
	Type     string `yaml:"type"`
	Selector string `yaml:"selector"`
	Input    string `yaml:"input"`
}

// ErrorBlock: when Selector matches the response, the request failed.
type ErrorBlock struct {
	Path     string    `yaml:"path"`
	Selector string    `yaml:"selector"`
	Message  *Selector `yaml:"message"`
}

// PageTest verifies that a login succeeded.
type PageTest struct {
	Path     string `yaml:"path"`
	Selector string `yaml:"selector"`
}

// Selector extracts one value from a DOM element or a JSON object.
type Selector struct {
	Selector  string     `yaml:"selector"`
	Optional  bool       `yaml:"optional"`
	Default   string     `yaml:"default"`
	Text      *string    `yaml:"text"`
	Attribute string     `yaml:"attribute"`
	Remove    string     `yaml:"remove"`
	Filters   []Filter   `yaml:"filters"`
	Case      OrderedMap `yaml:"case"`
}

// Filter is one value transformation; Args is always normalised to a list.
type Filter struct {
	Name string  `yaml:"name"`
	Args StrList `yaml:"args"`
}

// Search block.
type Search struct {
	Path                 string             `yaml:"path"`
	Paths                []SearchPath       `yaml:"paths"`
	Headers              map[string]StrList `yaml:"headers"`
	KeywordsFilters      []Filter           `yaml:"keywordsfilters"`
	AllowEmptyInputs     bool               `yaml:"allowEmptyInputs"`
	Inputs               OrderedMap         `yaml:"inputs"`
	Error                []ErrorBlock       `yaml:"error"`
	PreprocessingFilters []Filter           `yaml:"preprocessingfilters"`
	Rows                 Rows               `yaml:"rows"`
	Fields               Fields             `yaml:"fields"`
}

// Rows selects result rows.
type Rows struct {
	Selector                        `yaml:",inline"`
	After                           int       `yaml:"after"`
	DateHeaders                     *Selector `yaml:"dateheaders"`
	Count                           *Selector `yaml:"count"`
	Multiple                        bool      `yaml:"multiple"`
	MissingAttributeEqualsNoResults bool      `yaml:"missingAttributeEqualsNoResults"`
}

// RequestBlock is a templated HTTP request.
type RequestBlock struct {
	Path           string     `yaml:"path"`
	Method         string     `yaml:"method"`
	Inputs         OrderedMap `yaml:"inputs"`
	QuerySeparator string     `yaml:"queryseparator"`
}

// SearchPath is one search request.
type SearchPath struct {
	RequestBlock   `yaml:",inline"`
	Categories     []string  `yaml:"categories"`
	InheritInputs  *bool     `yaml:"inheritinputs"`
	FollowRedirect bool      `yaml:"followredirect"`
	Response       *Response `yaml:"response"`
}

// Response describes the response format of a search path.
type Response struct {
	Type             string  `yaml:"type"` // html (default) | json | xml
	NoResultsMessage *string `yaml:"noResultsMessage"`
}

// Download block.
type Download struct {
	Selectors []SelectorField    `yaml:"selectors"`
	Method    string             `yaml:"method"`
	Before    *BeforeBlock       `yaml:"before"`
	Infohash  *InfohashBlock     `yaml:"infohash"`
	Headers   map[string]StrList `yaml:"headers"`
	// Direct (Vault extension): the site serves the release itself (an
	// e-book file), not a torrent. Runner.OpenDirect follows the selectors
	// to the file; Download refuses such a definition.
	Direct bool `yaml:"direct"`
}

// SelectorField is a download-page selector.
type SelectorField struct {
	Selector          string   `yaml:"selector"`
	Attribute         string   `yaml:"attribute"`
	UseBeforeResponse bool     `yaml:"usebeforeresponse"`
	Filters           []Filter `yaml:"filters"`
	// Vault extensions for downloads that take several pages (download
	// selectors only):
	// Submit: the selector matches a <form>, which is submitted (its
	// method, action and fields) instead of a link being read.
	Submit bool `yaml:"submit"`
	// Follow: further steps, in order. The page this selector leads to (its
	// link or its submitted form) is fetched and Follow[0] is matched on it,
	// and so on; the last step leads to the download.
	Follow []SelectorField `yaml:"follow"`
}

// BeforeBlock is a request issued before the download.
type BeforeBlock struct {
	RequestBlock `yaml:",inline"`
	PathSelector *SelectorField `yaml:"pathselector"`
}

// InfohashBlock builds a magnet from a hash and a title on the details page.
type InfohashBlock struct {
	Hash              *SelectorField `yaml:"hash"`
	Title             *SelectorField `yaml:"title"`
	UseBeforeResponse bool           `yaml:"usebeforeresponse"`
}

// KV is one ordered key/value pair.
type KV struct{ Key, Value string }

// OrderedMap is a YAML mapping with scalar values, keeping definition order
// (inputs, case and options are order-sensitive).
type OrderedMap []KV

// UnmarshalYAML implements yaml.Unmarshaler.
func (m *OrderedMap) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected mapping", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], n.Content[i+1]
		val := v.Value
		if v.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: value of %q must be a scalar", v.Line, k.Value)
		}
		if v.Tag == "!!null" {
			val = ""
		}
		*m = append(*m, KV{Key: k.Value, Value: val})
	}
	return nil
}

// Get returns the value for key.
func (m OrderedMap) Get(key string) (string, bool) {
	for _, kv := range m {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return "", false
}

// Field is one named selector; Key may carry modifiers ("title|append").
type Field struct {
	Key string
	Selector
}

// Fields is an ordered mapping of selectors that allows duplicate keys.
type Fields []Field

// UnmarshalYAML implements yaml.Unmarshaler.
func (f *Fields) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!null" {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: expected mapping of fields", n.Line)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		var s Selector
		if err := n.Content[i+1].Decode(&s); err != nil {
			return fmt.Errorf("field %s: %w", n.Content[i].Value, err)
		}
		*f = append(*f, Field{Key: n.Content[i].Value, Selector: s})
	}
	return nil
}

// StrList accepts a scalar or a sequence of scalars.
type StrList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (l *StrList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return nil
		}
		*l = StrList{n.Value}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: expected scalar", c.Line)
			}
			*l = append(*l, c.Value)
		}
	default:
		return fmt.Errorf("line %d: expected scalar or list", n.Line)
	}
	return nil
}

func (l StrList) arg(i int) string {
	if i < len(l) {
		return l[i]
	}
	return ""
}

// Parse decodes and validates a Cardigann YAML definition.
func Parse(data []byte) (*Definition, error) {
	var d Definition
	err := yaml.Unmarshal(data, &d)
	if err != nil && strings.Contains(err.Error(), "unknown escape character") {
		// YAML 1.2 (YamlDotNet) allows "\/" in double-quoted strings, yaml.v3 does not
		d = Definition{}
		err = yaml.Unmarshal(slashEscape.ReplaceAll(data, []byte("$1$2/")), &d)
	}
	if err != nil {
		return nil, fmt.Errorf("cardigann: parse definition: %w", err)
	}
	if d.ID == "" {
		return nil, errors.New("cardigann: definition has no id")
	}
	if len(d.Links) == 0 {
		return nil, fmt.Errorf("cardigann: %s: definition has no links", d.ID)
	}
	if d.Search.Path == "" && len(d.Search.Paths) == 0 {
		return nil, fmt.Errorf("cardigann: %s: definition has no search paths", d.ID)
	}
	if len(d.Search.Paths) == 0 {
		d.Search.Paths = []SearchPath{{RequestBlock: RequestBlock{Path: d.Search.Path}}}
	}
	if d.Settings == nil {
		// Prowlarr's defaults for definitions without settings.
		d.Settings = []Setting{
			{Name: "username", Type: "text", Label: "Username"},
			{Name: "password", Type: "password", Label: "Password"},
		}
	}
	if d.Encoding == "" {
		d.Encoding = "UTF-8"
	}
	if _, err := lookupEncoding(d.Encoding); err != nil {
		return nil, fmt.Errorf("cardigann: %s: %w", d.ID, err)
	}
	return &d, nil
}

// slashEscape matches "\/" preceded by an even number of backslashes.
var slashEscape = regexp.MustCompile(`(^|[^\\])((?:\\\\)*)\\/`)
