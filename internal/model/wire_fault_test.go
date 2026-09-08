package model

import (
	"strings"
	"testing"
)

// A raise clause is authored for the moment it fires, so a key that silently does nothing is
// found at the worst possible time. `switch` and `on_error` around it already reject; Fault
// did not, and the published JSON Schema could not close it without disagreeing with the
// server. specs/language-server.md §5.
func TestARaiseClauseRejectsAnUnknownKey(t *testing.T) {
	var f Fault
	err := f.UnmarshalJSON([]byte(`{"code":"c","message":"m","mesage":"typo"}`))
	if err == nil {
		t.Fatal("a misspelled key in a raise clause was accepted")
	}
	if !strings.Contains(err.Error(), "mesage") {
		t.Errorf("the error must name the key that has no home, got: %v", err)
	}
}

func TestARaiseClauseStillAcceptsItsOwnFields(t *testing.T) {
	var f Fault
	if err := f.UnmarshalJSON([]byte(`{"code":"c","message":"m","data":"$: 1"}`)); err != nil {
		t.Fatalf("code/message/data are the clause's own fields: %v", err)
	}
	if f.Code != "c" || f.Message != "m" || f.Data == nil {
		t.Errorf("fields lost through the strict decode: %+v", f)
	}
}
