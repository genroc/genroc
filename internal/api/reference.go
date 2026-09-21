package api

// What the docs site needs from the registry that the OpenAPI document does not carry. Both
// come off the same `registry`, so the two reads cannot disagree; the second exists because
// the spec answers a different question. specs/docs-site.md.

import (
	"encoding/json"
	"net/http"
	"sort"
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

// ReferenceCode is one API error code as a reader meets it: the classification in the body, the
// status it renders as, and one line on what it means. The prose is data rather than only a doc
// comment because a doc comment reaches Go readers and nobody else, and this set is something a
// client written in any language has to handle.
type ReferenceCode struct {
	Code   Code   `json:"code"`
	Status int    `json:"status"`
	Means  string `json:"means"`
}

// codeMeanings is the user-facing line per code. Every Code must appear here, the same rule
// statusByCode carries and for the same reason; TestEveryCodeIsDocumented enforces it.
var codeMeanings = map[Code]string{
	CodeInvalid:         "the request is malformed or unacceptable — retrying it unchanged will never succeed",
	CodeNotFound:        "the definition, instance, channel or task named does not exist",
	CodeConflict:        "the request is well-formed and the target exists, but its current state forbids the operation; the identical request may succeed later",
	CodeUnsupported:     "the endpoint exists, but this server is not configured to serve it",
	CodeUnavailable:     "this server cannot serve requests right now, its database being unreachable — route elsewhere and retry",
	CodeUnauthenticated: "no identity was established",
	CodeForbidden:       "the caller is known and lacks the permission this action needs",
	CodeInternal:        "anything unclassified, which is a server fault until proven otherwise",
}

// ReferenceCodes returns every error code, ordered by status then name so the table reads as a
// progression rather than as whatever order a map produced.
func ReferenceCodes() []ReferenceCode {
	out := make([]ReferenceCode, 0, len(statusByCode))
	for code, status := range statusByCode {
		out = append(out, ReferenceCode{Code: code, Status: status, Means: codeMeanings[code]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Status != out[j].Status {
			return out[i].Status < out[j].Status
		}
		return out[i].Code < out[j].Code
	})
	return out
}
