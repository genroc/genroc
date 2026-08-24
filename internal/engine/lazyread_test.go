package engine

import (
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"genroc/internal/db"
	"genroc/internal/expression"
	"genroc/internal/idgen"
	"genroc/internal/model"
	"genroc/internal/shape"
)

// Go rather than e2e, and for a reason the HTTP surface cannot supply: whether a value was
// LOADED is invisible in the result. Content addressing gives a copied reference and a
// re-hashed one the same hash, so an API assertion passes either way. inst.ResolvedObjects is
// the memo the load populates, and it is the only place the difference shows.
// specs/lazy-context.md.

func lazyEngine(t *testing.T) (*db.DB, *Engine) {
	t.Helper()
	database, err := db.OpenSQLite(filepath.Join(t.TempDir(), "lazy.db"), "OFF")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database, New(database, time.Second, 1, false, time.Minute, 30*time.Second,
		LogConfig{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// storedContext saves an instance with ctxData and reads it back, so every oversized leaf
// carries the marker an advance would find. Real objects, real decode -- the fixture supplies
// only the shape.
func storedContext(t *testing.T, database *db.DB, ctxData map[string]any) *model.ProcessInstance {
	t.Helper()
	inst := &model.ProcessInstance{
		ID:             idgen.New(),
		ProcessName:    "lazy",
		ProcessVersion: 1,
		Task:           "b",
		ContextData:    ctxData,
		Status:         model.StatusRunning,
		CreatedAt:      time.Now(),
	}
	if err := database.SaveInstance(inst); err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	reloaded, err := database.GetInstance(inst.ID)
	if err != nil {
		t.Fatalf("GetInstance: %v", err)
	}
	return reloaded
}

// storedWithBigOutput is storedContext for the one shape the load-count tests need.
func storedWithBigOutput(t *testing.T, database *db.DB, big string) *model.ProcessInstance {
	t.Helper()
	inst := storedContext(t, database, map[string]any{
		"outputs": map[string]any{"a": map[string]any{"kept": big, "n": float64(1)}},
	})
	outs := inst.ContextData["outputs"].(map[string]any)["a"].(map[string]any)
	if _, isRef := outs["kept"].(*model.ObjectRef); !isRef {
		t.Fatalf("setup: kept = %T, want an externalized marker (raise the fixture size)", outs["kept"])
	}
	return inst
}

func rootsOf(t *testing.T, raw any) expression.Roots {
	t.Helper()
	sh := shape.Shape{Raw: raw}
	r, err := sh.Roots()
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	return r
}

func TestBuildEnv_CopyingASlotNeverLoadsIt(t *testing.T) {
	big := strings.Repeat("B", 4096)
	database, e := lazyEngine(t)
	inst := storedWithBigOutput(t, database, big)

	// `{final: "$: outputs.a"}` places the value; it never reads into it.
	env, err := e.buildEnv(inst, nil, rootsOf(t, map[string]any{"final": "$: outputs.a"}))
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	if n := len(inst.ResolvedObjects); n != 0 {
		t.Errorf("loaded %d objects for a slot the expression only copies; the marker should have travelled into the result and on into the next write", n)
	}
	got := env["outputs"].(map[string]any)["a"].(map[string]any)["kept"]
	if _, isRef := got.(*model.ObjectRef); !isRef {
		t.Errorf("env carries %T, want the marker: materializing here is what made a copy cost a load, a re-marshal and a re-hash", got)
	}
}

func TestBuildEnv_ReadingIntoASlotLoadsIt(t *testing.T) {
	big := strings.Repeat("B", 4096)
	database, e := lazyEngine(t)
	inst := storedWithBigOutput(t, database, big)

	// `{final: "$: outputs.a.kept"}` walks into the slot, so it must be materialized -- leaving
	// a marker here would hand one to whatever reads `final`.
	env, err := e.buildEnv(inst, nil, rootsOf(t, map[string]any{"final": "$: outputs.a.kept"}))
	if err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	if n := len(inst.ResolvedObjects); n != 1 {
		t.Fatalf("loaded %d objects, want 1: a read through the slot has to materialize it", n)
	}
	if got := env["outputs"].(map[string]any)["a"].(map[string]any)["kept"]; got != big {
		t.Errorf("env carries %T, want the loaded value", got)
	}
}

// A sibling read must not drag the big leaf in with it -- the whole point of cutting leaf by
// leaf rather than whole slots.
func TestBuildEnv_ReadingASiblingLeavesTheBigLeafAlone(t *testing.T) {
	big := strings.Repeat("B", 4096)
	database, e := lazyEngine(t)
	inst := storedWithBigOutput(t, database, big)

	if _, err := e.buildEnv(inst, nil, rootsOf(t, map[string]any{"final": "$: outputs.a.n"})); err != nil {
		t.Fatalf("buildEnv: %v", err)
	}
	// PINS THE DEFERRED HALF, and is meant to fail when it lands: reading any path through the
	// slot materializes the whole slot, so a sibling read still pays for the big leaf. Making
	// this 0 is what path-level laziness buys. specs/lazy-context.md.
	if n := len(inst.ResolvedObjects); n != 1 {
		t.Errorf("loaded %d objects reading outputs.a.n; if this is 0, path-level laziness has landed and this test should assert it instead", n)
	}
}
