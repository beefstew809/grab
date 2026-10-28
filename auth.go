package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "grab_session"
	loginCookie   = "grab_login"
	sessionTTL    = 30 * 24 * time.Hour
	loginTTL      = 10 * time.Minute
	// maxReturnLen keeps the signed login cookie under the browsers' 4 KB
	// cookie limit; a longer return path would silently break the login.
	maxReturnLen = 2048
)

type userKey struct{}

// UserFrom returns the user Auth.Wrap put on the request context.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok
}

// Auth decides who is calling the API.
type Auth interface {
	// Wrap guards /api/*: 401 JSON {"error":"login required"} when there is
	// no user, otherwise the user is available through UserFrom.
	Wrap(next http.Handler) http.Handler
	// Register mounts /auth/login, /auth/callback and /auth/logout.
	Register(mux *http.ServeMux)
}

// guard is Wrap for both modes. A grab API token always wins; without one the
// mode's ambient identity (session cookie, or the single local user) applies.
type guard struct {
	st *Store
	// origin is BaseURL's normalised origin; "" when unknown (AUTH=none).
	origin  string
	ambient func(r *http.Request) (User, bool, error)
}

func (g guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok, ok := bearer(r); ok {
			// A token is not ambient (a browser never attaches it on its own),
			// so cross-site requests can't ride on it: no origin check.
			u, err := g.st.TokenUser(tok)
			switch {
			case errors.Is(err, ErrNotFound):
				apiError(w, http.StatusUnauthorized, "invalid token")
			case err != nil:
				log.Printf("auth: token lookup: %v", err)
				apiError(w, http.StatusInternalServerError, "internal error")
			default:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
			}
			return
		}
		u, ok, err := g.ambient(r)
		if err != nil {
			log.Printf("auth: session lookup: %v", err)
			apiError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			apiError(w, http.StatusUnauthorized, "login required")
			return
		}
		if crossSiteWrite(r, g.origin) {
			apiError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, u)))
	})
}

// bearer extracts a grab API token ("Authorization: Bearer grab_…").
// Anything else is ignored, not rejected: a reverse proxy in front of grab
// may forward its own Basic credentials or bearer token, and refusing those
// would lock out (or, with AUTH=none, endlessly reload) every page.
func bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if len(h) < 7 || !strings.EqualFold(h[:7], "bearer ") {
		return "", false
	}
	tok := strings.TrimSpace(h[7:])
	return tok, strings.HasPrefix(tok, tokenPrefix)
}

// crossSiteWrite is the CSRF check for ambient credentials. SameSite=Lax
// already keeps the cookie off cross-site POSTs in current browsers; this
// covers older ones and same-site-but-cross-origin pages (a sibling
// subdomain). A request without Origin is let through: browsers send Origin
// on every cross-origin write, so its absence means a same-origin or
// non-browser client.
func crossSiteWrite(r *http.Request, origin string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return true
	}
	o := r.Header.Get("Origin")
	if o == "" || origin == "" {
		return false
	}
	return normOrigin(o) != origin
}

// normOrigin reduces a URL to scheme://host[:port] with default ports
// dropped, so "https://grab.example.com:443" matches a browser's Origin.
// Anything unparsable (including the literal "null") yields "".
func normOrigin(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if scheme == "https" {
		host = strings.TrimSuffix(host, ":443")
	} else if scheme == "http" {
		host = strings.TrimSuffix(host, ":80")
	}
	return scheme + "://" + host
}

// safeReturn keeps the post-login redirect on this site. "//host" and
// "/\host" are protocol-relative to browsers, and browsers strip tabs and
// newlines from URLs, so "/\t/host" would become "//host": control
// characters are refused too.
func safeReturn(ret string) string {
	if !strings.HasPrefix(ret, "/") || strings.HasPrefix(ret, "//") || strings.Contains(ret, `\`) ||
		len(ret) > maxReturnLen || strings.ContainsFunc(ret, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "/"
	}
	return ret
}

// localRedirect sets Location verbatim. http.Redirect would path.Clean it,
// which can rewrite the shared link the user is returning to.
func localRedirect(w http.ResponseWriter, path string) {
	w.Header().Set("Location", path)
	w.WriteHeader(http.StatusFound)
}

// page writes a minimal HTML page. msg must be a constant: it is not escaped
// and must never carry user data.
func page(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	fmt.Fprintf(w, "<!doctype html>\n<meta charset=\"utf-8\">\n<meta name=\"viewport\" content=\"width=device-width\">\n<title>grab</title>\n<p>%s</p>\n", msg)
}

// ---- AUTH=none ----

const localUserID = "local"

type noAuth struct{ guard }

// NewNoAuth makes every request the single local admin, users/local on disk.
func NewNoAuth(st *Store) (Auth, error) {
	u, err := st.UpsertUser(localUserID, localUserID, true)
	if err != nil {
		return nil, err
	}
	if u.Dir != localUserID {
		// Only possible after switching from AUTH=oidc with a user called "local".
		return nil, fmt.Errorf("auth: the folder users/%s already belongs to another user", localUserID)
	}
	return noAuth{guard{st: st, ambient: func(*http.Request) (User, bool, error) {
		// Re-read per request so settings changes (default target) show up.
		u, err := st.UserByID(localUserID)
		return u, err == nil, err
	}}}, nil
}

// Register: there is nothing to log in to, but the routes exist so a page
// that sends someone to /auth/login still lands where it meant to.
func (noAuth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", func(w http.ResponseWriter, r *http.Request) {
		localRedirect(w, safeReturn(r.URL.Query().Get("return")))
	})
	home := func(w http.ResponseWriter, r *http.Request) { localRedirect(w, "/") }
	mux.HandleFunc("GET /auth/callback", home)
	mux.HandleFunc("GET /auth/logout", home)
	mux.HandleFunc("POST /auth/logout", home)
}

// ---- AUTH=oidc ----

type oidcAuth struct {
	guard
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	provider *oidc.Provider
	hc       *http.Client
	key      []byte // HMAC key for the login-state cookie
	secure   bool
	allowed  string
	admin    string
}

// NewOIDCAuth discovers the provider (so it fails at startup, not at the
// first login, when the issuer is wrong) and loads or creates session.key.
func NewOIDCAuth(ctx context.Context, cfg Config, st *Store) (Auth, error) {
	key, err := loadSessionKey(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Timeout: 15 * time.Second}
	// The client in the context is kept by the provider for later JWKS fetches.
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, hc), cfg.OIDC.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery: %w", err)
	}
	a := &oidcAuth{
		oauth: oauth2.Config{
			ClientID:     cfg.OIDC.ClientID,
			ClientSecret: cfg.OIDC.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  strings.TrimRight(cfg.BaseURL.String(), "/") + "/auth/callback",
			Scopes:       cfg.OIDC.Scopes,
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.OIDC.ClientID}),
		provider: provider,
		hc:       hc,
		key:      key,
		secure:   cfg.BaseURL.Scheme == "https",
		allowed:  cfg.OIDC.AllowedGroup,
		admin:    cfg.OIDC.AdminGroup,
	}
	a.guard = guard{st: st, origin: normOrigin(cfg.BaseURL.String()), ambient: a.sessionUser}
	return a, nil
}

func (a *oidcAuth) sessionUser(r *http.Request) (User, bool, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return User{}, false, nil
	}
	u, err := a.st.SessionUser(c.Value)
	if errors.Is(err, ErrNotFound) {
		return User{}, false, nil
	}
	return u, err == nil, err
}

func (a *oidcAuth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("GET /auth/logout", a.logout)
	mux.HandleFunc("POST /auth/logout", a.logout)
}

func (a *oidcAuth) cookie(name, value string, ttl time.Duration) *http.Cookie {
	c := &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: a.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(ttl / time.Second)}
	if ttl <= 0 {
		c.MaxAge = -1
	}
	return c
}

// loginState rides in the grab_login cookie between /auth/login and
// /auth/callback, HMAC-signed so grab needs no server-side login table.
type loginState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	Return   string `json:"r"`
	Expires  int64  `json:"e"`
}

func (a *oidcAuth) mac(payload string) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write([]byte(loginCookie + "\x00" + payload))
	return m.Sum(nil)
}

func (a *oidcAuth) seal(ls loginState) string {
	b, _ := json.Marshal(ls)
	p := base64.RawURLEncoding.EncodeToString(b)
	return p + "." + base64.RawURLEncoding.EncodeToString(a.mac(p))
}

func (a *oidcAuth) unseal(v string) (loginState, bool) {
	var ls loginState
	p, sig, ok := strings.Cut(v, ".")
	if !ok {
		return ls, false
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, a.mac(p)) {
		return ls, false
	}
	b, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil || json.Unmarshal(b, &ls) != nil || time.Now().Unix() > ls.Expires {
		return ls, false
	}
	return ls, true
}

func (a *oidcAuth) login(w http.ResponseWriter, r *http.Request) {
	ls := loginState{
		State:    rand.Text(),
		Verifier: oauth2.GenerateVerifier(),
		Nonce:    rand.Text(),
		Return:   safeReturn(r.URL.Query().Get("return")),
		Expires:  time.Now().Add(loginTTL).Unix(),
	}
	http.SetCookie(w, a.cookie(loginCookie, a.seal(ls), loginTTL))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, a.oauth.AuthCodeURL(ls.State, oidc.Nonce(ls.Nonce), oauth2.S256ChallengeOption(ls.Verifier)), http.StatusFound)
}

const (
	msgDenied       = "Your account isn't allowed to use grab."
	msgLoginExpired = `Your login expired or was started in another tab. <a href="/">Try again</a>.`
	msgLoginFailed  = `Login failed. <a href="/">Try again</a>.`
)

func (a *oidcAuth) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := r.Cookie(loginCookie)
	// Single use: the state cookie goes whatever happens next.
	http.SetCookie(w, a.cookie(loginCookie, "", 0))
	if err != nil {
		page(w, http.StatusBadRequest, msgLoginExpired)
		return
	}
	ls, ok := a.unseal(c.Value)
	if !ok || subtle.ConstantTimeCompare([]byte(ls.State), []byte(q.Get("state"))) != 1 {
		page(w, http.StatusBadRequest, msgLoginExpired)
		return
	}
	if e := q.Get("error"); e != "" {
		log.Printf("oidc: provider returned error %q", e)
		page(w, http.StatusBadGateway, msgLoginFailed)
		return
	}
	ctx := oidc.ClientContext(r.Context(), a.hc)
	tok, err := a.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		// Log status and error code only: a RetrieveError carries the whole
		// response body, which is the provider's to fill.
		var re *oauth2.RetrieveError
		if errors.As(err, &re) {
			log.Printf("oidc: token exchange refused: HTTP %d %q", re.Response.StatusCode, re.ErrorCode)
		} else {
			log.Printf("oidc: token exchange failed: %v", err)
		}
		page(w, http.StatusBadGateway, msgLoginFailed)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		log.Print("oidc: no id_token in the token response (is openid in OIDC_SCOPES?)")
		page(w, http.StatusBadGateway, msgLoginFailed)
		return
	}
	idt, err := a.verifier.Verify(ctx, raw) // issuer, audience, expiry, signature
	if err != nil {
		log.Printf("oidc: id token rejected: %v", err)
		page(w, http.StatusBadGateway, msgLoginFailed)
		return
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(ls.Nonce)) != 1 || idt.Subject == "" {
		log.Print("oidc: id token rejected: nonce mismatch or no subject")
		page(w, http.StatusBadGateway, msgLoginFailed)
		return
	}
	var cl userClaims
	if err := idt.Claims(&cl); err != nil {
		log.Printf("oidc: id token claims: %v", err)
		page(w, http.StatusBadGateway, msgLoginFailed)
		return
	}
	if len(cl.Groups) == 0 || displayName(cl.Username, cl.Name, cl.Email) == "" {
		if !a.userInfo(ctx, tok, idt.Subject, &cl) {
			page(w, http.StatusBadGateway, msgLoginFailed)
			return
		}
	}
	groups := parseGroups(cl.Groups)
	if a.allowed != "*" && !slices.Contains(groups, a.allowed) {
		page(w, http.StatusForbidden, msgDenied)
		return
	}
	// Admin is decided here and stored: a group change applies at next login.
	u, err := a.st.UpsertUser(idt.Subject, displayName(cl.Username, cl.Name, cl.Email), slices.Contains(groups, a.admin))
	if err != nil {
		log.Printf("oidc: saving user: %v", err)
		page(w, http.StatusInternalServerError, msgLoginFailed)
		return
	}
	sid, err := a.st.CreateSession(u.ID, sessionTTL)
	if err != nil {
		log.Printf("oidc: creating session: %v", err)
		page(w, http.StatusInternalServerError, msgLoginFailed)
		return
	}
	http.SetCookie(w, a.cookie(sessionCookie, sid, sessionTTL))
	localRedirect(w, safeReturn(ls.Return))
}

// userClaims are the ID token or UserInfo claims grab reads.
type userClaims struct {
	Username string          `json:"preferred_username"`
	Name     string          `json:"name"`
	Email    string          `json:"email"`
	Groups   json.RawMessage `json:"groups"`
}

// userInfo fills the claims the ID token left out from the UserInfo
// endpoint: Authelia 4.39+ puts groups and preferred_username only there
// unless a claims policy adds them to the ID token. A provider without the
// endpoint keeps what the ID token had (no groups then fails closed). It
// reports false when the login must fail.
func (a *oidcAuth) userInfo(ctx context.Context, tok *oauth2.Token, sub string, cl *userClaims) bool {
	if a.provider.UserInfoEndpoint() == "" {
		return true
	}
	ui, err := a.provider.UserInfo(ctx, oauth2.StaticTokenSource(tok))
	if err != nil {
		// The error itself stays out of the log: on a non-200 it carries
		// the provider's response body.
		log.Print("oidc: userinfo request failed")
		return false
	}
	if ui.Subject != sub {
		log.Print("oidc: userinfo rejected: subject differs from the id token")
		return false
	}
	var more userClaims
	if err := ui.Claims(&more); err != nil {
		log.Printf("oidc: userinfo claims: %v", err)
		return false
	}
	if len(cl.Groups) == 0 {
		cl.Groups = more.Groups
	}
	if displayName(cl.Username, cl.Name, cl.Email) == "" {
		cl.Username, cl.Name, cl.Email = more.Username, more.Name, more.Email
	}
	return true
}

func (a *oidcAuth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := a.st.DeleteSession(c.Value); err != nil {
			log.Printf("auth: deleting session: %v", err)
		}
	}
	http.SetCookie(w, a.cookie(sessionCookie, "", 0))
	localRedirect(w, "/")
}

// parseGroups accepts only a JSON array of strings; an absent claim or any
// other shape means no groups, which fails closed on OIDC_ALLOWED_GROUP.
func parseGroups(raw json.RawMessage) []string {
	var g []string
	if len(raw) == 0 || json.Unmarshal(raw, &g) != nil {
		return nil
	}
	return g
}

// displayName also seeds the user's folder name at first login.
func displayName(username, name, email string) string {
	for _, s := range []string{username, name} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	local, _, _ := strings.Cut(strings.TrimSpace(email), "@")
	return local
}

// loadSessionKey returns DATA_DIR/session.key, creating 32 random bytes on
// first start. It must survive restarts, or every in-flight login breaks.
func loadSessionKey(dir string) ([]byte, error) {
	p := filepath.Join(dir, "session.key")
	key, err := os.ReadFile(p)
	if err == nil {
		if len(key) != 32 {
			return nil, fmt.Errorf("%s is not 32 bytes; delete it and restart to make a new one", p)
		}
		if st, err := os.Stat(p); err == nil && st.Mode().Perm() != 0o600 {
			if err := os.Chmod(p, 0o600); err != nil {
				return nil, err
			}
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	rand.Read(key)
	// Write-then-rename, so a crash never leaves a short key behind.
	f, err := os.CreateTemp(dir, ".session.key-*") // created 0600
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(key); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(f.Name(), p); err != nil {
		return nil, err
	}
	return key, nil
}
