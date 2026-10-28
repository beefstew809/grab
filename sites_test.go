package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestSharedURLPicksURLThenText(t *testing.T) {
	got, err := SharedURL("", "Look at this https://vm.tiktok.com/ZM1/ wow")
	if err != nil || got != "https://vm.tiktok.com/ZM1/" {
		t.Fatalf("text: got %q, %v", got, err)
	}
	got, err = SharedURL("https://youtu.be/x", "junk")
	if err != nil || got != "https://youtu.be/x" {
		t.Fatalf("url: got %q, %v", got, err)
	}
	if _, err = SharedURL("", "no link"); err == nil {
		t.Fatal("want error when no link present")
	}
}

func TestSiteFor(t *testing.T) {
	for in, want := range map[string]string{
		"https://m.youtube.com/watch?v=1":           "youtube",
		"https://old.reddit.com/r/x":                "reddit",
		"https://user@www.instagram.com:443/reel/1": "instagram",
		"https://notyoutube.com/x":                  "other",
		"https://fb.watch/abc":                      "facebook",
		"https://x.com/a/status/1":                  "twitter",
	} {
		if got := SiteFor(in); got != want {
			t.Errorf("SiteFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// failTransport fails the test if any request is attempted.
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("unexpected HTTP call to %s", r.URL)
	return nil, http.ErrNotSupported
}

func TestNormalizeFacebookIDInLink(t *testing.T) {
	hc := &http.Client{Transport: failTransport{t}}
	got := NormalizeFacebook(context.Background(), hc, "https://www.facebook.com/reel/1234567890")
	if want := "https://www.facebook.com/watch/?v=1234567890"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Non-facebook links come back untouched, with no HTTP call.
	other := "https://example.com/reel/1234567890"
	if got := NormalizeFacebook(context.Background(), hc, other); got != other {
		t.Fatalf("non-facebook changed: %q", got)
	}
}

func TestNormalizeFacebookShareRedirect(t *testing.T) {
	var landed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/share/r/abc" {
			http.Redirect(w, r, "/reel/9876543210/?s=1", http.StatusFound)
			return
		}
		landed = true
	}))
	defer srv.Close()
	always := func(*url.URL) bool { return true }
	got := normalizeFacebook(context.Background(), srv.Client(), srv.URL+"/share/r/abc", always)
	if want := "https://www.facebook.com/watch/?v=9876543210"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if landed {
		t.Fatal("redirect was followed")
	}
}

func TestNormalizeFacebookFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	in := srv.URL + "/share/r/abc"
	got := normalizeFacebook(context.Background(), srv.Client(), in, func(*url.URL) bool { return true })
	if got != in {
		t.Fatalf("got %q, want unchanged %q", got, in)
	}
}

func TestFacebookShortLinkRule(t *testing.T) {
	for in, want := range map[string]bool{
		"https://fb.watch/abc":                  true,
		"https://www.facebook.com/share/r/abc":  true,
		"https://www.facebook.com/share/v/abc":  true,
		"https://www.facebook.com/share/p/abc":  false,
		"https://www.facebook.com/watch/?v=123": false,
	} {
		u, _ := url.Parse(in)
		if got := isFacebookShortLink(u); got != want {
			t.Errorf("isFacebookShortLink(%q) = %v, want %v", in, got, want)
		}
	}
}
