package archtest

import "testing"

// Every write of an instance's context must set Objects. Omitting it is neither a compile error
// nor a failure near the change: the field defaults to "", so the instance's declaration is ERASED
// while its claims stand, and the damage is invisible until something compares the two. It was
// missed twice within minutes of the column being added. specs/object-store.md.
func TestInstanceWritesCarryObjects(t *testing.T) {
	requireFieldOnLiterals(t, "Objects", map[string]bool{
		"UpdateInstanceParams":         true,
		"UpdateInstanceProgressParams": true,
		"InsertInstanceParams":         true,
	}, "the instance's reference declaration would be erased while its claims stand")
}
