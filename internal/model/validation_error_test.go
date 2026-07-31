package model

import (
	"errors"
	"fmt"
	"testing"
)

// fieldsOf runs the struct-tag validation and returns the per-field detail, failing the
// test if the error is not a *ValidationError.
func fieldsOf(t *testing.T, d *ProcessDefinition) []FieldError {
	t.Helper()
	err := fmtValidationErr(v.Struct(d))
	if err == nil {
		t.Fatal("expected a validation error, got nil")
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error is %T, want *ValidationError: %v", err, err)
	}
	return ve.Fields
}

func fieldNames(fields []FieldError) []string {
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = f.Field
	}
	return out
}

// The whole point of carrying fields separately is that they locate the failure. A
// top-level field is easy; the case that matters is a failure inside the tasks slice,
// where the path has to name the index.
func TestValidationErrorFieldPaths(t *testing.T) {
	tests := []struct {
		name string
		def  *ProcessDefinition
		want []string
	}{
		{
			name: "top-level required field",
			def:  &ProcessDefinition{Tasks: []*Task{{ID: "a"}}},
			want: []string{"name"},
		},
		{
			name: "top-level slice rule",
			def:  &ProcessDefinition{Name: "p", Tasks: []*Task{}},
			want: []string{"tasks"},
		},
		{
			name: "nested failure names its index, not just the leaf",
			def:  &ProcessDefinition{Name: "p", Tasks: []*Task{{ID: "a"}, {ID: ""}}},
			want: []string{"tasks[1].id"},
		},
		{
			name: "every failing element is reported, each with its own index",
			def:  &ProcessDefinition{Name: "p", Tasks: []*Task{{ID: ""}, {ID: "b"}, {ID: ""}}},
			want: []string{"tasks[0].id", "tasks[2].id"},
		},
		{
			name: "failures at different depths coexist",
			def:  &ProcessDefinition{Tasks: []*Task{{ID: ""}}},
			want: []string{"name", "tasks[0].id"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fieldNames(fieldsOf(t, tt.def))
			if len(got) != len(tt.want) {
				t.Fatalf("fields = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("fields = %v, want %v", got, tt.want)
					break
				}
			}
		})
	}
}

// The path is the JSON path as the client wrote it — the root struct name is stripped,
// so a client can walk straight back into the document it submitted.
func TestValidationErrorFieldPathHasNoGoTypeName(t *testing.T) {
	for _, f := range fieldsOf(t, &ProcessDefinition{Name: "p", Tasks: []*Task{{ID: ""}}}) {
		if f.Field == "" {
			t.Error("empty field path")
		}
		if len(f.Field) >= len("ProcessDefinition") && f.Field[:len("ProcessDefinition")] == "ProcessDefinition" {
			t.Errorf("field %q still carries the Go struct name", f.Field)
		}
	}
}

// The rule and its parameter travel alongside the message, so a client can react to
// *which* constraint failed without matching English.
func TestValidationErrorCarriesRuleAndParam(t *testing.T) {
	fields := fieldsOf(t, &ProcessDefinition{Name: "p", Tasks: []*Task{}})
	if len(fields) != 1 {
		t.Fatalf("fields = %v, want exactly one", fieldNames(fields))
	}
	if fields[0].Rule != "min" {
		t.Errorf("rule = %q, want %q", fields[0].Rule, "min")
	}
	if fields[0].Param != "1" {
		t.Errorf("param = %q, want %q", fields[0].Param, "1")
	}

	required := fieldsOf(t, &ProcessDefinition{Tasks: []*Task{{ID: "a"}}})
	if required[0].Rule != "required" {
		t.Errorf("rule = %q, want %q", required[0].Rule, "required")
	}
}

// This is the reason fields exist rather than only the joined message: for a nested
// failure the message names the leaf ("id is required") and is identical for every
// failing task, so the message alone cannot tell them apart. The path can.
func TestValidationErrorMessageIsAmbiguousWhereTheFieldPathIsNot(t *testing.T) {
	fields := fieldsOf(t, &ProcessDefinition{Name: "p", Tasks: []*Task{{ID: ""}, {ID: ""}}})
	if len(fields) != 2 {
		t.Fatalf("fields = %v, want two", fieldNames(fields))
	}
	if fields[0].Message != fields[1].Message {
		t.Fatalf("expected the two messages to be indistinguishable, got %q and %q",
			fields[0].Message, fields[1].Message)
	}
	if fields[0].Field == fields[1].Field {
		t.Errorf("both failures report field %q; the index is what disambiguates them", fields[0].Field)
	}
}

// Error() keeps rendering the joined human form, so every caller that only prints the
// error — genctl among them — is unaffected by the type change.
func TestValidationErrorStringJoinsMessages(t *testing.T) {
	err := fmtValidationErr(v.Struct(&ProcessDefinition{Tasks: []*Task{{ID: ""}}}))
	if got, want := err.Error(), "name is required; id is required"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// The detail has to survive the context prefixes handlers add — applyBatch wraps every
// per-definition failure with the process name — or errReply would drop it.
func TestValidationErrorSurvivesWrapping(t *testing.T) {
	err := fmtValidationErr(v.Struct(&ProcessDefinition{Name: "p", Tasks: []*Task{{ID: ""}}}))
	wrapped := fmt.Errorf("order_pipeline: %w", err)

	var ve *ValidationError
	if !errors.As(wrapped, &ve) {
		t.Fatal("wrapped error no longer unwraps to *ValidationError")
	}
	if len(ve.Fields) != 1 || ve.Fields[0].Field != "tasks[0].id" {
		t.Errorf("fields = %v, want [tasks[0].id]", fieldNames(ve.Fields))
	}
}
