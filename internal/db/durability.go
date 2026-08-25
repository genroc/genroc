package db

import (
	"context"
	"fmt"

	dbgen "genroc/internal/db/gen"
)

// Durability is the ladder from specs/durability-levels.md §5, ordered weakest to
// strongest. It is read two ways, and the inversion is deliberate: an operator picks a
// LEVEL (a ceiling they lower to buy throughput), while each write declares a FLOOR (the
// weakest level at which it still fsyncs). A write syncs when level >= floor.
type Durability int

const (
	// DurabilityOnlyOnce keeps the guarantees that cannot be replayed: work handed in
	// from outside is never forgotten, and an only_once task never runs twice. Ordinary
	// task writes are not flushed, so a power cut replays them — which is what
	// at-least-once already sells.
	DurabilityOnlyOnce Durability = iota
	// DurabilityTerminal adds: a finished process stays finished. It is what stops a
	// poller seeing `completed` and then `running` again after a power cut.
	DurabilityTerminal
	// DurabilityStrict adds: no completed task ever repeats. Every commit is flushed.
	DurabilityStrict
)

// Floors a write declares. syncAlways is DurabilityOnlyOnce because it is the weakest
// level: `level >= syncAlways` holds everywhere, so it fsyncs whatever the operator chose.
// It is also the zero value, which is the point — a write that declares no floor gets it,
// so forgetting to classify a new write path costs throughput and never a guarantee.
const (
	syncAlways   = DurabilityOnlyOnce
	syncTerminal = DurabilityTerminal
	syncStrict   = DurabilityStrict
)

// syncs reports whether a write declaring `floor` must be flushed at this level.
func (d Durability) syncs(floor Durability) bool { return d >= floor }

func (d Durability) String() string {
	switch d {
	case DurabilityOnlyOnce:
		return "only-once"
	case DurabilityTerminal:
		return "terminal"
	case DurabilityStrict:
		return "strict"
	}
	return fmt.Sprintf("Durability(%d)", int(d))
}

// ParseDurability maps the operator-facing name onto a level. The names are the ladder's,
// not the engines': what `strict` costs differs per engine and per disk, and the guarantee
// is what an operator is choosing between.
func ParseDurability(s string) (Durability, error) {
	switch s {
	case "only-once":
		return DurabilityOnlyOnce, nil
	case "terminal":
		return DurabilityTerminal, nil
	case "strict":
		return DurabilityStrict, nil
	case "":
		// Not the CLI default (that is only-once, supplied explicitly by the flag). The
		// empty string reaches here only from a programmatic caller that named no level,
		// and an unnamed level must be the safe one.
		return DurabilityStrict, nil
	}
	return 0, fmt.Errorf("invalid durability %q (want only-once, terminal, or strict)", s)
}

// SetDurability sets the level every write is measured against. Call once at startup,
// before the engine runs: it is read on every write path and never re-read from config.
func (db *DB) SetDurability(d Durability) { db.durability.Store(int64(d)) }

func (db *DB) level() Durability { return Durability(db.durability.Load()) }

// instanceWriteFloor derives a write's floor from what it writes rather than from which
// caller made it: a terminal status IS the "a finished process stays finished" guarantee,
// so a new terminal status is classified correctly without anyone remembering to.
func instanceWriteFloor(status interface{ Terminal() bool }) Durability {
	if status.Terminal() {
		return syncTerminal
	}
	return syncStrict
}

// Flush makes every commit made so far durable, whatever level the write paths ran at.
// Both engines append to one WAL, so one flushed commit hardens the commits behind it
// (specs/durability-levels.md s3) -- which is why writing an otherwise meaningless row is
// enough, and why the caller does not have to name what it wants hardened.
//
// The only_once bracket is the caller: the claim is a task's at-most-once evidence and has
// to outlive a power cut before the request leaves, and the result has to outlive one after
// it comes back. At `strict` every commit is already flushed and this is a no-op.
func (db *DB) Flush(ctx context.Context) error {
	if db.level() == DurabilityStrict {
		return nil
	}
	err := db.withTxAt(ctx, syncAlways, func(qtx *dbgen.Queries, exec dbgen.DBTX) error {
		if db.dialect == "postgres" {
			// Assigning an XID is what makes the commit real, and a real commit at
			// synchronous_commit=on flushes. No row is written, so concurrent workers do not
			// queue behind one another on a shared row held across the fsync -- which is
			// exactly what a marker row would cost here, plus a dead tuple per flush.
			_, err := exec.ExecContext(ctx, "SELECT pg_current_xact_id()")
			return err
		}
		// SQLite has no equivalent: a page has to change for there to be anything to flush.
		// Its single writer already serialises every commit, so a shared row costs nothing.
		return qtx.BumpDurabilityMarker(ctx)
	})
	if err == nil {
		db.flushes.Add(1)
	}
	return err
}

// FlushCount is how many times Flush has committed on this process's DB handle. It is the
// observable for the only_once bracket, and it is kept in memory rather than read from
// durability_marker so it means the same thing on both engines -- the marker row only moves
// on SQLite. Resets with the process; it answers "did this run flush", not "how many ever".
func (db *DB) FlushCount() int64 { return db.flushes.Load() }
