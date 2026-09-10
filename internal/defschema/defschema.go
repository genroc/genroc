// Package defschema projects the definition language into a JSON Schema. It lives beside the
// language rather than in internal/api because it describes a definition, not an endpoint: the
// API serves the bytes, the docs site publishes them, completion reads them.
// specs/language-server.md §5.
package defschema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"genroc/internal/model"

	"github.com/swaggest/jsonschema-go"
)

var (
	processSchemaOnce  sync.Once
	processSchemaBytes []byte
)

// ShapeDefName keeps the Shape type's generated def name stable as ModelShape after the
// type moved from package model to package shape (swaggest would otherwise name it
// ShapeShape). Hand-written $refs — the Action schema and Shape's own self-recursion —
// point at ModelShape, so this mapping lets them resolve without edits. Exported because the
// OpenAPI builder reflects the same types and needs the same name.
func ShapeDefName(_ reflect.Type, defaultDefName string) string {
	if defaultDefName == "ShapeShape" {
		return "ModelShape"
	}
	return defaultDefName
}

// Process returns the JSON Schema for a process definition, built once. It is what an editor
// loads (`# yaml-language-server: $schema=`), what GET /process-schema.json serves, and what
// completion reads to know which keys are legal where — see specs/language-server.md §5 for
// why completion uses this and diagnostics do not.
func Process() []byte {
	processSchemaOnce.Do(func() {
		r := jsonschema.Reflector{}
		r.DefaultOptions = append(r.DefaultOptions,
			jsonschema.InterceptDefName(ShapeDefName),
			jsonschema.InterceptProp(func(params jsonschema.InterceptPropParams) error {
				if !params.Processed || params.Field.Type == nil || params.ParentSchema == nil {
					return nil
				}
				tag := params.Field.Tag.Get("json")
				if strings.Contains(tag, "omitempty") || params.Field.Type.Kind() == reflect.Ptr {
					return nil
				}
				for _, r := range params.ParentSchema.Required {
					if r == params.Name {
						return nil
					}
				}
				params.ParentSchema.Required = append(params.ParentSchema.Required, params.Name)
				return nil
			}),
			jsonschema.InterceptProp(func(params jsonschema.InterceptPropParams) error {
				if !params.Processed || params.PropertySchema == nil {
					return nil
				}
				if desc := params.Field.Tag.Get("description"); desc != "" {
					params.PropertySchema.WithDescription(desc)
				}
				return nil
			}),
		)
		r.DefaultOptions = append(r.DefaultOptions, jsonschema.InterceptSchema(closeStructs))
		s, err := r.Reflect(model.ProcessDefinition{})
		if err != nil {
			panic(fmt.Sprintf("processDefinitionSchema: %v", err))
		}
		b, _ := json.Marshal(s)
		processSchemaBytes = upgradeToDraft201909(b)
	})
	return processSchemaBytes
}

// closeStructs rejects unknown keys on every object reflected from a Go struct, because the server
// already does; without it the published schema accepted `on_eror:` on a task, a typo the editor
// passed and the server refused. A struct whose fields are open is left alone -- it carries its
// own additionalProperties, and a schema stricter than the server underlines working code.
// specs/language-server.md §5.
func closeStructs(params jsonschema.InterceptSchemaParams) (bool, error) {
	if !params.Processed || params.Schema == nil || params.Schema.AdditionalProperties != nil {
		return false, nil
	}
	if len(params.Schema.Properties) == 0 {
		return false, nil
	}
	params.Schema.WithAdditionalProperties(jsonschema.SchemaOrBool{TypeBoolean: new(bool)})
	return false, nil
}

// upgradeToDraft201909 rewrites a swaggest-generated schema to JSON Schema
// draft 2019-09 so that $ref and description can coexist on the same node
// (in draft-07, $ref silently swallows all sibling keywords).
func upgradeToDraft201909(b []byte) []byte {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return b
	}
	if defs, ok := root["definitions"]; ok {
		root["$defs"] = defs
		delete(root, "definitions")
	}
	root["$schema"] = "https://json-schema.org/draft/2019-09/schema"
	// Rewrite all $ref values from #/definitions/ to #/$defs/.
	rewriteRefs(root)
	out, _ := json.Marshal(root)
	return out
}

func rewriteRefs(v any) {
	switch node := v.(type) {
	case map[string]any:
		if ref, ok := node["$ref"].(string); ok {
			node["$ref"] = strings.ReplaceAll(ref, "#/definitions/", "#/$defs/")
		}
		for _, child := range node {
			rewriteRefs(child)
		}
	case []any:
		for _, child := range node {
			rewriteRefs(child)
		}
	}
}
