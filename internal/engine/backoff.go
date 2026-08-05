package engine

import (
	"math/rand/v2"
	"time"

	"genroc/internal/model"
)

// retryDelay returns how long to park before retry number `attempt` (1-based, so the first
// retry waits the policy's base delay exactly).
func (e *Engine) retryDelay(attempt int, r model.Retry) time.Duration {
	if e.immediateRetries {
		return 0
	}
	return backoff(attempt, r.Base(), r.Growth(), r.Ceiling())
}

// backoff grows base by factor, clamps at ceiling, then jitters within the upper half.
// Jitter stops a fleet re-hitting a recovering endpoint in lockstep, and only ever
// SHORTENS — the ceiling stays true and clock-advancing tests still expire timers.
func backoff(attempt int, base time.Duration, factor float64, ceiling time.Duration) time.Duration {
	// Accumulating in float64 rather than shifting a Duration: a wrapped or negative
	// Duration is a retry with no backoff at all, which is the one outcome worse than a
	// wait that is too long. Growth stops at the ceiling, so the loop cannot run away and
	// the multiply cannot reach the range where the conversion below overflows.
	d := float64(base)
	limit := float64(ceiling)
	// Guarded on factor, not just on the ceiling: factor 1 is a constant delay, and
	// without this an author's large attempt count would spin the loop to no effect.
	if factor > 1 {
		for i := 1; i < attempt && d < limit; i++ {
			d *= factor
		}
	}
	if d >= limit {
		d = limit
	}
	nominal := time.Duration(d)
	return nominal/2 + time.Duration(rand.Int64N(int64(nominal/2)+1))
}
