package archtest

import "testing"

// Every write of an instance's context must set Objects.
//
// The column lists what the context references, and the claims in object_refs are written from
// the same set. Omitting it is not a compile error and not a test failure anywhere near the
// change: the field defaults to "", so the instance's declaration is ERASED while its claims
// stand, and its own values start looking like content nothing accounts for. The GC does not care
// -- claims are what it reads -- so the damage is invisible until something compares the two.
//
// It was missed twice within minutes of the column being added (RetryProcess passing raw columns
// through, and the parent park in SpawnChildrenAndWait), which is what this exists to stop. It is
// the price of the references living beside the values rather than inside them; this check is how
// that price is paid once. specs/object-store.md.
func TestInstanceWritesCarryObjects(t *testing.T) {
	requireFieldOnLiterals(t, "Objects", map[string]bool{
		"UpdateInstanceParams":         true,
		"UpdateInstanceProgressParams": true,
		"InsertInstanceParams":         true,
	}, "the instance's reference declaration would be erased while its claims stand")
}
