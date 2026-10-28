package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// CookieSites are the sites that get a per-user cookie slot ("other" has none).
var CookieSites = []string{"youtube", "reddit", "instagram", "tiktok", "facebook", "twitter"}

// siteDomains maps a registrable domain to its site folder. Matching is by
// suffix, so subdomains (m.youtube.com) match but lookalikes (notyoutube.com)
// do not.
var siteDomains = []struct{ domain, site string }{
	{"youtube.com", "youtube"}, {"youtu.be", "youtube"},
	{"reddit.com", "reddit"}, {"redd.it", "reddit"},
	{"instagram.com", "instagram"},
	{"tiktok.com", "tiktok"},
	{"facebook.com", "facebook"}, {"fb.com", "facebook"}, {"fb.watch", "facebook"},
	{"x.com", "twitter"}, {"twitter.com", "twitter"},
}

var httpURLRe = regexp.MustCompile(`https?://[^\s<>"']+`)

// SharedURL returns the first http(s) URL in the share's url field, falling
// back to its text field. Share sheets often put the link in text alone, with
// surrounding prose.
func SharedURL(urlField, text string) (string, error) {
	for _, s := range []string{urlField, text} {
		if m := httpURLRe.FindString(s); m != "" {
			// Prose punctuation right after a link is not part of it.
			return strings.TrimRight(m, ".,;:!?)"), nil
		}
	}
	return "", errors.New("no link found in shared content")
}

// SiteFor returns the site folder for a link, or "other".
func SiteFor(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "other"
	}
	host := strings.ToLower(u.Hostname()) // drops user@ and :port
	for _, d := range siteDomains {
		if host == d.domain || strings.HasSuffix(host, "."+d.domain) {
			return d.site
		}
	}
	return "other"
}

var (
	fbIDPathRe  = regexp.MustCompile(`/(?:reel|videos)/(?:[^/?#]*/)?(\d{6,})`)
	fbIDQueryRe = regexp.MustCompile(`[?&]v=(\d{6,})`)
	fbShareRe   = regexp.MustCompile(`^/share/[rv]/`)
)

func facebookWatchURL(id string) string {
	return "https://www.facebook.com/watch/?v=" + id
}

func facebookID(s string) string {
	if m := fbIDPathRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	if m := fbIDQueryRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// isFacebookShortLink reports whether a facebook link hides its video id
// behind a redirect: fb.watch hosts and /share/r/ and /share/v/ paths.
func isFacebookShortLink(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if host == "fb.watch" || strings.HasSuffix(host, ".fb.watch") {
		return true
	}
	return SiteFor(u.String()) == "facebook" && fbShareRe.MatchString(u.Path)
}

// NormalizeFacebook rewrites a Facebook link to https://www.facebook.com/watch/?v=ID
// so yt-dlp can fetch it. Non-Facebook links, and links whose id cannot be
// found, come back unchanged.
func NormalizeFacebook(ctx context.Context, hc *http.Client, shared string) string {
	if SiteFor(shared) != "facebook" {
		return shared
	}
	return normalizeFacebook(ctx, hc, shared, isFacebookShortLink)
}

// normalizeFacebook takes the short-link rule as a parameter so tests can
// point it at a local server.
func normalizeFacebook(ctx context.Context, hc *http.Client, shared string, isShortLink func(*url.URL) bool) string {
	if id := facebookID(shared); id != "" {
		return facebookWatchURL(id)
	}
	u, err := url.Parse(shared)
	if err != nil || !isShortLink(u) {
		return shared
	}
	// Only the first redirect is needed: it carries the id. Following it would
	// land on a login wall.
	client := *hc
	client.Timeout = 10 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, shared, nil)
	if err != nil {
		return shared
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return shared
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
		return shared
	}
	if id := facebookID(loc); id != "" {
		return facebookWatchURL(id)
	}
	return shared
}
