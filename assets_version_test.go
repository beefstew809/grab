package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// The page is never cached, but proxies in front of grab (Cloudflare's
// browser-cache TTL) may keep app.js/app.css for an hour. Versioned links in
// the page make every release fetch fresh assets.
func TestIndexLinksVersionedAssets(t *testing.T) {
	static := fstest.MapFS{
		"index.html": {Data: []byte(`<link rel="stylesheet" href="/app.css"><script src="/app.js"></script>`)},
		"app.js":     {Data: []byte("// js")},
	}
	a := &App{Static: static, Version: "v2.0.4"}
	h := a.Routes()
	for _, path := range []string{"/", "/?url=https%3A%2F%2Fyoutu.be%2Fx", "/?settings", "/index.html"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, `src="/app.js?v=v2.0.4"`) || !strings.Contains(body, `href="/app.css?v=v2.0.4"`) {
			t.Fatalf("%s: got %d %q", path, w.Code, body)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache", path, cc)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("%s: Content-Type %q", path, ct)
		}
	}
	// The versioned asset URL itself still serves the file.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/app.js?v=v2.0.4", nil))
	if w.Code != 200 || w.Body.String() != "// js" {
		t.Fatalf("app.js?v=: got %d %q", w.Code, w.Body.String())
	}
}
