package engine

import (
	"time"

	"genroc/internal/model"
)

// readAttempts / readRetryDelay bound how long a read inside an advance keeps trying. Short on
// purpose: an outage takes care of itself -- persist fails too, the lease expires and another
// worker retries the whole advance -- so the retries only have to cover the window where the read
// fails and the write would have succeeded.
const (
	readAttempts   = 3
	readRetryDelay = 50 * time.Millisecond
)

// retryRead re-runs a database read inside an advance a few times before believing it. A blip and
// a real fault (a dangling reference, a malformed column) cannot be told apart by inspecting a
// driver error, so this asks the question instead of guessing: what survives every attempt fails
// the instance loudly. Abandoning the advance would livelock on a real fault, and killing the
// worker takes every other in-flight advance down with it. See internal/engine/CLAUDE.md.
func retryRead[T any](read func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for attempt := range readAttempts {
		if v, err = read(); err == nil {
			return v, nil
		}
		if attempt < readAttempts-1 {
			time.Sleep(readRetryDelay)
		}
	}
	return v, err
}

// bufferedSignal packs PeekSignal's three results so retryRead can carry them.
type bufferedSignal struct {
	id      string
	outcome model.ExternalOutcome
	ok      bool
}
