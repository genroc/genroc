package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"genroc/internal/model"
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

// streamPages walks a list endpoint forward (order=asc from base's *_after bound),
// handing each page to fn as it arrives — output starts on page one, a piped `head`
// costs nothing further. base must omit order/limit/after; fn's error aborts the walk.
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

// Which end of a sort a capped read keeps — the end a reader starts from. Time sorts keep
// the descending head and flip for display; a name sort already reads in display order,
// so it keeps the ascending head (the descending end would show the alphabetically last N).
const (
	newestFirst = true
	firstFirst  = false
)

// fetchOrdered delivers rows in display order: limit > 0 takes the newest/first N (no
// start point named); limit <= 0 streams ascending from base's *_after bound. Reports
// whether the cap dropped rows, so callers never truncate silently.
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

// listHead fetches up to limit items from one end of the sort (order asc|desc), following
// page.after; base carries only filters/sort. Items return in request order — desc callers
// reverse for display so the newest row lands nearest the prompt.
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

// printIndented writes a raw server response as indented JSON — the machine-readable form
// for a single object, echoed rather than re-encoded so nothing is lost on the way through.
func printIndented(raw json.RawMessage) {
	var buf bytes.Buffer
	json.Indent(&buf, raw, "", "  ")
	os.Stdout.Write(buf.Bytes())
	os.Stdout.Write([]byte("\n"))
}

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

// assert performs a lifecycle assertion (pause/resume/retry) and reports what it did.
// The outcome is read from the status line, which is where the server puts it — never
// from the message, so a reworded server string cannot reclassify an outcome.
// specs/id-list-commands.md.
func assert(url string) (model.Outcome, error) {
	var body struct {
		Outcome model.Outcome `json:"outcome"`
	}
	code, err := callStatus(url, http.MethodPost, nil, &body)
	if err != nil {
		return "", err
	}
	if code == http.StatusNoContent {
		// 204 carries no body by definition, so the status line is the whole answer.
		return model.OutcomeUnchanged, nil
	}
	return body.Outcome, nil
}

func call(url, method string, body any, out any) error {
	_, err := callStatus(url, method, body, out)
	return err
}

// callStatus is call plus the HTTP status of a success, for the callers that read their
// answer off the status line.
func callStatus(url, method string, body any, out any) (int, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("connect to server: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		var errResp struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(raw, &errResp); err != nil {
			return resp.StatusCode, fmt.Errorf("server error (status %d)", resp.StatusCode)
		}
		return resp.StatusCode, fmt.Errorf("server: %s", errResp.Error)
	}
	if out != nil && len(raw) > 0 {
		// Exact literals: a plain Unmarshal would round a large id back through
		// float64 purely for display, making the CLI disagree with the value the
		// server actually holds.
		return resp.StatusCode, numeric.Decode(raw, out)
	}
	return resp.StatusCode, nil
}
