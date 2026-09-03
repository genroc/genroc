package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// The login, end to end, against a real OIDC provider -- a small one, but one that signs with an
// actual key and is verified by the same code a deployment uses. specs/ui-component.md.

const (
	testClientID     = "genroc"
	testSharedSecret = "a-shared-secret-long-enough-to-be-taken-seriously"
)

// fakeIdP serves discovery, a JWKS and a token endpoint, and signs ID tokens.
type fakeIdP struct {
	*httptest.Server
	key   *rsa.PrivateKey
	nonce string // the nonce it will stamp into the next token
	sub   string
}

func newIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key, sub: "alice@example.test"}
	mux := http.NewServeMux()
	idp.Server = httptest.NewServer(mux)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 idp.URL,
			"authorization_endpoint": idp.URL + "/auth",
			"token_endpoint":         idp.URL + "/token",
			"jwks_uri":               idp.URL + "/keys",
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]any{{
			"kty": "RSA", "kid": "k1", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if id, secret, ok := r.BasicAuth(); !ok || id != testClientID || secret == "" {
			// A confidential client: the provider must see credentials, not just a code.
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// An access token too, as every provider returns: it is what the Cloud Identity call
		// for a `type: google` provider goes out as.
		json.NewEncoder(w).Encode(map[string]any{
			"id_token": idp.mint(t, idp.nonce, time.Hour), "access_token": "ya29.test",
		})
	})
	t.Cleanup(idp.Close)
	return idp
}

func (f *fakeIdP) mint(t *testing.T, nonce string, ttl time.Duration) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss": f.URL, "aud": testClientID, "sub": f.sub, "email": f.sub,
		"groups": []any{"admins"},
		"exp":    time.Now().Add(ttl).Unix(), "iat": time.Now().Unix(),
	}
	if nonce != "" {
		claims["nonce"] = nonce
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// harness wires genroc-ui to a fake IdP and a fake upstream that reports what it received.
type harness struct {
	ui       *httptest.Server
	srv      *uiServer
	idp      *fakeIdP
	client   *http.Client
	lastAuth chan string
}

// newHarnessWithPasswords is the local-login shape: one user, no provider. Kept separate from
// the OIDC harness because adding a password there would stop it redirecting straight through,
// which is the behaviour those tests are checking.
func newHarnessWithPasswords(t *testing.T) *harness {
	t.Helper()
	h := &harness{lastAuth: make(chan string, 32)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.lastAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(upstream.Close)

	insecure := false
	cfg := &Config{
		Server: upstream.URL, Listen: ":0", SecureCookie: &insecure,
		Token: Token{Issuer: "genroc-ui", Audience: "genroc", Secret: testSharedSecret},
		Roles: map[string][]string{"admins": {"admin"}},
	}
	cfg.Login.Passwords = []Password{{
		Email: "ada@example.test",
		// bcrypt of "demo"
		Hash:   "$2a$10$QRz1D8GwpXluI19IzzkBmOHMMJDsfAKs3P9zCMOi6ElVbXybUD5Y2",
		Groups: []string{"admins"},
	}}
	h.ui = httptest.NewUnstartedServer(nil)
	cfg.RedirectURL = "http://" + h.ui.Listener.Addr().String() + "/auth/callback"

	s, err := newServer(cfg, testLogger())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	h.srv = s
	h.ui.Config.Handler = s.routes()
	h.ui.Start()
	t.Cleanup(h.ui.Close)

	h.client = &http.Client{Jar: newJar(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return h
}

func newHarness(t *testing.T, withOIDC bool) *harness {
	t.Helper()
	h := &harness{lastAuth: make(chan string, 32)}
	h.idp = newIdP(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.lastAuth <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[]}`))
	}))
	t.Cleanup(upstream.Close)

	insecure := false
	cfg := &Config{Server: upstream.URL, Listen: ":0", SecureCookie: &insecure}
	if withOIDC {
		cfg.Login.Providers = []Provider{{
			ID: "test", Name: "Test IdP", Issuer: h.idp.URL,
			ClientID: testClientID, ClientSecret: "secret",
			Scopes: []string{"openid", "email"}, SubjectClaim: "email",
		}}
		cfg.Token = Token{Issuer: "genroc-ui", Audience: "genroc", Secret: testSharedSecret}
		cfg.Roles = map[string][]string{"admins": {"admin"}, "*": {"read"}}
	}
	// The callback is absolute and must be known before the provider is discovered, so the
	// listener is created first and the config completed against its address.
	h.ui = httptest.NewUnstartedServer(nil)
	cfg.RedirectURL = "http://" + h.ui.Listener.Addr().String() + "/auth/callback"

	s, err := newServer(cfg, testLogger())
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	h.srv = s
	h.ui.Config.Handler = s.routes()
	h.ui.Start()
	t.Cleanup(h.ui.Close)

	jar := newJar()
	h.client = &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return h
}

// login walks the whole flow the way a browser would, following each redirect by hand so every
// hop can be asserted on.
func (h *harness) login(t *testing.T) {
	t.Helper()
	resp, err := h.client.Get(h.ui.URL + "/auth/login?rd=/instances")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("/auth/login = %d, want 302", resp.StatusCode)
	}
	authURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	// The provider would show a login form here; it has already decided who this is, so all
	// that matters is that it stamps the nonce genroc-ui asked for into the token.
	h.idp.nonce = authURL.Query().Get("nonce")
	state := authURL.Query().Get("state")

	cb := h.ui.URL + "/auth/callback?code=abc&state=" + url.QueryEscape(state)
	resp2, err := h.client.Get(cb)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("/auth/callback = %d, want 302", resp2.StatusCode)
	}
	if got := resp2.Header.Get("Location"); got != "/instances" {
		t.Errorf("landed on %q, want the path login was started from", got)
	}
}

func TestLogin_AttachesTheIdPsTokenToProxiedAPICalls(t *testing.T) {
	h := newHarness(t, true)
	h.login(t)

	resp, err := h.client.Get(h.ui.URL + "/api/instances")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/instances = %d after login", resp.StatusCode)
	}
	got := <-h.lastAuth
	if !strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("upstream saw Authorization %q; the browser holds no credential of its own, so "+
			"genroc-ui attaching one is the entire point of this component", got)
	}
	// It is OUR token, not the IdP's: minted here, carrying PERMISSIONS the role map resolved,
	// so the server never sees a group. specs/ui-issued-tokens.md §0.
	raw := strings.TrimPrefix(got, "Bearer ")
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(raw, claims); err != nil {
		t.Fatalf("what was attached is not a JWT: %v", err)
	}
	if claims["iss"] != "genroc-ui" {
		t.Errorf("attached a token issued by %v; the upstream token must never leave this process", claims["iss"])
	}
	if claims["sub"] != "alice@example.test" {
		t.Errorf("sub = %v", claims["sub"])
	}
	perms := claims["perms"]
	list, ok := perms.([]any)
	if !ok || len(list) == 0 {
		t.Fatalf("perms = %v; the role map resolved `admins` and the token must carry the result", perms)
	}
	if _, ok := claims["groups"]; ok {
		t.Error("the minted token leaked groups; the server has no role map and must not need one")
	}
}

func TestForward_PassesAnExistingCredentialThroughUntouched(t *testing.T) {
	h := newHarness(t, true)
	req, _ := http.NewRequest(http.MethodGet, h.ui.URL+"/api/instances", nil)
	req.Header.Set("Authorization", "Bearer genroc_sk_machine")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := <-h.lastAuth; got != "Bearer genroc_sk_machine" {
		t.Fatalf("upstream saw %q; a caller with its own credential must reach the server "+
			"unchanged, or a machine token could never be used through this component", got)
	}
}

func TestForward_WithoutASessionAnswersByWhatTheCallerCanDoAboutIt(t *testing.T) {
	h := newHarness(t, true)

	xhr, _ := http.NewRequest(http.MethodGet, h.ui.URL+"/api/instances", nil)
	xhr.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(xhr)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("XHR without a session = %d, want 401: a fetch cannot follow a login redirect, "+
			"and would render the provider's page as a failed API call", resp.StatusCode)
	}

	doc, _ := http.NewRequest(http.MethodGet, h.ui.URL+"/api/instances", nil)
	doc.Header.Set("Accept", "text/html")
	resp2, err := h.client.Do(doc)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusFound {
		t.Errorf("document request without a session = %d, want a redirect to login", resp2.StatusCode)
	}
}

// Isolated so that ONLY the state can be the reason it fails: the login is started properly and
// the provider is told the real nonce, so the token that comes back is entirely valid. An
// earlier version of this test skipped that and passed on a nonce mismatch instead -- green
// while the state check was deleted, which is worse than having no test.
func TestCallback_RefusesAStateItDidNotIssue(t *testing.T) {
	h := newHarness(t, true)

	resp, err := h.client.Get(h.ui.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	authURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	h.idp.nonce = authURL.Query().Get("nonce") // everything else about this login is correct

	bad, err := h.client.Get(h.ui.URL + "/auth/callback?code=abc&state=not-the-one")
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusForbidden {
		t.Fatalf("callback with a mismatched state = %d, want 403 — without this check any site "+
			"could complete a login into this session with a code of its choosing", bad.StatusCode)
	}
	// And no session was established by it.
	for _, c := range h.client.Jar.Cookies(mustURL(h.ui.URL)) {
		if c.Name == sessionCookie && c.Value != "" {
			t.Fatal("a refused callback still set a session cookie")
		}
	}
}

// The nonce is the other half, and it binds a token to the login that asked for it: a valid
// token from a DIFFERENT login of the same user must not complete this one.
func TestCallback_RefusesATokenFromAnotherLogin(t *testing.T) {
	h := newHarness(t, true)

	resp, err := h.client.Get(h.ui.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	authURL, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	h.idp.nonce = "a-nonce-from-some-other-login" // signed, unexpired, right issuer and audience

	bad, err := h.client.Get(h.ui.URL + "/auth/callback?code=abc&state=" + url.QueryEscape(state))
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != http.StatusForbidden {
		t.Fatalf("callback accepted a token minted for a different login = %d, want 403", bad.StatusCode)
	}
}

func TestLogin_WillNotRedirectOffSite(t *testing.T) {
	h := newHarness(t, true)
	for _, rd := range []string{"https://evil.test/", "//evil.test/", "http://evil.test"} {
		resp, err := h.client.Get(h.ui.URL + "/auth/login?rd=" + url.QueryEscape(rd))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		// The redirect goes to the IdP; what matters is the return target stashed for later.
		for _, c := range h.client.Jar.Cookies(mustURL(h.ui.URL)) {
			if c.Name == returnCookie && c.Value != "/" {
				t.Errorf("rd=%q was stored as %q; an absolute return target makes this an open "+
					"redirect any site can point at itself", rd, c.Value)
			}
		}
		_ = loc
	}
}

func TestSession_ATamperedCookieIsTreatedAsAbsent(t *testing.T) {
	h := newHarness(t, true)
	h.login(t)

	// Swap in a token signed by a different key entirely.
	other := newIdP(t)
	other.sub = "mallory@evil.test"
	h.client.Jar.SetCookies(mustURL(h.ui.URL), []*http.Cookie{{
		Name: sessionCookie, Value: other.mint(t, "", time.Hour), Path: "/",
	}})

	req, _ := http.NewRequest(http.MethodGet, h.ui.URL+"/api/instances", nil)
	req.Header.Set("Accept", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a cookie signed by another key was accepted (%d). genroc-ui verifies what it "+
			"holds so a bad cookie fails HERE, rather than being attached, refused by the server, "+
			"and looping the browser through a login that looks broken", resp.StatusCode)
	}
}

func TestWithoutOIDC_ProxiesAsItArrives(t *testing.T) {
	h := newHarness(t, false)
	resp, err := h.client.Get(h.ui.URL + "/api/instances")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("= %d; with no IdP configured this is the laptop shape and must not gate", resp.StatusCode)
	}
	if got := <-h.lastAuth; got != "" {
		t.Errorf("attached %q with no login configured", got)
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// A login must never be sent back to a login. needLogin builds `?rd=<the request>`, so without
// this an /auth/ target nests one redirect inside the next until the browser gives up -- which
// is what a stray redirect chain produced before the guard existed.
func TestSafeReturn_RefusesOffSiteAndAuthPaths(t *testing.T) {
	cases := map[string]string{
		"/instances":            "/instances",
		"/":                     "/",
		"https://evil.test/":    "/",
		"//evil.test/":          "/",
		"/auth/login?rd=%2Fa":   "/",
		"/auth/callback?code=x": "/",
		"not-a-path":            "/",
		"":                      "/",
	}
	for in, want := range cases {
		if got := safeReturn(in); got != want {
			t.Errorf("safeReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

// The role map lives here now, so this is where it gets tested. specs/ui-issued-tokens.md §1.
func TestResolve_UnionsGroupsSubjectAndTheWildcard(t *testing.T) {
	roles := map[string][]string{"admins": {"admin"}, "*": {"read"}}
	users := map[string][]string{"ada@example.com": {"deploy"}}

	got := resolve(roles, users, identity{Subject: "ada@example.com"})
	if !has(got, "deploy") {
		t.Error("a subject entry did not grant; `users` is what serves a provider carrying no groups")
	}
	if !has(got, "read") {
		t.Error("the `*` rule did not apply to someone who logged in")
	}
	if has(got, "admin") {
		t.Error("granted a group the caller is not in")
	}
	if withGroup := resolve(roles, users, identity{Subject: "bob@x", Groups: []string{"admins"}}); !has(withGroup, "admin") {
		t.Error("a group the caller IS in did not grant")
	}
}

// The session cookie and the access token are both HS256 from one key. Only the audience keeps a
// 12-hour cookie from being replayed as a bearer credential, so it has to be checked.
func TestSigner_ASessionCookieCannotBeUsedAsAnAccessToken(t *testing.T) {
	s := &signer{secret: []byte(testSharedSecret), issuer: "genroc-ui", audience: "genroc",
		tokenTTL: time.Minute, sessionTTL: time.Hour}

	session, _, err := s.mintSession(identity{Subject: "ada@example.com", Groups: []string{"admins"}})
	if err != nil {
		t.Fatal(err)
	}
	// Read back as a session: fine.
	if id, err := s.readSession(session); err != nil || id.Subject != "ada@example.com" {
		t.Fatalf("readSession: %v %v", id, err)
	}
	// Presented to the genroc server: it pins aud=genroc, and this one says genroc-ui-session.
	claims := jwt.MapClaims{}
	_, err = jwt.NewParser(jwt.WithAudience("genroc"), jwt.WithIssuer("genroc-ui")).
		ParseWithClaims(session, claims, func(*jwt.Token) (any, error) { return []byte(testSharedSecret), nil })
	if err == nil {
		t.Fatal("a session cookie verified as an access token — the two are signed with one key, " +
			"so the audience is the only thing separating a long-lived cookie from a bearer credential")
	}

	// And an access token is not a session.
	access, err := s.mintAccess(identity{Subject: "ada@example.com"}, []string{"read"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.readSession(access); err == nil {
		t.Fatal("an access token was accepted as a session cookie")
	}
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// genroc-ui must not gate what the server serves openly. A probe has to answer before any
// identity exists; gating it made /healthz 401 through the UI while the server answered 200,
// which gets a container marked unhealthy for reasons nobody can find. api-auth.md §1.
func TestForward_DoesNotGateWhatTheServerServesOpenly(t *testing.T) {
	h := newHarness(t, true) // a login IS configured, so everything else is gated
	for _, path := range []string{"/healthz", "/public/openapi.json"} {
		resp, err := h.client.Get(h.ui.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d with no session, want 200 — the server serves it open and this "+
				"component must not add a gate the server does not have", path, resp.StatusCode)
		}
		if got := <-h.lastAuth; got != "" {
			t.Errorf("GET %s attached %q; an open path needs no credential invented for it", path, got)
		}
	}
}

// The login page is its own bundle, and it needs two things before any session exists: the page
// itself, and the list of ways in. Both must answer unauthenticated -- and the assets they pull
// must too, or the page renders blank at exactly the moment nobody can do anything about it.
func TestLoginPage_AndItsOptionsAnswerWithoutASession(t *testing.T) {
	h := newHarness(t, true)

	resp, err := h.client.Get(h.ui.URL + "/auth/options")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/auth/options = %d with no session; the page that calls it has no credential "+
			"and cannot get one without it", resp.StatusCode)
	}
	var opts struct {
		Providers []struct{ ID, Name string }
		Passwords bool
	}
	if err := json.NewDecoder(resp.Body).Decode(&opts); err != nil {
		t.Fatal(err)
	}
	if len(opts.Providers) != 1 || opts.Providers[0].ID != "test" {
		t.Fatalf("providers = %+v", opts.Providers)
	}
	if opts.Providers[0].Name != "Test IdP" {
		t.Errorf("name = %q; the button shows this", opts.Providers[0].Name)
	}
}

// A password login answers the login bundle's fetch: 204 with the cookie, or 401 with a reason.
func TestPasswordLogin_AnswersJSONAndSetsTheSession(t *testing.T) {
	h := newHarnessWithPasswords(t)

	// bcrypt of "demo"
	post := func(email, pw string) *http.Response {
		body := `{"email":` + jsonStr(email) + `,"password":` + jsonStr(pw) + `}`
		resp, err := h.client.Post(h.ui.URL+"/auth/password", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	bad := post("ada@example.test", "wrong")
	bad.Body.Close()
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d, want 401", bad.StatusCode)
	}
	unknown := post("nobody@example.test", "demo")
	unknown.Body.Close()
	if unknown.StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown email = %d; it must be indistinguishable from a wrong password, or the "+
			"response says which addresses are real", unknown.StatusCode)
	}

	ok := post("ada@example.test", "demo")
	ok.Body.Close()
	if ok.StatusCode != http.StatusNoContent {
		t.Fatalf("correct password = %d, want 204", ok.StatusCode)
	}
	// And the session it set actually works against the API.
	resp, err := h.client.Get(h.ui.URL + "/api/instances")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("API after a password login = %d", resp.StatusCode)
	}
	got := <-h.lastAuth
	if !strings.HasPrefix(got, "Bearer ") {
		t.Fatalf("upstream saw %q; a password login must produce the same minted token an OIDC "+
			"one does", got)
	}
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// The callback and the cookie's Secure flag are DERIVED, so a deployment states its address
// once — at the provider, where it must be registered anyway — rather than twice.
func TestDerived_CallbackURLAndCookieSecurityFollowTheRequest(t *testing.T) {
	s := &uiServer{cfg: &Config{}}

	plain := httptest.NewRequest(http.MethodGet, "http://genroc.example.com/auth/login", nil)
	if got := s.callbackURL(plain); got != "http://genroc.example.com/auth/callback" {
		t.Errorf("callbackURL = %q", got)
	}
	if s.secure(plain) {
		t.Error("Secure on a plain-HTTP request: the browser drops the cookie and nothing works")
	}

	fwd := httptest.NewRequest(http.MethodGet, "http://genroc.example.com/auth/login", nil)
	fwd.Header.Set("X-Forwarded-Proto", "https")
	if got := s.callbackURL(fwd); got != "https://genroc.example.com/auth/callback" {
		t.Errorf("behind a TLS-terminating proxy: callbackURL = %q", got)
	}
	if !s.secure(fwd) {
		t.Error("no Secure behind a TLS proxy: the session cookie travels weaker than the connection")
	}

	// Both stay overridable, for a proxy that rewrites Host or does not set X-Forwarded-Proto.
	no := false
	pinned := &uiServer{cfg: &Config{
		RedirectURL:  "https://pinned.example/auth/callback",
		SecureCookie: &no,
	}}
	if got := pinned.callbackURL(fwd); got != "https://pinned.example/auth/callback" {
		t.Errorf("an explicit redirect_url was ignored: %q", got)
	}
	if pinned.secure(fwd) {
		t.Error("an explicit secure_cookie=false was ignored")
	}
}

// A password is the only guessable secret reachable from outside, so failures are throttled.
// bcrypt slows a guess; it does not limit one, and an attacker parallelises the constant factor
// away.
func TestPasswordLogin_ThrottlesFailures(t *testing.T) {
	h := newHarnessWithPasswords(t)
	post := func(email, pw string) int {
		body := `{"email":` + jsonStr(email) + `,"password":` + jsonStr(pw) + `}`
		resp, err := h.client.Post(h.ui.URL+"/auth/password", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	for i := 0; i < maxEmailFailures; i++ {
		if got := post("ada@example.test", "wrong"); got != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, got)
		}
	}
	if got := post("ada@example.test", "wrong"); got != http.StatusTooManyRequests {
		t.Fatalf("attempt %d = %d, want 429 — without a limit bcrypt is a constant factor, "+
			"not a defence", maxEmailFailures+1, got)
	}
	// Throttled means throttled: the CORRECT password is refused too, or an attacker simply
	// keeps going until they find it.
	if got := post("ada@example.test", "demo"); got != http.StatusTooManyRequests {
		t.Fatalf("the right password was accepted while throttled (%d)", got)
	}
}

// A refusal that does not say for how long is retried, which is both the worse experience and
// more load than answering the question.
func TestPasswordLogin_TheRefusalSaysHowLongToWait(t *testing.T) {
	h := newHarnessWithPasswords(t)
	var last *http.Response
	for i := 0; i <= maxEmailFailures; i++ {
		body := `{"email":"ada@example.test","password":"wrong"}`
		resp, err := h.client.Post(h.ui.URL+"/auth/password", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if last != nil {
			last.Body.Close()
		}
		last = resp
	}
	defer last.Body.Close()
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("final attempt = %d, want 429", last.StatusCode)
	}
	var got struct{ Error string }
	if err := json.NewDecoder(last.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Error, "minute") {
		t.Errorf("message %q does not name the wait", got.Error)
	}
	if last.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After header; a client cannot back off on prose")
	}
}

func TestWaitFor_RoundsUpSoTheAnswerIsNeverTooEarly(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{time.Second, "a minute"},
		{time.Minute, "a minute"},
		// The rounding direction is the point: 61s must not say "a minute", or someone told to
		// wait one minute comes back to a second refusal.
		{61 * time.Second, "2 minutes"},
		{5 * time.Minute, "5 minutes"},
		{4*time.Minute + time.Millisecond, "5 minutes"},
	} {
		if got := waitFor(tc.in); got != tc.want {
			t.Errorf("waitFor(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A success clears the email's budget — those were a person mistyping — but not the address's,
// or one correct login would buy a fresh budget for every other account being worked through.
func TestLimiter_SuccessClearsTheEmailNotTheAddress(t *testing.T) {
	l := newLimiter()
	for i := 0; i < maxEmailFailures; i++ {
		l.fail("email:ada@example.test")
		l.fail("addr:10.0.0.1")
	}
	if ok, _ := l.allow("email:ada@example.test", maxEmailFailures); ok {
		t.Fatal("the email budget did not trip")
	}
	l.succeed("email:ada@example.test")
	if ok, _ := l.allow("email:ada@example.test", maxEmailFailures); !ok {
		t.Error("a correct password did not clear the email's history")
	}
	// The address keeps its count.
	for i := maxEmailFailures; i < maxAddrFailures; i++ {
		l.fail("addr:10.0.0.1")
	}
	if ok, _ := l.allow("addr:10.0.0.1", maxAddrFailures); ok {
		t.Error("the address budget was reset by an unrelated success")
	}
}

// The window expires, or a mistyped password would lock an account out forever.
func TestLimiter_ForgetsAfterTheWindow(t *testing.T) {
	now := time.Now()
	l := newLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < maxEmailFailures; i++ {
		l.fail("email:ada@example.test")
	}
	if ok, retry := l.allow("email:ada@example.test", maxEmailFailures); ok || retry <= 0 {
		t.Fatalf("expected a throttle with a retry hint, got ok=%v retry=%v", ok, retry)
	}
	now = now.Add(failureWindow + time.Second)
	if ok, _ := l.allow("email:ada@example.test", maxEmailFailures); !ok {
		t.Error("still throttled after the window; a typo would lock the account out for good")
	}
}

// Unbounded tracking is itself the attack: a fresh address per request would grow the map until
// the process dies.
func TestLimiter_SweepsExpiredKeys(t *testing.T) {
	now := time.Now()
	l := newLimiter()
	l.now = func() time.Time { return now }
	for i := 0; i < sweepAbove+10; i++ {
		l.fail("addr:" + strconv.Itoa(i))
	}
	grown := len(l.windows)
	now = now.Add(failureWindow + time.Second)
	l.fail("addr:trigger-the-sweep")
	if len(l.windows) >= grown {
		t.Fatalf("expired keys were not swept: %d before, %d after", grown, len(l.windows))
	}
}

// Sign-out clears the session, and is a POST because its whole job is changing state.
func TestLogout_ClearsTheSessionAndIsNotAGET(t *testing.T) {
	h := newHarnessWithPasswords(t)
	body := `{"email":"ada@example.test","password":"demo"}`
	resp, err := h.client.Post(h.ui.URL+"/auth/password", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, _ := h.client.Get(h.ui.URL + "/api/instances"); got.StatusCode != http.StatusOK {
		t.Fatalf("signed in but API = %d", got.StatusCode)
	}

	if got, err := h.client.Get(h.ui.URL + "/auth/logout"); err != nil {
		t.Fatal(err)
	} else if got.StatusCode == http.StatusNoContent {
		t.Error("GET /auth/logout signed the user out; any page that can make the browser " +
			"follow a link could then do it")
	}

	out, err := h.client.Post(h.ui.URL+"/auth/logout", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Close()
	if out.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /auth/logout = %d, want 204", out.StatusCode)
	}
	after, err := h.client.Get(h.ui.URL + "/api/instances")
	if err != nil {
		t.Fatal(err)
	}
	after.Body.Close()
	if after.StatusCode != http.StatusUnauthorized {
		t.Fatalf("still authenticated after signing out (%d)", after.StatusCode)
	}
}

// The whole point of `type: google`: membership comes from the API, not from the ID token. The
// fake IdP here asserts `groups: [admins]` the way any OIDC provider would; Google never does,
// so what the directory says has to win outright rather than being merged.
func TestGoogleType_TheFetchedGroupsReplaceTheTokensOwn(t *testing.T) {
	h := newHarness(t, true)
	h.srv.cfg.Login.Providers[0].Type = "google"
	h.srv.cfg.Roles = map[string][]string{
		"admins":               {"admin"}, // what the ID token claims, and must not be honoured
		"platform@example.com": {"deploy"},
		"*":                    {"read"},
	}
	h.srv.directory = stubDirectory(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"memberships": []map[string]any{
			{"groupKey": map[string]string{"id": "platform@example.com"}},
		}})
	})

	h.login(t)
	resp, err := h.client.Get(h.ui.URL + "/api/instances")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	perms := permsOf(t, <-h.lastAuth)
	if !slices.Contains(perms, "deploy") {
		t.Errorf("perms %v: the group Cloud Identity reported granted nothing", perms)
	}
	if slices.Contains(perms, "admin") {
		t.Errorf("perms %v: a `groups` claim in the ID token was honoured for a google provider, "+
			"so anyone whose IdP can mint that claim picks up whatever it maps to", perms)
	}
}

// A directory that cannot answer must not sign the person in with fewer permissions than they
// have: that reads as a broken role map and is debugged as one.
func TestGoogleType_AFailedFetchRefusesTheLogin(t *testing.T) {
	h := newHarness(t, true)
	h.srv.cfg.Login.Providers[0].Type = "google"
	h.srv.directory = stubDirectory(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})

	resp, err := h.client.Get(h.ui.URL + "/auth/login?rd=/instances")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	authURL, _ := url.Parse(resp.Header.Get("Location"))
	h.idp.nonce = authURL.Query().Get("nonce")
	cb, err := h.client.Get(h.ui.URL + "/auth/callback?code=abc&state=" +
		url.QueryEscape(authURL.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	cb.Body.Close()
	if cb.StatusCode == http.StatusFound {
		t.Fatal("the login succeeded although the groups could not be read")
	}
	// And no session was set, so the next request is not quietly a read-only one.
	api, err := h.client.Get(h.ui.URL + "/api/instances")
	if err != nil {
		t.Fatal(err)
	}
	api.Body.Close()
	if api.StatusCode != http.StatusUnauthorized {
		t.Errorf("API after a failed login = %d, want 401", api.StatusCode)
	}
}

func permsOf(t *testing.T, auth string) []string {
	t.Helper()
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(strings.TrimPrefix(auth, "Bearer "), claims); err != nil {
		t.Fatalf("minted token: %v", err)
	}
	return stringList(claims["perms"])
}
