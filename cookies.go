package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const maxCookieBytes = 1_000_000

var (
	errBadCookies = errors.New("that doesn't look like a cookies.txt file")
	errBadSite    = errors.New("unknown cookie site")
)

// CookieStore keeps one Netscape cookies.txt per user and site under Dir.
// MeTube reads the same path (COOKIES_DIR is mounted identically in both), so
// Path returns what MeTube will open. Contents are secrets: nothing here logs
// them, and no API returns them.
type CookieStore struct{ Dir string }

// ValidateCookies accepts a Netscape cookies file: at most 1 MB, with at
// least one data line of 7 tab-separated fields. It never echoes the data.
func ValidateCookies(data []byte) error {
	if len(data) > maxCookieBytes {
		return errBadCookies
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		// curl and yt-dlp write HttpOnly cookies behind a comment marker.
		line = strings.TrimPrefix(line, "#HttpOnly_")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if len(strings.Split(line, "\t")) == 7 {
			return nil
		}
	}
	return errBadCookies
}

func (c *CookieStore) file(userDir, site string) (string, error) {
	if !slices.Contains(CookieSites, site) {
		return "", errBadSite
	}
	if userDir == "" || userDir != filepath.Base(userDir) || userDir == "." || userDir == ".." {
		return "", errors.New("bad user directory")
	}
	return filepath.Join(c.Dir, userDir, site+".txt"), nil
}

// Put validates and stores the file: written to a temp file next to the
// target, then renamed, so MeTube never reads a half-written file.
func (c *CookieStore) Put(userDir, site string, data []byte) error {
	p, err := c.file(userDir, site)
	if err != nil {
		return err
	}
	if err := ValidateCookies(data); err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return errors.New("cookies: cannot create directory")
	}
	f, err := os.CreateTemp(dir, ".upload-*") // CreateTemp is 0600
	if err != nil {
		return errors.New("cookies: cannot create file")
	}
	defer os.Remove(f.Name()) // no-op after a successful rename
	_, werr := f.Write(data)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return errors.New("cookies: cannot write file")
	}
	if err := os.Rename(f.Name(), p); err != nil {
		return errors.New("cookies: cannot store file")
	}
	return nil
}

// Path is the cookie file for the user and site, if one exists.
func (c *CookieStore) Path(userDir, site string) (string, bool) {
	p, err := c.file(userDir, site)
	if err != nil {
		return "", false
	}
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return p, true
}

// List maps each site that has cookies to the file's mtime.
func (c *CookieStore) List(userDir string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	for _, site := range CookieSites {
		p, err := c.file(userDir, site)
		if err != nil {
			return nil, err
		}
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			out[site] = st.ModTime()
		}
	}
	return out, nil
}

// Delete removes the file; a missing one is not an error.
func (c *CookieStore) Delete(userDir, site string) error {
	p, err := c.file(userDir, site)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cookies: cannot delete file")
	}
	return nil
}

// cookieHints are the fragments yt-dlp errors carry when the site wants a
// login; matching is case-insensitive.
var cookieHints = []string{"sign in to confirm", "login required", "log in", "requires authentication",
	"use --cookies", "cookies", "rate-limit", "rate limit", "private video", "age-restricted",
	"empty media response"}

// NeedsCookies reports whether a download error looks fixable by uploading
// cookies for the site.
func NeedsCookies(msg string) bool {
	msg = strings.ToLower(msg)
	return slices.ContainsFunc(cookieHints, func(h string) bool { return strings.Contains(msg, h) })
}

func (a *App) listCookies(w http.ResponseWriter, r *http.Request) {
	out := map[string]map[string]string{}
	if a.Cookies != nil {
		times, err := a.Cookies.List(viewer(r).Dir)
		if err != nil {
			apiError(w, http.StatusInternalServerError, "cannot list cookies")
			return
		}
		for site, t := range times {
			out[site] = map[string]string{"updated": t.UTC().Format(time.RFC3339)}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// cookieSite writes the 404 itself for a site without a cookie slot.
func (a *App) cookieSite(w http.ResponseWriter, r *http.Request) (string, bool) {
	site := r.PathValue("site")
	if a.Cookies == nil || !slices.Contains(CookieSites, site) {
		apiError(w, http.StatusNotFound, "unknown site")
		return "", false
	}
	return site, true
}

func (a *App) putCookies(w http.ResponseWriter, r *http.Request) {
	site, ok := a.cookieSite(w, r)
	if !ok {
		return
	}
	// One byte past the limit, so an oversize body is detectable.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCookieBytes+1))
	if err != nil || len(body) > maxCookieBytes || ValidateCookies(bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))) != nil {
		apiError(w, http.StatusBadRequest, errBadCookies.Error())
		return
	}
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	if err := a.Cookies.Put(viewer(r).Dir, site, body); err != nil {
		apiError(w, http.StatusInternalServerError, "cannot store cookies")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) deleteCookies(w http.ResponseWriter, r *http.Request) {
	site, ok := a.cookieSite(w, r)
	if !ok {
		return
	}
	if err := a.Cookies.Delete(viewer(r).Dir, site); err != nil {
		apiError(w, http.StatusInternalServerError, "cannot delete cookies")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
