package validationtest

import (
	"strings"
	"testing"
)

// A config secret interpolated into the process output taints that output field
// in the inferred schema (so it can be redacted at the public boundary).
func TestSecretFlowsToOutput(t *testing.T) {
	out := runGenerate(t, `{
		"name": "p",
		"config_schema": {
			"type": "object",
			"required": ["api_key"],
			"properties": { "api_key": { "type": "string", "secret": true } }
		},
		"tasks": [
			{ "id": "t", "action": { "type": "fetch", "method": "post", "url": "http://x" }, "switch": "end" }
		],
		"output": { "token": "$: config.api_key", "name": "static" }
	}`)
	def := defOf(out, "output")
	if def.IsZero() {
		t.Fatalf("no \"output\" def; have %v", defKeys(out))
	}
	if def.Properties()["token"].IsZero() || !def.Properties()["token"].IsSecret() {
		t.Errorf("output.token should be marked secret, got %+v", def.Properties()["token"])
	}
	if !def.Properties()["name"].IsZero() && def.Properties()["name"].IsSecret() {
		t.Errorf("output.name should not be secret")
	}
}

// An optional config var (not required, no default) is nullable, so interpolating
// it into a rest endpoint URL is rejected at registration.
func TestConfigNullableEndpointRejected(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p",
		"config_schema": {
			"type": "object",
			"properties": { "server_url": { "type": "string" } }
		},
		"tasks": [
			{
				"id": "call",
				"action": { "type": "fetch", "method": "post", "url": "${ config.server_url }/second" },
				"switch": "end"
			}
		]
	}`)
	if err == nil || !strings.Contains(err.Error(), "may be null") {
		t.Fatalf("err = %v, want an error containing 'may be null'", err)
	}
}

// A required config var is guaranteed present (non-null), so the same endpoint is
// fine.
func TestConfigRequiredEndpointOK(t *testing.T) {
	runGenerate(t, `{
		"name": "p",
		"config_schema": {
			"type": "object",
			"required": ["server_url"],
			"properties": { "server_url": { "type": "string" } }
		},
		"tasks": [
			{
				"id": "call",
				"action": { "type": "fetch", "method": "post", "url": "${ config.server_url }/second" },
				"switch": "end"
			}
		]
	}`)
}

// A nullable config var interpolated into a header value is rejected too.
func TestConfigNullableHeaderRejected(t *testing.T) {
	err := runGenerateErr(t, `{
		"name": "p",
		"config_schema": {
			"type": "object",
			"properties": { "api_key": { "type": "string" } }
		},
		"tasks": [
			{
				"id": "call",
				"action": {
					"type": "fetch",
					"method": "post",
					"url": "http://x",
					"headers": { "Authorization": "Bearer ${ config.api_key }" }
				},
				"switch": "end"
			}
		]
	}`)
	if err == nil || !strings.Contains(err.Error(), "may be null") || !strings.Contains(err.Error(), "Authorization") {
		t.Fatalf("err = %v, want an error naming the Authorization header and 'may be null'", err)
	}
}

// A required config var in a header value is fine.
func TestConfigRequiredHeaderOK(t *testing.T) {
	runGenerate(t, `{
		"name": "p",
		"config_schema": {
			"type": "object",
			"required": ["api_key"],
			"properties": { "api_key": { "type": "string" } }
		},
		"tasks": [
			{
				"id": "call",
				"action": {
					"type": "fetch",
					"method": "post",
					"url": "http://x",
					"headers": { "Authorization": "Bearer ${ config.api_key }" }
				},
				"switch": "end"
			}
		]
	}`)
}

// A property with a default is always present, so it is non-null in the endpoint.
func TestConfigDefaultedEndpointOK(t *testing.T) {
	runGenerate(t, `{
		"name": "p",
		"config_schema": {
			"type": "object",
			"properties": { "server_url": { "type": "string", "default": "http://localhost" } }
		},
		"tasks": [
			{
				"id": "call",
				"action": { "type": "fetch", "method": "post", "url": "${ config.server_url }/second" },
				"switch": "end"
			}
		]
	}`)
}

// An optional config var can still be used if the expression supplies a default
// with ??, making it non-null.
func TestConfigNullableEndpointWithCoalesceOK(t *testing.T) {
	runGenerate(t, `{
		"name": "p",
		"config_schema": {
			"type": "object",
			"properties": { "server_url": { "type": "string" } }
		},
		"tasks": [
			{
				"id": "call",
				"action": { "type": "fetch", "method": "post", "url": "${ config.server_url ?? \"http://localhost\" }/second" },
				"switch": "end"
			}
		]
	}`)
}
