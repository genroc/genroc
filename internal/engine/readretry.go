package engine

import (
	"time"

	"genroc/internal/model"
)

// Short on purpose: a longer outage fails persist too, and another worker retries the whole
// advance. CLAUDE.md.
const (
	readAttempts   = 3
	readRetryDelay = 50 * time.Millisecond
)

// retryRead re-runs a read before believing it: a blip and a real fault look alike, so what
// survives every attempt fails the instance loudly. CLAUDE.md.
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
