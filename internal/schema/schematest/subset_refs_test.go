package schematest

import "testing"

func TestIsSubset_refs(t *testing.T) {
	cases := []struct {
		name  string
		sub   string
		super string
		want  bool
	}{
		{
			"ref to same type",
			`{"$defs":{"T":{"type":"integer"}},"$ref":"#/$defs/T"}`,
			`{"$defs":{"T":{"type":"integer"}},"$ref":"#/$defs/T"}`,
			true,
		},
		{
			"ref integer ⊆ ref number (widening)",
			`{"$defs":{"T":{"type":"integer"}},"$ref":"#/$defs/T"}`,
			`{"$defs":{"T":{"type":"number"}},"$ref":"#/$defs/T"}`,
			true,
		},
		{
			"ref number ⊄ ref integer",
			`{"$defs":{"T":{"type":"number"}},"$ref":"#/$defs/T"}`,
			`{"$defs":{"T":{"type":"integer"}},"$ref":"#/$defs/T"}`,
			false,
		},
		{
			"ref in sub, inline super",
			`{"$defs":{"T":{"type":"integer"}},"$ref":"#/$defs/T"}`,
			`{"type":"number"}`,
			true,
		},
		{
			// Dropping super's pool must not make it accept everything.
			"a super ref that resolves to nothing accepts nothing",
			`{"type":"string"}`,
			`{"$defs":{"Other":{"type":"number"}},"$ref":"#/$defs/Missing"}`,
			false,
		},
		{
			"a super ref with no pool at all accepts nothing",
			`{"type":"object"}`,
			`{"$ref":"#/$defs/T"}`,
			false,
		},
		{
			"inline sub, ref in super",
			`{"type":"integer"}`,
			`{"$defs":{"T":{"type":"number"}},"$ref":"#/$defs/T"}`,
			true,
		},
		{
			"ref to object with compatible properties",
			`{"$defs":{"T":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}},"$ref":"#/$defs/T"}`,
			`{"$defs":{"T":{"type":"object","properties":{"id":{"type":"number"}},"required":["id"]}},"$ref":"#/$defs/T"}`,
			true,
		},
		{
			"cross-schema refs resolved independently",
			`{"$defs":{"MyInt":{"type":"integer"}},"$ref":"#/$defs/MyInt"}`,
			`{"$defs":{"MyNum":{"type":"number"}},"$ref":"#/$defs/MyNum"}`,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertSubset(t, tc.sub, tc.super, tc.want)
		})
	}
}

func TestIsSubset_recursive(t *testing.T) {
	treeNodeInt := `{
		"$defs": {
			"TreeNode": {
				"type": "object",
				"properties": {
					"value": {"type": "integer"},
					"children": {
						"type": "array",
						"items": {"$ref": "#/$defs/TreeNode"}
					}
				},
				"required": ["value"]
			}
		},
		"$ref": "#/$defs/TreeNode"
	}`

	treeNodeNum := `{
		"$defs": {
			"TreeNode": {
				"type": "object",
				"properties": {
					"value": {"type": "number"},
					"children": {
						"type": "array",
						"items": {"$ref": "#/$defs/TreeNode"}
					}
				},
				"required": ["value"]
			}
		},
		"$ref": "#/$defs/TreeNode"
	}`

	linkedList := `{
		"$defs": {
			"Node": {
				"type": "object",
				"properties": {
					"data": {"type": "string"},
					"next": {
						"oneOf": [
							{"$ref": "#/$defs/Node"},
							{"type": "null"}
						]
					}
				},
				"required": ["data"]
			}
		},
		"$ref": "#/$defs/Node"
	}`

	t.Run("treeNode ⊆ treeNode (identity)", func(t *testing.T) {
		assertSubset(t, treeNodeInt, treeNodeInt, true)
	})
	t.Run("treeNode(integer) ⊆ treeNode(number)", func(t *testing.T) {
		assertSubset(t, treeNodeInt, treeNodeNum, true)
	})
	t.Run("treeNode(number) ⊄ treeNode(integer)", func(t *testing.T) {
		assertSubset(t, treeNodeNum, treeNodeInt, false)
	})
	t.Run("linkedList ⊆ linkedList (identity)", func(t *testing.T) {
		assertSubset(t, linkedList, linkedList, true)
	})
	t.Run("treeNode ⊄ linkedList (different structure)", func(t *testing.T) {
		assertSubset(t, treeNodeInt, linkedList, false)
	})
}

func TestIsSubset_nonrecursive_sub_recursive_super(t *testing.T) {
	// Super is the same recursive TreeNode used in TestIsSubset_recursive.
	treeNode := `{
		"$defs": {
			"TreeNode": {
				"type": "object",
				"properties": {
					"value": {"type": "integer"},
					"children": {
						"type": "array",
						"items": {"$ref": "#/$defs/TreeNode"}
					}
				},
				"required": ["value"]
			}
		},
		"$ref": "#/$defs/TreeNode"
	}`

	// A plain leaf: just {value: integer}, no children property at all.
	// Valid under TreeNode because children is optional.
	leaf := `{
		"type": "object",
		"properties": {
			"value": {"type": "integer"}
		},
		"required": ["value"]
	}`

	// A one-level tree: has children, but their items are plain leaves (non-recursive).
	// Each child satisfies TreeNode's requirements (value: integer, children optional).
	oneLevel := `{
		"type": "object",
		"properties": {
			"value": {"type": "integer"},
			"children": {
				"type": "array",
				"items": {
					"type": "object",
					"properties": {
						"value": {"type": "integer"}
					},
					"required": ["value"]
				}
			}
		},
		"required": ["value"]
	}`

	t.Run("plain leaf ⊆ recursive TreeNode", func(t *testing.T) {
		assertSubset(t, leaf, treeNode, true)
	})
	t.Run("one-level non-recursive tree ⊆ recursive TreeNode", func(t *testing.T) {
		assertSubset(t, oneLevel, treeNode, true)
	})
}

func TestIsSubset_inline_refs(t *testing.T) {
	sub := `{
		"$defs": {
			"Address": {
				"type": "object",
				"properties": {
					"street": {"type": "string"},
					"zip": {"type": "integer"}
				},
				"required": ["street", "zip"]
			}
		},
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"address": {"$ref": "#/$defs/Address"}
		},
		"required": ["name", "address"]
	}`
	superCompatible := `{
		"$defs": {
			"Address": {
				"type": "object",
				"properties": {
					"street": {"type": "string"},
					"zip": {"type": "number"}
				},
				"required": ["street", "zip"]
			}
		},
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"address": {"$ref": "#/$defs/Address"}
		},
		"required": ["name", "address"]
	}`
	superIncompatible := `{
		"$defs": {
			"Address": {
				"type": "object",
				"properties": {
					"street": {"type": "string"},
					"zip": {"type": "string"}
				},
				"required": ["street", "zip"]
			}
		},
		"type": "object",
		"properties": {
			"name": {"type": "string"},
			"address": {"$ref": "#/$defs/Address"}
		},
		"required": ["name", "address"]
	}`

	t.Run("inline ref property compatible", func(t *testing.T) {
		assertSubset(t, sub, superCompatible, true)
	})
	t.Run("inline ref property incompatible", func(t *testing.T) {
		assertSubset(t, sub, superIncompatible, false)
	})
}
