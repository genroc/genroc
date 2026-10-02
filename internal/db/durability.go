package db

import (
	"context"
	"fmt"

	dbgen "genroc/internal/db/gen"
)

// Durability is the ladder from specs/durability-levels.md §5, weakest first. An operator picks a
// LEVEL (a ceiling lowered for throughput); each write declares a FLOOR (the weakest level at
// which it still fsyncs). A write syncs when level >= floor.
type Durability int

const (
	// DurabilityOnlyOnce keeps what cannot be replayed: inbound work is never forgotten and an
	// only_once task never runs twice. Ordinary task writes may replay after a power cut.
	DurabilityOnlyOnce Durability = iota
	// DurabilityTerminal adds: a finished process stays finished. It is what stops a
	// poller seeing `completed` and then `running` again after a power cut.
	DurabilityTerminal
	// DurabilityStrict adds: no completed task ever repeats. Every commit is flushed.
	DurabilityStrict
)

// syncAlways is the weakest level, so it fsyncs at every level, and the zero value: forgetting
// to classify a new write path costs throughput, never a guarantee.
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

// ParseDurability maps the operator-facing name onto a level.
func ParseDurability(s string) (Durability, error) {
	switch s {
	case "only-once":
		return DurabilityOnlyOnce, nil
	case "terminal":
		return DurabilityTerminal, nil
	case "strict":
		return DurabilityStrict, nil
	case "":
		// Not the CLI default (the flag supplies only-once): only a programmatic caller naming
		// no level gets here, and an unnamed level must be the safe one.
		return DurabilityStrict, nil
	}
	return 0, fmt.Errorf("invalid durability %q (want only-once, terminal, or strict)", s)
}

// SetDurability sets the level every write is measured against. Call once at startup,
// before the engine runs: it is read on every write path and never re-read from config.
func (db *DB) SetDurability(d Durability) { db.durability.Store(int64(d)) }

func (db *DB) level() Durability { return Durability(db.durability.Load()) }

// instanceWriteFloor derives the floor from what is written, not who writes it, so a new
// terminal status is classified correctly without anyone remembering to.
func instanceWriteFloor(status interface{ Terminal() bool }) Durability {
	if status.Terminal() {
		return syncTerminal
	}
	return syncStrict
}

// Flush makes every commit so far durable, whatever level it ran at: both engines append to one
// WAL, so one flushed commit hardens those behind it. A no-op at `strict`.
// specs/durability-levels.md s3.
func (db *DB) Flush(ctx context.Context) error {
	if db.level() == DurabilityStrict {
		return nil
	}
	err := db.withTxAt(ctx, syncAlways, func(qtx *dbgen.Queries, exec dbgen.DBTX) error {
		if db.dialect == "postgres" {
			// An XID makes the commit real, and a real commit flushes. No marker row: it would
			// queue concurrent workers on its lock across the fsync, plus a dead tuple each.
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

// FlushCount is how many times Flush has committed in this process: "did this run flush", not
// "how many ever".
func (db *DB) FlushCount() int64 { return db.flushes.Load() }
