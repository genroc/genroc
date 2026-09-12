package errcode

import (
	"os"
	"strings"
	"testing"
)

// The documents that ENUMERATE the unknowable set instead of pointing at Unknowable(); each
// must name every member. internal/engine/CLAUDE.md is deliberately absent — it points at the
// var, which is why it cannot rot.
var unknowableDocs = []string{
	"../../specs/only-once-interrupted.md",
	"../../examples/order-fulfilment/README.md",
	"../../docs/src/content/docs/guides/process-definition/error-handling.mdx",
}

func TestUnknowableSetIsDocumented(t *testing.T) {
	for _, path := range unknowableDocs {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v — it spells the unknowable set out, so a move brings this list with it rather than skipping the check", path, err)
			continue
		}
		for _, code := range unknowable {
			if !strings.Contains(string(body), string(code)) {
				t.Errorf("%s never mentions %q, but it enumerates the unknowable set: add the code there too, "+
					"or rewrite the passage to point at errcode.Unknowable() and drop the file from unknowableDocs", path, code)
			}
		}
	}
}
