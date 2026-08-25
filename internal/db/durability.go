package db

import "fmt"

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
