package main

import (
	"regexp"
	"strings"
)

// Titles become the shared filename and the notification text, and some sites
// stuff them with noise. Keyed on the URL rather than MeTube's folder so
// downloads queued before Facebook and X got folders are cleaned too.
var (
	facebookHost = regexp.MustCompile(`^https?://([^/]*\.)?(facebook\.com|fb\.com|fb\.watch)(/|$)`)
	twitterHost  = regexp.MustCompile(`^https?://([^/]*\.)?(x\.com|twitter\.com)(/|$)`)

	// "332K views · 8.9K reactions | " ahead of the real title.
	facebookStats = regexp.MustCompile(`^[\d.,]+[KMB]? views?( · [\d.,]+[KMB]? (reactions?|comments?|shares?))* \| `)
	// yt-dlp's X title is "<uploader> - <tweet text>", and the text ends with
	// the t.co link to the media itself.
	tcoLink = regexp.MustCompile(`https?://t\.co/\w+`)
	spaces  = regexp.MustCompile(`\s+`)
)

// CleanTitle tidies a download's title for display and sharing. It never
// returns an empty string: if cleaning leaves nothing, the original stands.
func CleanTitle(url, title string) string {
	out := title
	switch {
	case facebookHost.MatchString(url):
		out = facebookStats.ReplaceAllString(out, "")
	case twitterHost.MatchString(url):
		out = tcoLink.ReplaceAllString(out, "")
		out = spaces.ReplaceAllString(out, " ")
		out = strings.TrimSpace(out)
		out = strings.TrimSuffix(out, " -")
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return title
	}
	return out
}
