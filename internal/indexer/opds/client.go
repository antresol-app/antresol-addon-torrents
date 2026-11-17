// Package opds reads OPDS 1.x catalogs (Flibusta and the like) as an
// acquisition source: search through the catalog's OpenSearch template,
// entries with their direct download links.
package opds

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/version"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/charmap"
)

// Limits of one response: a feed page, an OpenSearch description.
const (
	maxFeedBytes = 8 << 20
	maxPages     = 3
)

// Error is a failure with a Russian message for the sources screen; Is
// matches the sentinel it was made from.
type Error struct {
	Msg  string
	Err  error
	kind *Error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

func (e *Error) Is(t error) bool {
	k := e.kind
	if k == nil {
		k = e
	}
	return t == error(k)
}

func wrap(kind *Error, err error) error { return &Error{Msg: kind.Msg, Err: err, kind: kind} }

// Errors of a catalog.
var (
	ErrBadURL     = &Error{Msg: "Неверный адрес каталога: нужен адрес вида https://…"}
	ErrPrivate    = &Error{Msg: "Адрес ведёт в домашнюю сеть или на сам сервер. Если так и задумано, включите «Разрешить адреса домашней сети» в настройках источника."}
	ErrNotCatalog = &Error{Msg: "По этому адресу нет OPDS-каталога. Проверьте адрес — обычно он заканчивается на /opds."}
	ErrAuth       = &Error{Msg: "Каталог не принял логин и пароль."}
	ErrNoSearch   = &Error{Msg: "Каталог не поддерживает поиск."}
	ErrTooLarge   = &Error{Msg: "Каталог прислал слишком большой ответ."}
	ErrStatus     = &Error{Msg: "Каталог ответил ошибкой. Возможно, он временно недоступен."}
	// ErrForeignHost: the catalog points at another site (searches and
	// downloads stay on the catalog's host).
	ErrForeignHost = &Error{Msg: "Каталог ссылается на другой сайт"}
)

// AcqLink is a download link of an entry (Href is absolute).
type AcqLink struct {
	Href, Type string
	Size       int64
}

// Entry is a book of a result feed. Details is its web page (absolute),
// Lang its dc:language.
type Entry struct {
	Title, Author, ID, Details string
	Lang                       string
	Links                      []AcqLink
}

// Client reads one catalog. HTTP should come from NewHTTPClient (nil: one
// that refuses the home network).
type Client struct {
	URL, Username, Password string
	HTTP                    *http.Client
}

// Atom as OPDS catalogs send it (tags match any namespace).
type atomLink struct {
	Rel    string `xml:"rel,attr"`
	Href   string `xml:"href,attr"`
	Type   string `xml:"type,attr"`
	Length string `xml:"length,attr"`
}

type atomEntry struct {
	Title   string `xml:"title"`
	ID      string `xml:"id"`
	Authors []struct {
		Name string `xml:"name"`
	} `xml:"author"`
	Lang  string     `xml:"language"`
	Links []atomLink `xml:"link"`
}

type atomFeed struct {
	XMLName xml.Name
	Links   []atomLink  `xml:"link"`
	Entries []atomEntry `xml:"entry"`
}

type openSearch struct {
	XMLName xml.Name
	URLs    []struct {
		Type     string `xml:"type,attr"`
		Template string `xml:"template,attr"`
	} `xml:"Url"`
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return NewHTTPClient(false)
}

// base is the catalog root URL, checked.
func (c *Client) base() (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(c.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, ErrBadURL
	}
	return u, nil
}

// site is a catalog as reached: the configured root URL (the only host the
// login goes to) and the catalog's host, the host of the root feed after
// redirects (a mirror, www.). Documents linked from the catalog must be on
// one of the two; redirects may go anywhere (the address guard and the
// https→http refusal apply on every hop).
type site struct {
	root *url.URL
	host string
}

func (st *site) allowed(u *url.URL) bool {
	h := u.Hostname()
	return strings.EqualFold(h, st.root.Hostname()) || (st.host != "" && strings.EqualFold(h, st.host))
}

// Host is the catalog's host after redirects ("" before the root feed was
// read).
func (t Template) Host() string {
	if t.site == nil {
		return ""
	}
	return t.site.host
}

// get fetches one document of at most maxFeedBytes and returns it with its
// final URL (relative links resolve against it).
func (c *Client) get(ctx context.Context, st *site, u *url.URL) ([]byte, *url.URL, error) {
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, nil, ErrBadURL
	}
	if !st.allowed(u) {
		return nil, nil, ErrForeignHost
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, wrap(ErrBadURL, err)
	}
	req.Header.Set("Accept", "application/atom+xml, application/opensearchdescription+xml;q=0.9, application/xml;q=0.8, */*;q=0.5")
	req.Header.Set("User-Agent", version.UserAgent)
	credHost := st.root.Hostname()
	if c.Username != "" && strings.EqualFold(u.Hostname(), credHost) {
		req.SetBasicAuth(c.Username, c.Password)
	}
	resp, err := SameSite(c.http(), credHost, false).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, nil, ErrAuth
	case resp.StatusCode != http.StatusOK:
		return nil, nil, wrap(ErrStatus, fmt.Errorf("HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > maxFeedBytes {
		return nil, nil, ErrTooLarge
	}
	final := u
	if resp.Request != nil && resp.Request.URL != nil {
		final = resp.Request.URL
	}
	return body, final, nil
}

// decode parses XML; non-UTF-8 documents are read as Windows-1251 (the
// only other charset Russian catalogs use).
func decode(body []byte, v any) error {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.Strict = false
	d.CharsetReader = func(label string, in io.Reader) (io.Reader, error) {
		switch strings.ToLower(label) {
		case "windows-1251", "cp1251":
			return charmap.Windows1251.NewDecoder().Reader(in), nil
		case "utf-8", "utf8", "":
			return in, nil
		}
		return nil, fmt.Errorf("charset %s", label)
	}
	return d.Decode(v)
}

func (c *Client) feed(ctx context.Context, st *site, u *url.URL) (atomFeed, *url.URL, error) {
	body, final, err := c.get(ctx, st, u)
	if err != nil {
		return atomFeed{}, nil, err
	}
	var f atomFeed
	if err := decode(body, &f); err != nil || f.XMLName.Local != "feed" {
		if err == nil {
			err = fmt.Errorf("root element %q", f.XMLName.Local)
		}
		return atomFeed{}, nil, wrap(ErrNotCatalog, err)
	}
	return f, final, nil
}

// open reads the root feed; the host it ends on becomes the catalog's.
func (c *Client) open(ctx context.Context) (*site, atomFeed, *url.URL, error) {
	root, err := c.base()
	if err != nil {
		return nil, atomFeed{}, nil, err
	}
	st := &site{root: root}
	f, final, err := c.feed(ctx, st, root)
	if err != nil {
		return nil, atomFeed{}, nil, err
	}
	st.host = final.Hostname()
	return st, f, final, nil
}

// Test reads the root feed.
func (c *Client) Test(ctx context.Context) error {
	_, _, _, err := c.open(ctx)
	return err
}

// CatalogHost reads the root feed and returns the catalog's host after
// redirects.
func (c *Client) CatalogHost(ctx context.Context) (string, error) {
	st, _, _, err := c.open(ctx)
	if err != nil {
		return "", err
	}
	return st.host, nil
}

// Search finds books: Template, then SearchWith.
func (c *Client) Search(ctx context.Context, q string) ([]Entry, error) {
	t, err := c.Template(ctx)
	if err != nil {
		return nil, err
	}
	return c.SearchWith(ctx, t, q)
}

// Template is a catalog's search template, found once for several queries.
type Template struct {
	site *site
	searchTemplate
}

// Template reads the root feed's search link: an OpenSearch description,
// or an Atom template.
func (c *Client) Template(ctx context.Context) (Template, error) {
	st, f, final, err := c.open(ctx)
	if err != nil {
		return Template{}, err
	}
	tmpl, err := c.template(ctx, st, final, f)
	if err != nil {
		return Template{}, err
	}
	return Template{st, tmpl}, nil
}

// SearchWith reads the result feed of q for up to 3 pages (rel="next").
func (c *Client) SearchWith(ctx context.Context, t Template, q string) ([]Entry, error) {
	st, tmpl := t.site, t.searchTemplate
	if st == nil {
		return nil, ErrNoSearch
	}
	u, err := url.Parse(fillTemplate(tmpl.template, q))
	if err != nil {
		return nil, wrap(ErrNoSearch, err)
	}
	next := tmpl.base.ResolveReference(u)
	var out []Entry
	seen := map[string]bool{}
	followedBooks := false
	for page := 0; page < maxPages && next != nil; page++ {
		f, cur, err := c.feed(ctx, st, next)
		if err != nil {
			if page > 0 {
				return out, nil // later pages are a bonus
			}
			return nil, err
		}
		next = nil
		n := 0
		for _, e := range f.Entries {
			ent, ok := entryOf(cur, e)
			if !ok {
				continue
			}
			n++
			key := ent.ID
			if key == "" {
				key = ent.Links[0].Href
			}
			if !seen[key] {
				seen[key] = true
				out = append(out, ent)
			}
		}
		if n == 0 && !followedBooks {
			// Flibusta's plain search answers «Поиск книг» / «Поиск
			// авторов»: follow the books one
			if l, ok := bookSearchLink(cur, f); ok {
				followedBooks = true
				next = l
				page--
				continue
			}
		}
		for _, l := range f.Links {
			if l.Rel == "next" && l.Href != "" {
				if nu, err := cur.Parse(l.Href); err == nil {
					next = nu
				}
				break
			}
		}
	}
	return out, nil
}

type searchTemplate struct {
	template string
	base     *url.URL
}

// template finds the search link of the root feed (read from rootURL).
func (c *Client) template(ctx context.Context, st *site, rootURL *url.URL, f atomFeed) (searchTemplate, error) {
	var osd, atom []atomLink
	for _, l := range f.Links {
		if l.Rel != "search" || l.Href == "" {
			continue
		}
		if strings.Contains(l.Type, "opensearchdescription") {
			osd = append(osd, l)
		} else if strings.Contains(l.Href, "{searchTerms}") {
			atom = append(atom, l)
		}
	}
	for _, l := range osd {
		u, err := rootURL.Parse(l.Href)
		if err != nil {
			continue
		}
		body, final, err := c.get(ctx, st, u)
		if err != nil {
			if len(atom) > 0 {
				break
			}
			return searchTemplate{}, err
		}
		var d openSearch
		if decode(body, &d) != nil {
			continue
		}
		best := ""
		for _, x := range d.URLs {
			if !strings.Contains(x.Template, "{searchTerms}") {
				continue
			}
			if strings.Contains(x.Type, "atom") {
				best = x.Template
				break
			}
			if best == "" {
				best = x.Template
			}
		}
		if best != "" {
			return searchTemplate{best, final}, nil
		}
	}
	if len(atom) > 0 {
		return searchTemplate{atom[0].Href, rootURL}, nil
	}
	return searchTemplate{}, ErrNoSearch
}

var reOptionalParam = regexp.MustCompile(`\{[A-Za-z:]+\?\}`)

// fillTemplate puts q into {searchTerms} and drops optional parameters.
func fillTemplate(t, q string) string {
	t = strings.ReplaceAll(t, "{searchTerms}", url.QueryEscape(q))
	t = strings.ReplaceAll(t, "{startPage}", "1")
	t = strings.ReplaceAll(t, "{count}", "50")
	return reOptionalParam.ReplaceAllString(t, "")
}

// bookSearchLink is the «search books» entry of a navigation answer.
func bookSearchLink(cur *url.URL, f atomFeed) (*url.URL, bool) {
	for _, e := range f.Entries {
		for _, l := range e.Links {
			if strings.Contains(l.Href, "searchType=books") {
				if u, err := cur.Parse(l.Href); err == nil {
					return u, true
				}
			}
		}
	}
	return nil, false
}

// entryOf converts an entry with acquisition links (relative to cur).
func entryOf(cur *url.URL, e atomEntry) (Entry, bool) {
	out := Entry{Title: clean(e.Title), ID: strings.TrimSpace(e.ID), Lang: strings.ToLower(strings.TrimSpace(e.Lang))}
	var authors []string
	for _, a := range e.Authors {
		if n := clean(a.Name); n != "" {
			authors = append(authors, n)
		}
	}
	out.Author = strings.Join(authors, ", ")
	for _, l := range e.Links {
		u, err := cur.Parse(strings.TrimSpace(l.Href))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		switch {
		case strings.HasPrefix(l.Rel, "http://opds-spec.org/acquisition"):
			size, _ := strconv.ParseInt(l.Length, 10, 64)
			out.Links = append(out.Links, AcqLink{Href: u.String(), Type: strings.ToLower(strings.TrimSpace(l.Type)), Size: max(size, 0)})
		case l.Rel == "alternate" && strings.Contains(l.Type, "html") && out.Details == "":
			out.Details = u.String()
		}
	}
	return out, len(out.Links) > 0 && out.Title != ""
}

func clean(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return strings.Join(strings.Fields(s), " ")
}

// Formats: an acquisition type → format name, in order of preference.
type Formats []struct{ Format, Type string }

// BookFormats: EPUB > FB2 > MOBI > PDF (what the book scanner opens).
var BookFormats = Formats{
	{"EPUB", "application/epub+zip"},
	{"FB2", "application/x-fictionbook+xml"}, {"FB2", "application/fb2+zip"}, {"FB2", "application/x-zip-compressed-fb2"},
	{"FB2", "application/fb2"}, {"FB2", "text/fb2+xml"},
	{"MOBI", "application/x-mobipocket-ebook"}, {"AZW3", "application/x-mobi8-ebook"}, {"AZW3", "application/vnd.amazon.ebook"},
	{"PDF", "application/pdf"},
}

// ComicFormats: CBZ > CBR > CB7 (what the comics scanner opens).
var ComicFormats = Formats{
	{"CBZ", "application/vnd.comicbook+zip"}, {"CBZ", "application/x-cbz"},
	{"CBR", "application/vnd.comicbook-rar"}, {"CBR", "application/x-cbr"},
	{"CB7", "application/x-cb7"},
}

// mediaType strips parameters ("application/epub+zip; charset=…").
func mediaType(t string) string {
	t, _, _ = strings.Cut(strings.ToLower(t), ";")
	return strings.TrimSpace(t)
}

// FormatOf names the format of an acquisition type ("" = unknown).
func FormatOf(typ string) string {
	t := mediaType(typ)
	for _, fs := range []Formats{BookFormats, ComicFormats} {
		for _, f := range fs {
			if f.Type == t {
				return f.Format
			}
		}
	}
	return ""
}

// Best picks the most preferred link of formats; ok is false when the
// entry has none of them.
func Best(links []AcqLink, formats Formats) (AcqLink, bool) {
	for _, f := range formats {
		for _, l := range links {
			if mediaType(l.Type) == f.Type {
				return l, true
			}
		}
	}
	return AcqLink{}, false
}

// ErrorMessage is the Russian message of a catalog error ("" when err is
// not one: a network failure, a timeout).
func ErrorMessage(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Msg
	}
	return ""
}
