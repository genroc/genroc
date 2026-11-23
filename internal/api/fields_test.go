package api

import (
	"testing"

	"genroc/internal/model"
)

// A field is an address within one definition, and of several submitted the root or a shared task
// id is in every one: without its definition a client cannot place it, so it is never sent.
func TestAFieldIsAlwaysInADefinition(t *testing.T) {
	ve := &model.ValidationError{Fields: []model.FieldError{{Field: "tasks", Rule: "min", Message: "m"}}}
	if got := fieldsOf(invalid("%w", inDefinition{3, "p", ve})); len(got) != 1 || got[0].DefinitionIndex != 3 {
		t.Errorf("got %+v, want the one field placed in definition 3", got)
	}
	if got := fieldsOf(invalid("%w", ve)); got != nil {
		t.Errorf("sent %+v with no definition — it would read as definition 0", got)
	}
}

func TestSeveralRefusalsEachKeepTheirDefinition(t *testing.T) {
	field := func(msg string) *model.ValidationError {
		return &model.ValidationError{Fields: []model.FieldError{{Field: "name", Rule: "unique", Message: msg}}}
	}
	got := fieldsOf(invalid("%w", refusals{{0, "p", field("first")}, {2, "p", field("second")}}))
	if len(got) != 2 || got[0].DefinitionIndex != 0 || got[1].DefinitionIndex != 2 || got[1].Message != "second" {
		t.Errorf("got %+v, want first in definition 0 and second in definition 2", got)
	}
}
