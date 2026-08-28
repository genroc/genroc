package db

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestKeysetPredicate(t *testing.T) {
	created := instancePaginator.sorts["created"] // {created_at:int, id:text}
	tests := []struct {
		name     string
		cmp      string
		vals     []any
		wantPred string
		wantArgs []any
	}{
		{
			name:     "ascending scan",
			cmp:      ">",
			vals:     []any{int64(1000), "abc"},
			wantPred: "((created_at > ?) OR (created_at = ? AND id > ?))",
			wantArgs: []any{int64(1000), int64(1000), "abc"},
		},
		{
			name:     "descending scan",
			cmp:      "<",
			vals:     []any{int64(1000), "abc"},
			wantPred: "((created_at < ?) OR (created_at = ? AND id < ?))",
			wantArgs: []any{int64(1000), int64(1000), "abc"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pred, args := keysetPredicate(created, tt.cmp, tt.vals)
			if pred != tt.wantPred {
				t.Errorf("pred:\n got %q\nwant %q", pred, tt.wantPred)
			}
			if len(args) != len(tt.wantArgs) {
				t.Fatalf("args: got %d, want %d", len(args), len(tt.wantArgs))
			}
			for i := range args {
				if args[i] != tt.wantArgs[i] {
					t.Errorf("arg %d: got %v, want %v", i, args[i], tt.wantArgs[i])
				}
			}
		})
	}
}

func TestBuildSQL(t *testing.T) {
	cols := instanceSummaryColumns

	// First page, defaults (created desc): page query + exact LIMIT (no look-ahead).
	b, err := instancePaginator.query(PageReq{}).build()
	if err != nil {
		t.Fatal(err)
	}
	if want := "SELECT " + cols + " FROM process_instances ORDER BY created_at DESC, id DESC LIMIT ?"; b.pageSQL != want {
		t.Errorf("pageSQL:\n got %q\nwant %q", b.pageSQL, want)
	}
	if !argsEqual(b.pageArgs, []any{int64(20)}) {
		t.Errorf("pageArgs = %v, want [20]", b.pageArgs)
	}

	// Count with no page boundaries (empty page): both sides are literal 0.
	csql, cargs := b.countQuery(nil, nil)
	if csql != "SELECT 0, 0" {
		t.Errorf("countQuery (no bounds) = %q", csql)
	}
	if !argsEqual(cargs, nil) {
		t.Errorf("count args = %v, want []", cargs)
	}

	// Count with boundaries: two bounded subqueries — before-the-first (desc → >)
	// and after-the-last (desc → <), each capped via LIMIT; args before-then-after.
	csql, cargs = b.countQuery([]any{int64(1000), "a"}, []any{int64(500), "b"})
	wantCount := "SELECT " +
		"(SELECT COUNT(*) FROM (SELECT 1 FROM process_instances WHERE ((created_at > ?) OR (created_at = ? AND id > ?)) LIMIT 1001) c), " +
		"(SELECT COUNT(*) FROM (SELECT 1 FROM process_instances WHERE ((created_at < ?) OR (created_at = ? AND id < ?)) LIMIT 1001) c)"
	if csql != wantCount {
		t.Errorf("countQuery:\n got %q\nwant %q", csql, wantCount)
	}
	if !argsEqual(cargs, []any{int64(1000), int64(1000), "a", int64(500), int64(500), "b"}) {
		t.Errorf("count args = %v", cargs)
	}

	// Filter + forward cursor: cursor predicate on the page; count keeps only filters.
	cur, err := encodeCursor("created", true, instancePaginator.sorts["created"], []any{int64(1000), "id-x"})
	if err != nil {
		t.Fatal(err)
	}
	b, err = instancePaginator.query(PageReq{Limit: 10, After: cur}).
		EqIf("status", "running", true).build()
	if err != nil {
		t.Fatal(err)
	}
	wantPage := "SELECT " + cols + " FROM process_instances WHERE status = ? AND " +
		"((created_at < ?) OR (created_at = ? AND id < ?)) ORDER BY created_at DESC, id DESC LIMIT ?"
	if b.pageSQL != wantPage {
		t.Errorf("pageSQL:\n got %q\nwant %q", b.pageSQL, wantPage)
	}
	if !argsEqual(b.pageArgs, []any{"running", int64(1000), int64(1000), "id-x", int64(10)}) {
		t.Errorf("pageArgs = %v", b.pageArgs)
	}
	// The count's inner keeps the filter and prepends it before the keyset args.
	csql, cargs = b.countQuery([]any{int64(5), "z"}, nil)
	if csql != "SELECT (SELECT COUNT(*) FROM (SELECT 1 FROM process_instances "+
		"WHERE status = ? AND ((created_at > ?) OR (created_at = ? AND id > ?)) LIMIT 1001) c), 0" {
		t.Errorf("filtered count = %q", csql)
	}
	if !argsEqual(cargs, []any{"running", int64(5), int64(5), "z"}) {
		t.Errorf("filtered count args = %v", cargs)
	}

	// Backward paging flips the scan to ASC (orient reverses the slice back).
	b, _ = instancePaginator.query(PageReq{Before: cur}).build()
	if !strings.Contains(b.pageSQL, "ORDER BY created_at ASC, id ASC") {
		t.Errorf("backward pageSQL should scan ASC: %q", b.pageSQL)
	}

	// Filtering an undeclared column is a programming error surfaced at build.
	if _, err := instancePaginator.query(PageReq{}).Eq("status; DROP", "x").build(); err == nil {
		t.Error("expected error filtering an undeclared column")
	}
	// Unknown sort key is rejected.
	if _, err := instancePaginator.query(PageReq{Sort: "nope"}).build(); err == nil {
		t.Error("expected error for unknown sort key")
	}
}

func argsEqual(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCursorCodec(t *testing.T) {
	mode := instancePaginator.sorts["created"]
	tok, err := encodeCursor("created", true, mode, []any{int64(1717171717123), "01J9X"})
	if err != nil {
		t.Fatal(err)
	}
	vals, err := decodeCursor(tok, "created", true, mode)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := vals[0].(int64); !ok || v != 1717171717123 {
		t.Errorf("vals[0] = %v (%T)", vals[0], vals[0])
	}
	if v, ok := vals[1].(string); !ok || v != "01J9X" {
		t.Errorf("vals[1] = %v (%T)", vals[1], vals[1])
	}

	// Rejections: wrong direction, wrong sort, malformed, wrong type.
	if _, err := decodeCursor(tok, "created", false, mode); err == nil {
		t.Error("expected rejection: direction mismatch")
	}
	if _, err := decodeCursor(tok, "updated", true, mode); err == nil {
		t.Error("expected rejection: sort mismatch")
	}
	if _, err := decodeCursor("!!!", "created", true, mode); err == nil {
		t.Error("expected rejection: malformed token")
	}
	bad, _ := json.Marshal(cursorToken{K: "created", D: true, V: []any{"not-a-number", "id"}})
	if _, err := decodeCursor(base64.RawURLEncoding.EncodeToString(bad), "created", true, mode); err == nil {
		t.Error("expected rejection: int column with non-numeric value")
	}
	if _, err := encodeCursor("created", true, mode, []any{int64(1)}); err == nil {
		t.Error("expected encode error for wrong value count")
	}
}

func TestOrient(t *testing.T) {
	mode := sortMode{{"x", kindInt}}
	valsOf := func(_ string, r int64) []any { return []any{r} }

	// Forward: order preserved; boundaries are the first and last rows.
	fwd := built{mode: mode, sort: "x", limit: 2, backward: false}
	items, first, last := orient(fwd, []int64{10, 20}, valsOf)
	if len(items) != 2 || items[0] != 10 || items[1] != 20 {
		t.Errorf("items = %v, want [10 20]", items)
	}
	if first[0] != int64(10) || last[0] != int64(20) {
		t.Errorf("bounds = %v / %v, want 10 / 20", first, last)
	}

	// Backward: scanned in reverse, flipped to display order; boundaries follow.
	bwd := built{mode: mode, sort: "x", limit: 2, backward: true}
	items, first, last = orient(bwd, []int64{30, 20}, valsOf)
	if len(items) != 2 || items[0] != 20 || items[1] != 30 {
		t.Errorf("items = %v, want [20 30] (reversed to display order)", items)
	}
	if first[0] != int64(20) || last[0] != int64(30) {
		t.Errorf("bounds = %v / %v, want 20 / 30", first, last)
	}

	// Empty page: nil boundaries.
	items, first, last = orient(fwd, []int64{}, valsOf)
	if len(items) != 0 || first != nil || last != nil {
		t.Errorf("empty page: items=%v first=%v last=%v", items, first, last)
	}
}
