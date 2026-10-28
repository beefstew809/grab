package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// fakeIdP is a minimal OIDC provider: discovery, JWKS, an /authorize that
// logs everyone in at once, and a /token that checks PKCE and returns an
// RS256 ID token carrying the nonce it saw at /authorize.
type fakeIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey

	mu        sync.Mutex
	sub       string
	username  string
	groups    any // usually []string; anything else tests the "no groups" rule
	nonce     string
	challenge string
	badNonce  bool // sign the ID token with a nonce grab never issued
	// info is what /userinfo returns besides sub (Authelia 4.39+ puts
	// groups and preferred_username only there); infoSub overrides sub.
	info    map[string]any
	infoSub string
	infoHit int
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key, sub: "sub-alice", username: "alice", groups: []string{"grab-users"}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		u := idp.srv.URL
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                u,
			"authorization_endpoint":                u + "/authorize",
			"token_endpoint":                        u + "/token",
			"jwks_uri":                              u + "/jwks",
			"userinfo_endpoint":                     u + "/userinfo",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "PKCE required", http.StatusBadRequest)
			return
		}
		idp.mu.Lock()
		idp.nonce, idp.challenge = q.Get("nonce"), q.Get("code_challenge")
		idp.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code=c&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		idp.mu.Lock()
		defer idp.mu.Unlock()
		idp.infoHit++
		if r.Header.Get("Authorization") != "Bearer at" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		out := map[string]any{"sub": cmp.Or(idp.infoSub, idp.sub)}
		for k, v := range idp.info {
			out[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		idp.mu.Lock()
		defer idp.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("code") != "c" || base64.RawURLEncoding.EncodeToString(sum[:]) != idp.challenge {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		claims := map[string]any{
			"iss": idp.srv.URL, "aud": "grab", "sub": idp.sub,
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
			"nonce": idp.nonce, "preferred_username": idp.username,
		}
		if idp.badNonce {
			claims["nonce"] = "replayed"
		}
		if idp.groups != nil {
			claims["groups"] = idp.groups
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600,
			"id_token": idp.sign(t, claims),
		})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func (idp *fakeIdP) sign(t *testing.T, claims map[string]any) string {
	sig, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: idp.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := sig.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (idp *fakeIdP) set(sub, username string, groups any) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	idp.sub, idp.username, idp.groups = sub, username, groups
}

type authEnv struct {
	idp    *fakeIdP
	st     *Store
	auth   Auth
	srv    *httptest.Server
	client *http.Client // cookie jar, does NOT follow redirects
	cfg    Config

	lastBody string
}

func newAuthEnv(t *testing.T, allowed string) *authEnv {
	t.Helper()
	idp := newFakeIdP(t)
	st, _ := testStore(t)
	e := &authEnv{idp: idp, st: st}
	var h http.Handler
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(e.srv.Close)
	base, _ := url.Parse(e.srv.URL)
	e.cfg = Config{Auth: "oidc", BaseURL: base, DataDir: t.TempDir(), OIDC: OIDCConfig{
		Issuer: idp.srv.URL, ClientID: "grab", ClientSecret: "s3cret",
		AllowedGroup: allowed, AdminGroup: "grab-admins", Scopes: []string{"openid", "profile", "groups"},
	}}
	auth, err := NewOIDCAuth(context.Background(), e.cfg, e.st)
	if err != nil {
		t.Fatal(err)
	}
	e.auth = auth
	app := &App{Static: publicFS(), Store: e.st, SharesDir: t.TempDir(), Auth: auth}
	h = app.Routes()
	jar, _ := cookiejar.New(nil)
	e.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e
}

// publicFS is the install shell: what Chrome fetches, without cookies, to
// install the PWA.
func publicFS() fstest.MapFS {
	fs := staticFS()
	for _, n := range []string{"manifest.json", "icon-192.png", "app.js"} {
		fs[n] = &fstest.MapFile{Data: []byte(n)}
	}
	return fs
}

// get fetches without following redirects; the body lands in e.lastBody.
func (e *authEnv) get(t *testing.T, rawURL string) *http.Response {
	t.Helper()
	resp, err := e.client.Get(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	e.lastBody = string(b)
	return resp
}

// login drives /auth/login → IdP → /auth/callback and returns the callback's
// response (not followed).
func (e *authEnv) login(t *testing.T, ret string) *http.Response {
	t.Helper()
	resp := e.get(t, e.srv.URL+"/auth/login?return="+url.QueryEscape(ret))
	for i := 0; i < 2; i++ {
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("step %d: got %d, want 302", i, resp.StatusCode)
		}
		loc, err := resp.Location()
		if err != nil {
			t.Fatal(err)
		}
		resp = e.get(t, loc.String())
	}
	return resp
}

func (e *authEnv) sessionCookie() *http.Cookie {
	u, _ := url.Parse(e.srv.URL)
	for _, c := range e.client.Jar.Cookies(u) {
		if c.Name == "grab_session" {
			return c
		}
	}
	return nil
}

func TestPublicShellNeedsNoLogin(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	for _, p := range []string{"/manifest.json", "/sw.js", "/icon-192.png", "/", "/healthz"} {
		if resp := e.get(t, e.srv.URL+p); resp.StatusCode != 200 {
			t.Errorf("%s: got %d", p, resp.StatusCode)
		}
	}
}

func TestAPIRequiresLogin(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	resp, err := e.client.Get(e.srv.URL + "/api/items")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]string
	json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != 401 || body["error"] != "login required" || len(body) != 1 {
		t.Fatalf("got %d %v", resp.StatusCode, body)
	}
}

func TestLoginFlowCreatesSession(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	resp := e.login(t, "/")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("callback: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var sess *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "grab_session" {
			sess = c
		}
	}
	if sess == nil || !sess.HttpOnly || sess.Secure || sess.SameSite != http.SameSiteLaxMode ||
		sess.Path != "/" || sess.MaxAge != 30*24*3600 {
		t.Fatalf("session cookie: %+v", sess)
	}
	if r := e.get(t, e.srv.URL+"/api/items"); r.StatusCode != 200 {
		t.Fatalf("/api/items after login: %d", r.StatusCode)
	}
	u, err := e.st.UserByID("sub-alice")
	if err != nil || u.Dir != "alice" || u.Name != "alice" || u.Admin {
		t.Fatalf("user %+v %v", u, err)
	}
	// The login state is single-use: replaying the callback must not log in again.
	if r := e.get(t, e.srv.URL+"/auth/callback?code=c&state=x"); r.StatusCode == http.StatusFound {
		t.Fatalf("replayed callback redirected: %d", r.StatusCode)
	}
}

func TestLoginCookieSecureOnHTTPS(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	e.cfg.BaseURL, _ = url.Parse("https://grab.example.com")
	a, err := NewOIDCAuth(context.Background(), e.cfg, e.st)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/auth/login?return=/", nil))
	cs := w.Result().Cookies()
	if len(cs) != 1 || cs[0].Name != "grab_login" || !cs[0].Secure || !cs[0].HttpOnly || cs[0].MaxAge != 600 {
		t.Fatalf("login cookie: %+v", cs)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if got := loc.Query().Get("redirect_uri"); got != "https://grab.example.com/auth/callback" {
		t.Fatalf("redirect_uri %q", got)
	}
}

func TestSessionKeyPersisted(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	p := filepath.Join(e.cfg.DataDir, "session.key")
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 || st.Size() != 32 {
		t.Fatalf("session.key: %v %v", st, err)
	}
	before, _ := os.ReadFile(p)
	if _, err := NewOIDCAuth(context.Background(), e.cfg, e.st); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Fatal("session.key regenerated on restart")
	}
}

func TestLoginReturnKeepsSharedLink(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	ret := "/?url=https%3A%2F%2Fyoutu.be%2Fabc"
	resp := e.login(t, ret)
	if got := resp.Header.Get("Location"); resp.StatusCode != http.StatusFound || got != ret {
		t.Fatalf("got %d %q, want %q", resp.StatusCode, got, ret)
	}
}

func TestLoginReturnRejectsOffsite(t *testing.T) {
	for _, ret := range []string{"//evil.example/x", "https://evil.example", `/\evil`, "/\t/evil.example", "evil", ""} {
		e := newAuthEnv(t, "grab-users")
		resp := e.login(t, ret)
		if got := resp.Header.Get("Location"); got != "/" {
			t.Errorf("return %q: Location %q", ret, got)
		}
	}
}

func TestCallbackRejectsWrongNonce(t *testing.T) {
	e := newAuthEnv(t, "*")
	e.idp.badNonce = true
	if resp := e.login(t, "/"); resp.StatusCode == http.StatusFound || e.sessionCookie() != nil {
		t.Fatalf("got %d, session %v", resp.StatusCode, e.sessionCookie())
	}
}

// A valid login cookie with someone else's state (login CSRF) is refused.
func TestCallbackRejectsWrongState(t *testing.T) {
	e := newAuthEnv(t, "*")
	resp := e.get(t, e.srv.URL+"/auth/login?return=/")
	resp = e.get(t, resp.Header.Get("Location"))
	cbURL, _ := url.Parse(resp.Header.Get("Location"))
	q := cbURL.Query()
	q.Set("state", "attacker-state")
	cbURL.RawQuery = q.Encode()
	if cb := e.get(t, cbURL.String()); cb.StatusCode != 400 || e.sessionCookie() != nil {
		t.Fatalf("got %d", cb.StatusCode)
	}
}

// The 10-minute limit is in the signed value too, not just the cookie's Max-Age.
func TestCallbackRejectsExpiredLoginState(t *testing.T) {
	e := newAuthEnv(t, "*")
	a := e.auth.(*oidcAuth)
	v := a.seal(loginState{State: "st", Verifier: "v", Nonce: "n", Return: "/", Expires: time.Now().Add(-time.Second).Unix()})
	u, _ := url.Parse(e.srv.URL)
	e.client.Jar.SetCookies(u, []*http.Cookie{{Name: "grab_login", Value: v, Path: "/"}})
	if cb := e.get(t, e.srv.URL+"/auth/callback?code=c&state=st"); cb.StatusCode != 400 {
		t.Fatalf("got %d", cb.StatusCode)
	}
}

// The login cookie is signed: a client can't swap in its own return path.
func TestCallbackRejectsTamperedLoginCookie(t *testing.T) {
	e := newAuthEnv(t, "*")
	resp := e.get(t, e.srv.URL+"/auth/login?return=/")
	resp = e.get(t, resp.Header.Get("Location")) // IdP → callback URL
	u, _ := url.Parse(e.srv.URL)
	var c *http.Cookie
	for _, k := range e.client.Jar.Cookies(u) {
		if k.Name == "grab_login" {
			c = k
		}
	}
	payload, sig, _ := strings.Cut(c.Value, ".")
	b, _ := base64.RawURLEncoding.DecodeString(payload)
	b = []byte(strings.Replace(string(b), `"r":"/"`, `"r":"https://evil.example"`, 1))
	c.Value = base64.RawURLEncoding.EncodeToString(b) + "." + sig
	e.client.Jar.SetCookies(u, []*http.Cookie{c})
	cb := e.get(t, resp.Header.Get("Location"))
	if cb.StatusCode != 400 || e.sessionCookie() != nil {
		t.Fatalf("got %d %q", cb.StatusCode, cb.Header.Get("Location"))
	}
}

func TestLoginDeniedOutsideAllowedGroup(t *testing.T) {
	for _, groups := range []any{[]string{}, nil, "grab-users", []any{"grab-users", 7}} {
		e := newAuthEnv(t, "grab-users")
		e.idp.set("sub-bob", "bob", groups)
		resp := e.login(t, "/")
		if resp.StatusCode != 403 || e.sessionCookie() != nil {
			t.Errorf("groups %#v: got %d, session %v", groups, resp.StatusCode, e.sessionCookie())
		}
		if _, err := e.st.UserByID("sub-bob"); err == nil {
			t.Errorf("groups %#v: denied user was stored", groups)
		}
	}

	e := newAuthEnv(t, "*")
	e.idp.set("sub-bob", "bob", []string{})
	if resp := e.login(t, "/"); resp.StatusCode != http.StatusFound || e.sessionCookie() == nil {
		t.Fatalf("AllowedGroup=*: got %d", resp.StatusCode)
	}
}

// Authelia 4.39+ leaves groups and preferred_username out of the ID token
// unless a claims policy adds them; grab then asks the UserInfo endpoint.
func TestLoginReadsUserInfoWhenIDTokenLacksClaims(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	e.idp.set("sub-erin", "", nil)
	e.idp.info = map[string]any{"preferred_username": "erin", "email": "erin@example.com",
		"groups": []string{"grab-users", "grab-admins"}}
	if resp := e.login(t, "/"); resp.StatusCode != http.StatusFound || e.sessionCookie() == nil {
		t.Fatalf("got %d %q", resp.StatusCode, e.lastBody)
	}
	u, err := e.st.UserByID("sub-erin")
	if err != nil || u.Name != "erin" || u.Dir != "erin" || !u.Admin {
		t.Fatalf("%+v %v", u, err)
	}
}

func TestLoginUserInfoRules(t *testing.T) {
	// Groups in the ID token: UserInfo is not asked.
	e := newAuthEnv(t, "grab-users")
	e.idp.info = map[string]any{"groups": []string{"grab-admins"}}
	if resp := e.login(t, "/"); resp.StatusCode != http.StatusFound || e.idp.infoHit != 0 {
		t.Fatalf("got %d, userinfo asked %d times", resp.StatusCode, e.idp.infoHit)
	}
	// Same shape rule as the ID token: not an array of strings = no groups.
	e = newAuthEnv(t, "grab-users")
	e.idp.set("sub-bob", "bob", nil)
	e.idp.info = map[string]any{"groups": []any{"grab-users", 7}}
	if resp := e.login(t, "/"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mixed groups: got %d", resp.StatusCode)
	}
	// UserInfo about someone else is refused.
	e = newAuthEnv(t, "grab-users")
	e.idp.set("sub-bob", "bob", nil)
	e.idp.info, e.idp.infoSub = map[string]any{"groups": []string{"grab-users"}}, "sub-mallory"
	if resp := e.login(t, "/"); resp.StatusCode != http.StatusBadGateway || e.sessionCookie() != nil {
		t.Fatalf("subject mismatch: got %d", resp.StatusCode)
	}
}

func TestLoginDeniedPage(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	e.idp.set("sub-bob", "bob", []string{"other"})
	cb := e.login(t, "/")
	body := e.lastBody
	if cb.StatusCode != 403 || !strings.Contains(body, "Your account isn't allowed to use grab.") ||
		strings.Contains(body, "bob") || !strings.HasPrefix(cb.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("%d %q %q", cb.StatusCode, cb.Header.Get("Content-Type"), body)
	}
}

func TestAdminFromGroup(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	e.idp.set("sub-carol", "carol", []string{"grab-users", "grab-admins"})
	e.login(t, "/")
	if u, err := e.st.UserByID("sub-carol"); err != nil || !u.Admin {
		t.Fatalf("%+v %v", u, err)
	}
}

func TestBearerToken(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	u, err := e.st.UpsertUser("sub-dave", "dave", false)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := e.st.CreateToken(u.ID, "shortcut")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		tok  string
		want int
	}{{tok, 200}, {"grab_wrong", 401}, {tok + "x", 401}} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/api/items", nil)
		req.Header.Set("Authorization", "Bearer "+c.tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("token %q: got %d, want %d", c.tok[:9], resp.StatusCode, c.want)
		}
	}
}

// Only "Bearer grab_…" is a grab token. A proxy in front of grab may forward
// its own bearer token; treating that as a bad grab token answered 401, which
// sent the AUTH=none page into an endless reload.
func TestForeignAuthorizationIgnored(t *testing.T) {
	type call struct {
		authz string
		want  int
	}
	run := func(t *testing.T, h http.Handler, cookie *http.Cookie, calls []call) {
		t.Helper()
		for _, c := range calls {
			req := httptest.NewRequest("GET", "/api/items", nil)
			if c.authz != "" {
				req.Header.Set("Authorization", c.authz)
			}
			if cookie != nil {
				req.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != c.want {
				t.Errorf("Authorization %q: got %d, want %d", c.authz, w.Code, c.want)
			}
		}
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})

	t.Run("none", func(t *testing.T) {
		st, _ := testStore(t)
		a, err := NewNoAuth(st)
		if err != nil {
			t.Fatal(err)
		}
		run(t, a.Wrap(ok), nil, []call{
			{"Bearer eyJhbGciOiJSUzI1NiJ9.proxy.token", 200},
			{"bearer proxy-token", 200},
			{"Basic dXNlcjpwYXNz", 200},
			{"Bearer grab_wrong", 401},
		})
	})
	t.Run("oidc", func(t *testing.T) {
		e := newAuthEnv(t, "grab-users")
		e.login(t, "/")
		sess := e.sessionCookie()
		if sess == nil {
			t.Fatal("no session")
		}
		h := e.auth.Wrap(ok)
		run(t, h, &http.Cookie{Name: sess.Name, Value: sess.Value}, []call{
			{"Bearer proxy-token", 200},
			{"Bearer grab_wrong", 401},
		})
		run(t, h, nil, []call{{"Bearer proxy-token", 401}})
	})
}

func TestCrossOriginWriteRejected(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	e.login(t, "/")
	sess := e.sessionCookie()
	if sess == nil {
		t.Fatal("no session")
	}
	u, _ := e.st.UserByID("sub-alice")
	tok, _, _ := e.st.CreateToken(u.ID, "t")

	var reached bool
	h := e.auth.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		if _, ok := UserFrom(r.Context()); !ok {
			t.Error("no user in context")
		}
	}))
	for _, c := range []struct {
		name, method, origin, site string
		bearer                     bool
		want                       int
	}{
		{"cross-origin POST", "POST", "https://evil.example", "", false, 403},
		{"cross-origin DELETE", "DELETE", "https://evil.example", "", false, 403},
		{"cross-origin PUT", "PUT", "https://evil.example", "", false, 403},
		{"null origin", "POST", "null", "", false, 403},
		{"same host other scheme", "POST", strings.Replace(e.srv.URL, "http:", "https:", 1), "", false, 403},
		{"cross-site, no Origin", "POST", "", "cross-site", false, 403},
		{"same origin", "POST", e.srv.URL, "same-origin", false, 200},
		{"no Origin", "POST", "", "", false, 200},
		{"cross-origin GET", "GET", "https://evil.example", "", false, 200},
		{"bearer exempt", "POST", "https://evil.example", "cross-site", true, 200},
	} {
		reached = false
		req := httptest.NewRequest(c.method, "/api/downloads", nil)
		if c.bearer {
			req.Header.Set("Authorization", "Bearer "+tok)
		} else {
			req.AddCookie(&http.Cookie{Name: sess.Name, Value: sess.Value})
		}
		if c.origin != "" {
			req.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			req.Header.Set("Sec-Fetch-Site", c.site)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.want || reached != (c.want == 200) {
			t.Errorf("%s: got %d (handler reached %v), want %d", c.name, w.Code, reached, c.want)
		}
	}
}

func TestLogoutEndsSession(t *testing.T) {
	e := newAuthEnv(t, "grab-users")
	e.login(t, "/")
	sess := e.sessionCookie()
	resp := e.get(t, e.srv.URL+"/auth/logout")
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("logout: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Even with the old cookie replayed, the session is gone server-side.
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/items", nil)
	req.AddCookie(&http.Cookie{Name: sess.Name, Value: sess.Value})
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatalf("after logout: %d", r.StatusCode)
	}
}

func TestNoAuthModeIsLocalAdmin(t *testing.T) {
	st, _ := testStore(t)
	a, err := NewNoAuth(st)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Static: staticFS(), Store: st, SharesDir: t.TempDir(), Auth: a}
	if w := get(t, app.Routes(), "/api/items"); w.Code != 200 {
		t.Fatalf("/api/items: %d %s", w.Code, w.Body)
	}
	var u User
	var ok bool
	h := a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { u, ok = UserFrom(r.Context()) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/items", nil))
	if !ok || u.ID != "local" || u.Dir != "local" || !u.Admin {
		t.Fatalf("%+v %v", u, ok)
	}
}

// Without an Auth the API must fail closed, not open.
func TestNilAuthFailsClosed(t *testing.T) {
	app := &App{Static: staticFS()}
	if w := get(t, app.Routes(), "/api/items"); w.Code != 401 {
		t.Fatalf("got %d", w.Code)
	}
}
