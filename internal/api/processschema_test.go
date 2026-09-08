package api

import (
	"strings"
	"testing"

	"genroc/internal/model"
	"genroc/internal/numeric"

	"github.com/xeipuuv/gojsonschema"
)

// The published schema is what an editor loads (`# yaml-language-server: $schema=`), and it is
// generated while the rules it describes are hand-written — so nothing but this compares them.
// It drifted: `on_eror:` on a task and `tsaks:` at the root were accepted by the schema and
// rejected by the server. specs/language-server.md §5.
//
// Scope is unknown keys, the one thing both can decide. Cross-field rules (a goto naming a real
// task, catch-all-last) are not expressible in JSON Schema and are deliberately absent here.
func TestPublishedSchemaAgreesWithTheServerOnUnknownKeys(t *testing.T) {
	for _, c := range []struct {
		where  string
		doc    string
		reject bool
	}{
		{"a valid definition", `{"name":"x","tasks":[{"id":"a","switch":"end","action":{"type":"fetch","url":"u"}}]}`, false},
		{"root", `{"name":"x","tasks":[],"zzz":1}`, true},
		{"task", `{"name":"x","tasks":[{"id":"a","switch":"end","zzz":1}]}`, true},
		{"action.fetch", `{"name":"x","tasks":[{"id":"a","switch":"end","action":{"type":"fetch","url":"u","zzz":1}}]}`, true},
		{"on_error rule", `{"name":"x","tasks":[{"id":"a","switch":"end","on_error":[{"goto":"end","zzz":1}]}]}`, true},
		{"switch case", `{"name":"x","tasks":[{"id":"a","switch":[{"goto":"end","zzz":1}]}]}`, true},
		{"retry", `{"name":"x","tasks":[{"id":"a","switch":"end","on_error":[{"retry":{"attempts":1,"zzz":1},"goto":"end"}]}]}`, true},
		{"timeout object", `{"name":"x","tasks":[{"id":"a","switch":"end","timeout":{"for":"1s","zzz":1}}]}`, true},
		{"raise", `{"name":"x","tasks":[{"id":"a","switch":[{"raise":{"code":"c","message":"m","zzz":1}}]}]}`, true},

		// The mirror-image bug: a schema stricter than the server underlines working code.
		{"an output shape is free-form", `{"name":"x","tasks":[{"id":"a","switch":"end"}],"output":{"anything":"$: 1","nested":{"k":"v"}}}`, false},
		{"a task body is free-form", `{"name":"x","tasks":[{"id":"a","switch":"end","action":{"type":"fetch","url":"u","body":{"any":"thing"}}}]}`, false},
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

// The gap the table above cannot close. A user-supplied JSON Schema (`input_schema`,
// `config_schema`, `$defs`) reflects to an opaque `{type: object, additionalProperties: true}`
// because schema.Schema's node is private, so the editor accepts a keyword the server refuses
// by allowlist. Closing it means generating a meta-schema for that allowlist; until then this
// records which way the two disagree. specs/language-server.md §5.
func TestUserSuppliedSchemasAreNotYetCheckedByThePublishedSchema(t *testing.T) {
	const doc = `{"name":"x","tasks":[],"input_schema":{"type":"object","zzz":1}}`
	if !serverRejects(doc) {
		t.Fatal("the server no longer refuses an unsupported schema keyword")
	}
	if schemaRejects(t, doc) {
		t.Fatal("the published schema now checks user schemas too - delete this test and add " +
			"the case to the agreement table above")
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
