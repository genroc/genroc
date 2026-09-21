package engine

import (
	"encoding/json"
	"testing"

	"genroc/internal/model"
)

// TestEvalConfigNamespace verifies the resolved config map is reachable in
// expressions under the "config" namespace.
func TestEvalConfigNamespace(t *testing.T) {
	ctx := map[string]any{"input": nil, "outputs": map[string]any{}}
	config := map[string]any{"flag": true, "url": "http://x", "port": int64(8080)}

	cases := []struct {
		expr string
		want bool
	}{
		{"config.flag == true", true},
		{`config.url == "http://x"`, true},
		{"config.port == 8080", true},
		{"config.missing == null", true},
	}
	for _, tc := range cases {
		got, err := evalBool(tc.expr, ctx, config, nil)
		if err != nil {
			t.Fatalf("evalBool(%q): %v", tc.expr, err)
		}
		if got != tc.want {
			t.Errorf("evalBool(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

// TestEvalNilConfig ensures a nil config does not break expression evaluation:
// referencing config.* yields nil rather than erroring.
func TestEvalNilConfig(t *testing.T) {
	ctx := map[string]any{"input": nil, "outputs": map[string]any{}}
	got, err := evalBool("config.anything == null", ctx, nil, nil)
	if err != nil {
		t.Fatalf("evalBool with nil config: %v", err)
	}
	if !got {
		t.Errorf("config.anything should be nil when config is nil")
	}
}

// Config is never persisted: it is re-resolved from the OS environment at the start of every
// tick, so a value that changes between ticks is picked up by the next one.
// docs reference/definition/expressions.mdx.
func TestConfigReResolvedEveryTick(t *testing.T) {
	database := openTestDB(t)
	eng := tickEngine(t, database)

	var def model.ProcessDefinition
	if err := json.Unmarshal([]byte(`{
		"name": "cfgtick",
		"config_schema": {"type":"object","required":["token"],"properties":{"token":{"type":"string"}}},
		"tasks": [{"id":"work","switch":"end"}]
	}`), &def); err != nil {
		t.Fatalf("unmarshal definition: %v", err)
	}
	if err := database.SaveDefinition(&def, 1, nil, "cfgtick-hash", "", ""); err != nil {
		t.Fatalf("SaveDefinition: %v", err)
	}
	inst := &model.ProcessInstance{
		ID: database.NextID(), ProcessName: "cfgtick", ProcessVersion: 1,
		Task: "work", State: map[string]any{}, Status: model.StatusRunning,
	}
	if err := database.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}

	for _, want := range []string{"first", "second"} {
		t.Setenv("GENROC_CFGTICK_TOKEN", want)
		if _, _, outcome := eng.prepareAdvance(inst); outcome != nil {
			t.Fatalf("prepareAdvance stopped the instance with GENROC_CFGTICK_TOKEN=%q", want)
		}
		if got := inst.Config["token"]; got != want {
			t.Errorf("config.token = %v, want %q — config must be re-read from the environment each tick, not carried over from the last one", got, want)
		}
	}
}
