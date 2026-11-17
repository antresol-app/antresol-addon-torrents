package cardigann

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/PuerkitoBio/goquery"
)

// Prowlarr treats these fields as optional even without "optional: true".
var optionalFields = []string{"imdb", "imdbid", "tmdbid", "rageid", "tvdbid", "tvmazeid", "traktid", "doubanid", "poster", "banner", "description", "genre"}

// Search runs every search path of the definition and parses the results.
func (r *Runner) Search(ctx context.Context, q Query) ([]Result, error) {
	if err := r.setup(); err != nil {
		return nil, err
	}
	if err := r.ensureLogin(ctx); err != nil {
		return nil, err
	}
	s := &r.Def.Search
	vars := r.baseVars()
	if len(q.Config) > 0 {
		cfg := vars["Config"].(map[string]any)
		for k, v := range q.Config {
			cfg[k] = v
		}
	}
	vars["Query"] = r.queryVars(q)
	query := vars["Query"].(map[string]any)

	env := r.filterEnv(vars)
	kw, err := env.apply(query["Keywords"].(string), s.KeywordsFilters)
	if err != nil {
		return nil, fmt.Errorf("cardigann: %s: keywordsfilters: %w", r.Def.ID, err)
	}
	vars["Keywords"] = kw

	cats := r.cats.toTracker(q.Categories)
	if len(cats) == 0 {
		cats = r.cats.defaults
	}
	// A tracker with hundreds of forums per kind (RuTracker: 420 for music)
	// answers nothing at all to a filter that long: search everywhere and
	// keep the results of the wanted categories here.
	var keep map[int]bool
	if len(cats) > maxTrackerCategories && len(q.Categories) > 0 {
		cats, keep = nil, wanted(q.Categories)
	}
	headers, err := r.headers(s.Headers, vars)
	if err != nil {
		return nil, err
	}

	var results []Result
	var seen []string
	for i := range s.Paths {
		sp := &s.Paths[i]
		vars["Categories"] = cats
		if len(sp.Categories) > 0 && len(cats) > 0 {
			negate := sp.Categories[0] == "!"
			var in, out []string
			for _, c := range cats {
				if slices.Contains(sp.Categories, c) {
					in = append(in, c)
				} else {
					out = append(out, c)
				}
			}
			if (len(in) > 0) == negate {
				continue
			}
			if negate {
				vars["Categories"] = out
			} else {
				vars["Categories"] = in
			}
		}
		req, err := r.searchRequest(sp, vars, headers)
		if err != nil {
			return nil, fmt.Errorf("cardigann: %s: %w", r.Def.ID, err)
		}
		if req.method == http.MethodGet {
			if slices.Contains(seen, req.url) {
				continue
			}
			seen = append(seen, req.url)
		}
		resp, err := r.fetchAuthed(ctx, req)
		if err != nil {
			return nil, err
		}
		if resp.isRedirect() {
			return nil, fmt.Errorf("%w: %s перенаправил на %s", ErrBadResponse, r.Def.Name, resp.location)
		}
		// some trackers (MegaPeer) answer a search that finds nothing with
		// a 404 page; without keywords (the source check) a 404 still
		// means the search page is gone
		if resp.status == http.StatusNotFound && kw != "" {
			continue
		}
		if resp.status != http.StatusOK {
			return nil, fmt.Errorf("%w: %s ответил %d", ErrBadResponse, r.Def.Name, resp.status)
		}
		page, err := r.parseResults(resp, sp, vars)
		if err != nil {
			return nil, fmt.Errorf("cardigann: %s: %w", r.Def.ID, err)
		}
		for _, rel := range page {
			if keep == nil || len(rel.Category) == 0 || slices.ContainsFunc(rel.Category, func(c int) bool { return keep[c] }) {
				results = append(results, rel)
			}
		}
	}
	return results, nil
}

// maxTrackerCategories is the longest category filter sent to a tracker.
const maxTrackerCategories = 200

// queryVars fills .Query.* like GetQueryVariableDefaults and the per-type
// GetSearchRequests of Prowlarr.
func (r *Runner) queryVars(q Query) map[string]any {
	m := map[string]any{}
	for _, k := range []string{"Type", "Q", "Limit", "Offset", "Extended", "APIKey", "Genre", "Movie", "Year", "IMDBID",
		"IMDBIDShort", "TMDBID", "Series", "Ep", "Season", "TVDBID", "TVRageID", "TVMazeID", "TraktID", "DoubanID",
		"Episode", "Album", "Artist", "Label", "Track", "Author", "Title", "Publisher", "Keywords"} {
		m[k] = ""
	}
	typ := q.Type
	if typ == "" {
		typ = deriveType(q)
	}
	m["Type"] = typ
	m["Q"] = strings.TrimSpace(q.Keywords)
	cats := make([]string, len(q.Categories))
	for i, c := range q.Categories {
		cats[i] = strconv.Itoa(c)
	}
	m["Categories"] = cats
	if q.Season > 0 {
		m["Season"] = strconv.Itoa(q.Season)
	}
	if q.Episode > 0 {
		m["Ep"] = strconv.Itoa(q.Episode)
	}
	switch {
	case q.Season > 0 && q.Episode > 0:
		m["Episode"] = fmt.Sprintf("S%02dE%02d", q.Season, q.Episode)
	case q.Season > 0:
		m["Episode"] = fmt.Sprintf("S%02d", q.Season)
	}
	if id := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(q.IMDBID)), "tt"); id != "" {
		if n, err := strconv.Atoi(id); err == nil && n > 0 {
			m["IMDBIDShort"] = fmt.Sprintf("%07d", n)
			m["IMDBID"] = "tt" + m["IMDBIDShort"].(string)
		}
	}
	var kw []string
	for _, k := range []string{"Q", "Series", "Movie", "Year", "Episode"} {
		if v := m[k].(string); strings.TrimSpace(v) != "" {
			kw = append(kw, v)
		}
	}
	m["Keywords"] = strings.Join(kw, " ")
	return m
}

func deriveType(q Query) string {
	if q.Season > 0 || q.Episode > 0 {
		return "tvsearch"
	}
	movie, tv := false, false
	for _, c := range q.Categories {
		movie = movie || c/1000 == 2
		tv = tv || c/1000 == 5
	}
	switch {
	case q.IMDBID != "" && !tv, movie && !tv:
		return "movie"
	case tv && !movie:
		return "tvsearch"
	}
	return "search"
}

func (r *Runner) searchRequest(sp *SearchPath, vars map[string]any, headers map[string]string) (*request, error) {
	// path variables are URL-encoded (WebUtility.UrlEncode, "+" -> "%20")
	p, err := renderTemplate(sp.Path, vars, url.QueryEscape)
	if err != nil {
		return nil, err
	}
	target, err := resolve(r.base, strings.ReplaceAll(p, "+", "%20"))
	if err != nil {
		return nil, err
	}
	var pairs []formPair
	inputs := []OrderedMap{sp.Inputs}
	if sp.InheritInputs == nil || *sp.InheritInputs {
		inputs = []OrderedMap{r.Def.Search.Inputs, sp.Inputs}
	}
	for _, in := range inputs {
		for _, kv := range in {
			if kv.Key == "$raw" {
				raw, err := renderTemplate(kv.Value, vars, url.QueryEscape)
				if err != nil {
					return nil, err
				}
				for _, part := range strings.Split(raw, "&") {
					k, v, _ := strings.Cut(part, "=")
					if k != "" {
						pairs = append(pairs, formPair{key: k, value: v, raw: true})
					}
				}
				continue
			}
			v, err := renderTemplate(kv.Value, vars, nil)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(v) != "" || r.Def.Search.AllowEmptyInputs {
				pairs = append(pairs, formPair{key: kv.Key, value: v})
			}
		}
	}
	req := &request{method: http.MethodGet, url: target, headers: headers, follow: sp.FollowRedirect || r.Def.FollowRedirect}
	if strings.EqualFold(sp.Method, "post") {
		req.method, req.form = http.MethodPost, pairs
	} else if len(pairs) > 0 {
		req.url = appendQuery(target, encodePairs(r.enc, pairs, sp.QuerySeparator))
	}
	return req, nil
}

// parseResults dispatches on response.type (CardigannParser.ParseResponse).
func (r *Runner) parseResults(resp *response, sp *SearchPath, vars map[string]any) ([]Result, error) {
	text := r.text(resp)
	if err := r.checkSearchErrors(text, vars); err != nil {
		return nil, err
	}
	typ := "html"
	if sp.Response != nil && sp.Response.Type != "" {
		typ = strings.ToLower(sp.Response.Type)
	}
	if typ == "json" {
		return r.parseJSON(text, resp.url, sp, vars)
	}
	return r.parseDOM(text, typ == "xml", resp.url, vars)
}

func (r *Runner) checkSearchErrors(text string, vars map[string]any) error {
	errs := r.Def.Search.Error
	if len(errs) == 0 {
		return nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(text))
	if err != nil {
		return nil
	}
	for _, b := range errs {
		el, err := queryFirst(doc.Selection, b.Selector)
		if err != nil || el.Length() == 0 {
			continue
		}
		msg := el.Text()
		if b.Message != nil {
			if m, _, err := r.handleSelector(b.Message, doc.Selection.Children().First(), r.filterEnv(vars), false); err == nil && m != "" {
				msg = m
			}
		}
		return fmt.Errorf("%w: %s", ErrBadResponse, strings.Join(strings.Fields(msg), " "))
	}
	return nil
}

func (r *Runner) parseDOM(text string, isXML bool, pageURL *url.URL, vars map[string]any) ([]Result, error) {
	s := &r.Def.Search
	env := r.filterEnv(vars)
	if len(s.PreprocessingFilters) > 0 {
		var err error
		if text, err = env.apply(text, s.PreprocessingFilters); err != nil {
			return nil, err
		}
	}
	var doc *goquery.Document
	var err error
	if isXML {
		doc, err = parseXMLDocument(text)
	} else {
		doc, err = goquery.NewDocumentFromReader(strings.NewReader(text))
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadResponse, err)
	}
	rowsSel, err := renderTemplate(s.Rows.Selector.Selector, vars, nil)
	if err != nil {
		return nil, err
	}
	found, err := queryAll(doc.Selection, rowsSel)
	if err != nil {
		return nil, err
	}
	var rows []*goquery.Selection
	found.Each(func(_ int, row *goquery.Selection) { rows = append(rows, row) })
	if after := s.Rows.After; after > 0 {
		var merged []*goquery.Selection
		for i := 0; i < len(rows); i += after + 1 {
			for j := 1; j <= after && i+j < len(rows); j++ {
				rows[i].AppendSelection(rows[i+j].Contents())
			}
			merged = append(merged, rows[i])
		}
		rows = merged
	}
	var out []Result
	for _, row := range rows {
		res, err := r.parseRow(vars, pageURL, false, func(sel *Selector, required bool) (string, bool, error) {
			return r.handleSelector(sel, row, env, required)
		})
		if err != nil || res == nil {
			continue
		}
		if res.PublishDate.IsZero() && s.Rows.DateHeaders != nil {
			res.PublishDate = r.dateFromHeaders(row, env)
		}
		r.finish(res)
		if res.Title != "" {
			out = append(out, *res)
		}
	}
	return out, nil
}

// dateFromHeaders walks back from row to the nearest date header row.
func (r *Runner) dateFromHeaders(row *goquery.Selection, env *filterEnv) time.Time {
	prev := func(n *goquery.Selection) *goquery.Selection {
		if p := n.Prev(); p.Length() > 0 {
			return p
		}
		return n.Parent().Prev()
	}
	for cur := prev(row); cur.Length() > 0; cur = prev(cur) {
		if v, ok, err := r.handleSelector(r.Def.Search.Rows.DateHeaders, cur, env, true); err == nil && ok {
			if t, err := parseUnknown(v, time.Now()); err == nil {
				return t
			}
			return time.Time{}
		}
	}
	return time.Time{}
}

func (r *Runner) parseJSON(text string, pageURL *url.URL, sp *SearchPath, vars map[string]any) ([]Result, error) {
	rows := &r.Def.Search.Rows
	if sp.Response != nil && sp.Response.NoResultsMessage != nil {
		msg := *sp.Response.NoResultsMessage
		if (msg != "" && strings.Contains(text, msg)) || (msg == "" && strings.TrimSpace(text) == "") {
			return nil, nil
		}
	}
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: некорректный JSON: %v", ErrBadResponse, err)
	}
	env := r.filterEnv(vars)
	if rows.Count != nil {
		if v, _, err := r.handleJSONSelector(rows.Count, doc, env, true); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n < 1 {
				return nil, nil
			}
		}
	}
	rowSel, err := renderTemplate(rows.Selector.Selector, vars, nil)
	if err != nil {
		return nil, err
	}
	path, _, _ := strings.Cut(rowSel, ":")
	tok, ok := selectToken(doc, path)
	arr, isArr := tok.([]any)
	if !ok || !isArr {
		if rows.MissingAttributeEqualsNoResults || (ok && tok == nil) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: нет списка результатов %q", ErrBadResponse, path)
	}
	var out []Result
	for _, row := range arr {
		if _, ok := jsonFieldSelector(row, rowSel[len(path):]); !ok {
			continue
		}
		obj := row
		if rows.Attribute != "" {
			v, ok := selectToken(row, rows.Attribute)
			if !ok || v == nil {
				if rows.MissingAttributeEqualsNoResults {
					continue
				}
				return nil, fmt.Errorf("%w: нет поля %q", ErrBadResponse, rows.Attribute)
			}
			obj = v
		}
		items := []any{obj}
		if rows.Multiple {
			switch x := obj.(type) {
			case []any:
				items = x
			case map[string]any:
				items = items[:0]
				for _, v := range x {
					items = append(items, v)
				}
			}
		}
		for _, item := range items {
			res, err := r.parseRow(vars, pageURL, true, func(sel *Selector, required bool) (string, bool, error) {
				parent := item
				if strings.HasPrefix(sel.Selector, "..") {
					parent = row
				}
				return r.handleJSONSelector(sel, parent, env, required)
			})
			if err != nil {
				return nil, err
			}
			r.finish(res)
			if res.Title != "" {
				out = append(out, *res)
			}
		}
	}
	return out, nil
}

type getter func(sel *Selector, required bool) (string, bool, error)

// parseRow evaluates all fields of one row. With strict (JSON, as in
// Prowlarr) a failing required field fails the response; for HTML it is
// skipped and the row kept.
func (r *Runner) parseRow(vars map[string]any, pageURL *url.URL, strict bool, get getter) (*Result, error) {
	fields := r.Def.Search.Fields
	res := map[string]any{}
	for _, f := range fields {
		name, _, _ := strings.Cut(f.Key, "|")
		res[name] = ""
	}
	vars["Result"] = res
	rel := &Result{DownloadVolumeFactor: 1, UploadVolumeFactor: 1}
	for i := range fields {
		f := &fields[i]
		parts := strings.Split(f.Key, "|")
		name, mods := parts[0], parts[1:]
		optional := slices.Contains(optionalFields, f.Key) || slices.Contains(mods, "optional") || f.Optional
		value, found, err := get(&f.Selector, !optional)
		if err == nil && optional && (!found || strings.TrimSpace(value) == "") {
			def, derr := renderTemplate(f.Default, vars, nil)
			if derr != nil || strings.TrimSpace(def) == "" {
				res[name] = ""
				continue
			}
			value = def
		}
		if err == nil {
			var parsed string
			if parsed, err = r.applyField(rel, name, mods, value, pageURL); err == nil {
				res[name] = parsed
				continue
			}
		}
		res[name] = ""
		if optional {
			continue
		}
		if strict {
			return nil, fmt.Errorf("%w: поле %s: %v", ErrBadResponse, f.Key, err)
		}
		r.log().Debug("cardigann: field failed", "indexer", r.Def.ID, "field", f.Key, "err", err)
	}
	return rel, nil
}

// applyField stores a field value into the release (CardigannParser.ParseFields).
func (r *Runner) applyField(rel *Result, name string, mods []string, value string, pageURL *url.URL) (string, error) {
	switch name {
	case "download":
		if value == "" {
			rel.Link = ""
			return "", nil
		}
		if strings.HasPrefix(value, "magnet:") {
			rel.Magnet = value
			return value, nil
		}
		u, err := resolve(pageURL, value)
		if err != nil {
			return "", err
		}
		rel.Link = u
		return u, nil
	case "magnet":
		rel.Magnet = value
	case "infohash":
		rel.InfoHash = value
	case "details", "comments":
		u, err := resolve(pageURL, value)
		if err != nil {
			return "", err
		}
		if name == "details" || rel.Details == "" {
			rel.Details = u
		}
		return u, nil
	case "title":
		if slices.Contains(mods, "append") {
			rel.Title += value
		} else {
			rel.Title = value
		}
		return rel.Title, nil
	case "category", "categorydesc":
		cats := r.cats.fromTracker(value)
		if name == "categorydesc" {
			cats = r.cats.fromTrackerDesc(value)
		}
		if len(cats) > 0 {
			if rel.Category == nil || slices.Contains(mods, "noappend") {
				rel.Category = cats
			} else {
				for _, c := range cats {
					if !slices.Contains(rel.Category, c) {
						rel.Category = append(rel.Category, c)
					}
				}
			}
		}
		parts := make([]string, len(rel.Category))
		for i, c := range rel.Category {
			parts[i] = strconv.Itoa(c)
		}
		return strings.Join(parts, ","), nil
	case "size":
		rel.Size = parseBytes(value)
		return strconv.FormatInt(rel.Size, 10), nil
	case "seeders", "leechers":
		n := coerceInt(value)
		if n >= 5000000 {
			n = 0
		}
		if name == "seeders" {
			rel.Seeders = n
		} else {
			rel.Leechers = n
		}
		return strconv.Itoa(n), nil
	case "date":
		t, err := parseUnknown(value, time.Now())
		if err != nil {
			return "", err
		}
		rel.PublishDate = t
		return formatDate(t), nil
	case "downloadvolumefactor", "uploadvolumefactor":
		f, err := coerceFloat(value)
		if err != nil {
			return "", err
		}
		if name == "downloadvolumefactor" {
			rel.DownloadVolumeFactor = f
		} else {
			rel.UploadVolumeFactor = f
		}
		return strconv.FormatFloat(f, 'f', -1, 64), nil
	case "files", "grabs", "year", "minimumseedtime":
		return strconv.Itoa(coerceInt(value)), nil
	}
	return value, nil
}

// finish derives magnet/infohash like the end of CardigannParser.ParseResponse.
func (r *Runner) finish(rel *Result) {
	rel.InfoHash = normalizeHash(rel.InfoHash)
	if rel.Magnet == "" && rel.InfoHash != "" && r.Def.Type != "private" {
		rel.Magnet = buildMagnet(rel.InfoHash, rel.Title)
	}
	if rel.Magnet != "" && rel.InfoHash == "" {
		rel.InfoHash = normalizeHash(infoHashFromMagnet(rel.Magnet))
	}
}

// coerceInt keeps digits only ("1 520", " 87" -> 1520, 87).
func coerceInt(s string) int {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	n, _ := strconv.Atoi(b.String())
	return n
}

// coerceFloat is ParseUtil.CoerceDouble: "," is a decimal separator and only
// the last "." separates decimals.
func coerceFloat(s string) (float64, error) {
	var b strings.Builder
	for _, c := range s {
		if (c >= '0' && c <= '9') || c == '.' || c == ',' {
			b.WriteRune(c)
		}
	}
	v := strings.ReplaceAll(b.String(), ",", ".")
	if v == "" {
		return 0, nil
	}
	if strings.Count(v, ".") > 1 {
		last := strings.LastIndex(v, ".")
		v = strings.ReplaceAll(v[:last], ".", "") + v[last:]
	}
	return strconv.ParseFloat(v, 64)
}

// parseBytes is ParseUtil.GetBytes (plus Russian unit names).
func parseBytes(s string) int64 {
	var unit strings.Builder
	for _, c := range s {
		if unicode.IsLetter(c) {
			unit.WriteRune(unicode.ToLower(c))
		}
	}
	u := strings.NewReplacer("i", "", "кб", "kb", "мб", "mb", "гб", "gb", "тб", "tb").Replace(unit.String())
	v, err := coerceFloat(s)
	if err != nil {
		return 0
	}
	mult := 1.0
	switch {
	case strings.Contains(u, "kb"):
		mult = 1 << 10
	case strings.Contains(u, "mb"):
		mult = 1 << 20
	case strings.Contains(u, "gb"):
		mult = 1 << 30
	case strings.Contains(u, "tb"):
		mult = 1 << 40
	}
	return int64(v * mult)
}
