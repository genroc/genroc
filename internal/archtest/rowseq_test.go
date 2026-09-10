package archtest

import "testing"

// Every write of a log row or a signal must set Seq: ids do not sort, so it is what orders two rows
// written inside one millisecond, and the column defaults to 0 rather than erroring. An ordering
// test does NOT catch this -- rows minted at the same width happen to sort, so a missing seq looks
// correct until the counter crosses a padding boundary.
func TestLogAndSignalWritesCarrySeq(t *testing.T) {
	requireFieldOnLiterals(t, "Seq", map[string]bool{
		"InsertLogParams":    true,
		"InsertSignalParams": true,
	}, "rows it writes order by an id that does not sort")
}
