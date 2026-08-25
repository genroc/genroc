package engine

import (
	"time"

	"genroc/internal/model"
)

// readAttempts / readRetryDelay bound how long a read inside an advance keeps trying. Short on
// purpose: this exists to ride out a dropped connection, not an outage. A longer outage takes
// care of itself -- the terminal outcome the failure produces cannot be WRITTEN either, so
// persist fails, nothing is recorded, the lease expires and another worker retries the whole
// advance. The retries only have to cover the window where the read fails and the write would
// have succeeded.
const (
	readAttempts   = 3
	readRetryDelay = 50 * time.Millisecond
)

// retryRead re-runs a database read inside an advance a few times before believing it.
//
// A read failure has two causes and they need opposite handling: a blip (retrying works) or a
// real fault -- a dangling object reference, a malformed column, a definition that is gone
// (retrying fails identically). Driver errors cannot be told apart by inspection, so this asks
// the question instead of guessing at it. What survives every attempt is treated as real, and
// fails the instance loudly with a reason.
//
// Why not the alternatives: abandoning the advance livelocks on a real fault, and killing the
// worker needs the same classification this avoids plus it takes every other in-flight advance
// down with it (see internal/engine/CLAUDE.md, and specs/external-outcome-as-signal.md for how
// this came up). database/sql discards a bad pooled connection and dials a new one, so attempt
// two is already on a fresh socket -- a process restart buys nothing a retry does not.
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
