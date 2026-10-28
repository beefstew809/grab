package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cookieSample = "# Netscape HTTP Cookie File\n.example.com\tTRUE\t/\tTRUE\t0\tsid\tabc123\n"

func TestValidateCookies(t *testing.T) {
	for name, tc := range map[string]struct {
		data string
		ok   bool
	}{
		"valid":        {cookieSample, true},
		"httponly":     {"#HttpOnly_.example.com\tTRUE\t/\tTRUE\t0\tsid\tabc\r\n", true},
		"html":         {"<html><body>Login</body></html>", false},
		"only comment": {"# Netscape HTTP Cookie File\n# more\n\n", false},
		"six fields":   {".example.com\tTRUE\t/\tTRUE\t0\tsid\n", false},
		"too big":      {cookieSample + strings.Repeat("#", 1_000_000), false},
	} {
		if err := ValidateCookies([]byte(tc.data)); (err == nil) != tc.ok {
			t.Errorf("%s: err %v", name, err)
		}
	}
}

func TestCookiePutListDelete(t *testing.T) {
	c := &CookieStore{Dir: t.TempDir()}
	if m, err := c.List("alex"); err != nil || m == nil || len(m) != 0 {
		t.Fatalf("empty list: %v %v", m, err)
	}
	if err := c.Put("alex", "youtube", []byte(cookieSample)); err != nil {
		t.Fatal(err)
	}
	p, ok := c.Path("alex", "youtube")
	if !ok || p != filepath.Join(c.Dir, "alex", "youtube.txt") {
		t.Fatalf("path %q %v", p, ok)
	}
	if st, err := os.Stat(p); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file %v %v", st, err)
	}
	if st, _ := os.Stat(filepath.Dir(p)); st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	if es, _ := os.ReadDir(filepath.Dir(p)); len(es) != 1 {
		t.Errorf("temp file left behind: %v", es)
	}
	if m, err := c.List("alex"); err != nil || len(m) != 1 || m["youtube"].IsZero() {
		t.Fatalf("list %v %v", m, err)
	}
	if err := c.Delete("alex", "youtube"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Path("alex", "youtube"); ok {
		t.Error("still there after delete")
	}
	if m, _ := c.List("alex"); len(m) != 0 {
		t.Errorf("list after delete %v", m)
	}
}

func TestCookieSiteValidated(t *testing.T) {
	c := &CookieStore{Dir: t.TempDir()}
	for _, site := range []string{"../../etc", "other", ""} {
		if err := c.Put("alex", site, []byte(cookieSample)); err == nil {
			t.Errorf("Put %q accepted", site)
		}
		if _, ok := c.Path("alex", site); ok {
			t.Errorf("Path %q ok", site)
		}
		if err := c.Delete("alex", site); err == nil {
			t.Errorf("Delete %q accepted", site)
		}
	}
	if err := c.Put("../evil", "youtube", []byte(cookieSample)); err == nil {
		t.Error("traversing user dir accepted")
	}
}

func TestCookieAPI(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	_, bob := e.user(t, "bob", false)
	if w := e.req(t, tok, "PUT", "/api/cookies/youtube", cookieSample); w.Code != 204 {
		t.Fatalf("put %d %s", w.Code, w.Body)
	}
	w := e.req(t, tok, "GET", "/api/cookies", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"youtube":{"updated":"`) || strings.Contains(w.Body.String(), "abc123") {
		t.Fatalf("list %d %s", w.Code, w.Body)
	}
	if w := e.req(t, bob, "GET", "/api/cookies", ""); strings.TrimSpace(w.Body.String()) != "{}" {
		t.Errorf("bob sees %s", w.Body)
	}
	if w := e.req(t, tok, "PUT", "/api/cookies/other", cookieSample); w.Code != 404 {
		t.Errorf("other: %d", w.Code)
	}
	if w := e.req(t, tok, "DELETE", "/api/cookies/nope", ""); w.Code != 404 {
		t.Errorf("unknown: %d", w.Code)
	}
	if w := e.req(t, tok, "PUT", "/api/cookies/reddit", strings.Repeat("x", 1_000_001)); w.Code != 400 {
		t.Errorf("oversize: %d", w.Code)
	}
	if w := e.req(t, tok, "DELETE", "/api/cookies/youtube", ""); w.Code != 204 {
		t.Errorf("delete: %d", w.Code)
	}
	if _, ok := e.cookies.Path("alex", "youtube"); ok {
		t.Error("not deleted")
	}
}

func TestCookieUploadErrorDoesNotEchoContent(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	w := e.req(t, tok, "PUT", "/api/cookies/youtube", "SECRETVALUE")
	if w.Code != 400 || strings.Contains(w.Body.String(), "SECRETVALUE") ||
		!strings.Contains(w.Body.String(), "doesn't look like a cookies.txt file") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestTrackerPassesCookieFile(t *testing.T) {
	e := newGrabEnv(t)
	_, alex := e.user(t, "alex", false)
	_, bob := e.user(t, "bob", false)
	if err := e.cookies.Put("alex", "instagram", []byte(cookieSample)); err != nil {
		t.Fatal(err)
	}
	e.submit(t, alex, `{"url":"https://www.instagram.com/reel/abc/"}`)
	e.submit(t, bob, `{"url":"https://www.instagram.com/reel/def/"}`)
	e.tick(t)
	got := map[string]string{}
	for _, a := range e.mt.adds {
		got[a.URL] = a.CookieFile
	}
	if got["https://www.instagram.com/reel/abc/"] != filepath.Join(e.cookies.Dir, "alex", "instagram.txt") ||
		got["https://www.instagram.com/reel/def/"] != "" || len(got) != 2 {
		t.Fatalf("adds %+v", e.mt.adds)
	}
}

func TestNeedsCookies(t *testing.T) {
	for msg, want := range map[string]bool{
		"ERROR: [youtube] x: Sign in to confirm you're not a bot": true,
		"Login required to view this":                             true,
		"This video is Private Video":                             true,
		"Use --cookies for the authentication":                    true,
		"Instagram: Empty media response (rate-limit)":            true,
		"HTTP Error 404":                                          false,
		"":                                                        false,
	} {
		if NeedsCookies(msg) != want {
			t.Errorf("%q: want %v", msg, want)
		}
	}
}
