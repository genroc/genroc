// Package idgen mints every id genroc stores: instances, log rows, buffered signals, API
// tokens. `<worker>-<counter>` in Crockford base32 -- `01-0001` on a fresh install,
// `04w8-1yjpx80` a year in. No randomness and no clock: a worker number allocated once per
// process (db.open, from a counter that only increases) plus a counter within it is unique by
// construction.
//
// Each half is padded to a MINIMUM width and then grows. The padding buys nothing but a stable
// column and ids that look alike -- it is affordable precisely because the id carries no
// ordering, so the width may change whenever the numbers outgrow it.
//
// An id SORTS AS NOTHING: `01-000z` follows `01-0010` as text, and a run that crosses a width
// boundary inverts outright.
// That is deliberate. Ordering a log trail inside a millisecond, where created_at cannot
// separate two rows, is `process_logs.seq`'s job (migration 042): the counter beside the id
// rather than smuggled through its rendering, which is what lets the id be this short.
//
// The alphabet drops I, L, O and U, so an id survives being read aloud or copied off a screen.
// The separator is `-` and never `.`: an external task's token is `<instance-id>.<task_epoch>`
// and parses by cutting at the first dot.
package idgen

import (
	"fmt"
	"slices"
	"sync/atomic"
)

const (
	workerBits = 24
	// Sized so overflow is unreachable rather than distant: log rows dominate the count, and
	// at 10k ids/second -- more than the database sustains -- this is 223 years of one process
	// running without a restart. 36 bits was 80 days at that rate, which is a deadline, not a
	// bound.
	counterBits = 46

	MaxWorker  = 1<<workerBits - 1
	maxCounter = 1<<counterBits - 1

	// Minimum widths, not fixed ones: 1024 process starts and a million ids per process fit
	// without growing, which covers a development session and most of a small deployment.
	workerMin  = 2
	counterMin = 4
)

// Crockford base32: no I, L, O or U, so a mistyped id is a refusal rather than another row.
const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// Minter is one process's id space. The zero value is not usable; construct it with NewMinter,
// which is called once per open database (the worker number comes from there).
type Minter struct {
	worker  uint64
	counter atomic.Uint64
}

// NewMinter fails rather than wrapping when the counter outgrows the field: a wrapped worker
// number would silently mint ids another process already owns, which is the one failure this
// scheme exists to rule out.
func NewMinter(worker int64) (*Minter, error) {
	if worker < 0 || worker > MaxWorker {
		return nil, fmt.Errorf("worker number %d is outside the %d-bit field ids reserve for it; "+
			"the id_counters row has been incremented %d times", worker, workerBits, worker)
	}
	return &Minter{worker: uint64(worker)}, nil
}

// Next returns an id no other process can mint and this one has not minted before, with the
// counter behind it -- what a row stores in a `seq` column when its order has to survive a
// millisecond it shares with another row.
//
// The counter starts at zero on every construction BECAUSE the worker number is fresh on every
// one: a restart gets a new namespace rather than resuming an old one, which is what makes the
// counter safe to keep in memory.
func (m *Minter) Next() (string, int64) {
	n := m.counter.Add(1)
	if n > maxCounter {
		// 68 billion ids into one process: reachable only by a mint loop, and wrapping would
		// hand back ids this process already used. Nothing sane recovers, so say what happened.
		panic(fmt.Sprintf("idgen: worker %d has minted %d ids, past the %d-bit counter",
			m.worker, n, counterBits))
	}
	return base32(m.worker, workerMin) + "-" + base32(n, counterMin), int64(n)
}

// base32 renders v, left-padded to at least min digits.
func base32(v uint64, min int) string {
	var out []byte
	for v > 0 {
		out = append(out, alphabet[v&31])
		v >>= 5
	}
	for len(out) < min {
		out = append(out, '0')
	}
	slices.Reverse(out)
	return string(out)
}
