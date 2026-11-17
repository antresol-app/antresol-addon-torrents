package cardigann

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/antresol-app/antresol-addon-torrents/internal/corecopy/i18n"
)

// Login authenticates according to login.method (post, form, cookie, get,
// oneurl) and, when login.test.path is set, verifies the session. Without a
// login block it is a no-op.
func (r *Runner) Login(ctx context.Context) error {
	if err := r.setup(); err != nil {
		return err
	}
	if r.Def.Login == nil {
		return nil
	}
	r.loginMu.Lock()
	defer r.loginMu.Unlock()
	err := r.doLogin(ctx)
	if err == nil {
		err = r.verifyLogin(ctx)
	}
	r.mu.Lock()
	r.loggedIn = err == nil
	r.mu.Unlock()
	if err == nil {
		r.noteCookies()
	}
	return err
}

// noteCookies keeps the session cookies of a cookie-login definition (a
// sign-in again starts from them, not from the configured cookie) and
// reports them to OnCookies when they changed since the last report (see
// OnCookies). The caller has just had a signed-in response.
func (r *Runner) noteCookies() {
	if r.Def.Login == nil || r.Def.Login.Method != "cookie" {
		return
	}
	cur := r.Cookies()
	set := cookieSet(cur)
	if set == "" {
		return
	}
	now := time.Now()
	r.mu.Lock()
	r.liveCookie = cur
	if r.OnCookies == nil || set == r.sentSet || !r.sentAt.IsZero() && now.Sub(r.sentAt) < CookieInterval {
		r.mu.Unlock()
		return
	}
	r.sentSet, r.sentAt = set, now
	r.mu.Unlock()
	r.OnCookies(cur)
}

// cookieSet is a "a=b; c=d" header as a sorted set, to compare sessions.
func cookieSet(header string) string {
	var parts []string
	for _, p := range strings.Split(header, ";") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	slices.Sort(parts)
	return strings.Join(parts, "; ")
}

func (r *Runner) ensureLogin(ctx context.Context) error {
	if r.Def.Login == nil {
		return nil
	}
	r.mu.Lock()
	ok := r.loggedIn
	r.mu.Unlock()
	if ok {
		return nil
	}
	return r.Login(ctx)
}

func (r *Runner) doLogin(ctx context.Context) error {
	l := r.Def.Login
	vars := r.baseVars()
	headers, err := r.headers(firstHeaders(l.Headers, r.Def.Search.Headers), vars)
	if err != nil {
		return err
	}
	loginURL := ""
	if l.Path != "" || l.Method != "cookie" {
		p, err := renderTemplate(l.Path, vars, nil)
		if err != nil {
			return err
		}
		if loginURL, err = resolve(r.base, p); err != nil {
			return err
		}
	}
	inputs := func() ([]formPair, error) {
		var pairs []formPair
		for _, in := range l.Inputs {
			v, err := renderTemplate(in.Value, vars, nil)
			if err != nil {
				return nil, err
			}
			pairs = append(pairs, formPair{key: in.Key, value: v})
		}
		return pairs, nil
	}
	switch l.Method {
	case "post":
		pairs, err := inputs()
		if err != nil {
			return err
		}
		r.setCookieHeader(strings.Join(l.Cookies, "; "))
		resp, err := r.do(ctx, &request{method: http.MethodPost, url: loginURL, form: pairs, headers: headers, referer: r.base.String(), follow: true})
		if err != nil {
			return err
		}
		return r.checkErrors(resp, l.Error, vars)
	case "form":
		return r.formLogin(ctx, loginURL, headers, vars)
	case "cookie":
		r.mu.Lock()
		cookie := r.liveCookie // a session the site refreshed beats the configured cookie
		r.mu.Unlock()
		if cookie == "" {
			cookie = strings.TrimSpace(r.Settings["cookie"])
		}
		if cookie == "" {
			if v, ok := l.Inputs.Get("cookie"); ok {
				cookie, _ = renderTemplate(v, vars, nil)
			}
		}
		if strings.TrimSpace(cookie) == "" {
			return loginFailed(i18n.Errorf("не указан cookie"))
		}
		r.setCookieHeader(cookie)
		r.mu.Lock()
		r.sentSet = cookieSet(r.Cookies()) // the applied cookies are known: not a change
		r.mu.Unlock()
		return nil
	case "get", "oneurl":
		target := loginURL
		if l.Method == "get" {
			pairs, err := inputs()
			if err != nil {
				return err
			}
			target = appendQuery(loginURL, encodePairs(r.enc, pairs, "&"))
		} else {
			one, _ := l.Inputs.Get("oneurl")
			s, err := renderTemplate(one, vars, nil)
			if err != nil {
				return err
			}
			if target, err = resolve(r.base, strings.TrimSuffix(r.renderOr(l.Path, vars), "/")+s); err != nil {
				return err
			}
		}
		resp, err := r.do(ctx, &request{method: http.MethodGet, url: target, headers: headers, referer: r.base.String(), follow: true})
		if err != nil {
			return err
		}
		return r.checkErrors(resp, l.Error, vars)
	default:
		return fmt.Errorf("cardigann: %s: login method %q is not supported", r.Def.ID, l.Method)
	}
}

func (r *Runner) renderOr(tpl string, vars map[string]any) string {
	s, err := renderTemplate(tpl, vars, nil)
	if err != nil {
		return tpl
	}
	return s
}

// formLogin loads the login page, fills the form and submits it.
func (r *Runner) formLogin(ctx context.Context, loginURL string, headers map[string]string, vars map[string]any) error {
	l := r.Def.Login
	r.setCookieHeader(strings.Join(l.Cookies, "; "))
	landing, err := r.do(ctx, &request{method: http.MethodGet, url: loginURL, headers: headers, referer: r.base.String(), follow: true})
	if err != nil {
		return err
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(r.text(landing)))
	if err != nil {
		return loginFailed(i18n.Errorf("страница входа не читается: %v", err))
	}
	formSel := l.Form
	if formSel == "" {
		formSel = "form"
	}
	form, err := queryFirst(doc.Selection, formSel)
	if err != nil || form.Length() == 0 {
		return loginFailed(i18n.Errorf("на странице %s нет формы входа", loginURL))
	}
	var pairs []formPair
	set := func(k, v string) {
		for i := range pairs {
			if pairs[i].key == k {
				pairs[i].value = v
				return
			}
		}
		pairs = append(pairs, formPair{key: k, value: v})
	}
	form.Find("input").Each(func(_ int, in *goquery.Selection) {
		name, ok := in.Attr("name")
		if !ok || name == "" {
			return
		}
		if _, disabled := in.Attr("disabled"); disabled {
			return
		}
		typ := strings.ToLower(in.AttrOr("type", ""))
		if _, checked := in.Attr("checked"); (typ == "checkbox" || typ == "radio") && !checked {
			return
		}
		set(name, in.AttrOr("value", ""))
	})
	for _, in := range l.Inputs {
		v, err := renderTemplate(in.Value, vars, nil)
		if err != nil {
			return err
		}
		key := in.Key
		if l.Selectors {
			el, err := queryFirst(doc.Selection, in.Key)
			if err != nil || el.Length() == 0 {
				return loginFailed(i18n.Errorf("поле %s не найдено", in.Key))
			}
			key = el.AttrOr("name", in.Key)
		}
		set(key, v)
	}
	env := r.filterEnv(vars)
	root := doc.Selection.Children().First()
	for _, si := range l.SelectorInputs {
		v, found, err := r.handleSelector(&si.Selector, root, env, !si.Optional)
		if err != nil {
			return loginFailed(i18n.Errorf("поле %s: %v", si.Key, err))
		}
		if found || !si.Optional {
			set(si.Key, v)
		}
	}
	var query []formPair
	for _, si := range l.GetSelectorInputs {
		v, found, err := r.handleSelector(&si.Selector, root, env, !si.Optional)
		if err != nil {
			return loginFailed(i18n.Errorf("поле %s: %v", si.Key, err))
		}
		if found || !si.Optional {
			query = append(query, formPair{key: si.Key, value: v})
		}
	}
	action := form.AttrOr("action", "")
	if l.SubmitPath != "" {
		action = l.SubmitPath
	}
	submit, err := resolve(landing.url, action)
	if err != nil {
		return err
	}
	submit = appendQuery(submit, encodePairs(r.enc, query, "&"))

	if doc.Find(`script[src*="simpleCaptcha"]`).Length() > 0 {
		capURL, _ := resolve(r.base, "simpleCaptcha.php?numImages=1")
		resp, err := r.do(ctx, &request{method: http.MethodGet, url: capURL, referer: loginURL})
		if err != nil {
			return err
		}
		var sc struct {
			Images []struct {
				Hash string `json:"hash"`
			} `json:"images"`
		}
		if json.Unmarshal(resp.body, &sc) != nil || len(sc.Images) == 0 {
			return fmt.Errorf("%w: %w", ErrCaptcha, i18n.Errorf("simpleCaptcha не распознана"))
		}
		set("captchaSelection", sc.Images[0].Hash)
		set("submitme", "X")
	}
	if c := l.Captcha; c != nil {
		text := r.Settings["captcha"]
		if text == "" {
			text = r.Settings["CAPTCHA"]
		}
		switch {
		case text != "":
			key := c.Input
			if l.Selectors {
				if el, err := queryFirst(doc.Selection, c.Input); err == nil && el.Length() > 0 {
					key = el.AttrOr("name", c.Input)
				}
			}
			set(key, text)
		case c.Selector != "":
			if el, err := queryFirst(doc.Selection, c.Selector); err == nil && el.Length() > 0 {
				return ErrCaptcha
			}
		}
	}

	req := &request{method: http.MethodPost, url: submit, form: pairs, headers: headers, referer: loginURL, follow: true}
	if strings.EqualFold(form.AttrOr("enctype", ""), "multipart/form-data") {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for _, p := range pairs {
			mw.WriteField(encodeString(r.enc, p.key), encodeString(r.enc, p.value))
		}
		mw.Close()
		req.body, req.contentType = buf.Bytes(), mw.FormDataContentType()
	}
	resp, err := r.do(ctx, req)
	if err != nil {
		return err
	}
	// sites like RuTracker answer a failed login with a captcha: say so
	// instead of reporting a wrong password
	if c := l.Captcha; c != nil && c.Selector != "" && r.Settings["captcha"] == "" && r.Settings["CAPTCHA"] == "" {
		if after, err := goquery.NewDocumentFromReader(strings.NewReader(r.text(resp))); err == nil {
			if el, err := queryFirst(after.Selection, c.Selector); err == nil && el.Length() > 0 {
				return ErrCaptcha
			}
		}
	}
	return r.checkErrors(resp, l.Error, vars)
}

// CookieName is a pasted cookie's name without what is not a name around
// it: a header copied from a terminal or devtools can start with a prompt
// («❯   bb_guid»), and the cookie jar drops a name like that silently.
func CookieName(k string) string {
	return strings.TrimFunc(k, func(c rune) bool { return !isTokenRune(c) })
}

// isTokenRune: a character of an RFC 6265 cookie-name (an RFC 2616 token).
func isTokenRune(c rune) bool {
	return c > 0x20 && c < 0x7f && !strings.ContainsRune(`()<>@,;:\"/[]?={} `, c)
}

// setCookieHeader stores "a=b; c=d" cookies for the site (and subdomains).
func (r *Runner) setCookieHeader(header string) {
	if r.client.Jar == nil || strings.TrimSpace(header) == "" {
		return
	}
	var cs []*http.Cookie
	for _, part := range strings.Split(header, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		k = CookieName(k)
		if !ok || k == "" {
			continue
		}
		switch strings.ToUpper(k) {
		case "COMMENT", "COMMENTURL", "DISCORD", "DOMAIN", "EXPIRES", "MAX-AGE", "PATH", "PORT", "SECURE", "VERSION", "HTTPONLY", "SAMESITE":
			continue
		}
		c := &http.Cookie{Name: k, Value: strings.Trim(strings.TrimSpace(v), `"`), Path: "/"}
		if h := r.base.Hostname(); !isIP(h) && strings.Contains(h, ".") {
			c.Domain = h
		}
		cs = append(cs, c)
	}
	r.client.Jar.SetCookies(r.base, cs)
}

// checkErrors is CardigannRequestGenerator.CheckForError.
func (r *Runner) checkErrors(resp *response, blocks []ErrorBlock, vars map[string]any) error {
	if resp.status == http.StatusUnauthorized {
		return loginFailed(i18n.Errorf("неверный логин или пароль"))
	}
	if len(blocks) == 0 {
		return nil
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(r.text(resp)))
	if err != nil {
		return nil
	}
	for _, b := range blocks {
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
		msg = strings.Join(strings.Fields(msg), " ")
		if msg == "" {
			msg = "сайт сообщил об ошибке"
		}
		return fmt.Errorf("%w: %s", ErrLoginFailed, msg)
	}
	return nil
}

// verifyLogin opens login.test.path and checks login.test.selector.
func (r *Runner) verifyLogin(ctx context.Context) error {
	t := r.Def.Login.Test
	if t == nil || t.Path == "" {
		return nil
	}
	p, err := renderTemplate(t.Path, r.baseVars(), nil)
	if err != nil {
		return err
	}
	target, err := resolve(r.base, p)
	if err != nil {
		return err
	}
	resp, err := r.do(ctx, &request{method: http.MethodGet, url: target, referer: r.base.String(), follow: r.Def.FollowRedirect})
	if err != nil {
		return err
	}
	if r.loginNeeded(resp) {
		if r.Def.Login.Method == "cookie" {
			return loginFailed(i18n.Errorf("cookie устарел или неверен"))
		}
		return loginFailed(i18n.Errorf("сайт не принял вход"))
	}
	return nil
}

// loginNeeded is CardigannRequestGenerator.CheckIfLoginIsNeeded.
func (r *Runner) loginNeeded(resp *response) bool {
	l := r.Def.Login
	if l == nil {
		return false
	}
	if resp.isRedirect() || resp.status == http.StatusUnauthorized || resp.status == http.StatusForbidden {
		return true
	}
	if resp.status >= 400 {
		return false // a server error, not a lost session: reported by the caller
	}
	if l.Test == nil || l.Test.Selector == "" {
		return false
	}
	ct := resp.header.Get("Content-Type")
	if ct != "" && !strings.Contains(ct, "text/html") {
		return false
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(r.text(resp)))
	if err != nil {
		return true
	}
	m, err := compileSelector(l.Test.Selector)
	return err != nil || doc.FindMatcher(m).Length() == 0
}

// loginFailed: ErrLoginFailed with a user-facing detail (an i18n error,
// so the detail reads in the request's language).
func loginFailed(detail error) error { return fmt.Errorf("%w: %w", ErrLoginFailed, detail) }

var errSessionLost = loginFailed(i18n.Errorf("сайт снова просит войти"))

// fetchAuthed sends req and, if the response shows the session is gone,
// logs in again once and repeats the request.
func (r *Runner) fetchAuthed(ctx context.Context, req *request) (*response, error) {
	return r.fetchRelogin(ctx, req, r.loginNeeded)
}

// fetchRelogin is fetchAuthed with a custom "session lost" check.
func (r *Runner) fetchRelogin(ctx context.Context, req *request, needed func(*response) bool) (*response, error) {
	resp, err := r.do(ctx, req)
	if err != nil || r.Def.Login == nil || !needed(resp) {
		if err == nil && r.Def.Login != nil && resp.status < 400 {
			r.noteCookies()
		}
		return resp, err
	}
	if strings.HasPrefix(resp.location, "magnet:") {
		return resp, nil
	}
	if err := r.Login(ctx); err != nil {
		return nil, err
	}
	if resp, err = r.do(ctx, req); err != nil {
		return nil, err
	}
	if needed(resp) && !strings.HasPrefix(resp.location, "magnet:") {
		return nil, errSessionLost
	}
	r.noteCookies()
	return resp, nil
}
