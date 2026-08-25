package db

// The ladder's two readings and the direction each must fail in.
// specs/durability-levels.md §5.

import "testing"

func TestDurability_FloorIsSafeAtZero(t *testing.T) {
	// The rule the classification rests on: a write that declares nothing must flush at
	// every level, so forgetting to classify a new write path costs throughput and never a
	// guarantee. That only holds while the zero value is the WEAKEST level.
	var unclassified Durability // what a forgotten floor is
	for _, level := range []Durability{DurabilityOnlyOnce, DurabilityTerminal, DurabilityStrict} {
		if !level.syncs(unclassified) {
			t.Fatalf("level %s does not flush an unclassified write; a forgotten floor must cost throughput, not a guarantee", level)
		}
	}
}

func TestDurability_LadderIsStrictlyIncreasing(t *testing.T) {
	// Each level must flush everything the level below it does. If this inverts, lowering
	// the flag would silently ADD a guarantee somewhere and the levels stop being a ladder.
	floors := []Durability{syncAlways, syncTerminal, syncStrict}
	for _, floor := range floors {
		for _, pair := range [][2]Durability{
			{DurabilityOnlyOnce, DurabilityTerminal},
			{DurabilityTerminal, DurabilityStrict},
		} {
			lower, higher := pair[0], pair[1]
			if lower.syncs(floor) && !higher.syncs(floor) {
				t.Fatalf("%s flushes floor %s but %s does not; the ladder is not monotonic", lower, higher, floor)
			}
		}
	}
}

func TestDurability_WhatEachLevelFlushes(t *testing.T) {
	// Pins the table in §5 as code. A floor moving between rows is a guarantee change, and
	// this is what makes that show up as a failing test rather than a throughput number.
	cases := []struct {
		level, floor Durability
		want         bool
	}{
		{DurabilityOnlyOnce, syncAlways, true},    // inbound work: never dropped, at any level
		{DurabilityOnlyOnce, syncTerminal, false}, // a finished process may rewind
		{DurabilityOnlyOnce, syncStrict, false},   // ordinary task writes replay
		{DurabilityTerminal, syncAlways, true},
		{DurabilityTerminal, syncTerminal, true}, // finished stays finished
		{DurabilityTerminal, syncStrict, false},
		{DurabilityStrict, syncAlways, true},
		{DurabilityStrict, syncTerminal, true},
		{DurabilityStrict, syncStrict, true}, // every commit
	}
	for _, c := range cases {
		if got := c.level.syncs(c.floor); got != c.want {
			t.Errorf("level %s, floor %s: flushes=%v, want %v", c.level, c.floor, got, c.want)
		}
	}
}

func TestParseDurability(t *testing.T) {
	for in, want := range map[string]Durability{
		"only-once": DurabilityOnlyOnce,
		"terminal":  DurabilityTerminal,
		"strict":    DurabilityStrict,
		"":          DurabilityStrict, // no level named: the safe one, not the CLI's default
	} {
		got, err := ParseDurability(in)
		if err != nil || got != want {
			t.Errorf("ParseDurability(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"none", "accepted", "STRICT", "off", "full"} {
		if _, err := ParseDurability(bad); err == nil {
			t.Errorf("ParseDurability(%q) accepted an unknown level", bad)
		}
	}
}
