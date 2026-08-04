package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"genroc/internal/numeric"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
)

func callGet(url string, out any) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &errResp); err != nil {
			return fmt.Errorf("server error (status %d)", resp.StatusCode)
		}
		return fmt.Errorf("server: %s", errResp.Error)
	}
	if out != nil {
		// Exact literals: a plain Unmarshal would round a large id back through
		// float64 purely for display, making the CLI disagree with the value the
		// server actually holds.
		return numeric.Decode(raw, out)
	}
	return nil
}

// page is the {items, page:{...}} envelope every list endpoint now returns.
type page[T any] struct {
	Items []T `json:"items"`
	Page  struct {
		After string `json:"after"`
	} `json:"page"`
}

// appendQuery adds one query parameter to a URL that may already carry a query string.
func appendQuery(u, key, val string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + key + "=" + url.QueryEscape(val)
}

// pageMax is the paginator's per-page cap (internal/db paginate.go maxLimit). Asking
// for it explicitly keeps a long forward walk from costing a round trip per 20 rows.
const pageMax = 100

// streamPages walks a list endpoint forward, handing each page to fn as it arrives rather
// than accumulating — so output starts on the first page and a piped `head` or a Ctrl-C
// costs nothing further. base carries the filters, including the *_after bound this walk
// starts from, and must omit order/limit/after. fn's error aborts the walk.
//
// order=asc flips the endpoint's newest-first default: paired with an *_after bound it
// walks from that point toward now, which is both the reading order and the display order,
// so nothing has to be buffered to be reversed.
func streamPages[T any](base string, fn func([]T) error) error {
	after := ""
	for {
		u := appendQuery(base, "order", "asc")
		u = appendQuery(u, "limit", strconv.Itoa(pageMax))
		if after != "" {
			u = appendQuery(u, "after", after)
		}
		var p page[T]
		if err := callGet(u, &p); err != nil {
			return err
		}
		if err := fn(p.Items); err != nil {
			return err
		}
		// after is set only while more rows remain, so its absence ends the walk.
		if p.Page.After == "" {
			return nil
		}
		after = p.Page.After
	}
}

// Which end of a sort a capped read keeps, which is the end a reader starts from. Time
// sorts run newest-first, so the interesting rows are at the descending head and the page
// is flipped to display oldest→newest. A name sort already reads in its display direction,
// so the interesting rows are at the ascending head and nothing is flipped — capping a
// name sort from the descending end would show the alphabetically *last* N.
const (
	newestFirst = true
	firstFirst  = false
)

// fetchOrdered delivers a list endpoint's rows to emit in display order, taking the two
// routes from whether a start point was named:
//
//   - limit > 0 — none named. Take N from the head of the sort's reading direction, per
//     newestFirst/firstFirst above. N is bounded by construction, so collecting before the
//     first line prints costs nothing.
//   - limit <= 0 — base carries an *_after bound. Walk ascending from it, emitting each
//     page as it lands. That read is unbounded, which is exactly when buffering is wrong.
//
// It reports whether the limit actually dropped anything, so a caller can say so rather
// than truncate in silence.
func fetchOrdered[T any](base string, limit int, desc bool, emit func([]T) error) (bool, error) {
	if limit <= 0 {
		return false, streamPages(base, emit)
	}
	// One past the limit: whether that row came back is what separates a read that was
	// cut short from one that ended on its own, so neither is ever reported as the other.
	// It is dropped before display.
	order := "asc"
	if desc {
		order = "desc"
	}
	rows, err := listHead[T](base, order, limit+1)
	if err != nil {
		return false, err
	}
	capped := len(rows) > limit
	if capped {
		rows = rows[:limit]
	}
	if desc {
		slices.Reverse(rows)
	}
	return capped, emit(rows)
}

// listAll fetches every page of a list endpoint, following page.after until absent
// (set only while more rows remain). base must omit an after cursor.
func listAll[T any](base string) ([]T, error) {
	var all []T
	after := ""
	for {
		u := base
		if after != "" {
			u = appendQuery(u, "after", after)
		}
		var p page[T]
		if err := callGet(u, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Items...)
		if p.Page.After == "" {
			return all, nil
		}
		after = p.Page.After
	}
}

// listHead fetches up to limit items from one end of a list endpoint's sort, following
// page.after across pages until it has that many (or the source runs out). order is "desc"
// for the newest N or "asc" for the first N; base carries only filters/sort and must omit
// order/limit/after. Items come back in the requested order — a desc caller reverses them
// for display so the newest row lands at the bottom, nearest the prompt (tail-style).
func listHead[T any](base, order string, limit int) ([]T, error) {
	all := make([]T, 0, limit)
	after := ""
	for len(all) < limit {
		u := appendQuery(base, "order", order)
		u = appendQuery(u, "limit", strconv.Itoa(limit-len(all)))
		if after != "" {
			u = appendQuery(u, "after", after)
		}
		var p page[T]
		if err := callGet(u, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Items...)
		if p.Page.After == "" || len(p.Items) == 0 {
			break
		}
		after = p.Page.After
	}
	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// printJSONItems writes items as an indented JSON array — the shared, lossless --json
// output. An empty result renders as [] rather than null.
func printJSONItems(items []json.RawMessage) {
	if items == nil {
		items = []json.RawMessage{}
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		fatal("%v", err)
	}
	os.Stdout.Write(b)
	os.Stdout.Write([]byte("\n"))
}

func call(url, method string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to server: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &errResp); err != nil {
			return fmt.Errorf("server error (status %d)", resp.StatusCode)
		}
		return fmt.Errorf("server: %s", errResp.Error)
	}
	if out != nil {
		// Exact literals: a plain Unmarshal would round a large id back through
		// float64 purely for display, making the CLI disagree with the value the
		// server actually holds.
		return numeric.Decode(raw, out)
	}
	return nil
}
