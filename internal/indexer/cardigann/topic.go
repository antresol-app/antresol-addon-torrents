package cardigann

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// ErrForeignTopic: a release page that is not on the source's site.
var ErrForeignTopic = errors.New("cardigann: release page not on the source's site")

// maxTopicText bounds the text TopicText returns.
const maxTopicText = 64 << 10

// TopicText reads a release's own page (its Details link) with the
// source's session and returns the text of its first post (RuTracker's
// .post_body; else the page body), one line per <br> or block. It never
// signs in: a page that asks for a sign-in is an error, so reading many
// pages during a search cannot set off the tracker's login protection.
// Only a page on the source's own site (or a subdomain) is read.
func (r *Runner) TopicText(ctx context.Context, link string) (string, error) {
	if err := r.setup(); err != nil {
		return "", err
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !sameSite(r.base, u) {
		return "", ErrForeignTopic
	}
	resp, err := r.do(ctx, &request{method: http.MethodGet, url: u.String(), follow: true, referer: r.base.String()})
	if err != nil {
		return "", err
	}
	if sessionLost(resp) || resp.status != http.StatusOK {
		return "", fmt.Errorf("%w: %s ответил %d", ErrBadResponse, r.Def.Name, resp.status)
	}
	return topicText(r.text(resp))
}

// sameSite: u's host is base's or a subdomain of it.
func sameSite(base, u *url.URL) bool {
	if base == nil {
		return false
	}
	b, h := strings.ToLower(base.Hostname()), strings.ToLower(u.Hostname())
	return h != "" && (h == b || !isIP(b) && strings.HasSuffix(h, "."+b))
}

// topicText is the text of a release page's first post, line by line.
func topicText(page string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(page))
	if err != nil {
		return "", err
	}
	doc.Find("script, style").Remove()
	doc.Find("br").ReplaceWithHtml("\n")
	doc.Find("div, p, li, tr, h1, h2, h3, h4, span.post-br, hr").Each(func(_ int, s *goquery.Selection) {
		s.AppendHtml("\n")
	})
	sel := doc.Find(".post_body").First()
	if sel.Length() == 0 {
		sel = doc.Find("body")
	}
	text := sel.Text()
	if len(text) > maxTopicText {
		text = text[:maxTopicText]
	}
	return text, nil
}
