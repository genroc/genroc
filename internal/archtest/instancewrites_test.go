package archtest

import "testing"

// Objects defaults to "", so an omission ERASES the declaration while its claims stand,
// invisibly. specs/object-store.md.
func TestInstanceWritesCarryObjects(t *testing.T) {
	requireFieldOnLiterals(t, "Objects", map[string]bool{
		"UpdateInstanceParams":         true,
		"UpdateInstanceProgressParams": true,
		"InsertInstanceParams":         true,
	}, "the instance's reference declaration would be erased while its claims stand")
}
