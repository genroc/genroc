package api

import (
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/numeric"

	"github.com/xeipuuv/gojsonschema"
)

// The published schema is generated while the rules it describes are hand-written, so nothing
// but this compares them — and it drifted (`on_eror:`, `tsaks:` accepted by one, rejected by the
// other). Scope is unknown keys, the one thing both can decide; cross-field rules are not
// expressible in JSON Schema. specs/language-server.md §5.
func TestPublishedSchemaAgreesWithTheServerOnUnknownKeys(t *testing.T) {
	for _, c := range []struct {
		where  string
		doc    string
		reject bool
	}{
		{"a valid definition", `{"name":"x","tasks":[{"id":"a","switch":"end","action":{"type":"fetch","method":"post","url":"u"}}]}`, false},
		{"root", `{"name":"x","tasks":[],"zzz":1}`, true},
		{"task", `{"name":"x","tasks":[{"id":"a","switch":"end","zzz":1}]}`, true},
		{"action.fetch", `{"name":"x","tasks":[{"id":"a","switch":"end","action":{"type":"fetch","method":"post","url":"u","zzz":1}}]}`, true},
		{"on_error rule", `{"name":"x","tasks":[{"id":"a","switch":"end","on_error":[{"goto":"end","zzz":1}]}]}`, true},
		{"switch case", `{"name":"x","tasks":[{"id":"a","switch":[{"goto":"end","zzz":1}]}]}`, true},
		{"retry", `{"name":"x","tasks":[{"id":"a","switch":"end","on_error":[{"retry":{"attempts":1,"zzz":1},"goto":"end"}]}]}`, true},
		{"timeout object", `{"name":"x","tasks":[{"id":"a","switch":"end","timeout":{"for":"1s","zzz":1}}]}`, true},
		{"raise", `{"name":"x","tasks":[{"id":"a","switch":[{"raise":{"code":"c","message":"m","zzz":1}}]}]}`, true},
		// A user-supplied schema was the last divergence: it reflected to an opaque object, so
		// the editor accepted a keyword the server refuses by allowlist. schema.JSONSchemaBytes
		// derives the keywords from that same allowlist now.
		{"input_schema", `{"name":"x","tasks":[],"input_schema":{"type":"object","zzz":1}}`, true},
		{"a $defs entry", `{"name":"x","tasks":[],"$defs":{"D":{"type":"object","zzz":1}}}`, true},
		{"a schema keyword that IS allowed", `{"name":"x","tasks":[{"id":"a","switch":"end"}],"input_schema":{"type":"object","properties":{"a":{"type":"string","minLength":1}},"required":["a"]}}`, false},

		// The mirror-image bug: a schema stricter than the server underlines working code.
		{"an output shape is free-form", `{"name":"x","tasks":[{"id":"a","switch":"end"}],"output":{"anything":"$: 1","nested":{"k":"v"}}}`, false},
		{"a task body is free-form", `{"name":"x","tasks":[{"id":"a","switch":"end","action":{"type":"fetch","method":"post","url":"u","body":{"any":"thing"}}}]}`, false},
	} {
		t.Run(c.where, func(t *testing.T) {
			server := serverRejects(c.doc)
			schema := schemaRejects(t, c.doc)
			if server != c.reject {
				t.Fatalf("the fixture is wrong: server reject = %v, want %v", server, c.reject)
			}
			if schema != server {
				verb := map[bool]string{true: "rejects", false: "accepts"}
				t.Errorf("the editor and the server disagree: schema %s, server %s\n  %s",
					verb[schema], verb[server], c.doc)
			}
		})
	}
}

func serverRejects(doc string) bool {
	var d model.ProcessDefinition
	if err := numeric.DecodeStrict([]byte(doc), &d); err != nil {
		return true
	}
	return d.Validate() != nil
}

func schemaRejects(t *testing.T, doc string) bool {
	t.Helper()
	r, err := gojsonschema.Validate(
		gojsonschema.NewBytesLoader(ProcessSchema()),
		gojsonschema.NewStringLoader(doc))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if !r.Valid() {
		msgs := make([]string, 0, len(r.Errors()))
		for _, e := range r.Errors() {
			msgs = append(msgs, e.String())
		}
		t.Logf("schema errors: %s", strings.Join(msgs, "; "))
	}
	return !r.Valid()
}
