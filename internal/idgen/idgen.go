// Package idgen mints every id genroc stores: instances, log rows, buffered signals, API tokens.
// One opaque token, eight characters and widening only when the numbers behind it do: `6fah8w2p`.
//
// It is a (worker, counter) pair, scattered. The worker number comes from a database counter that
// only increases (db.open), so the pair is unique by construction -- no randomness, no clock, and
// neither number bounded -- and every step is INJECTIVE: a suffix-free pairing, a Feistel
// permutation, a positional rendering. Two mints cannot meet the way two hashes can.
//
// THE CONSTANTS CAN NEVER CHANGE. Ids on disk came from this exact map, and a different one can
// land on them. Scattering hides the pair and nothing more -- the constants are right here, and
// an id is not secret-grade (specs/api-auth.md).
//
// Three properties the rendering carries, each load-bearing elsewhere:
//
//   - It SORTS AS NOTHING. Ordering rows that share a millisecond is `seq`'s job (migration 042).
//   - The leading character is a DIGIT, so a process name cannot be mistaken for an id where
//     `upgrade` and `compat` read either in one positional -- `catcher` is otherwise a fine id.
//   - No `.`, which would split an external task's `<instance-id>.<task_epoch>` token, and no
//     I, L, O or U, so an id survives being read aloud.
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

	// Both hold the id WIDTH still, which is worth more than the characters they cost: one
	// process gets 1.3M ids before its widen, and the floor does not move for 32k process
	// starts (a restart every ten minutes for seven months).
	//
	// minValue is the first value that renders minChars wide, added to every pair so none is
	// narrower -- without it a fresh install's ids widen four times inside 42k mints. An offset
	// and not a padded string: render's leading character is base 10 and may be `0`, so
	// "0"+<narrower id> would collide with an id that starts with one.
	//
	// minGroups pads the worker's code so the counter sits at a fixed height for every worker
	// under 32768; without it the width drops 64-fold each time the worker grows a group.
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

// Stream returns an independent counter under the same worker number, so one kind of row does not
// run another's numbers up -- an instance follows the previous instance, not the fifteen log rows
// written between them.
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
	return render(scatter(pair(m.worker, n))), int64(n)
}

// pair packs the two numbers into one, the worker in the low bits as 6-bit groups (five of
// value, one continuation) and the counter above them. The code is SUFFIX-free, so the two can
// always be told apart reading from the bottom -- which is what makes the pairing injective
// without bounding either of them. A fixed field would have been a cliff instead: past it a
// process could not start at all.
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

// scatter permutes v inside the base32 width it already has, so consecutive pairs render as
// unrelated ids of the same length. Widths do not overlap and each permutation is bijective, so
// the whole map is injective -- the property ids need and hashes lack.
//
// A Feistel network, not a multiply: consecutive pairs differ by 2^workerBits, and multiplying
// by a constant carries that stride straight through, leaving every id in a run sharing its
// tail. Feistel is invertible whatever its round function, so non-linearity costs nothing.
func scatter(v uint64) uint64 {
	lo, size := widthOf(v)

	// Feistel needs a power-of-two domain; cycle-walking maps the excess back in, which keeps
	// the whole thing a bijection on [0, size). The domain is under 32/31 of size, so it walks
	// about 1.03 times on average.
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

// widthOf returns the range v renders in: k characters hold 10*32^(k-1) values, the leading one
// being a digit. The ranges partition the numbers, so a value's width is fixed by its magnitude
// and scattering inside one cannot change an id's length.
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
