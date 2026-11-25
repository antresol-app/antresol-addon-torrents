package cardigann

import (
	"net/url"
	"strings"
)

// SiteHost is the lower-case host of a site address: a URL, or a bare host
// such as an API host. "" when it names none.
func SiteHost(site string) string {
	site = strings.TrimSpace(site)
	if site == "" {
		return ""
	}
	if !strings.Contains(site, "//") {
		site = "//" + site
	}
	u, err := url.Parse(site)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// HostOnSite: host is the host of one of the site addresses or, when
// subdomains is set, a subdomain of it (never of an IP).
func HostOnSite(sites []string, host string, subdomains bool) bool {
	host = strings.ToLower(host)
	if host == "" {
		return false
	}
	for _, site := range sites {
		h := SiteHost(site)
		if h != "" && (host == h || subdomains && !isIP(h) && strings.HasSuffix(host, "."+h)) {
			return true
		}
	}
	return false
}
