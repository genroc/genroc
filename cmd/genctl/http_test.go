package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
)

type idRow struct {
	ID string `json:"id"`
}

func rowIDs(rows []idRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// pagedServer serves `total` synthetic items (i0 = newest … i{total-1} = oldest) as
// keyset pages, the way the real list endpoints do: it honors order/limit/after and
// caps a single page at pageCap. The after cursor is the index of the next item.
// It rejects order != desc, so a test using it also proves the caller asked for the newest end.
func pagedServer(total, pageCap int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("order") != "desc" {
			http.Error(w, `{"error":"expected order=desc"}`, http.StatusBadRequest)
			return
		}
		start := 0
		if a := q.Get("after"); a != "" {
			start, _ = strconv.Atoi(a)
		}
		limit := pageCap
		if l, err := strconv.Atoi(q.Get("limit")); err == nil && l < limit {
			limit = l
		}
		end := min(start+limit, total)
		items := make([]json.RawMessage, 0, end-start)
		for i := start; i < end; i++ {
			items = append(items, json.RawMessage(fmt.Sprintf(`{"id":"i%d"}`, i)))
		}
		after := ""
		if end < total {
			after = strconv.Itoa(end)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": items,
			"page":  map[string]any{"after": after},
		})
	}))
}

// forwardServer serves `total` synthetic items (i0 = oldest … i{total-1} = newest) as
// keyset pages, capping a page at pageCap. It rejects order != asc, so the test also
// proves streamPages asks for ascending rather than inheriting the endpoint's default —
// which is descending for instances and would print that list upside down.
func forwardServer(total, pageCap int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("order") != "asc" {
			http.Error(w, `{"error":"expected order=asc"}`, http.StatusBadRequest)
			return
		}
		start := 0
		if a := q.Get("after"); a != "" {
			start, _ = strconv.Atoi(a)
		}
		limit := pageCap
		if l, err := strconv.Atoi(q.Get("limit")); err == nil && l < limit {
			limit = l
		}
		end := min(start+limit, total)
		items := make([]json.RawMessage, 0, end-start)
		for i := start; i < end; i++ {
			items = append(items, json.RawMessage(fmt.Sprintf(`{"id":"i%d"}`, i)))
		}
		after := ""
		if end < total {
			after = strconv.Itoa(end)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": items,
			"page":  map[string]any{"after": after},
		})
	}))
}

func TestStreamPages(t *testing.T) {
	tests := []struct {
		name           string
		total, pageCap int
		wantPages      int
		want           []string
	}{
		{"single page", 3, 100, 1, []string{"i0", "i1", "i2"}},
		{"follows the cursor across pages", 5, 2, 3, []string{"i0", "i1", "i2", "i3", "i4"}},
		{"exact page multiple — the last page ends the walk", 4, 2, 2, []string{"i0", "i1", "i2", "i3"}},
		{"empty source", 0, 2, 1, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := forwardServer(tt.total, tt.pageCap)
			defer ts.Close()

			got, pages := []string{}, 0
			err := streamPages(ts.URL, func(rows []idRow) error {
				pages++
				got = append(got, rowIDs(rows)...)
				return nil
			})
			if err != nil {
				t.Fatalf("streamPages: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			// Pages are handed over as they arrive, not accumulated — a single callback
			// for a multi-page walk would mean the whole trail was buffered first.
			if pages != tt.wantPages {
				t.Fatalf("callback ran %d times, want %d (one per page)", pages, tt.wantPages)
			}
		})
	}
}

// fetchOrdered picks its route from the limit, and both must arrive oldest-first: a tail
// via the descending fetch flipped back, an uncapped read via the forward walk. A regression
// either way silently turns the newest-N tail into the oldest-N head, or vice versa.
func TestFetchOrdered(t *testing.T) {
	// Newest-first (i0 newest) for the descending route, oldest-first for the forward one
	// — the same underlying rows, indexed from whichever end each endpoint scans.
	t.Run("a limit tails the newest N, oldest-first", func(t *testing.T) {
		ts := pagedServer(10, 4)
		defer ts.Close()

		var got []string
		calls := 0
		capped, err := fetchOrdered(ts.URL, 3, newestFirst, func(rows []idRow) error {
			calls++
			got = append(got, rowIDs(rows)...)
			return nil
		})
		if err != nil {
			t.Fatalf("fetchOrdered: %v", err)
		}
		// i2,i1,i0 — the three newest, flipped into display order. The 4th row fetched to
		// detect capping must not reach emit.
		if want := []string{"i2", "i1", "i0"}; !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if calls != 1 {
			t.Fatalf("emit ran %d times, want 1 (a bounded tail arrives whole)", calls)
		}
		if !capped {
			t.Fatal("capped = false, but 10 rows were cut to 3")
		}
	})

	// A trail that ends exactly on the limit was not cut short, and saying it was would
	// send the reader chasing entries that do not exist.
	t.Run("a source that ends on the limit is not reported as capped", func(t *testing.T) {
		ts := pagedServer(3, 4)
		defer ts.Close()

		var got []string
		capped, err := fetchOrdered(ts.URL, 3, newestFirst, func(rows []idRow) error {
			got = append(got, rowIDs(rows)...)
			return nil
		})
		if err != nil {
			t.Fatalf("fetchOrdered: %v", err)
		}
		if want := []string{"i2", "i1", "i0"}; !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if capped {
			t.Fatal("capped = true, but the source held exactly 3 rows")
		}
	})

	// A name sort already reads in its display direction, so its cap keeps the first N and
	// flips nothing. Capping it from the descending end would show the alphabetically last
	// N — right count, wrong rows.
	t.Run("firstFirst keeps the head of an ascending sort, unflipped", func(t *testing.T) {
		ts := forwardServer(10, 4)
		defer ts.Close()

		var got []string
		capped, err := fetchOrdered(ts.URL, 3, firstFirst, func(rows []idRow) error {
			got = append(got, rowIDs(rows)...)
			return nil
		})
		if err != nil {
			t.Fatalf("fetchOrdered: %v", err)
		}
		if want := []string{"i0", "i1", "i2"}; !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if !capped {
			t.Fatal("capped = false, but 10 rows were cut to 3")
		}
	})

	t.Run("no limit streams forward, page by page", func(t *testing.T) {
		ts := forwardServer(5, 2)
		defer ts.Close()

		var got []string
		calls := 0
		capped, err := fetchOrdered(ts.URL, 0, newestFirst, func(rows []idRow) error {
			calls++
			got = append(got, rowIDs(rows)...)
			return nil
		})
		if err != nil {
			t.Fatalf("fetchOrdered: %v", err)
		}
		if want := []string{"i0", "i1", "i2", "i3", "i4"}; !slices.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		if calls != 3 {
			t.Fatalf("emit ran %d times, want 3 (one per page, not buffered)", calls)
		}
		if capped {
			t.Fatal("capped = true on an uncapped read")
		}
	})
}

// A callback error aborts the walk in place, so a broken pipe (or a full disk) stops the
// fetch instead of draining every remaining page into a writer that cannot take them.
func TestStreamPagesStopsOnCallbackError(t *testing.T) {
	ts := forwardServer(100, 2)
	defer ts.Close()

	calls := 0
	err := streamPages(ts.URL, func([]idRow) error {
		calls++
		return fmt.Errorf("write failed")
	})
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("err = %v, want the callback's error", err)
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times, want 1 (the walk should stop on the first error)", calls)
	}
}

func TestListHead(t *testing.T) {
	tests := []struct {
		name           string
		total, pageCap int
		limit          int
		want           []string
	}{
		{"single page, limit within page", 5, 100, 3, []string{"i0", "i1", "i2"}},
		{"crosses page boundaries to fill the limit", 5, 2, 5, []string{"i0", "i1", "i2", "i3", "i4"}},
		{"stops at the limit mid-source", 10, 2, 3, []string{"i0", "i1", "i2"}},
		{"exact page multiple", 4, 2, 4, []string{"i0", "i1", "i2", "i3"}},
		{"limit exceeds source — stops when exhausted", 3, 2, 10, []string{"i0", "i1", "i2"}},
		{"empty source", 0, 2, 5, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := pagedServer(tt.total, tt.pageCap)
			defer ts.Close()

			rows, err := listHead[idRow](ts.URL, "desc", tt.limit)
			if err != nil {
				t.Fatalf("listHead: %v", err)
			}
			if got := rowIDs(rows); !slices.Equal(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// A well-behaved server never returns more than the requested limit, but listHead
// truncates defensively in case one does — verify the guard so an over-eager page can
// never leak past --limit.
func TestListHeadTruncatesOverfetch(t *testing.T) {
	// Ignore the requested per-page limit and always hand back a full 4-item page.
	overfetch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := 0
		if a := r.URL.Query().Get("after"); a != "" {
			start, _ = strconv.Atoi(a)
		}
		end := min(start+4, 10)
		items := make([]json.RawMessage, 0, end-start)
		for i := start; i < end; i++ {
			items = append(items, json.RawMessage(fmt.Sprintf(`{"id":"i%d"}`, i)))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": items,
			"page":  map[string]any{"after": strconv.Itoa(end)},
		})
	}))
	defer overfetch.Close()

	rows, err := listHead[idRow](overfetch.URL, "desc", 3)
	if err != nil {
		t.Fatalf("listHead: %v", err)
	}
	if got := rowIDs(rows); !slices.Equal(got, []string{"i0", "i1", "i2"}) {
		t.Fatalf("got %v, want the first 3 (truncated)", got)
	}
}
