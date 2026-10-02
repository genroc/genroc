package api

import (
	"context"
	"strings"
	"sync"
	"time"

	"genroc/internal/db"
)

// Authorization: specs/api-auth.md §0, §3, §9. Identity is NOT here — nothing below may ask
// which mode established a Principal.

// Perm is a coarse capability over the API surface. Five, deliberately: a set small enough that
// a reviewer can hold it, and the axis a scoped grant later narrows rather than replaces.
type Perm string

const (
	// PermWorker is the low-trust inbound zone: claim, renew, release, resolve, signal.
	PermWorker Perm = "worker"
	// PermRead is every GET, plus the analyses that write nothing (validate, compat).
	PermRead Perm = "read"
	// PermOperate acts on RUNS: start, pause, resume, retry.
	PermOperate Perm = "operate"
	// PermDeploy changes WHAT RUNS: definitions, channels, upgrade. `upgrade` is here rather
	// than under operate because it changes which version an instance executes.
	PermDeploy Perm = "deploy"
	// PermAdmin is everything, and satisfies every other permission.
	PermAdmin Perm = "admin"
)

// Grant is a permission plus the constraint narrowing it to some resources. Constraint is
// unused in v1 but present, since adding it later means revisiting every call site.
// specs/api-auth.md §3, §9.
type Grant struct {
	Perm Perm
	// Constraint, when set, limits this grant to matching resources. The vocabulary is meant
	// to be the queue's own (process, version, task) — see §9 before inventing another.
	Constraint *GrantConstraint
}

// GrantConstraint is declared but never populated in v1. It is here so the shape of an
// authorization decision is settled before anything depends on it.
type GrantConstraint struct {
	Process string
	Version int
	Task    string
}

// Principal is who is asking and what they may do. Nothing downstream can tell which mode made
// it, which lets several run at once. specs/api-auth.md §2.
type Principal struct {
	Subject string  // who, for the audit trail
	Grants  []Grant // RESOLVED — the only thing an authorization decision reads
	Source  string  // which mode admitted it; for the trail, never for a decision
}

// Actor renders the principal as `source:subject` (`token:ci`) for the audit trail. Nil yields
// "", so an Open action reaching a write path records no actor. specs/api-auth.md section 7.
func (p *Principal) Actor() string {
	if p == nil {
		return ""
	}
	return p.Source + ":" + p.Subject
}

// anonymousAdmin is what `mode: none` produces. Source `no-auth`, not `none`: beside `startup:`
// and `cli:` on a token row, `none:` reads as a missing value.
func anonymousAdmin() *Principal {
	return &Principal{Subject: "anonymous", Grants: []Grant{{Perm: PermAdmin}}, Source: "no-auth"}
}

// Allows: an EMPTY allow list means admin-only, the fail-closed default. A nil Principal is
// refused.
func (p *Principal) Allows(allow []Perm) bool {
	if p == nil {
		return false
	}
	for _, g := range p.Grants {
		if g.Perm == PermAdmin {
			return true
		}
		for _, need := range allow {
			if g.Perm == need {
				return true
			}
		}
	}
	return false
}

// authorize is the ONE gate every transport passes through. It is §3's coarse half; a scoped
// grant's resource half runs later, in the handler that loaded the target.
func authorize(a actionDef, p *Principal) *Error {
	if a.Open {
		return nil
	}
	if p == nil {
		return apiErrf(CodeUnauthenticated, "no identity was established for this request")
	}
	if !p.Allows(a.Allow) {
		return forbidden("%q requires %s", a.Name, describeAllow(a.Allow))
	}
	return nil
}

// describeAllow words what an action needs, for the 403 body. An empty list is admin-only,
// which the message must say plainly — "requires one of []" tells a reader nothing.
func describeAllow(allow []Perm) string {
	if len(allow) == 0 {
		return "the admin permission"
	}
	names := make([]string, len(allow))
	for i, p := range allow {
		names[i] = string(p)
	}
	if len(names) == 1 {
		return "the " + names[0] + " permission"
	}
	return "one of: " + strings.Join(names, ", ")
}

// ── identity ─────────────────────────────────────────────────────────────────────

// Authenticator turns a credential into a Principal; a nil one on the Server is `mode: none`.
// (nil, nil) means "not mine" (401 if nobody claims it); an error means it could not DECIDE.
type Authenticator interface {
	Authenticate(ctx context.Context, credential string) (*Principal, error)
}

// chainAuth takes the first authenticator that recognises the credential. An error stops the
// chain: falling through would turn an outage into a 401 storm. specs/api-auth.md §2.
type chainAuth []Authenticator

// Chain combines identity modes that all read the same bearer credential. One authenticator is
// returned unwrapped, so the common single-mode case carries no indirection.
func Chain(auths ...Authenticator) Authenticator {
	if len(auths) == 1 {
		return auths[0]
	}
	return chainAuth(auths)
}

func (c chainAuth) Authenticate(ctx context.Context, credential string) (*Principal, error) {
	for _, a := range c {
		p, err := a.Authenticate(ctx, credential)
		if err != nil {
			return nil, err
		}
		if p != nil {
			return p, nil
		}
	}
	return nil, nil
}

// TokenLookup is the half of the database token mode needs, kept narrow so the authenticator
// can be tested without one.
type TokenLookup interface {
	LookupToken(ctx context.Context, secret string) (db.APIToken, bool, error)
	TouchToken(ctx context.Context, id string, at int64) error
}

// touchInterval throttles the last-used write, which sits on the READ path: one write per
// token per minute rather than per request.
const touchInterval = time.Minute

// TokenAuth is `mode: token` — genroc's own credentials, hashed in the database. §5.
type TokenAuth struct {
	store TokenLookup
	now   func() time.Time

	// lastTouch is per-token throttle state, owned by this struct rather than living at
	// package level: it is per-server state with a lifetime, not a lookup table.
	mu        sync.Mutex
	lastTouch map[string]time.Time
}

func NewTokenAuth(store TokenLookup) *TokenAuth {
	return &TokenAuth{store: store, now: time.Now, lastTouch: map[string]time.Time{}}
}

func (a *TokenAuth) Authenticate(ctx context.Context, credential string) (*Principal, error) {
	if credential == "" {
		return nil, nil
	}
	tok, ok, err := a.store.LookupToken(ctx, credential)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	grants := make([]Grant, 0, len(tok.Perms))
	for _, p := range tok.Perms {
		grants = append(grants, Grant{Perm: Perm(p)})
	}
	a.touch(ctx, tok.ID)
	// The id alone, not "token:"+id: Actor() prefixes the source, and a labelless token would
	// otherwise be attributed as `token:token:<id>`.
	subject := tok.Label
	if subject == "" {
		subject = tok.ID
	}
	return &Principal{Subject: subject, Grants: grants, Source: "token"}, nil
}

// touch records use, at most once per touchInterval per token, and never fails the request:
// a credential that authenticated must not be refused because a bookkeeping write did not land.
func (a *TokenAuth) touch(ctx context.Context, id string) {
	now := a.now()
	a.mu.Lock()
	last, seen := a.lastTouch[id]
	if seen && now.Sub(last) < touchInterval {
		a.mu.Unlock()
		return
	}
	a.lastTouch[id] = now
	a.mu.Unlock()
	_ = a.store.TouchToken(ctx, id, now.UnixMilli())
}

// bearerToken treats a non-Bearer header as absent, not rejected, so a proxy adding its own
// scheme cannot lock a caller out.
func bearerToken(header string) string {
	const scheme = "Bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(header[len(scheme):])
}
