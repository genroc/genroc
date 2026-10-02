package schematest

import (
	"testing"

	"genroc/internal/schema"
)

// Only same-kind nesting flattens; each level is order-insensitive.
func TestCanonicalize_CrossKindNesting_EqualPairs(t *testing.T) {
	objA := objP("a", prim("integer"))
	objB := objP("b", prim("string"))
	objC := objP("c", prim("boolean"))
	objD := objP("d", prim("number"))

	tests := []struct {
		name string
		a, b schema.Schema
	}{
		{
			"oneOf-of-objects nested in anyOf",
			anyOf(oneOf(objP("x", oneOf(prim("integer"), prim("integer"))), objB), objC),
			anyOf(objC, oneOf(objB, objP("x", prim("integer")))),
		},
		{
			"anyOf nested in oneOf",
			oneOf(anyOf(objA, objB), objC),
			oneOf(objC, anyOf(objB, objA)),
		},
		{
			"allOf nested in oneOf",
			oneOf(allOf(objA, objB), objC),
			oneOf(objC, allOf(objB, objA)),
		},
		{
			"oneOf nested in allOf",
			allOf(oneOf(objA, objB), objC),
			allOf(objC, oneOf(objB, objA)),
		},
		{
			"singleton oneOf wrapping an anyOf unwraps to the anyOf",
			oneOf(anyOf(objA, objB)),
			anyOf(objB, objA),
		},
		{
			"three levels: allOf-in-oneOf-in-anyOf, deep order + spelling normalization",
			anyOf(oneOf(allOf(objA, objB), objC), objP("d", oneOf(prim("number"), prim("number")))),
			anyOf(objD, oneOf(objC, allOf(objB, objA))),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if ja, jb := canonJSON(t, tt.a), canonJSON(t, tt.b); ja != jb {
				t.Errorf("canonical forms differ:\n  a: %s\n  b: %s", ja, jb)
			}
		})
	}
}

// Cross-kind nesting whose innermost variants are all simple primitives still
// collapses to a single {type:[...]} — a union of unions is a flat union.
func TestCanonicalize_CrossKindNesting_SimpleCollapse(t *testing.T) {
	tests := []struct {
		name string
		in   schema.Schema
		want schema.Schema
	}{
		{
			"oneOf-of-simple nested in anyOf merges to a flat type array",
			anyOf(oneOf(prim("integer"), prim("string")), prim("boolean")),
			prim("boolean", "integer", "string"),
		},
		{
			"object property: anyOf-of-oneOf of simple types",
			objP("v", anyOf(oneOf(prim("integer"), prim("integer")), prim("string"))),
			objP("v", prim("integer", "string")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonJSON(t, tt.in)
			want := canonJSON(t, tt.want)
			if got != want {
				t.Errorf("collapse mismatch:\n  got:  %s\n  want: %s", got, want)
			}
		})
	}
}

// allOf must NOT be merged into a type array, even when its members are simple:
// allOf[integer, string] is an (empty) intersection, not the union integer|string.
func TestCanonicalize_AllOfNeverMergesSimple(t *testing.T) {
	got := canonJSON(t, allOf(prim("integer"), prim("string")))
	union := canonJSON(t, prim("integer", "string"))
	if got == union {
		t.Errorf("allOf[integer,string] wrongly collapsed to the union %s", got)
	}
	// It should remain an allOf of the two (sorted) members.
	want := canonJSON(t, allOf(prim("string"), prim("integer")))
	if got != want {
		t.Errorf("allOf canonical mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}
