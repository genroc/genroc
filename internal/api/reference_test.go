package api

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/model"
)

// statusByCode already carries "every Code must appear here"; the prose needs the same rule and
// gets no help from the compiler. A code with no line reaches the reference as a blank cell,
// which reads as an omission in the docs rather than as the missing entry it is.
func TestEveryCodeIsDocumented(t *testing.T) {
	if len(statusByCode) < 5 {
		t.Fatalf("statusByCode carries %d codes, which cannot be right", len(statusByCode))
	}
	for _, c := range ReferenceCodes() {
		if c.Means == "" {
			t.Errorf("%q has a status but no meaning; add it to codeMeanings", c.Code)
		}
		if c.Status == 0 {
			t.Errorf("%q has no status", c.Code)
		}
	}
	for code := range codeMeanings {
		if _, ok := statusByCode[code]; !ok {
			t.Errorf("codeMeanings carries %q, which is not a Code any reply can hold", code)
		}
	}
}

// The status filter's values used to be a string in a struct tag beside the model, and a copy
// is how `cancelling` and `cancelled` went undocumented for as long as they did: a status added
// after the tag was written changed nothing that could fail. They are derived now, and this is
// what says so — a hand-written enum tag on that parameter would pass every other test.
func TestTheStatusFilterOffersEveryStatus(t *testing.T) {
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Enum []string `json:"enum"`
			} `json:"schemas"`
		} `json:"components"`
		// A path item carries `servers` beside its methods, so the methods cannot be decoded
		// as a uniform map -- /healthz has both.
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(Spec(), &doc); err != nil {
		t.Fatal(err)
	}
	var op struct {
		Parameters []struct {
			Name   string `json:"name"`
			Schema struct {
				Ref  string   `json:"$ref"`
				Enum []string `json:"enum"`
			} `json:"schema"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(doc.Paths["/instances"]["get"], &op); err != nil {
		t.Fatal(err)
	}
	var offered []string
	for _, p := range op.Parameters {
		if p.Name != "status" {
			continue
		}
		offered = p.Schema.Enum
		if len(offered) == 0 {
			offered = doc.Components.Schemas[strings.TrimPrefix(p.Schema.Ref, "#/components/schemas/")].Enum
		}
	}
	if len(offered) == 0 {
		t.Fatal("GET /instances documents no values for its status filter")
	}
	have := map[string]bool{}
	for _, s := range offered {
		have[s] = true
	}
	for _, info := range model.Statuses() {
		if !have[string(info.Status)] {
			t.Errorf("status %q exists but the filter does not offer it", info.Status)
		}
	}
	if len(offered) != len(model.Statuses()) {
		t.Errorf("the filter offers %d values and the model declares %d", len(offered), len(model.Statuses()))
	}
}

func TestActionNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range registry {
		if seen[a.Name] {
			t.Errorf("%q names two actions: the dispatcher reaches only the first, and the spec's operationIds collide", a.Name)
		}
		seen[a.Name] = true
	}
}
