package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

type failSender struct{}

func (failSender) Send(context.Context, []string, string, string) error {
	return errors.New("apprise exited 1")
}

type settingsT struct {
	*grabEnv
	notifier *Notifier
}

// settingsEnv is the grab env with a Notifier in the given mode and a
// recording Sender; users act through bearer tokens.
func settingsEnv(t *testing.T, mode string) (*settingsT, *fakeSender) {
	t.Helper()
	e := &settingsT{grabEnv: newGrabEnv(t)}
	n, fs, _ := testNotifier(t, mode)
	n.Store = e.st
	if mode == "none" {
		n.InstanceURLs = []string{"json://example.com/instance"}
	}
	auth, err := NewNoAuth(e.st)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Cookies: e.cookies, Static: staticFS(), Store: e.st, SharesDir: e.shares, Conv: e.conv, Auth: auth, Notifier: n}
	e.h = a.Routes()
	e.notifier = n
	return e, fs
}

func jsonBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not json: %s", w.Body)
	}
	return m
}

func TestMeDefaultTarget(t *testing.T) {
	e, _ := settingsEnv(t, "oidc")
	_, tok := e.user(t, "alex", true)
	m := jsonBody(t, e.req(t, tok, "GET", "/api/me", ""))
	if m["name"] != "alex" || m["admin"] != true || m["auth"] != "oidc" || m["defaultTarget"] != "original" {
		t.Fatalf("me: %v", m)
	}
	targets := m["targets"].([]any)
	if len(targets) != len(Presets) || targets[0].(map[string]any)["label"] != "Download only" {
		t.Errorf("targets: %v", targets)
	}
	if w := e.req(t, tok, "PUT", "/api/me", `{"defaultTarget":"signal"}`); w.Code != 204 {
		t.Fatalf("put %d %s", w.Code, w.Body)
	}
	if m := jsonBody(t, e.req(t, tok, "GET", "/api/me", "")); m["defaultTarget"] != "signal" {
		t.Errorf("round trip: %v", m["defaultTarget"])
	}
	if w := e.req(t, tok, "PUT", "/api/me", `{"defaultTarget":"fax"}`); w.Code != 400 {
		t.Errorf("unknown target: %d", w.Code)
	}
}

func TestNotifyURLsWriteOnly(t *testing.T) {
	e, _ := settingsEnv(t, "oidc")
	_, tok := e.user(t, "alex", false)
	if w := e.req(t, tok, "PUT", "/api/me/notify", `{"urls":["tgram://SECRETTOKEN/1"]}`); w.Code != 204 {
		t.Fatalf("put %d %s", w.Code, w.Body)
	}
	w := e.req(t, tok, "GET", "/api/me", "")
	n := jsonBody(t, w)["notify"].(map[string]any)
	if n["set"] != true || n["count"] != float64(1) || n["instanceWide"] != false || strings.Contains(w.Body.String(), "SECRETTOKEN") {
		t.Fatalf("notify: %s", w.Body)
	}
	if w := e.req(t, tok, "PUT", "/api/me/notify", `{"urls":["not a url"]}`); w.Code != 400 {
		t.Errorf("bad url: %d", w.Code)
	}
	if w := e.req(t, tok, "DELETE", "/api/me/notify", ""); w.Code != 204 {
		t.Errorf("delete: %d", w.Code)
	}
	if n := jsonBody(t, e.req(t, tok, "GET", "/api/me", ""))["notify"].(map[string]any); n["set"] != false || n["count"] != float64(0) {
		t.Errorf("after delete: %v", n)
	}
}

func TestNotifyNoneMode(t *testing.T) {
	e, fs := settingsEnv(t, "none")
	_, tok := e.user(t, "alex", true)
	n := jsonBody(t, e.req(t, tok, "GET", "/api/me", ""))["notify"].(map[string]any)
	if n["set"] != true || n["count"] != float64(1) || n["instanceWide"] != true {
		t.Errorf("none notify: %v", n)
	}
	w := e.req(t, tok, "PUT", "/api/me/notify", `{"urls":["tgram://x/1"]}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "set APPRISE_URLS instead") {
		t.Errorf("put in none mode: %d %s", w.Code, w.Body)
	}
	if w := e.req(t, tok, "POST", "/api/me/notify/test", ""); w.Code != 204 || len(fs.got) != 1 || fs.got[0].urls[0] != "json://example.com/instance" {
		t.Errorf("test: %d %v", w.Code, fs.got)
	}
}

func TestTokenCreateOnce(t *testing.T) {
	e, _ := settingsEnv(t, "oidc")
	_, tok := e.user(t, "alex", false)
	w := e.req(t, tok, "POST", "/api/tokens", `{"label":"  phone  "}`)
	m := jsonBody(t, w)
	plain, _ := m["token"].(string)
	if w.Code != 201 || plain == "" || m["label"] != "phone" || m["id"] == "" {
		t.Fatalf("create %d %s", w.Code, w.Body)
	}
	if w := e.req(t, plain, "GET", "/api/me", ""); w.Code != 200 {
		t.Fatalf("new token: %d", w.Code)
	}
	w = e.req(t, tok, "GET", "/api/tokens", "")
	if strings.Contains(w.Body.String(), `"token"`) || strings.Contains(w.Body.String(), plain) {
		t.Fatalf("list leaks token: %s", w.Body)
	}
	var list []map[string]any
	json.Unmarshal(w.Body.Bytes(), &list)
	if len(list) != 2 {
		t.Fatalf("list: %s", w.Body)
	}
	if w := e.req(t, tok, "DELETE", "/api/tokens/"+m["id"].(string), ""); w.Code != 204 {
		t.Fatalf("delete %d", w.Code)
	}
	if w := e.req(t, plain, "GET", "/api/me", ""); w.Code != 401 {
		t.Errorf("deleted token still works: %d", w.Code)
	}
	for _, body := range []string{`{"label":""}`, `{"label":"   "}`, `{"label":"` + strings.Repeat("x", 65) + `"}`, `nope`} {
		if w := e.req(t, tok, "POST", "/api/tokens", body); w.Code != 400 {
			t.Errorf("label %q: %d", body, w.Code)
		}
	}
	if w := e.req(t, tok, "POST", "/api/tokens", `{"label":"`+strings.Repeat("x", 64)+`"}`); w.Code != 201 {
		t.Errorf("64 chars: %d", w.Code)
	}
}

func TestTokenDeleteIsPerUser(t *testing.T) {
	e, _ := settingsEnv(t, "oidc")
	_, alex := e.user(t, "alex", false)
	_, bob := e.user(t, "bob", false)
	m := jsonBody(t, e.req(t, alex, "POST", "/api/tokens", `{"label":"x"}`))
	e.req(t, bob, "DELETE", "/api/tokens/"+m["id"].(string), "")
	if w := e.req(t, m["token"].(string), "GET", "/api/me", ""); w.Code != 200 {
		t.Errorf("bob deleted alex's token: %d", w.Code)
	}
}

func TestSettingsNeverReturnSecrets(t *testing.T) {
	e, _ := settingsEnv(t, "oidc")
	_, tok := e.user(t, "alex", false)
	e.req(t, tok, "PUT", "/api/me/notify", `{"urls":["tgram://SECRETURL/1"]}`)
	m := jsonBody(t, e.req(t, tok, "POST", "/api/tokens", `{"label":"x"}`))
	plain := m["token"].(string)
	if w := e.req(t, tok, "PUT", "/api/cookies/youtube", cookieSample); w.Code != 204 {
		t.Fatalf("cookies %d %s", w.Code, w.Body)
	}
	for _, p := range []string{"/api/me", "/api/tokens", "/api/cookies", "/api/items"} {
		w := e.req(t, tok, "GET", p, "")
		for _, secret := range []string{"SECRETURL", plain, "abc123"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Errorf("GET %s returned %q", p, secret)
			}
		}
	}
}

func TestNotifyTestSends(t *testing.T) {
	e, fs := settingsEnv(t, "oidc")
	_, tok := e.user(t, "alex", false)
	if w := e.req(t, tok, "POST", "/api/me/notify/test", ""); w.Code != 409 || !strings.Contains(w.Body.String(), "no notification URLs saved") {
		t.Errorf("no urls: %d %s", w.Code, w.Body)
	}
	e.req(t, tok, "PUT", "/api/me/notify", `{"urls":["tgram://tok/1","json://example.com/h"]}`)
	if w := e.req(t, tok, "POST", "/api/me/notify/test", ""); w.Code != 204 {
		t.Fatalf("test %d %s", w.Code, w.Body)
	}
	if len(fs.got) != 1 || len(fs.got[0].urls) != 2 || fs.got[0].urls[0] != "tgram://tok/1" || fs.got[0].title != "Test from grab" {
		t.Errorf("sent: %+v", fs.got)
	}
}

func TestNotifyTestFailure(t *testing.T) {
	e, _ := settingsEnv(t, "oidc")
	_, tok := e.user(t, "alex", false)
	e.notifier.Sender = failSender{}
	e.req(t, tok, "PUT", "/api/me/notify", `{"urls":["tgram://SECRETURL/1"]}`)
	w := e.req(t, tok, "POST", "/api/me/notify/test", "")
	if w.Code != 502 || strings.TrimSpace(w.Body.String()) != `{"error":"notification failed"}` {
		t.Errorf("%d %s", w.Code, w.Body)
	}
}
