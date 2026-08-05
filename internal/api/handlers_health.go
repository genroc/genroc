package api

import (
	"context"
	"time"
)

// healthPingTimeout bounds the readiness check's database round-trip. Short on purpose: a
// probe that hangs is indistinguishable from one that fails, and the supervisor's own
// probe deadline is the only thing left to notice.
const healthPingTimeout = 2 * time.Second

// health answers exactly one question — can this worker reach its database — the only
// failure the caller can act on by routing elsewhere. LeaseAgeMs is reported, never
// judged: it grows through blips the gate recovers from; failing on it restarts workers
// that were about to be fine.
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
