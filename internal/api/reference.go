package api

// What the docs site needs from the registry that the OpenAPI document does not carry. Both
// come off the same `registry`, so the two reads cannot disagree; the second exists because
// the spec answers a different question. specs/docs-site.md.

import (
	"encoding/json"
	"net/http"
)

// ReferenceAction is one endpoint's documentation-only surface.
type ReferenceAction struct {
	Method string `json:"method"`
	Path   string `json:"path"`

	// Permissions that admit the call, ANY one of them sufficing. Empty means admin-only,
	// which is the registry's fail-closed default rather than an omission; Open marks the
	// probe that needs none. Nothing in the spec says this — it has no vocabulary for a
	// permission that is not an auth scheme.
	Permissions []string `json:"permissions"`
	Open        bool     `json:"open"`

	// The registry's example values, marshalled. The spec reflects zero values instead
	// (openapi.go's zeroOf, and the reason is there), so these exist nowhere else.
	Request  json.RawMessage `json:"request,omitempty"`
	Response json.RawMessage `json:"response,omitempty"`
}

// Reference returns every action in registry order, which is also the order the spec lists them.
func Reference() []ReferenceAction {
	out := make([]ReferenceAction, 0, len(registry))
	for _, a := range registry {
		ref := ReferenceAction{Method: a.Method, Path: a.Path, Open: a.Open}
		for _, p := range a.Allow {
			ref.Permissions = append(ref.Permissions, string(p))
		}
		// A GET carries no body, and the registry's Req there is the query struct's
		// stand-in rather than something a caller sends.
		if a.Method != http.MethodGet {
			ref.Request = exampleJSON(a.Req)
		}
		ref.Response = exampleJSON(a.Resp)
		out = append(out, ref)
	}
	return out
}

// exampleJSON renders an example, or nothing when the registry entry carries only a type. A
// zero-valued struct marshals to a shape with every field null, which reads as a claim about
// what an endpoint returns rather than as the absence of an example.
func exampleJSON(v any) json.RawMessage {
	if v == nil {
		return nil
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil || len(b) == 0 {
		return nil
	}
	switch string(b) {
	case "null", "{}", "[]", `""`, "0":
		return nil
	}
	return b
}
