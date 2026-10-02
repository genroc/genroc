package archtest

import "testing"

// Seq orders rows written inside one millisecond, and the column defaults to 0 rather than
// erroring.
func TestLogAndSignalWritesCarrySeq(t *testing.T) {
	requireFieldOnLiterals(t, "Seq", map[string]bool{
		"InsertLogParams":    true,
		"InsertSignalParams": true,
	}, "rows it writes order by an id that does not sort")
}
