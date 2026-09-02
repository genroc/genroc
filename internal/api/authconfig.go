package api

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// The auth configuration file. specs/api-auth.md §4 — a FILE rather than a table, because the
// policy governing an API must not be editable through that API: a `deploy` permission that can
// rewrite the role map is `admin` wearing a disguise. Mounted read-only from a ConfigMap is also
// the k8s-idiomatic shape, and it needs no bootstrapping story.
//
// It configures exactly one thing: how a JWT from the deployment's IdP is verified, and what its
// roles mean here. Machine credentials are `-auth token` and live in the database.
// specs/auth-two-credentials.md.

type AuthConfig struct {
	// Mode must be "jwt". It stays an explicit field rather than being inferred from the `jwt`
	// block, so that an older `mode: header` file fails loudly instead of being read as an
	// absent jwt config that authenticates nobody.
	Mode string        `yaml:"mode"`
	JWT  JWTModeConfig `yaml:"jwt"`
	// Roles maps an asserted role to permissions; `*` applies to any authenticated caller.
	Roles map[string][]string `yaml:"roles"`
	// Users maps a SUBJECT to permissions, unioned with its roles. Groups are the better axis;
	// this is for providers that carry none at all (Dex's staticPasswords, for one). §4.
	Users map[string][]string `yaml:"users"`
}

// JWTModeConfig is `mode: jwt`. Issuer, Audience and Algorithms have no defaults and are
// refused when empty: each is a known way JWT deployments are broken and none is the default in
// most libraries. specs/api-auth.md §2.4.
type JWTModeConfig struct {
	// Exactly one of JWKSURL / JWKSFile. The file is not a lesser option: it is what makes this
	// mode testable without standing up an IdP, and the answer for an air-gapped deployment or
	// for pinning a key rather than trusting a fetch. A mode that can only be exercised
	// end-to-end is one whose edge cases never get a test. specs/api-auth.md §2.4.
	JWKSURL  string `yaml:"jwks_url"`
	JWKSFile string `yaml:"jwks_file"`
	// Issuer is pinned so a second, attacker-chosen issuer with a valid JWKS is not accepted.
	// It is whatever the token's `iss` carries, which for a brokered setup is the FRONT-channel
	// URL and need not be the host JWKSURL is fetched from.
	Issuer string `yaml:"issuer"`
	// Audience is pinned because without it a token the IdP minted for a DIFFERENT application
	// verifies here too -- the same signature, a different intended audience. This is the most
	// common real-world JWT bug and it turns any other app in the tenant into a genroc credential.
	Audience string `yaml:"audience"`
	// Algorithms is pinned to what the issuer actually uses. `alg: none` and RS256->HS256
	// confusion are both live vulnerability classes, and both are configuration, not cryptography.
	Algorithms []string `yaml:"algorithms"`
	// SubjectClaim / RolesClaim default to `sub` and `groups`.
	SubjectClaim string `yaml:"subject_claim"`
	RolesClaim   string `yaml:"roles_claim"`
	// Leeway absorbs clock skew on exp/nbf; empty means 30s. A fixed zero fails on real clusters.
	Leeway string `yaml:"leeway"`
}

// validate enforces §2.4. These are refused at load rather than defaulted, because every
// default that could be chosen here is one that silently accepts more than the operator meant.
func (j JWTModeConfig) validate(path string) error {
	switch {
	case j.JWKSURL == "" && j.JWKSFile == "":
		return fmt.Errorf("%s: jwt needs jwks_url or jwks_file", path)
	case j.JWKSURL != "" && j.JWKSFile != "":
		return fmt.Errorf("%s: jwt takes jwks_url or jwks_file, not both", path)
	case j.Issuer == "":
		return fmt.Errorf("%s: jwt.issuer is required — unpinned, a second issuer with a "+
			"valid JWKS is accepted too", path)
	case j.Audience == "":
		return fmt.Errorf("%s: jwt.audience is required — unpinned, a token your IdP minted "+
			"for a different application verifies here too", path)
	case len(j.Algorithms) == 0:
		return fmt.Errorf("%s: jwt.algorithms is required (e.g. [RS256]) — unpinned, "+
			"`alg: none` and RS256/HS256 confusion are both accepted", path)
	}
	for _, alg := range j.Algorithms {
		if strings.EqualFold(alg, "none") {
			return fmt.Errorf("%s: jwt.algorithms must not contain %q: it accepts unsigned tokens", path, alg)
		}
	}
	return nil
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
	if cfg.Mode != "jwt" {
		// Named rather than generic: `header` was a real mode and its config files exist, so the
		// error has to say what replaced it rather than only that this value is wrong.
		return nil, fmt.Errorf("%s: mode must be \"jwt\" (got %q). genroc verifies JWTs and "+
			"issues its own tokens; it reads no identity headers. See "+
			"specs/auth-two-credentials.md", path, cfg.Mode)
	}
	if err := cfg.JWT.validate(path); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// grantsFor maps a caller's roles and subject to permissions. `*` applies to any authenticated
// caller, which is how a deployment says "everyone the IdP authenticated may read".
//
// specs/api-auth.md §2.3 is why this lives here and not in the IdP: the token says who and what
// group, this says what that may do. An IdP has no idea what `deploy` means in genroc.
func grantsFor(roleMap, userMap map[string][]string, subject string, roles []string) []Grant {
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
		add(roleMap[r])
	}
	add(userMap[subject])
	add(roleMap["*"])
	return out
}
