package api

import (
	"context"
	"time"
)

// healthPingTimeout bounds the readiness check's database round-trip. Short on purpose: a
// probe that hangs is indistinguishable from one that fails, and the supervisor's own
// probe deadline is the only thing left to notice.
const healthPingTimeout = 2 * time.Second

// health answers the readiness probe. The verdict is exactly one question — can this worker
// reach its database — because that is the only failure the *caller* can act on by routing
// elsewhere. LeaseAgeMs is reported rather than judged: it grows through a transient blip
// the engine recovers from by itself (leaseGate repairs and backs off), so failing the
// probe on it would restart workers that were about to be fine.
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
