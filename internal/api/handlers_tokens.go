package api

import (
	"context"
	"encoding/json"
	"time"

	"genroc/internal/db"
)

// Token management over the API. specs/api-auth.md §5.
//
// Every action here is admin-gated: minting a token is granting access, and listing them tells
// an attacker which credentials exist and what they can reach. `genroc token` is the same set
// against the database, for when the API is not reachable (§5.3).

func (h *Handlers) createToken(raw json.RawMessage) Reply {
	req, err := decodeBody[CreateTokenReq](raw)
	if err != nil {
		return errReply(err)
	}
	perms, err := validPerms(req.Perms)
	if err != nil {
		return errReply(err)
	}
	// 0: a machine credential does not expire. Rotating a worker token is a deploy, not a clock,
	// and a fleet that starts failing at 3am because a token lapsed is worse than one that keeps
	// working until someone revokes it.
	tok, err := h.db.MintToken(context.Background(), req.Label, perms, 0)
	if err != nil {
		return errReply(err)
	}
	return okReply(CreateTokenResp{ID: tok.ID, Token: tok.Secret, Label: tok.Label, Perms: tok.Perms})
}

func (h *Handlers) listTokens() Reply {
	rows, err := h.db.ListTokens(context.Background())
	if err != nil {
		return errReply(err)
	}
	out := make([]TokenResp, 0, len(rows))
	for _, r := range rows {
		out = append(out, TokenResp{
			ID: r.ID, Label: r.Label, Perms: r.Perms,
			CreatedAt:  millisTime(r.CreatedAt),
			LastUsedAt: millisTime(r.LastUsedAt),
			RevokedAt:  millisTime(r.RevokedAt),
			ExpiresAt:  millisTime(r.ExpiresAt),
		})
	}
	return okReply(map[string]any{"items": out})
}

func (h *Handlers) revokeToken(id string) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	if err := h.db.RevokeToken(context.Background(), id); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"revoked": true})
}

// validPerms refuses an unknown permission rather than dropping it. A token minted with a typo
// would grant less than the caller asked for, and they would discover it from a 403 somewhere
// unrelated.
func validPerms(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, invalid("perms is required (admin, deploy, operate, read, worker)")
	}
	known := map[Perm]bool{PermAdmin: true, PermDeploy: true, PermOperate: true, PermRead: true, PermWorker: true}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if !known[Perm(p)] {
			return nil, invalid("unknown permission %q; valid: admin, deploy, operate, read, worker", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// millisTime renders a stored timestamp as RFC3339, matching every other response, and returns
// "" for zero so `omitempty` drops the field. A never-used token showing 1970 would read as a
// date rather than as an absence.
func millisTime(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// mintSessionToken issues the browser a bearer token carrying exactly the permissions the
// proxy's identity resolved to. specs/api-auth.md §9.
//
// A real token row, not a signed blob, so the everyday properties hold: it is listable,
// revocable, and attributable like any other. It is labelled by subject so an operator reading
// `genctl token list` can tell a person's session from a machine's credential.
func (h *Handlers) mintSessionToken(ctx context.Context, p *Principal, ttl time.Duration) (db.APIToken, error) {
	perms := make([]string, 0, len(p.Grants))
	for _, g := range p.Grants {
		perms = append(perms, string(g.Perm))
	}
	if len(perms) == 0 {
		// The proxy authenticated them and the role map gives them nothing. Minting an empty
		// token would produce 403s on every call with no clue why; saying so here names the
		// actual fix, which is a `roles:` entry.
		return db.APIToken{}, forbidden("%q maps to no permissions; add a roles entry for %v",
			p.Subject, p.Roles)
	}
	// Expires, unlike a machine token. The exchange cannot return a token it issued before --
	// only the hash is stored -- so every page load mints, and a permanent one would accumulate
	// live admin credentials for the life of the deployment.
	return h.db.MintToken(ctx, "session:"+p.Subject, perms, db.Now().Add(ttl).UnixMilli())
}
