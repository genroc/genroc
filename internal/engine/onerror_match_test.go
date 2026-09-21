package engine

import (
	"errors"
	"testing"

	"genroc/internal/errcode"
	"genroc/internal/model"
)

// The on_error contract a definition is written against: rules are tried top to bottom, a rule
// with no `code` catches anything, and where a rule carries a `case` BOTH selectors must hold —
// a false case falls through to the next rule rather than swallowing the error.
// docs guides/process-definition/error-handling.mdx.
func TestMatchOnError_SelectorsAndOrder(t *testing.T) {
	// The case strings are the fixture's own vocabulary, so a row states its selector's verdict
	// rather than an expression the matcher never parses.
	eval := func(c string) (bool, error) {
		switch c {
		case "yes":
			return true, nil
		case "no":
			return false, nil
		}
		return false, errors.New("unevaluable")
	}
	for _, tc := range []struct {
		name  string
		rules []model.ErrorCase
		code  errcode.Code
		want  string // the matched rule's goto, "" for no match
	}{
		{"first matching rule wins", []model.ErrorCase{
			{Code: []string{"http.404"}, Goto: "a"},
			{Code: []string{"http.%"}, Goto: "b"},
		}, "http.404", "a"},
		{"a later rule catches what an earlier one does not", []model.ErrorCase{
			{Code: []string{"http.404"}, Goto: "a"},
			{Code: []string{"http.%"}, Goto: "b"},
		}, "http.500", "b"},
		{"a rule with no code is a catch-all", []model.ErrorCase{
			{Goto: "a"},
		}, "result.parse", "a"},
		{"a case alone selects, with no code to match", []model.ErrorCase{
			{Case: "yes", Goto: "a"},
		}, "http.500", "a"},
		{"a case alone that declines catches nothing", []model.ErrorCase{
			{Case: "no", Goto: "a"},
		}, "http.500", ""},
		{"both selectors must hold", []model.ErrorCase{
			{Code: []string{"http.404"}, Case: "no", Goto: "a"},
		}, "http.404", ""},
		{"a declined rule falls through to the next", []model.ErrorCase{
			{Code: []string{"http.404"}, Case: "no", Goto: "a"},
			{Code: []string{"http.404"}, Goto: "b"},
		}, "http.404", "b"},
		// MatchCode is deliberately not SQL LIKE: '_' is a literal, so this names one code.
		{"underscore is literal, not a wildcard", []model.ErrorCase{
			{Code: []string{"http.5_0"}, Goto: "a"},
		}, "http.500", ""},
		{"an unmatched code catches nothing", []model.ErrorCase{
			{Code: []string{"http.404"}, Goto: "a"},
		}, "http.500", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := matchOnErrorWith(&model.Task{ID: "t", OnError: tc.rules}, tc.code, eval)
			if err != nil {
				t.Fatalf("matchOnErrorWith: %v", err)
			}
			switch {
			case tc.want == "" && got != nil:
				t.Errorf("%s caught by the rule going to %q; it must not be caught at all", tc.code, got.Goto)
			case tc.want != "" && got == nil:
				t.Errorf("%s was not caught; the rule going to %q should have taken it", tc.code, tc.want)
			case got != nil && got.Goto != tc.want:
				t.Errorf("%s routed to %q, want %q", tc.code, got.Goto, tc.want)
			}
		})
	}
}

// A case that cannot be evaluated is an error, never a quiet non-match: swallowing it would
// route the instance by a selector nobody computed.
func TestMatchOnError_UnevaluableCaseIsAnError(t *testing.T) {
	task := &model.Task{ID: "t", OnError: []model.ErrorCase{{Code: []string{"http.500"}, Case: "boom", Goto: "a"}}}
	if _, err := matchOnErrorWith(task, "http.500", eval500); err == nil {
		t.Error("an unevaluable case was treated as a non-match; it must surface as an error")
	}
}

func eval500(string) (bool, error) { return false, errors.New("unevaluable") }
