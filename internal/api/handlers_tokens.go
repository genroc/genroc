package api

import (
	"context"
	"encoding/json"
	"time"
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
	tok, err := h.db.MintToken(context.Background(), req.Label, perms)
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
