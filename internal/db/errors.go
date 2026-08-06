package db

import "errors"

// Classification sentinels: callers branch on the KIND of failure, and the API maps kinds
// to HTTP statuses in one place. Wrap with %w, keeping the human wording in the prefix.
// Anything unwrapped classifies as internal — deliberately the pessimistic default.
var (
	// ErrNotFound means the row a caller named does not exist. Deliberately distinct from
	// sql.ErrNoRows: only *some* empty scans mean that — an absent parent in FinishChild or an
	// empty signal queue is control flow, and those must keep testing sql.ErrNoRows.
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
	// was superseded and the whole transaction rolled back. The caller must DROP its
	// outcome — a failure write would be the clobber itself. specs/lease-fencing.md.
	ErrLeaseLost = errors.New("lease lost")
)
