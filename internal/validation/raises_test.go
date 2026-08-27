package validation

import (
	"encoding/json"
	"testing"

	"genroc/internal/model"
)

// raiseTypesOf infers a definition from its JSON and returns the payload type of each code it
// raises, as JSON — the shape a caller's `raises` declaration is checked against.
func raiseTypesOf(t *testing.T, doc string) map[string]string {
	t.Helper()
	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(doc), &def); err != nil {
		t.Fatalf("parse definition: %v", err)
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("validate definition: %v", err)
	}
	sf, err := Generate(&def)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	out := make(map[string]string, len(sf.Raises))
	for code, s := range sf.Raises {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal %s: %v", code, err)
		}
		out[code] = string(b)
	}
	return out
}

// SchemaFile.Raises is the error channel's ProcessOutput, and checkDeclaredRaises decides a
// caller's declaration against it. Every rule below changes which declarations register, and
// none of them shows up as a compile error if it is dropped.
func TestRaiseTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want map[string]string
	}{
		{
			name: "a clause attaching nothing types as null, because the slot is cleared",
			doc: `{"name":"p","tasks":[
				{"id":"a","switch":[{"case":"true","raise":{"code":"gone","message":"m"}},{"goto":"end"}]}]}`,
			want: map[string]string{"gone": `{"type":"null"}`},
		},
		{
			name: "a literal payload is exact, and every key it writes is guaranteed",
			doc: `{"name":"p","tasks":[
				{"id":"a","switch":[{"case":"true","raise":{"code":"no","message":"m","data":{"why":"card","n":3}}},{"goto":"end"}]}]}`,
			want: map[string]string{
				"no": `{"type":"object","properties":{"n":{"type":"integer"},"why":{"type":"string"}},"required":["n","why"]}`,
			},
		},
		{
			name: "two clauses on one code union: either may fire, so a caller must accept both",
			doc: `{"name":"p","tasks":[
				{"id":"a","switch":[{"case":"true","raise":{"code":"no","message":"m","data":{"why":"card"}}},{"goto":"$b"}]},
				{"id":"b","switch":[{"case":"true","raise":{"code":"no","message":"m","data":{"code":51}}},{"goto":"end"}]}]}`,
			want: map[string]string{
				"no": `{"anyOf":[{"type":"object","properties":{"why":{"type":"string"}},"required":["why"]},` +
					`{"type":"object","properties":{"code":{"type":"integer"}},"required":["code"]}]}`,
			},
		},
		{
			name: "a data-less clause beside a payload one adds the null arm rather than replacing it",
			doc: `{"name":"p","tasks":[
				{"id":"a","switch":[{"case":"true","raise":{"code":"no","message":"m","data":{"why":"card"}}},{"goto":"$b"}]},
				{"id":"b","switch":[{"case":"true","raise":{"code":"no","message":"m"}},{"goto":"end"}]}]}`,
			want: map[string]string{
				"no": `{"oneOf":[{"type":"object","properties":{"why":{"type":"string"}},"required":["why"]},{"type":"null"}]}`,
			},
		},
		{
			name: "identical arms collapse: a union no arm is alone in says nothing extra",
			doc: `{"name":"p","tasks":[
				{"id":"a","switch":[{"case":"true","raise":{"code":"no","message":"m","data":{"why":"card"}}},{"goto":"$b"}]},
				{"id":"b","switch":[{"case":"true","raise":{"code":"no","message":"m","data":{"why":"card"}}},{"goto":"end"}]}]}`,
			want: map[string]string{
				"no": `{"type":"object","properties":{"why":{"type":"string"}},"required":["why"]}`,
			},
		},
		{
			name: "a panic contributes nothing: no rule can ever catch one",
			doc: `{"name":"p","tasks":[
				{"id":"a","switch":[{"case":"true","panic":{"code":"broken","message":"m","data":{"why":"bug"}}},{"goto":"end"}]}]}`,
			want: map[string]string{},
		},
		{
			name: "an on_error raise is typed in ITS scope, so it can recompose the error it caught",
			doc: `{"name":"p","tasks":[
				{"id":"a","action":{"type":"fetch","url":"http://x","responses":{"404":{"type":"object","properties":{"detail":{"type":"string"}},"required":["detail"]}}},
				 "on_error":[{"code":["http.404"],"raise":{"code":"missing","message":"m","data":{"detail":"$: error.data.detail"}}}],
				 "switch":[{"goto":"end"}]}]}`,
			want: map[string]string{
				"missing": `{"type":"object","properties":{"detail":{"type":"string"}},"required":["detail"]}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := raiseTypesOf(t, tc.doc)
			if len(got) != len(tc.want) {
				t.Fatalf("raise set is %v, want keys %v — the set must be exactly the codes a caller may declare", got, tc.want)
			}
			for code, want := range tc.want {
				if got[code] != want {
					t.Errorf("%s payload type\n got: %s\nwant: %s", code, got[code], want)
				}
			}
		})
	}
}
