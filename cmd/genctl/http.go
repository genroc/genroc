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

// authToken is set ONCE in main before dispatch and never written again. The package-state ban's
// reasons (goroutines, GC roots) do not apply to a single-shot CLI.
var authToken string

// authGet is http.Get plus the credential. Go attaches basic-auth from URL userinfo on its
// own, so a deployment behind a proxy that wants that still works without this.
func authGet(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}
	return http.DefaultClient.Do(req)
}

func callGet(url string, out any) error {
	resp, err := authGet(url)
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
		// Not json.Unmarshal: a large literal must survive display. specs/number-precision.md.
		return numeric.Decode(raw, out)
	}
	return nil
}

// page is the {items, page:{...}} envelope every list endpoint returns.
type page[T any] struct {
	Items []T `json:"items"`
	Page  struct {
		After string `json:"after"`
	} `json:"page"`
}

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

// streamPages walks ascending from base's *_after bound, handing fn each page as it arrives.
// base must omit order/limit/after; fn's error aborts the walk.
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

// Which end of a sort a capped read keeps. Time sorts keep the descending head and flip for
// display; a name sort keeps the ascending head, else it would show the alphabetically last N.
const (
	newestFirst = true
	firstFirst  = false
)

// fetchOrdered delivers rows in display order: limit > 0 takes the newest/first N, limit <= 0
// streams ascending from base's *_after bound. Reports whether the cap dropped rows.
func fetchOrdered[T any](base string, limit int, desc bool, emit func([]T) error) (bool, error) {
	if limit <= 0 {
		return false, streamPages(base, emit)
	}
	// One past the limit, dropped before display: it is what tells a cut-short read from one
	// that ended on its own.
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

// listHead fetches up to limit items from one end of the sort; base carries only filters/sort.
// Items return in request order — desc callers reverse them for display.
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

// printIndented echoes rather than re-encodes, so nothing is lost on the way through.
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

// assert reads the outcome off the status line, never the message, so a reworded server string
// cannot reclassify it. specs/id-list-commands.md.
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
	if authToken != "" {
		req.Header.Set("Authorization", "Bearer "+authToken)
	}

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
		if e := decodeServerError(raw, resp.StatusCode); e != nil {
			return resp.StatusCode, e
		}
	}
	if out != nil && len(raw) > 0 {
		// Not json.Unmarshal: a large literal must survive display. specs/number-precision.md.
		return resp.StatusCode, numeric.Decode(raw, out)
	}
	return resp.StatusCode, nil
}

// serverError keeps the per-field detail rather than flattening it into the message, so a
// caller holding the source can print a line per field.
type serverError struct {
	Message string
	Fields  []serverField
}

type serverField struct {
	Field   string `json:"field"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
}

func (e *serverError) Error() string { return "server: " + e.Message }

func decodeServerError(raw []byte, status int) error {
	var body struct {
		Error  string        `json:"error"`
		Fields []serverField `json:"fields"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("server error (status %d)", status)
	}
	return &serverError{Message: body.Error, Fields: body.Fields}
}
