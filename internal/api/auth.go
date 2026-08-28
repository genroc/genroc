package api

import "strings"

// Authorization. specs/api-auth.md — §0 for why genroc owns this rather than delegating it to
// an ingress, §3 for the permission set, §9 for the scoped grants this shape must not foreclose.
//
// Identity is NOT here: a Principal arrives already established, by whatever mode the
// deployment configured, and nothing below may ask which mode produced it.

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

// Grant is a permission a principal holds, with room for the constraint that narrows it to a
// subset of resources. Constraint is unused in v1 and the field exists anyway: a bare Perm
// cannot express "resolve tasks in `approval`", and adding the field later means revisiting
// every call site. specs/api-auth.md §3, §9.
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

// Principal is who is asking, resolved to what they may do. Every identity mode produces one
// and nothing downstream can tell them apart — which is what lets a deployment run several at
// once. specs/api-auth.md §2.
type Principal struct {
	Subject string   // who, for the audit trail
	Roles   []string // as asserted by an IdP; empty for a genroc token
	Grants  []Grant  // RESOLVED — the only thing an authorization decision reads
	Source  string   // which mode admitted it; for the trail, never for a decision
}

// anonymousAdmin is the principal `mode: none` produces. It is the pre-auth behaviour written
// down rather than a special case in the check: with auth off every caller is an operator, and
// the startup warning (not this) is what says so out loud.
func anonymousAdmin() *Principal {
	return &Principal{Subject: "anonymous", Grants: []Grant{{Perm: PermAdmin}}, Source: "none"}
}

// Allows reports whether this principal may take an action admitted by any of `allow`.
//
// An EMPTY allow list means admin-only. That is the fail-closed default: an endpoint added to
// the registry without a permission is closed rather than open, which is the direction a
// mistake here has to fall. `internal/api/CLAUDE.md` states the rule; TestEveryActionDeclares-
// APermission is what stops it being reached by accident rather than by intent.
//
// A nil Principal is refused. Callers that have not established one yet must not reach here.
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

// authorize is the ONE gate every transport passes through, so a mode wired into HTTP cannot
// leave TCP and UDS open. It answers the coarse half of §3's two-phase check — "does this
// principal hold the permission at all" — and a scoped grant's resource half runs later, inside
// the handler that loaded the target.
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
