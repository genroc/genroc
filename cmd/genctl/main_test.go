package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"genroc/internal/idgen"

	"github.com/google/uuid"
)

func toJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestInferScalar(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"true", true},
		{"false", false},
		{"null", nil},
		// Numbers keep their literal rather than converting through int64/float64,
		// so a value past either range survives; see TestSetScalar* in
		// yamlnum_test.go.
		{"3", json.Number("3")},
		{"-7", json.Number("-7")},
		{"1.5", json.Number("1.5")},
		{"hello", "hello"},
		{"007abc", "007abc"},
		{"", ""},
	}
	for _, c := range cases {
		if got := inferScalar(c.in); got != c.want {
			t.Errorf("inferScalar(%q) = %v (%T), want %v (%T)", c.in, got, got, c.want, c.want)
		}
	}
}

func TestApplySetNesting(t *testing.T) {
	m := map[string]any{}
	for _, kv := range []string{"name=Sam", "user.age=30", "user.admin=true"} {
		if err := applySet(m, kv); err != nil {
			t.Fatalf("applySet(%q): %v", kv, err)
		}
	}
	if got, want := toJSON(t, m), `{"name":"Sam","user":{"admin":true,"age":30}}`; got != want {
		t.Errorf("nested set:\n got %s\nwant %s", got, want)
	}

	// Missing '=' and a dotted path through a non-object are reported.
	if err := applySet(map[string]any{}, "novalue"); err == nil {
		t.Error("expected error for missing '='")
	}
	clash := map[string]any{"a": "scalar"}
	if err := applySet(clash, "a.b=1"); err == nil {
		t.Error("expected error setting through a non-object")
	}
}

func TestBuildInput(t *testing.T) {
	// Neither source nor --set: value is absent.
	if v, present, err := buildInput("", "", nil); err != nil || present || v != nil {
		t.Errorf("buildInput(empty) = %v, present=%v, err=%v", v, present, err)
	}

	// Relaxed JSON literal (unquoted keys, bare values).
	v, present, err := buildInput("{name: Sam, count: 3}", "", nil)
	if err != nil || !present {
		t.Fatalf("relaxed input: present=%v err=%v", present, err)
	}
	if got, want := toJSON(t, v), `{"count":3,"name":"Sam"}`; got != want {
		t.Errorf("relaxed input:\n got %s\nwant %s", got, want)
	}

	// --set overrides a field from the literal and adds a new one.
	v, _, err = buildInput("{count: 1}", "", []string{"count=2", "active=true"})
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	if got, want := toJSON(t, v), `{"active":true,"count":2}`; got != want {
		t.Errorf("override:\n got %s\nwant %s", got, want)
	}

	// --set with no base builds an object from scratch.
	v, _, err = buildInput("", "", []string{"x.y=1"})
	if err != nil {
		t.Fatalf("set-only: %v", err)
	}
	if got, want := toJSON(t, v), `{"x":{"y":1}}`; got != want {
		t.Errorf("set-only:\n got %s\nwant %s", got, want)
	}

	// --set requires the base to be an object.
	if _, _, err := buildInput("5", "", []string{"a=1"}); err == nil {
		t.Error("expected error: --set on a non-object base")
	}

	// The literal and file sources are mutually exclusive.
	if _, _, err := buildInput("{a: 1}", "some/path.json", nil); err == nil {
		t.Error("expected error: both a literal and -f given")
	}
}

func TestBuildInputFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in.json")
	// JSON content parses via the YAML (superset) parser.
	if err := os.WriteFile(path, []byte(`{"k": 1}`), 0600); err != nil {
		t.Fatal(err)
	}

	// -f reads the base from a bare file path...
	v, present, err := buildInput("", path, nil)
	if err != nil || !present {
		t.Fatalf("read -f file: present=%v err=%v", present, err)
	}
	if got, want := toJSON(t, v), `{"k":1}`; got != want {
		t.Errorf("read -f file:\n got %s\nwant %s", got, want)
	}

	// ...and --set still overlays on top of the file base.
	v, _, err = buildInput("", path, []string{"k=2", "extra=true"})
	if err != nil {
		t.Fatalf("file + set: %v", err)
	}
	if got, want := toJSON(t, v), `{"extra":true,"k":2}`; got != want {
		t.Errorf("file + set:\n got %s\nwant %s", got, want)
	}

	// A missing file surfaces as an error.
	if _, _, err := buildInput("", filepath.Join(dir, "nope.json"), nil); err == nil {
		t.Error("expected error for a missing -f file")
	}
}

func TestInputValidationError(t *testing.T) {
	if d, ok := inputValidationError(fmt.Errorf("server: input validation: /count: want integer")); !ok || d != "/count: want integer" {
		t.Errorf("got (%q, %v)", d, ok)
	}
	if _, ok := inputValidationError(fmt.Errorf("server: process not found")); ok {
		t.Error("non-validation error should not match")
	}
}

func TestParseWhen(t *testing.T) {
	// A duration counts back from now; the sign is ignored, since "--since -2h" and
	// "--since 2h" are the same intent.
	for _, s := range []string{"2h", "-2h"} {
		got, err := parseWhen("--since", s)
		if err != nil {
			t.Fatalf("parseWhen(%q): %v", s, err)
		}
		if want := time.Now().Add(-2 * time.Hour).UnixMilli(); got < want-5_000 || got > want+5_000 {
			t.Errorf("parseWhen(%q) = %d, want ~%d (2h back from now)", s, got, want)
		}
	}
	// A zone-less timestamp is read in the local zone, not UTC — the whole point of the
	// flag is that a user types the wall clock they see.
	for _, s := range []string{"2026-07-31", "2026-07-31 00:00", "2026-07-31T00:00:00"} {
		got, err := parseWhen("--since", s)
		if err != nil {
			t.Fatalf("parseWhen(%q): %v", s, err)
		}
		want := time.Date(2026, 7, 31, 0, 0, 0, 0, time.Local).UnixMilli()
		if got != want {
			t.Errorf("parseWhen(%q) = %d, want %d (local midnight)", s, got, want)
		}
	}
	// An explicit zone is honored over the local one.
	if got, err := parseWhen("--since", "2026-07-31T00:00:00Z"); err != nil || got != time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("parseWhen(RFC3339) = (%d, %v), want the instant the zone names", got, err)
	}
	// A bare integer must not be read as epoch millis: "--since 30" means half an hour
	// to whoever types it, and returning the whole trail instead would be silent.
	for _, s := range []string{"30", "1754308800000", "yesterday", ""} {
		if _, err := parseWhen("--since", s); err == nil {
			t.Errorf("parseWhen(%q) succeeded, want an error", s)
		}
	}
	// The error names the flag the user actually typed, so --until does not report --since.
	_, err := parseWhen("--until", "nope")
	if err == nil || !strings.Contains(err.Error(), "--until") {
		t.Errorf("parseWhen(--until) error = %v, want it to name --until", err)
	}
}

func TestApplyWindow(t *testing.T) {
	// --until alone bounds the read but keeps the cap: the newest N before that instant
	// is the useful reading, and only --since names a place to walk forward from.
	q := url.Values{}
	if got := applyWindow(q, "", "2026-07-31", "created_at", 20); got != 20 {
		t.Errorf("limit with --until only = %d, want 20 (still capped)", got)
	}
	if q.Get("created_before") == "" || q.Get("created_after") != "" {
		t.Errorf("--until only set %v, want created_before alone", q)
	}

	// --since lifts the cap, and the column decides the parameter names.
	q = url.Values{}
	if got := applyWindow(q, "2026-07-01", "2026-07-31", "updated_at", 20); got != 0 {
		t.Errorf("limit with --since = %d, want 0 (uncapped)", got)
	}
	if q.Get("updated_after") == "" || q.Get("updated_before") == "" {
		t.Errorf("both bounds on updated_at = %v", q)
	}
	if q.Get("created_after") != "" || q.Get("created_before") != "" {
		t.Errorf("bounded the wrong column: %v", q)
	}
}

func TestTimeFormatting(t *testing.T) {
	now := time.Now()
	// shortTime: relative ages for recent timestamps.
	if got := shortTime(now.Add(-5 * time.Minute).Format(time.RFC3339)); got != "5m ago" {
		t.Errorf("shortTime(5m) = %q", got)
	}
	if got := shortTime(now.Add(-3 * time.Hour).Format(time.RFC3339)); got != "3h ago" {
		t.Errorf("shortTime(3h) = %q", got)
	}
	// Unparseable input is returned unchanged.
	if got := shortTime("not-a-time"); got != "not-a-time" {
		t.Errorf("shortTime(garbage) = %q", got)
	}
}

// The shape check in instanceIDsAndFlags rejects an argument that cannot name a row, so
// it has to accept every id the server can actually mint — including the DERIVED ones.
// A child id is not a fresh v7: idgen builds siblings by 128-bit arithmetic on the
// parent's id (Add/After/ChildBase, kept sortable so the DB can lock a tree by id alone),
// and that arithmetic can carry into the variant nibble. uuid.Parse validates the textual
// form only, which is what makes this safe — tightening isInstanceRef to check Version()
// or Variant() would start refusing real children deep in a sibling run, and only there.
func TestIsInstanceRefAcceptsDerivedChildIDs(t *testing.T) {
	root := idgen.NewV7()
	base := idgen.ChildBase(root.String())
	ids := map[string]uuid.UUID{
		"root":                 root,
		"child base":           base,
		"sibling +1":           idgen.Add(base, 1),
		"sibling +1000":        idgen.Add(base, 1000),
		"carry into variant":   idgen.Add(base, 1<<62),
		"carry into high word": idgen.Add(base, ^uint64(0)),
		"after":                idgen.After(base),
		"grandchild":           idgen.ChildBase(idgen.Add(base, 3).String()),
	}
	for name, id := range ids {
		if !isInstanceRef(id.String()) {
			t.Errorf("%s: %q is a real instance id and was refused as malformed", name, id)
		}
	}

	for _, arg := range []string{"@last", strings.ToUpper(root.String())} {
		if !isInstanceRef(arg) {
			t.Errorf("%q must be accepted", arg)
		}
	}
	for _, arg := range []string{"", "ID", "STATUS", "weather-logger@v7", "9m", "not-a-uuid"} {
		if isInstanceRef(arg) {
			t.Errorf("%q is not an id and must be refused before anything is sent", arg)
		}
	}
}
