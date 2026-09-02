// genroc-ui serves the genroc web UI, logs a person in, and mints the token the genroc server
// verifies. specs/ui-component.md, specs/ui-issued-tokens.md.
//
// It ISSUES. It authenticates a person against an OIDC provider or a password in its own
// config, resolves their groups to permissions through the role map, and signs a short-lived
// token carrying those permissions. The genroc server verifies it and applies them; it has no
// role map and never learns what a group is.
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	"genroc/ui/oidc"
)

//go:embed all:web
var webAssets embed.FS

const (
	sessionCookie = "genroc_session"
	stateCookie   = "genroc_oidc_state"
	nonceCookie   = "genroc_oidc_nonce"
	returnCookie  = "genroc_oidc_return"
	// providerCookie remembers WHICH provider a login is with, so the callback can verify the
	// token against the right issuer. It cannot go in the URL: the callback is one registered
	// address shared by every provider.
	providerCookie = "genroc_oidc_provider"
)

func main() {
	configPath := flag.String("config", os.Getenv("GENROC_UI_CONFIG"),
		"Path to the YAML config ($GENROC_UI_CONFIG). Without it, -server alone runs a UI with no login: requests are proxied as they arrive, which is the local shape against a server running -auth none or a pasted token.")
	server := flag.String("server", envOr("GENROC_SERVER", "http://localhost:8449"),
		"The genroc API to proxy to, when no -config is given ($GENROC_SERVER).")
	listen := flag.String("http", "", "Listen address; overrides the config.")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := &Config{Server: *server, Listen: ":8448"}
	if *configPath != "" {
		var err error
		if cfg, err = LoadConfig(*configPath); err != nil {
			log.Error("config", "err", err)
			os.Exit(1)
		}
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	s, err := newServer(cfg, log)
	if err != nil {
		log.Error("startup", "err", err)
		os.Exit(1)
	}
	log.Info("genroc-ui listening", "addr", cfg.Listen, "upstream", cfg.Server,
		"providers", len(cfg.Login.Providers), "passwords", len(cfg.Login.Passwords))
	if err := http.ListenAndServe(cfg.Listen, s.routes()); err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

type uiServer struct {
	cfg   *Config
	log   *slog.Logger
	proxy *httputil.ReverseProxy
	sign  *signer
	// providers, keyed by id, discovered once at startup.
	providers map[string]*oidc.Provider
	order     []Provider // config order, for a stable button list
	assets    fs.FS      // the built bundles: the app, the login page, and their assets
}

func newServer(cfg *Config, log *slog.Logger) (*uiServer, error) {
	target, err := url.Parse(cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("server %q: %w", cfg.Server, err)
	}
	assets, err := fs.Sub(webAssets, "web")
	if err != nil {
		return nil, err // the embed is a compile-time constant; a failure here is a build error
	}
	s := &uiServer{
		cfg: cfg, log: log,
		proxy:     httputil.NewSingleHostReverseProxy(target),
		providers: map[string]*oidc.Provider{},
		assets:    assets,
	}
	if !cfg.loginConfigured() {
		// Loud, because in a deployment it is a mistake; not fatal, because on a laptop it is
		// the point.
		log.Warn("no login configured: requests are proxied as they arrive")
		return s, nil
	}

	secret, err := cfg.Token.resolveSecret("config")
	if err != nil {
		return nil, err
	}
	sessionTTL, err := cfg.sessionTTL()
	if err != nil {
		return nil, fmt.Errorf("session_ttl: %w", err)
	}
	tokenTTL, err := cfg.tokenTTL()
	if err != nil {
		return nil, fmt.Errorf("token.ttl: %w", err)
	}
	s.sign = &signer{
		secret: []byte(secret), issuer: cfg.Token.Issuer, audience: cfg.Token.Audience,
		tokenTTL: tokenTTL, sessionTTL: sessionTTL,
	}

	for _, p := range cfg.Login.Providers {
		scopes := p.Scopes
		if len(scopes) == 0 {
			scopes = []string{"openid", "email", "profile", "groups"}
		}
		// Discovery happens at STARTUP, so a provider that cannot be reached or names a
		// different issuer fails here rather than at somebody's first login.
		prov, err := oidc.Discover(context.Background(), oidc.Config{
			Issuer: p.Issuer, DiscoveryURL: p.DiscoveryURL, TokenURL: p.TokenURL,
			JWKSURL: p.JWKSURL, ClientID: p.ClientID, ClientSecret: p.ClientSecret,
			Scopes: scopes,
		})
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", p.ID, err)
		}
		s.providers[p.ID] = prov
		s.order = append(s.order, p)
		log.Info("provider ready", "id", p.ID, "issuer", p.Issuer)
	}
	return s, nil
}

func (s *uiServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", s.login)
	mux.HandleFunc("GET /auth/options", s.options)
	mux.HandleFunc("POST /auth/password", s.passwordLogin)
	mux.HandleFunc("GET /auth/callback", s.callback)
	mux.HandleFunc("GET /auth/logout", s.logout)

	files := http.FileServer(http.FS(s.assets))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if isUpstream(r.URL.Path) {
			s.forward(w, r)
			return
		}
		// A real file in the bundle is served to anyone. It has to be: the login page is one of
		// them, and so are the script and stylesheet it needs to render. A built bundle
		// discloses nothing -- what it can DO is decided by the credential it obtains.
		if _, err := fs.Stat(s.assets, strings.TrimPrefix(r.URL.Path, "/")); err == nil {
			files.ServeHTTP(w, r)
			return
		}
		// Anything else is the app's own document, and that IS behind the login: without this
		// the page loads, its first API call 401s, and nothing sends the browser anywhere.
		if s.sign != nil {
			if _, ok := s.session(r); !ok {
				s.needLogin(w, r)
				return
			}
		}
		r = r.Clone(r.Context())
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	})
	return mux
}

func isUpstream(p string) bool {
	return isOpen(p) || strings.HasPrefix(p, "/api/")
}

// isOpen names the paths the SERVER serves without a credential, and genroc-ui must not gate
// what the server does not: a probe has to answer before any identity exists, and the public
// docs disclose nothing stored. api-auth.md §1.
//
// Gating these is not a theoretical mistake -- it made `/healthz` return 401 through the UI
// while answering 200 on the server, which is exactly the shape that gets a container marked
// unhealthy for reasons nobody can find.
func isOpen(p string) bool {
	return p == "/healthz" || strings.HasPrefix(p, "/public/")
}

// forward: pass an existing credential through untouched, else mint one from the session.
func (s *uiServer) forward(w http.ResponseWriter, r *http.Request) {
	if isOpen(r.URL.Path) {
		s.proxy.ServeHTTP(w, r)
		return
	}
	if r.Header.Get("Authorization") != "" {
		// A pasted genroc_sk_*, or a caller with its own token. This component does not judge
		// credentials; the server does.
		s.proxy.ServeHTTP(w, r)
		return
	}
	if s.sign == nil {
		s.proxy.ServeHTTP(w, r)
		return
	}
	id, ok := s.session(r)
	if !ok {
		s.needLogin(w, r)
		return
	}
	perms := resolve(s.cfg.Roles, s.cfg.Users, id)
	tok, err := s.sign.mintAccess(id, perms)
	if err != nil {
		s.log.Error("mint access token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	s.proxy.ServeHTTP(w, r)
}

func (s *uiServer) session(r *http.Request) (identity, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return identity{}, false
	}
	id, err := s.sign.readSession(c.Value)
	if err != nil {
		return identity{}, false
	}
	return id, true
}

// needLogin answers a browser and an XHR differently, because only one can follow a redirect
// usefully: a fetch would render the login page as a failed API call.
func (s *uiServer) needLogin(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/auth/login?rd="+url.QueryEscape(safeReturn(r.URL.RequestURI())), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprint(w, `{"error":"not signed in","code":"unauthenticated"}`)
}

// login shows the chooser, or skips it. With exactly one way in and nothing to type, a screen
// asking the question with one answer is a click nobody should have to make.
func (s *uiServer) login(w http.ResponseWriter, r *http.Request) {
	if s.sign == nil {
		http.Error(w, "no login is configured", http.StatusNotImplemented)
		return
	}
	rd := safeReturn(r.URL.Query().Get("rd"))
	if id := r.URL.Query().Get("provider"); id != "" {
		s.startOIDC(w, r, id, rd)
		return
	}
	if len(s.order) == 1 && len(s.cfg.Login.Passwords) == 0 {
		s.startOIDC(w, r, s.order[0].ID, rd)
		return
	}
	s.serveLoginPage(w, r)
}

func (s *uiServer) startOIDC(w http.ResponseWriter, r *http.Request, id, rd string) {
	prov, ok := s.providers[id]
	if !ok {
		http.Error(w, "unknown provider", http.StatusNotFound)
		return
	}
	state, err := oidc.RandomState()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	nonce, err := oidc.RandomState()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.setTemp(w, r, stateCookie, state)
	s.setTemp(w, r, nonceCookie, nonce)
	s.setTemp(w, r, returnCookie, rd)
	s.setTemp(w, r, providerCookie, id)
	http.Redirect(w, r, prov.AuthCodeURL(s.callbackURL(r), state, nonce), http.StatusFound)
}

func (s *uiServer) callback(w http.ResponseWriter, r *http.Request) {
	if s.sign == nil {
		http.Error(w, "no login is configured", http.StatusNotImplemented)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "login failed: "+e, http.StatusForbidden)
		return
	}
	state, err := r.Cookie(stateCookie)
	if err != nil || state.Value == "" || state.Value != r.URL.Query().Get("state") {
		// Without this any site could complete a login into this session with a code of its own.
		http.Error(w, "login state did not match; start again at /auth/login", http.StatusForbidden)
		return
	}
	pc, err := r.Cookie(providerCookie)
	if err != nil {
		http.Error(w, "login state did not match; start again at /auth/login", http.StatusForbidden)
		return
	}
	prov, ok := s.providers[pc.Value]
	if !ok {
		http.Error(w, "unknown provider", http.StatusForbidden)
		return
	}
	nonceVal := ""
	if n, err := r.Cookie(nonceCookie); err == nil {
		nonceVal = n.Value
	}

	raw, err := prov.Exchange(r.Context(), r.URL.Query().Get("code"), s.callbackURL(r), nonceVal)
	if err != nil {
		s.log.Warn("token exchange failed", "provider", pc.Value, "err", err)
		http.Error(w, "login failed", http.StatusForbidden)
		return
	}
	subClaim, grpClaim := s.claimNames(pc.Value)
	claims, err := prov.Claims(r.Context(), raw, nonceVal, subClaim, grpClaim)
	if err != nil {
		s.log.Warn("verify id token", "provider", pc.Value, "err", err)
		http.Error(w, "login failed", http.StatusForbidden)
		return
	}
	// The provider's token has now done its whole job. It is not stored anywhere.
	s.establish(w, r, identity{Subject: claims.Subject, Groups: claims.Groups})
}

func (s *uiServer) claimNames(providerID string) (string, string) {
	for _, p := range s.cfg.Login.Providers {
		if p.ID == providerID {
			return p.SubjectClaim, p.GroupsClaim
		}
	}
	return "", ""
}

// setSession mints the session and sets the cookie. Separate from any response, because the two
// login paths answer differently: the OIDC callback redirects a browser, the password endpoint
// answers a fetch.
func (s *uiServer) setSession(w http.ResponseWriter, r *http.Request, id identity) error {
	tok, exp, err := s.sign.mintSession(id)
	if err != nil {
		return err
	}
	s.clearTemp(w, r, stateCookie)
	s.clearTemp(w, r, nonceCookie)
	s.clearTemp(w, r, providerCookie)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode,
		Expires: exp,
	})
	return nil
}

// establish is the OIDC half: set the session, then land the browser where it was going.
func (s *uiServer) establish(w http.ResponseWriter, r *http.Request, id identity) {
	if err := s.setSession(w, r, id); err != nil {
		s.log.Error("mint session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	rd := "/"
	if c, err := r.Cookie(returnCookie); err == nil {
		rd = safeReturn(c.Value)
	}
	s.clearTemp(w, r, returnCookie)
	s.log.Info("signed in", "subject", id.Subject, "groups", id.Groups)
	http.Redirect(w, r, rd, http.StatusFound)
}

func (s *uiServer) logout(w http.ResponseWriter, r *http.Request) {
	s.clearTemp(w, r, sessionCookie)
	http.Redirect(w, r, "/", http.StatusFound)
}

// safeReturn sanitises where a login lands, and refuses two things: an ABSOLUTE target would be
// an open redirect any site could point at itself, and one under /auth/ would nest, because
// needLogin builds `?rd=<the request>` and would wrap its own redirect forever.
func safeReturn(rd string) string {
	if !strings.HasPrefix(rd, "/") || strings.HasPrefix(rd, "//") || strings.HasPrefix(rd, "/auth/") {
		return "/"
	}
	return rd
}

// secure reports whether cookies should carry the Secure attribute. Derived from the request
// rather than configured, because the two are never independent: over HTTP a Secure cookie is
// silently dropped and nothing works, over HTTPS its absence is a downgrade. Guessing wrong is a
// footgun in both directions, and the request already knows.
//
// `secure_cookie` overrides it, for the one case the request cannot see: a proxy terminating TLS
// that does not set X-Forwarded-Proto.
func (s *uiServer) secure(r *http.Request) bool {
	if s.cfg.SecureCookie != nil {
		return *s.cfg.SecureCookie
	}
	return isHTTPS(r)
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// callbackURL is where a provider sends the browser back. Derived from the request, so a
// deployment states its own address once -- at the provider, where it must be registered
// anyway -- rather than twice.
//
// The Host header is the client's to set, so this is not a value to trust. It is not trusted: it
// goes to the provider, which accepts only redirect URIs registered with it in advance and
// rejects anything else. `redirect_url` pins it for a deployment behind a proxy that rewrites
// Host.
func (s *uiServer) callbackURL(r *http.Request) string {
	if s.cfg.RedirectURL != "" {
		return s.cfg.RedirectURL
	}
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/auth/callback"
}

func (s *uiServer) setTemp(w http.ResponseWriter, r *http.Request, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/",
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode,
		MaxAge: int((10 * time.Minute).Seconds()),
	})
}

func (s *uiServer) clearTemp(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: "", Path: "/",
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
