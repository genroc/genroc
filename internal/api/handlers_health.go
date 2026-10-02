package api

import (
	"context"
	"time"
)

// healthPingTimeout is short: a probe that hangs is indistinguishable from one that fails.
const healthPingTimeout = 2 * time.Second

// health answers only "can this worker reach its database". LeaseAgeMs is reported, never
// judged (CLAUDE.md).
func (h *Handlers) health() Reply {
	ctx, cancel := context.WithTimeout(context.Background(), healthPingTimeout)
	defer cancel()

	if err := h.db.Ping(ctx); err != nil {
		return errReply(unavailable("database unreachable: %w", err))
	}
	return okReply(HealthResp{
		Status:     "ok",
		Worker:     h.engine.WorkerID(),
		Database:   h.db.Dialect(),
		LeaseAgeMs: h.engine.LeaseAge().Milliseconds(),
		ManualTick: h.engine.ManualTick(),
	})
}
