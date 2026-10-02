// Package idgen mints every id genroc stores: an injective scatter of a (worker, counter) pair,
// unique by construction. THE CONSTANTS CAN NEVER CHANGE: ids on disk came from this map.
// An id sorts as nothing (`seq` orders rows), leads with a digit, and excludes `.` and I/L/O/U.
package idgen

import (
	"fmt"
	"math/bits"
	"sync/atomic"
)

const (
	// Feistel rounds. Four is the usual floor for a permutation that looks unstructured; the
	// round constants are arbitrary and chosen for spread, not secrecy.
	rounds = 4

	// Both hold the id WIDTH still. minValue is an offset, not padding: "0"+<narrower id> would
	// collide with an id that starts with one. minGroups fixes the counter's height below 32768.
	minChars  = 8
	minValue  = 10 * (1<<(5*(minChars-1)) - 1) / 31
	minGroups = 3
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

// Stream returns an independent counter under the same worker. Ids from two streams can be
// EQUAL: an id resolves only with its table, and object_refs keys by owner kind.
func (m *Minter) Stream() *Minter { return &Minter{worker: m.worker} }

// Next returns a never-minted id and its counter (a row's `seq`). The counter lives in memory,
// safe only because every Minter gets a fresh worker number.
func (m *Minter) Next() (string, int64) {
	n := m.counter.Add(1)
	return render(scatter(pair(m.worker, n))), int64(n)
}

// pair codes the worker SUFFIX-free below the counter, injective without bounding either. A
// fixed field would be a cliff: past it a process could not start at all.
func pair(worker, counter uint64) uint64 {
	var low, shift uint64
	for {
		group := worker & 31 << 1
		if worker >>= 5; worker > 0 {
			group |= 1
		}
		low |= group << shift
		shift += 6
		if worker == 0 {
			break
		}
	}
	shift = max(shift, 6*minGroups)
	if shift >= 64 || counter >= 1<<(64-shift)-1 {
		panic("idgen: the worker and counter no longer pair into 64 bits")
	}
	return minValue + (counter<<shift | low)
}

// scatter permutes within v's own width, keeping the map injective. Feistel, not a multiply:
// a multiply carries the worker stride through, so every id in a run shares its tail.
func scatter(v uint64) uint64 {
	lo, size := widthOf(v)

	// Cycle-walking maps Feistel's power-of-two excess back in, keeping a bijection on [0, size).
	half := (bits.Len64(size-1) + 1) / 2
	x := v - lo
	for {
		x = feistel(x, uint(half))
		if x < size {
			return lo + x
		}
	}
}

func feistel(x uint64, half uint) uint64 {
	mask := uint64(1)<<half - 1
	l, r := x>>half&mask, x&mask
	for i := range rounds {
		l, r = r, l^(mix(r+uint64(i)*0x9e3779b97f4a7c15)&mask)
	}
	return l<<half | r
}

// mix is splitmix64's finalizer: cheap, and it moves every input bit into the high ones.
func mix(z uint64) uint64 {
	z ^= z >> 30
	z *= 0xbf58476d1ce4e5b9
	z ^= z >> 27
	z *= 0x94d049bb133111eb
	return z ^ z>>31
}

// widthOf: k characters hold 10*32^(k-1) values. The ranges partition the numbers, so
// scattering inside one cannot change an id's length.
func widthOf(v uint64) (lo, size uint64) {
	lo, size = 0, 10
	for v >= lo+size {
		lo, size = lo+size, size*32
	}
	return lo, size
}

func render(v uint64) string {
	lo, size := widthOf(v)
	n := v - lo
	// Most significant first. The leading place is worth 10 rather than 32, so the digit it
	// emits is always one of the alphabet's first ten symbols.
	out := make([]byte, 0, 8)
	for size /= 10; ; size /= 32 {
		out = append(out, alphabet[n/size%32])
		n %= size
		if size == 1 {
			return string(out)
		}
	}
}
