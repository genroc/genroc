package model

import (
	"strings"
	"testing"

	"genroc/internal/schema"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// cfgSchema parses a JSON schema for a test definition, panicking on error
// (test fixtures are constants).
func cfgSchema(src string) *schema.Schema {
	raw, err := schema.Parse([]byte(src))
	if err != nil {
		panic(err)
	}
	s := raw.AssumeNormalized()
	return &s
}

func TestResolveConfig(t *testing.T) {
	// Process "billing" → process-scoped keys GENROC_BILLING_<NAME>.
	def := &ProcessDefinition{
		Name: "billing",
		ConfigSchema: cfgSchema(`{
			"type": "object",
			"required": ["SERVER_URL"],
			"properties": {
				"SERVER_URL": {"type": "string"},
				"PORT":       {"type": "integer"},
				"RATE":       {"type": "number"},
				"DEBUG":      {"type": "boolean"},
				"REGION":     {"type": "string", "default": "us"}
			}
		}`),
	}

	t.Run("process-scoped coerces types and applies default", func(t *testing.T) {
		cfg, err := def.ResolveConfig(lookupFrom(map[string]string{
			"GENROC_BILLING_SERVER_URL": "http://x",
			"GENROC_BILLING_PORT":       "8080",
			"GENROC_BILLING_RATE":       "1.5",
			"GENROC_BILLING_DEBUG":      "true",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg["SERVER_URL"] != "http://x" {
			t.Errorf("SERVER_URL = %v", cfg["SERVER_URL"])
		}
		if cfg["PORT"] != int64(8080) {
			t.Errorf("PORT = %v (%T), want int64(8080)", cfg["PORT"], cfg["PORT"])
		}
		if cfg["RATE"] != 1.5 {
			t.Errorf("RATE = %v", cfg["RATE"])
		}
		if cfg["DEBUG"] != true {
			t.Errorf("DEBUG = %v", cfg["DEBUG"])
		}
		if cfg["REGION"] != "us" {
			t.Errorf("REGION = %v, want default us", cfg["REGION"])
		}
	})

	t.Run("falls back to GENROC_GLOBAL_ when no process-scoped var", func(t *testing.T) {
		cfg, err := def.ResolveConfig(lookupFrom(map[string]string{
			"GENROC_GLOBAL_SERVER_URL": "http://global",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg["SERVER_URL"] != "http://global" {
			t.Errorf("SERVER_URL = %v, want http://global", cfg["SERVER_URL"])
		}
	})

	t.Run("process-scoped overrides global", func(t *testing.T) {
		cfg, err := def.ResolveConfig(lookupFrom(map[string]string{
			"GENROC_GLOBAL_SERVER_URL":  "http://global",
			"GENROC_BILLING_SERVER_URL": "http://billing",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg["SERVER_URL"] != "http://billing" {
			t.Errorf("SERVER_URL = %v, want http://billing (process scope wins)", cfg["SERVER_URL"])
		}
	})

	t.Run("missing required in both tiers is rejected", func(t *testing.T) {
		_, err := def.ResolveConfig(lookupFrom(map[string]string{}))
		if err == nil || !strings.Contains(err.Error(), "GENROC_BILLING_SERVER_URL") || !strings.Contains(err.Error(), "GENROC_GLOBAL_SERVER_URL") {
			t.Fatalf("err = %v, want both keys named", err)
		}
	})

	t.Run("optional unset is omitted", func(t *testing.T) {
		cfg, err := def.ResolveConfig(lookupFrom(map[string]string{
			"GENROC_GLOBAL_SERVER_URL": "http://x",
		}))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, ok := cfg["PORT"]; ok {
			t.Errorf("PORT should be omitted when unset, got %v", cfg["PORT"])
		}
	})

	t.Run("uncoercible values are rejected", func(t *testing.T) {
		for _, tc := range []struct{ key, val string }{
			{"GENROC_GLOBAL_PORT", "notanint"},
			{"GENROC_GLOBAL_RATE", "notanumber"},
			{"GENROC_GLOBAL_DEBUG", "maybe"},
		} {
			_, err := def.ResolveConfig(lookupFrom(map[string]string{
				"GENROC_GLOBAL_SERVER_URL": "http://x",
				tc.key:                     tc.val,
			}))
			if err == nil {
				t.Errorf("%s=%q: expected coercion error", tc.key, tc.val)
			}
		}
	})

	t.Run("nil config_schema resolves empty", func(t *testing.T) {
		cfg, err := (&ProcessDefinition{Name: "p"}).ResolveConfig(lookupFrom(nil))
		if err != nil || len(cfg) != 0 {
			t.Fatalf("cfg=%v err=%v, want empty/nil", cfg, err)
		}
	})
}

// enum (a scalar constraint) is enforced by validating the assembled object
// against config_schema after coercion.
func TestResolveConfigEnum(t *testing.T) {
	def := &ProcessDefinition{
		Name: "p",
		ConfigSchema: cfgSchema(`{
			"type": "object",
			"required": ["ENV"],
			"properties": {"ENV": {"type": "string", "enum": ["dev", "prod"]}}
		}`),
	}
	if _, err := def.ResolveConfig(lookupFrom(map[string]string{"GENROC_GLOBAL_ENV": "prod"})); err != nil {
		t.Fatalf("prod should be valid: %v", err)
	}
	if _, err := def.ResolveConfig(lookupFrom(map[string]string{"GENROC_GLOBAL_ENV": "staging"})); err == nil {
		t.Fatal("staging should be rejected by enum")
	}
}

// Process names that aren't env-safe are normalized to an UPPER_SNAKE token.
func TestResolveConfigProcessNameNormalization(t *testing.T) {
	def := &ProcessDefinition{
		Name:         "order-flow.v2",
		ConfigSchema: cfgSchema(`{"type":"object","required":["URL"],"properties":{"URL":{"type":"string"}}}`),
	}
	cfg, err := def.ResolveConfig(lookupFrom(map[string]string{
		"GENROC_ORDER_FLOW_V2_URL": "http://x",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg["URL"] != "http://x" {
		t.Errorf("URL = %v, want http://x (process name → ORDER_FLOW_V2)", cfg["URL"])
	}
}

// A lowercase / camelCase config name binds to the UPPER_SNAKE environment
// variable, while the declared name remains the key in the config namespace.
func TestResolveConfigCaseInsensitiveName(t *testing.T) {
	def := &ProcessDefinition{
		Name: "billing",
		ConfigSchema: cfgSchema(`{
			"type": "object",
			"required": ["server_url"],
			"properties": {
				"server_url": {"type": "string"},
				"e2e_port":   {"type": "integer"},
				"apiKey":     {"type": "string"}
			}
		}`),
	}
	cfg, err := def.ResolveConfig(lookupFrom(map[string]string{
		"GENROC_GLOBAL_SERVER_URL": "http://x",
		"GENROC_BILLING_E2E_PORT":  "8080",
		"GENROC_GLOBAL_API_KEY":    "k",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg["server_url"] != "http://x" {
		t.Errorf("server_url = %v, want http://x (binds to GENROC_GLOBAL_SERVER_URL)", cfg["server_url"])
	}
	if cfg["e2e_port"] != int64(8080) {
		t.Errorf("e2e_port = %v, want 8080 (binds to GENROC_BILLING_E2E_PORT)", cfg["e2e_port"])
	}
	if cfg["apiKey"] != "k" {
		t.Errorf("apiKey = %v, want k (binds to GENROC_GLOBAL_API_KEY)", cfg["apiKey"])
	}
}

func TestEnvToken(t *testing.T) {
	cases := map[string]string{
		// already snake / upper
		"billing":    "BILLING",
		"server_url": "SERVER_URL",
		"SERVER_URL": "SERVER_URL",
		// non-alphanumerics collapse to a single '_'
		"order-flow": "ORDER_FLOW",
		"billing.v2": "BILLING_V2",
		"My Proc":    "MY_PROC",
		"a--b":       "A_B",
		// camelCase / PascalCase humps split
		"apiKey":      "API_KEY",
		"serverUrl":   "SERVER_URL",
		"URLPath":     "URL_PATH",
		"apiURL":      "API_URL",
		"oauth2Token": "OAUTH2_TOKEN",
	}
	for in, want := range cases {
		if got := envToken(in); got != want {
			t.Errorf("envToken(%q) = %q, want %q", in, got, want)
		}
	}
}

// A coercion failure on a secret var must not echo the value into the error.
func TestResolveConfigRedactsSecretInError(t *testing.T) {
	def := &ProcessDefinition{
		Name: "p",
		ConfigSchema: cfgSchema(`{
			"type": "object",
			"required": ["API_KEY"],
			"properties": {"API_KEY": {"type": "number", "secret": true}}
		}`),
	}
	_, err := def.ResolveConfig(lookupFrom(map[string]string{"GENROC_GLOBAL_API_KEY": "supersecret"}))
	if err == nil {
		t.Fatal("expected a coercion error")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Errorf("secret value leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("expected redacted value in error, got: %v", err)
	}
}

// A non-secret var's bad value is shown to aid debugging.
func TestResolveConfigShowsNonSecretInError(t *testing.T) {
	def := &ProcessDefinition{
		Name:         "p",
		ConfigSchema: cfgSchema(`{"type":"object","required":["PORT"],"properties":{"PORT":{"type":"integer"}}}`),
	}
	_, err := def.ResolveConfig(lookupFrom(map[string]string{"GENROC_GLOBAL_PORT": "abc"}))
	if err == nil || !strings.Contains(err.Error(), "abc") {
		t.Errorf("non-secret value should be shown for debugging, got: %v", err)
	}
}

func TestSecretConfigValues(t *testing.T) {
	def := &ProcessDefinition{
		ConfigSchema: cfgSchema(`{
			"type": "object",
			"properties": {
				"API_KEY":    {"type": "string", "secret": true},
				"SERVER_URL": {"type": "string"}
			}
		}`),
	}
	secrets := def.SecretConfigValues(map[string]any{"API_KEY": "s3cr3t", "SERVER_URL": "http://x"})
	if len(secrets) != 1 || secrets[0] != "s3cr3t" {
		t.Errorf("SecretConfigValues = %v, want [s3cr3t]", secrets)
	}
}

func TestValidateConfigSchema(t *testing.T) {
	tests := []struct {
		name    string
		cs      *schema.Schema
		wantErr string
	}{
		{"nil is ok", nil, ""},
		{"valid flat object", cfgSchema(`{"type":"object","required":["OK"],"properties":{"OK":{"type":"integer"}}}`), ""},
		{"not an object", cfgSchema(`{"type":"string"}`), `must be type "object"`},
		{"nested object property", cfgSchema(`{"type":"object","properties":{"X":{"type":"object","properties":{"a":{"type":"string"}}}}}`), "unsupported type"},
		{"scalar with nested structure", cfgSchema(`{"type":"object","properties":{"X":{"type":"string","items":{"type":"string"}}}}`), "primitive value"},
		// A config value arrives as an env string, so only the four primitives can be
		// parsed out of one.
		{"unsupported type", cfgSchema(`{"type":"object","properties":{"X":{"type":"date"}}}`), "unsupported type"},
		{"non-primitive type", cfgSchema(`{"type":"object","properties":{"X":{"type":"array"}}}`), "unsupported type"},
		// An unknown ({}) config var is meaningless for the same reason: it declares no
		// type at all, so it lands on the single-primitive-type rule.
		{"unknown type", cfgSchema(`{"type":"object","properties":{"X":{}}}`), "single primitive type"},
		{"combinator", cfgSchema(`{"type":"object","oneOf":[{"type":"string"}]}`), "oneOf/anyOf/allOf"},
		{"required unknown property", cfgSchema(`{"type":"object","required":["NOPE"],"properties":{"X":{"type":"string"}}}`), "unknown property"},
		{"required with default", cfgSchema(`{"type":"object","required":["X"],"properties":{"X":{"type":"string","default":"a"}}}`), "cannot be both required and have a default"},
		{"invalid name", cfgSchema(`{"type":"object","properties":{"bad-name":{"type":"string"}}}`), "valid identifier"},
		{"env-key collision", cfgSchema(`{"type":"object","properties":{"server_url":{"type":"string"},"SERVER_URL":{"type":"string"}}}`), "same environment variable suffix"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfigSchema(tt.cs)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}
