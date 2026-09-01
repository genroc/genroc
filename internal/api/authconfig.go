package api

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The auth configuration file. specs/api-auth.md §4 — a FILE rather than a table, because the
// policy governing an API must not be editable through that API: a `deploy` permission that can
// rewrite the role map is `admin` wearing a disguise. Mounted read-only from a ConfigMap is also
// the k8s-idiomatic shape, and it needs no bootstrapping story.

type AuthConfig struct {
	Mode   string              `yaml:"mode"` // none | token | header
	Header HeaderModeConfig    `yaml:"header"`
	Roles  map[string][]string `yaml:"roles"` // asserted role -> permissions
	// Users maps a SUBJECT to permissions, unioned with its roles. Groups are the better axis;
	// this is for providers that supply none (GitHub is OAuth2, Google omits them). §4.
	Users map[string][]string `yaml:"users"`
	// SessionTTL bounds a token from /session/token ("8h"); empty means defaultSessionTTL.
	// The exchange cannot return a token it issued (only the hash is stored), so every call
	// mints — without an expiry each page load leaves a permanent credential behind.
	SessionTTL string `yaml:"session_ttl"`
}

// A working day plus slack: no re-auth mid-task, dead by morning.
const defaultSessionTTL = 12 * time.Hour

type HeaderModeConfig struct {
	Subject string `yaml:"subject"` // header carrying who the caller is
	Roles   string `yaml:"roles"`   // header carrying their roles, comma-separated
	// TrustedProxies is REQUIRED in header mode and has no default. Header trust is a total
	// bypass if genroc is reachable directly — one port-forward past the ingress and any caller
	// asserts any identity — so refusing to start without it is the only guard that cannot be
	// forgotten. specs/api-auth.md §6.
	TrustedProxies []string `yaml:"trusted_proxies"`
}

func LoadAuthConfig(path string) (*AuthConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg AuthConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.Mode == "header" {
		if cfg.Header.Subject == "" {
			return nil, fmt.Errorf("%s: header.subject is required in header mode", path)
		}
		if len(cfg.Header.TrustedProxies) == 0 {
			return nil, fmt.Errorf("%s: header.trusted_proxies is required in header mode — "+
				"without it any caller that reaches this port can assert any identity", path)
		}
	}
	if cfg.SessionTTL != "" {
		d, err := time.ParseDuration(cfg.SessionTTL)
		if err != nil {
			return nil, fmt.Errorf("%s: session_ttl %q: %w", path, cfg.SessionTTL, err)
		}
		// Zero is refused rather than read as "never": that is the behaviour this field exists
		// to remove, and spelling it as a duration would make it look deliberate.
		if d <= 0 {
			return nil, fmt.Errorf("%s: session_ttl must be positive; got %q", path, cfg.SessionTTL)
		}
	}
	return &cfg, nil
}

// HeaderAuth is `mode: header`: a proxy authenticated the caller and forwarded the result.
//
// **Two ways this fails, and this type can only guard one.** `trusted_proxies` stops a caller
// that reaches genroc directly. It does NOT stop a trusted proxy that FORWARDS a client's copy
// of the identity header on a route where it does not set one — a forgery laundered into a
// trusted assertion, byte-identical to a legitimate one on arrival. The proxy has to strip;
// nothing here can tell the difference. specs/api-auth.md §6.
// Weaker than a signed token and not legacy — it is the compatibility surface for setups that
// produce no verifiable one (GitHub is not OIDC; Google omits groups from the ID token; a mesh
// asserts a workload identity). specs/api-auth.md §2.2.
type HeaderAuth struct {
	cfg        HeaderModeConfig
	roles      map[string][]string
	users      map[string][]string
	sessionTTL time.Duration
	trusted    []*net.IPNet
}

// SessionTTL is how long a token minted by /session/token stays valid.
func (h *HeaderAuth) SessionTTL() time.Duration { return h.sessionTTL }

func NewHeaderAuth(cfg *AuthConfig) (*HeaderAuth, error) {
	h := &HeaderAuth{cfg: cfg.Header, roles: cfg.Roles, users: cfg.Users, sessionTTL: defaultSessionTTL}
	if cfg.SessionTTL != "" {
		d, err := time.ParseDuration(cfg.SessionTTL)
		if err != nil {
			return nil, fmt.Errorf("session_ttl %q: %w", cfg.SessionTTL, err)
		}
		h.sessionTTL = d
	}
	for _, c := range cfg.Header.TrustedProxies {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			// A bare address is accepted as a /32 or /128: an operator naming one proxy should
			// not have to know CIDR notation to say so.
			if ip := net.ParseIP(c); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				n = &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
			} else {
				return nil, fmt.Errorf("trusted_proxies: %q is not an address or CIDR", c)
			}
		}
		h.trusted = append(h.trusted, n)
	}
	return h, nil
}

// PrincipalFrom reads the forwarded identity, but only from a peer inside trusted_proxies.
// A request from anywhere else gets NO principal rather than an error: it may still be carrying
// a bearer token another mode will accept, and refusing here would break a fleet that runs both.
func (h *HeaderAuth) PrincipalFrom(r *http.Request) *Principal {
	if !h.trustedPeer(r.RemoteAddr) {
		return nil
	}
	subject := strings.TrimSpace(r.Header.Get(h.cfg.Subject))
	if subject == "" {
		return nil
	}
	var roles []string
	if h.cfg.Roles != "" {
		for _, x := range strings.Split(r.Header.Get(h.cfg.Roles), ",") {
			if x = strings.TrimSpace(x); x != "" {
				roles = append(roles, x)
			}
		}
	}
	return &Principal{Subject: subject, Roles: roles, Grants: h.grantsFor(subject, roles), Source: "header"}
}

// grantsFor maps asserted roles to permissions. `*` applies to any authenticated caller, which
// is how a deployment says "everyone who gets past the proxy may read".
func (h *HeaderAuth) grantsFor(subject string, roles []string) []Grant {
	seen := map[Perm]bool{}
	var out []Grant
	add := func(perms []string) {
		for _, p := range perms {
			if perm := Perm(p); !seen[perm] {
				seen[perm] = true
				out = append(out, Grant{Perm: perm})
			}
		}
	}
	for _, r := range roles {
		add(h.roles[r])
	}
	add(h.users[subject])
	add(h.roles["*"])
	return out
}

func (h *HeaderAuth) trustedPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range h.trusted {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
