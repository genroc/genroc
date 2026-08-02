package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"genroc/internal/errcode"
	"genroc/internal/model"
)

func TestEvaluator(t *testing.T) {
	tests := []struct {
		expr    string
		ctx     map[string]interface{}
		want    bool
		wantErr bool
	}{
		{"outputs.task.ok == true", map[string]interface{}{"outputs": map[string]any{"task": map[string]any{"ok": true}}}, true, false},
		{"outputs.task.ok == true", map[string]interface{}{"outputs": map[string]any{"task": map[string]any{"ok": false}}}, false, false},
		{"outputs.task.amount > 100", map[string]interface{}{"outputs": map[string]any{"task": map[string]any{"amount": 200}}}, true, false},
		{"outputs.task.amount > 100", map[string]interface{}{"outputs": map[string]any{"task": map[string]any{"amount": 50}}}, false, false},
		{"input.a == true && input.b == true", map[string]interface{}{"input": map[string]any{"a": true, "b": true}}, true, false},
		{"input.a == true && input.b == true", map[string]interface{}{"input": map[string]any{"a": true, "b": false}}, false, false},
		{"invalid %%% expr", nil, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got, err := evalBool(tt.expr, tt.ctx, nil, nil)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if got != tt.want {
				t.Errorf("evalBool(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestEvaluator_EvalBool_WithSelf(t *testing.T) {
	ctx := map[string]any{"outputs": map[string]any{}, "input": map[string]any{}}

	tests := []struct {
		name string
		expr string
		self any
		want bool
	}{
		{"self field true", "self.paid == true", map[string]any{"paid": true}, true},
		{"self field false", "self.paid == true", map[string]any{"paid": false}, false},
		{"self nested field", "self.result.ok == true", map[string]any{"result": map[string]any{"ok": true}}, true},
		{"self nil when no action", "self == null", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evalBool(tt.expr, ctx, nil, tt.self)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("evalBool(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

// Both non-single arities are unreachable through registration, so they are tested here
// directly. They stay reachable via the decoder, which runs over stored rows that never
// re-validate — and both would otherwise degrade silently rather than fail.
func TestDelayArity(t *testing.T) {
	tests := []struct {
		name    string
		spec    model.DelaySpec
		wantErr string
	}{
		{"for only", model.DelaySpec{For: "2h30m"}, ""},
		{"until only", model.DelaySpec{Until: "+2d 08:00"}, ""},
		// A row carrying only the removed `ms` decodes to this: Action takes no
		// DisallowUnknownFields, so `ms` is dropped and both slots are absent.
		{"neither", model.DelaySpec{}, "no delay set"},
		// Preferring `for` here would wait a fraction of the intended time if `until` held
		// the real deadline, with nothing reporting it.
		{"both", model.DelaySpec{For: "1h", Until: "+1d 08:00"}, "both"},
		// Zero is a real value, not absence — it must not be mistaken for an unset slot.
		{"explicit zero for", model.DelaySpec{For: float64(0)}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := delayArity(tt.spec)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDelayMillis(t *testing.T) {
	tests := []struct {
		name    string
		v       any
		want    int64
		wantErr bool
	}{
		{"int", 30000, 30000, false},
		{"int64", int64(5000), 5000, false},
		{"float64", float64(3000), 3000, false},
		{"json.Number", json.Number("250"), 250, false},
		{"negative passes through", int64(-5), -5, false},
		// A numeric string was the old `ms` spelling. It is now a literal handled by the
		// delayspec grammar (which rejects it as unitless), so it must not coerce here.
		{"numeric string", "30000", 0, true},
		{"non-numeric string", "abc", 0, true},
		{"fractional json.Number", json.Number("1.5"), 0, true},
		{"bool", true, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := delayMillis(tt.v)
			if tt.wantErr {
				if err == nil {
					t.Errorf("delayMillis(%#v) = %d, want error", tt.v, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("delayMillis(%#v) unexpected error: %v", tt.v, err)
			}
			if got != tt.want {
				t.Errorf("delayMillis(%#v) = %d, want %d", tt.v, got, tt.want)
			}
		})
	}
}

func TestIsRetryAllowed(t *testing.T) {
	bp := func(b bool) *bool { return &b }

	tests := []struct {
		name     string
		onlyOnce *bool
		errCode  errcode.Code
		matched  *model.ErrorCase
		want     bool
	}{
		// only_once nil / false — no restriction
		{"nil only_once allows http.500", nil, "http.500", nil, true},
		{"nil only_once allows any code", nil, "output.invalid", nil, true},
		{"false only_once allows http.500", bp(false), "http.500", nil, true},

		// only_once true — pre.* is always allowed
		{"true: pre.error allowed", bp(true), "pre.error", nil, true},
		{"true: pre.timeout allowed", bp(true), "pre.timeout", nil, true},
		{"true: pre.exec allowed", bp(true), "pre.exec", nil, true},
		{"true: pre.anything allowed", bp(true), "pre.whatever", nil, true},

		// only_once true — non-pre.* blocked without override
		{"true: http.500 blocked", bp(true), "http.500", nil, false},
		{"true: http.timeout blocked", bp(true), "http.timeout", nil, false},
		{"true: output.invalid blocked", bp(true), "output.invalid", nil, false},
		{"true: child.failed blocked", bp(true), "child.failed", nil, false},

		// only_once true — not_reached:true overrides any error code
		{"true + not_reached:true allows http.422", bp(true), "http.422", &model.ErrorCase{NotReached: bp(true)}, true},
		{"true + not_reached:true allows http.500", bp(true), "http.500", &model.ErrorCase{NotReached: bp(true)}, true},
		{"true + not_reached:true allows output.invalid", bp(true), "output.invalid", &model.ErrorCase{NotReached: bp(true)}, true},

		// only_once true — not_reached:false does not override
		{"true + not_reached:false still allows pre.error", bp(true), "pre.error", &model.ErrorCase{NotReached: bp(false)}, true},
		{"true + not_reached:false still blocks http.500", bp(true), "http.500", &model.ErrorCase{NotReached: bp(false)}, false},

		// only_once true — nil matched (no on_error rule matched)
		{"true + nil matched + pre.error allowed", bp(true), "pre.error", nil, true},
		{"true + nil matched + http.500 blocked", bp(true), "http.500", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := &model.Task{ID: "s", OnlyOnce: tt.onlyOnce,
				Action: &model.Action{Type: model.ActionTypeFetch, URL: "http://x"}}
			got := isRetryAllowed(task, tt.errCode, tt.matched)
			if got != tt.want {
				t.Errorf("isRetryAllowed(%q) = %v, want %v", tt.errCode, got, tt.want)
			}
		})
	}
}
