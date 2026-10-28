package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func oidcEnv() map[string]string {
	return map[string]string{
		"AUTH": "oidc", "BASE_URL": "https://grab.example.com",
		"OIDC_ISSUER": "https://idp.example.com", "OIDC_CLIENT_ID": "id",
		"OIDC_CLIENT_SECRET": "s3cr3t", "OIDC_ALLOWED_GROUP": "*", "OIDC_ADMIN_GROUP": "admins",
	}
}

func noneEnv(extra map[string]string) map[string]string {
	m := map[string]string{"AUTH": "none", "BASE_URL": "https://grab.example.com/"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestLoadConfigNoneMinimal(t *testing.T) {
	c, err := LoadConfig(envMap(noneEnv(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL.String() != "https://grab.example.com" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if c.Encoder != EncX264 || c.ListenAddr != ":8080" || c.DeleteAfter != 0 {
		t.Errorf("defaults: %+v", c)
	}
	if c.SharesDir != "/shares" || c.DataDir != "/data" || c.CookiesDir != "/cookies" ||
		c.MeTubeURL != "http://metube:8081" || c.VAAPIDevice != "/dev/dri/renderD128" {
		t.Errorf("defaults: %+v", c)
	}
}

func TestLoadConfigRequiresAuth(t *testing.T) {
	for _, a := range []string{"", "basic"} {
		_, err := LoadConfig(envMap(map[string]string{"AUTH": a, "BASE_URL": "https://grab.example.com"}))
		if err == nil || !strings.Contains(err.Error(), "AUTH") {
			t.Errorf("AUTH=%q: err = %v", a, err)
		}
	}
}

func TestLoadConfigRequiresBaseURL(t *testing.T) {
	for _, b := range []string{"", "grab.example.com", "ftp://grab.example.com"} {
		_, err := LoadConfig(envMap(map[string]string{"AUTH": "none", "BASE_URL": b}))
		if err == nil || !strings.Contains(err.Error(), "BASE_URL") {
			t.Errorf("BASE_URL=%q: err = %v", b, err)
		}
	}
}

func TestLoadConfigOIDCRequired(t *testing.T) {
	if _, err := LoadConfig(envMap(oidcEnv())); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_CLIENT_SECRET", "OIDC_ALLOWED_GROUP", "OIDC_ADMIN_GROUP"} {
		m := oidcEnv()
		delete(m, k)
		_, err := LoadConfig(envMap(m))
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("missing %s: err = %v", k, err)
		}
	}
}

func TestLoadConfigOIDCScopesDefault(t *testing.T) {
	c, err := LoadConfig(envMap(oidcEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"openid", "profile", "email", "groups"}; !reflect.DeepEqual(c.OIDC.Scopes, want) {
		t.Errorf("scopes = %v", c.OIDC.Scopes)
	}
	m := oidcEnv()
	m["OIDC_SCOPES"] = "openid email"
	c, err = LoadConfig(envMap(m))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"openid", "email"}; !reflect.DeepEqual(c.OIDC.Scopes, want) {
		t.Errorf("scopes = %v", c.OIDC.Scopes)
	}
}

func TestLoadConfigAppriseURLs(t *testing.T) {
	c, err := LoadConfig(envMap(noneEnv(map[string]string{"APPRISE_URLS": "a://x, b://y\nc://z"})))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a://x", "b://y", "c://z"}; !reflect.DeepEqual(c.AppriseURLs, want) {
		t.Errorf("urls = %v", c.AppriseURLs)
	}
	m := oidcEnv()
	m["APPRISE_URLS"] = "a://x"
	if _, err := LoadConfig(envMap(m)); err == nil || !strings.Contains(err.Error(), "APPRISE_URLS") {
		t.Errorf("oidc + APPRISE_URLS: err = %v", err)
	}
}

func TestLoadConfigDeleteAfter(t *testing.T) {
	c, err := LoadConfig(envMap(noneEnv(map[string]string{"DELETE_AFTER_DAYS": "7"})))
	if err != nil || c.DeleteAfter != 7*24*time.Hour {
		t.Errorf("7: %v %v", c.DeleteAfter, err)
	}
	for _, v := range []string{"x", "-1"} {
		if _, err := LoadConfig(envMap(noneEnv(map[string]string{"DELETE_AFTER_DAYS": v}))); err == nil {
			t.Errorf("%q: want error", v)
		}
	}
}

func TestLoadConfigEncoder(t *testing.T) {
	c, err := LoadConfig(envMap(noneEnv(map[string]string{"ENCODER": "vaapi"})))
	if err != nil || c.Encoder != EncVAAPI {
		t.Errorf("vaapi: %v %v", c.Encoder, err)
	}
	if _, err := LoadConfig(envMap(noneEnv(map[string]string{"ENCODER": "qsv"}))); err == nil {
		t.Error("qsv: want error")
	}
}

func TestLoadConfigJellyfin(t *testing.T) {
	c, _ := LoadConfig(envMap(noneEnv(map[string]string{"JELLYFIN_URL": "http://jf:8096"})))
	if c.Jellyfin.Enabled() {
		t.Error("URL without key must be disabled")
	}
	c, _ = LoadConfig(envMap(noneEnv(map[string]string{"JELLYFIN_URL": "http://jf:8096", "JELLYFIN_API_KEY": "k"})))
	if !c.Jellyfin.Enabled() {
		t.Error("want enabled")
	}
}

func TestLoadConfigErrorDoesNotEchoSecret(t *testing.T) {
	m := oidcEnv()
	delete(m, "OIDC_ISSUER")
	_, err := LoadConfig(envMap(m))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("error leaks secret: %v", err)
	}
}

func TestLoadConfigDeleteAfterCapped(t *testing.T) {
	if _, err := LoadConfig(envMap(noneEnv(map[string]string{"DELETE_AFTER_DAYS": "36500"}))); err != nil {
		t.Errorf("36500: %v", err)
	}
	// Far larger values overflow time.Duration into a negative max age.
	for _, v := range []string{"36501", "200000000"} {
		if _, err := LoadConfig(envMap(noneEnv(map[string]string{"DELETE_AFTER_DAYS": v}))); err == nil ||
			!strings.Contains(err.Error(), "DELETE_AFTER_DAYS") {
			t.Errorf("%s: err = %v", v, err)
		}
	}
}

func TestLoadConfigDirsMustBeAbsolute(t *testing.T) {
	for _, k := range []string{"SHARES_DIR", "DATA_DIR", "COOKIES_DIR"} {
		for _, v := range []string{"shares", "./data", "../x"} {
			_, err := LoadConfig(envMap(noneEnv(map[string]string{k: v})))
			if err == nil || !strings.Contains(err.Error(), k) {
				t.Errorf("%s=%q: err = %v", k, v, err)
			}
		}
		if _, err := LoadConfig(envMap(noneEnv(map[string]string{k: "/srv/grab/x"}))); err != nil {
			t.Errorf("%s absolute: %v", k, err)
		}
	}
}

func TestLoadConfigBaseURLRejectsExtras(t *testing.T) {
	for _, b := range []string{
		"https://user:hunter2@grab.example.com",
		"https://grab.example.com/?next=1",
		"https://grab.example.com/?",
		"https://grab.example.com/#top",
		"https://grab.example.com/#",
	} {
		_, err := LoadConfig(envMap(map[string]string{"AUTH": "none", "BASE_URL": b}))
		if err == nil || !strings.Contains(err.Error(), "BASE_URL") || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("BASE_URL=%q: err = %v", b, err)
		}
	}
	if _, err := LoadConfig(envMap(map[string]string{"AUTH": "none", "BASE_URL": "https://grab.example.com:8443/grab/"})); err != nil {
		t.Errorf("port and path: %v", err)
	}
}
