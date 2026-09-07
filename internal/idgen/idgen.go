// Package idgen mints every id genroc stores: instances, log rows, buffered signals, API tokens.
// `<worker>-<counter>` in Crockford base32 -- `01-0001` on a fresh install, `4w8-1yjpx80` a year
// in. The worker number comes from a database counter that only increases (db.open), so an id is
// unique by construction: no randomness, no clock, and nothing sized in advance.
//
// An id SORTS AS NOTHING. A run at one width sorts by accident and inverts as soon as either half
// grows a digit; ordering rows that share a millisecond is `seq`'s job (migration 042). The
// alphabet drops I, L, O and U so an id survives being read aloud, and it never contains `.` --
// an external task's token is `<instance-id>.<task_epoch>`, cut at the first one.
package idgen

import (
	"fmt"
	"slices"
	"sync/atomic"
)

// Minimum widths, not fixed ones: 1024 process starts and a million ids per process fit without
// growing, and neither half has a ceiling.
const (
	workerMin  = 2
	counterMin = 4
)

const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// Minter is one counter in one process's id space. Construct it with NewMinter, and Stream for
// each further sequence sharing that worker number.
type Minter struct {
	worker  uint64
	counter atomic.Uint64
}

func NewMinter(worker int64) (*Minter, error) {
	if worker < 0 {
		return nil, fmt.Errorf("worker number %d is negative; ids in that namespace would not be "+
			"this process's own", worker)
	}
	return &Minter{worker: uint64(worker)}, nil
}

// Stream returns an independent counter under the same worker number, so one kind of row does not
// run another's numbers up -- an instance is `01-0002` after the previous one, not `01-000g`
// because fifteen log rows landed in between.
//
// Ids from two streams can therefore be EQUAL. Nothing resolves a bare id without knowing its
// table; the one place kinds meet is object_refs, keyed (hash, owner_kind, owner_id).
func (m *Minter) Stream() *Minter { return &Minter{worker: m.worker} }

// Next returns an id no other process can mint and this one has not minted before, with the
// counter behind it -- what a row stores in `seq` when its order has to survive a millisecond it
// shares with another row.
//
// The counter starts at zero on every construction BECAUSE the worker number is fresh on every
// one, which is what makes it safe to keep in memory.
func (m *Minter) Next() (string, int64) {
	n := m.counter.Add(1)
	if n == 0 {
		panic("idgen: the mint counter wrapped") // 2^64 mints; wrapping reissues ids in silence
	}
	return base32(m.worker, workerMin) + "-" + base32(n, counterMin), int64(n)
}

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
