// Package defschema projects the definition language into a JSON Schema: the API serves it, the
// docs site publishes it, completion reads it. specs/language-server.md §5.
package defschema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"genroc/internal/model"
	"genroc/internal/sources"

	"github.com/swaggest/jsonschema-go"
)

var (
	processSchemaOnce  sync.Once
	processSchemaBytes []byte
)

// ShapeDefName names Shape's def ModelShape, not swaggest's ShapeShape: hand-written $refs point
// there. Exported for the OpenAPI builder, which reflects the same types.
func ShapeDefName(_ reflect.Type, defaultDefName string) string {
	if defaultDefName == "ShapeShape" {
		return "ModelShape"
	}
	return defaultDefName
}

// Process returns the process-definition JSON Schema, built once. Completion reads it and
// diagnostics do not -- specs/language-server.md §5.
func Process() []byte {
	processSchemaOnce.Do(func() {
		processSchemaBytes = reflectSchema(model.ProcessDefinition{}, "processDefinitionSchema",
			jsonschema.InterceptDefName(ShapeDefName))
	})
	return processSchemaBytes
}

// Config returns the JSON Schema for a project's `.genroc`, reflected from sources.Config. Not
// cached: it is built once per generator run, and a second Once is a second archtest exception.
func Config() []byte {
	return withoutNull(reflectSchema(sources.Config{}, "configSchema"))
}

// Manifest returns the JSON Schema of what a resolver reads on stdin; Reply, of what it writes.
func Manifest() []byte { return withoutNull(reflectSchema(sources.Manifest{}, "manifestSchema")) }

func Reply() []byte { return withoutNull(reflectSchema(sources.Reply{}, "replySchema")) }

// The reflector makes a non-omitempty slice nullable, and nothing in a `.genroc` may be null.
// Not the process schema: its nullability is pinned against the server by its own tests.
func withoutNull(b []byte) []byte {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return b
	}
	var walk func(v any)
	walk = func(v any) {
		switch node := v.(type) {
		case map[string]any:
			if types, ok := node["type"].([]any); ok {
				kept := make([]any, 0, len(types))
				for _, t := range types {
					if t != "null" {
						kept = append(kept, t)
					}
				}
				if len(kept) == 1 {
					node["type"] = kept[0]
				} else {
					node["type"] = kept
				}
			}
			for _, child := range node {
				walk(child)
			}
		case []any:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(root)
	out, _ := json.Marshal(root)
	return out
}

// reflectSchema is the projection both schemas share: a field is required unless its json tag
// says omitempty or it is a pointer, `description` tags become the text an editor shows, and
// every struct is closed to unknown keys because the reader is.
func reflectSchema(root any, what string, extra ...func(*jsonschema.ReflectContext)) []byte {
	r := jsonschema.Reflector{}
	r.DefaultOptions = append(r.DefaultOptions, extra...)
	r.DefaultOptions = append(r.DefaultOptions,
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
		jsonschema.InterceptSchema(closeStructs),
	)
	s, err := r.Reflect(root)
	if err != nil {
		panic(fmt.Sprintf("%s: %v", what, err))
	}
	b, _ := json.Marshal(s)
	return upgradeToDraft201909(b)
}

// closeStructs closes every struct-reflected object because the server does. An open struct keeps
// its own additionalProperties -- a schema stricter than the server underlines working code.
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

// Draft 2019-09 because in draft-07 a $ref silently swallows its sibling keywords, description
// included.
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
