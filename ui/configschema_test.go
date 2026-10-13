package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestConfigSchema_EveryFieldIsDescribed(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal(configSchema(), &root); err != nil {
		t.Fatal(err)
	}
	objects := map[string]any{"ui.yaml": root}
	for name, def := range root["$defs"].(map[string]any) {
		objects[name] = def
	}
	for name, obj := range objects {
		for field, p := range obj.(map[string]any)["properties"].(map[string]any) {
			if d, _ := p.(map[string]any)["description"].(string); strings.TrimSpace(d) == "" {
				t.Errorf("%s.%s has no description tag, so the reference shows an empty row", name, field)
			}
		}
	}
}

// A default tag is prose to the code; these are the ones the code can be asked about.
func TestConfigSchema_DefaultsAreTheOnesTheCodeApplies(t *testing.T) {
	tag := func(typ reflect.Type, yamlName string) string {
		for i := range typ.NumField() {
			if f := typ.Field(i); strings.Split(f.Tag.Get("yaml"), ",")[0] == yamlName {
				return f.Tag.Get("default")
			}
		}
		t.Fatalf("%s has no field %q", typ, yamlName)
		return ""
	}
	cfg, err := LoadConfig(writeConfig(t, "login:\n  passwords:\n    - {email: a@b.c, hash: \"$2a$10$x\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	session, _ := cfg.sessionTTL()
	token, _ := cfg.tokenTTL()
	for _, c := range []struct{ field, documented, applied string }{
		{"listen", tag(reflect.TypeFor[Config](), "listen"), cfg.Listen},
		{"token.issuer", tag(reflect.TypeFor[Token](), "issuer"), cfg.Token.Issuer},
		{"token.audience", tag(reflect.TypeFor[Token](), "audience"), cfg.Token.Audience},
		{"session_ttl", tag(reflect.TypeFor[Config](), "session_ttl"), session.String()},
		{"token.ttl", tag(reflect.TypeFor[Token](), "ttl"), token.String()},
	} {
		if d, err := time.ParseDuration(c.documented); err == nil {
			c.documented = d.String()
		}
		if c.documented != c.applied {
			t.Errorf("%s: the reference says the default is %q, the code applies %q", c.field, c.documented, c.applied)
		}
	}
}
