package main

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// maxDeleteAfterDays caps DELETE_AFTER_DAYS well below time.Duration's limit.
const maxDeleteAfterDays = 36500

type OIDCConfig struct {
	Issuer, ClientID, ClientSecret, AllowedGroup, AdminGroup string
	Scopes                                                   []string
}

type JellyfinConfig struct{ URL, APIKey, LibraryID string }

// Enabled reports whether the Jellyfin integration is configured.
func (j JellyfinConfig) Enabled() bool { return j.URL != "" && j.APIKey != "" }

type Config struct {
	Auth                                                  string   // "none" | "oidc"
	BaseURL                                               *url.URL // no trailing slash
	ListenAddr, MeTubeURL, SharesDir, DataDir, CookiesDir string
	OIDC                                                  OIDCConfig
	AppriseURLs                                           []string
	Encoder                                               Encoder
	VAAPIDevice                                           string
	HealthcheckFile                                       string
	DeleteAfter                                           time.Duration // 0 = never
	Jellyfin                                              JellyfinConfig
}

// LoadConfig builds a Config from environment lookups. Errors name the
// offending variable but never echo any value, so secrets cannot leak.
func LoadConfig(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	var c Config

	c.Auth = get("AUTH", "")
	switch c.Auth {
	case "none", "oidc":
	case "":
		return c, fmt.Errorf("AUTH is required (none or oidc)")
	default:
		return c, fmt.Errorf("AUTH must be none or oidc")
	}

	raw := get("BASE_URL", "")
	if raw == "" {
		return c, fmt.Errorf("BASE_URL is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return c, fmt.Errorf("BASE_URL must be an http(s) URL")
	}
	// Links and the OIDC redirect are built by appending to BASE_URL, so a
	// query or fragment would end up in the middle of them; credentials in it
	// would be sent to every phone in notification links.
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return c, fmt.Errorf("BASE_URL must not contain credentials, a query or a fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	c.BaseURL = u

	c.ListenAddr = get("LISTEN_ADDR", ":8080")
	c.MeTubeURL = get("METUBE_URL", "http://metube:8081")
	c.SharesDir = get("SHARES_DIR", "/shares")
	c.DataDir = get("DATA_DIR", "/data")
	c.CookiesDir = get("COOKIES_DIR", "/cookies")
	// Relative paths would depend on the working directory, and MeTube must
	// see the same paths for SHARES_DIR and COOKIES_DIR.
	for _, d := range []struct{ name, val string }{
		{"SHARES_DIR", c.SharesDir}, {"DATA_DIR", c.DataDir}, {"COOKIES_DIR", c.CookiesDir},
	} {
		if !filepath.IsAbs(d.val) {
			return c, fmt.Errorf("%s must be an absolute path", d.name)
		}
	}
	c.VAAPIDevice = get("VAAPI_DEVICE", "/dev/dri/renderD128")
	c.HealthcheckFile = get("HEALTHCHECK_FILE", "")

	switch enc := Encoder(get("ENCODER", string(EncX264))); enc {
	case EncX264, EncVAAPI:
		c.Encoder = enc
	default:
		return c, fmt.Errorf("ENCODER must be libx264 or vaapi")
	}

	if d := get("DELETE_AFTER_DAYS", ""); d != "" {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return c, fmt.Errorf("DELETE_AFTER_DAYS must be a non-negative whole number")
		}
		// Much larger values overflow time.Duration (about 292 years).
		if n > maxDeleteAfterDays {
			return c, fmt.Errorf("DELETE_AFTER_DAYS must be at most %d (100 years)", maxDeleteAfterDays)
		}
		c.DeleteAfter = time.Duration(n) * 24 * time.Hour
	}

	apprise := strings.FieldsFunc(getenv("APPRISE_URLS"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if c.Auth == "oidc" {
		if len(apprise) > 0 {
			return c, fmt.Errorf("APPRISE_URLS is only valid with AUTH=none (notifications are per-user with oidc)")
		}
		c.OIDC = OIDCConfig{
			Issuer:       get("OIDC_ISSUER", ""),
			ClientID:     get("OIDC_CLIENT_ID", ""),
			ClientSecret: get("OIDC_CLIENT_SECRET", ""),
			AllowedGroup: get("OIDC_ALLOWED_GROUP", ""),
			AdminGroup:   get("OIDC_ADMIN_GROUP", ""),
			Scopes:       strings.Fields(get("OIDC_SCOPES", "openid profile email groups")),
		}
		for _, r := range []struct{ name, val string }{
			{"OIDC_ISSUER", c.OIDC.Issuer},
			{"OIDC_CLIENT_ID", c.OIDC.ClientID},
			{"OIDC_CLIENT_SECRET", c.OIDC.ClientSecret},
			{"OIDC_ALLOWED_GROUP", c.OIDC.AllowedGroup},
			{"OIDC_ADMIN_GROUP", c.OIDC.AdminGroup},
		} {
			if r.val == "" {
				return c, fmt.Errorf("%s is required when AUTH=oidc", r.name)
			}
		}
	} else {
		c.AppriseURLs = apprise
	}

	c.Jellyfin = JellyfinConfig{URL: get("JELLYFIN_URL", ""), APIKey: get("JELLYFIN_API_KEY", ""), LibraryID: get("JELLYFIN_LIBRARY_ID", "")}
	return c, nil
}
