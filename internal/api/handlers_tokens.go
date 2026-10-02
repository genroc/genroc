package api

import (
	"context"
	"encoding/json"
	"time"
)

// Token management, all admin-gated: minting grants access, and a listing maps credentials.
// `genroc token` is the same set against the database. specs/api-auth.md §5.

func (h *Handlers) createToken(raw json.RawMessage, actor string) Reply {
	req, err := decodeBody[CreateTokenReq](raw)
	if err != nil {
		return errReply(err)
	}
	perms, err := validPerms(req.Perms)
	if err != nil {
		return errReply(err)
	}
	// 0: a machine credential does not expire — rotating a worker token is a deploy, not a
	// clock.
	tok, err := h.db.MintToken(context.Background(), req.Label, perms, 0, actor)
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
			Actor:      r.Actor,
			RevokedBy:  r.RevokedBy,
		})
	}
	return okReply(map[string]any{"items": out})
}

func (h *Handlers) revokeToken(id string, actor string) Reply {
	if id == "" {
		return invalid("id is required").reply()
	}
	if err := h.db.RevokeToken(context.Background(), id, actor); err != nil {
		return errReply(err)
	}
	return okReply(map[string]any{"revoked": true})
}

// validPerms refuses an unknown permission rather than dropping it: a typo would grant less,
// discovered as a 403 elsewhere.
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

// millisTime returns "" for zero so `omitempty` drops it: a never-used token must not show
// 1970.
func millisTime(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
