package db

import "errors"

// The persistence layer's two classification sentinels. They exist so a caller can
// branch on *what kind* of failure it got without re-deriving it from a message or
// from sql.ErrNoRows — in particular so the API layer can map a failure to an HTTP
// status in one place instead of at every call site.
//
// Wrap with %w and keep the human wording in the prefix:
//
//	fmt.Errorf("definition %q v%d: %w", name, version, ErrNotFound)
//
// Anything not wrapped in one of these is an internal failure by default; that is the
// classification the API layer applies, and it is deliberately the pessimistic one.
var (
	// ErrNotFound means the row a caller named does not exist. It is deliberately
	// distinct from sql.ErrNoRows: an empty scan is a driver-level fact, and only
	// *some* empty scans mean "you asked for something that isn't there". The rest
	// are ordinary control flow — an absent parent in FinishChild, an empty signal
	// queue in ArmExternalOrConsumeSignal — and those must keep testing
	// errors.Is(err, sql.ErrNoRows) instead of being promoted to this.
	ErrNotFound = errors.New("not found")

	// ErrConflict means the request is well-formed and the target exists, but its
	// current state does not admit the operation — resuming a process that is not
	// paused, retrying one that is not failed, signalling one that has settled.
	// Distinct from a bad request: the same call may succeed later without changing.
	ErrConflict = errors.New("conflict")

	// ErrInvalid means the arguments are wrong independently of any state, so the
	// same call will never succeed — naming a descendant where a tree root is
	// required, for instance. The contrast with ErrConflict is exactly "retrying
	// this is pointless" vs "retrying this may work later".
	ErrInvalid = errors.New("invalid argument")

	// ErrLeaseLost means a fenced write matched no row: the lease grant (lease_epoch)
	// the advance was operating under has been superseded by a newer claim. The whole
	// transaction rolls back with it, so a lost lease never leaks partial effects.
	// The caller must DROP its outcome — writing a failure instead would be exactly
	// the clobber the fence prevents. See specs/lease-fencing.md.
	ErrLeaseLost = errors.New("lease lost")
)
