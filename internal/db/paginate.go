package db

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// pageCountCap bounds how far the before/after counts scan: each is counted up to
// pageCountCap+1 rows, so the work per count is bounded regardless of table size. A
// reported count of pageCountCap+1 therefore means "more than pageCountCap" (the UI
// renders it as e.g. "1000+").
const pageCountCap = 1000

// Bidirectional keyset pagination shared by every list endpoint (sqlc cannot express a
// dynamic ORDER BY). Column names and operators come only from paginator whitelists and
// every value is a bound ? ($N on Postgres) — no injection surface; keep it that way.

// colKind tells the cursor codec how to decode a key column's value.
type colKind int

const (
	kindText colKind = iota
	kindInt
)

// keyCol is one column of a sort order: a TRUSTED SQL column name (whitelist
// only, never user input) plus its value type for cursor encoding.
type keyCol struct {
	col  string
	kind colKind
}

// sortMode is the ordered column list for one named sort. The columns must together be
// UNIQUE (the last is the tiebreaker) or the keyset skips/repeats rows; direction applies
// uniformly, keeping the predicate an OR-chain (SQLite has no row-value tuples).
type sortMode []keyCol

// paginator is the configure-once policy for one listing: table, projection, the
// index-backed sortable and filterable whitelists, defaults. baseWhere is a TRUSTED
// constant predicate (no placeholders) always ANDed in — e.g. for partial-index matching.
type paginator struct {
	table      string // FROM clause (trusted constant), e.g. "process_instances"
	columns    string // SELECT list (trusted constant)
	baseWhere  string // always-applied constant predicate, no ? placeholders ("" = none)
	sorts      map[string]sortMode
	filterCols []string // columns the wrapper may filter on (whitelist)
	defSort    string   // default sort-mode key (PageReq.Sort == "")
	defDesc    bool     // default direction (PageReq.Desc == nil)
	defLimit   int
	maxLimit   int
}

func (pg paginator) allowsFilter(col string) bool {
	for _, c := range pg.filterCols {
		if c == col {
			return true
		}
	}
	return false
}

// PageReq is the decoded pagination input from the API layer. After/Before are
// opaque cursors from a previous page; Before pages backward. At most one should
// be set (Before wins if both are). A nil Desc uses the paginator's default
// direction; an empty Sort selects the default sort.
type PageReq struct {
	Sort   string
	Desc   *bool
	Limit  int
	After  string
	Before string
}

// Window bounds a list on one timestamp column, in unix millis. Either end may be zero,
// meaning unbounded on that side.
//
// The range is half-open — After <= t < Before. Both ends inclusive would double-count a
// row sitting exactly on a boundary when a caller walks a trail window by window (14:00,
// 15:00, 16:00 …), which is the main reason to name both ends at once. Note the field
// names read as the query params they come from (created_after / created_before), so
// "after" here is "at or after".
//
// A column is bounded by naming it: Window is not tied to a particular one, so a list with
// two timestamps takes two Windows rather than one the query has to guess a column for.
type Window struct {
	After  int64
	Before int64
}

// apply adds this window's bounds to q on col. A zero end adds no condition, so the zero
// Window is "no bound at all" and costs nothing.
func (w Window) apply(q *listQuery, col string) *listQuery {
	return q.GteIf(col, w.After, w.After > 0).LtIf(col, w.Before, w.Before > 0)
}

// PageInfo is the navigation metadata returned alongside a page of items. Sort and
// Order echo the effective sort key and direction (so a caller sees what the
// defaults resolved to). ItemsBefore/ItemsAfter are how many rows fall outside the
// page in display order, each counted up to pageCountCap+1 — a value of
// pageCountCap+1 means "more than that" (render as "1000+"). After/Before are the
// cursors to pass back as ?after / ?before; each is set only in a direction that
// has more rows (After iff ItemsAfter>0, Before iff ItemsBefore>0), so cursor
// presence is itself the has-more signal and a page-to-end loop ends when After is
// absent.
type PageInfo struct {
	Size        int    `json:"size"`
	ItemsBefore int64  `json:"items_before"`
	ItemsAfter  int64  `json:"items_after"`
	Sort        string `json:"sort"`
	Order       string `json:"order"`
	After       string `json:"after,omitempty"`
	Before      string `json:"before,omitempty"`
}

// built is the assembled, ready-to-run plan for one request: the page statement
// plus the scaffolding countQuery needs to assemble the two capped before/after
// counts (once the page's boundary rows are known) and the context orient needs to
// flip a backward page.
type built struct {
	pageSQL  string
	pageArgs []any

	// Count scaffolding: each side is a bounded subquery —
	// SELECT COUNT(*) FROM (countInner WHERE <countConds AND keyset> LIMIT cap+1).
	// Args per side: countPrefixArgs (CTE seed), countFilterArgs, then keyset args.
	countInner      string
	countConds      []string
	countPrefixArgs []any
	countFilterArgs []any

	mode     sortMode
	sort     string
	desc     bool
	limit    int
	backward bool
}

// listQuery accumulates the dynamic filters of one request. Build it with
// paginator.query(req); add filters with Eq/EqIf/Gte/GteIf; finish with build (or
// buildSource for a custom static prefix such as a recursive CTE). A filter on an
// undeclared column is a programming error surfaced as a build error.
type listQuery struct {
	pg    paginator
	req   PageReq
	conds []string
	args  []any
	err   error
}

func (pg paginator) query(req PageReq) *listQuery {
	return &listQuery{pg: pg, req: req}
}

// Eq adds "col = ?" binding value. col must be a declared filter column.
func (q *listQuery) Eq(col string, value any) *listQuery { return q.cond(col, "=", value) }

// EqIf adds "col = ?" only when include is true (optional/skip-when-empty filters).
func (q *listQuery) EqIf(col string, value any, include bool) *listQuery {
	if include {
		return q.cond(col, "=", value)
	}
	return q
}

// GteIf adds "col >= ?" only when include is true.
func (q *listQuery) GteIf(col string, value any, include bool) *listQuery {
	if include {
		return q.cond(col, ">=", value)
	}
	return q
}

// LtIf adds "col < ?" only when include is true.
func (q *listQuery) LtIf(col string, value any, include bool) *listQuery {
	if include {
		return q.cond(col, "<", value)
	}
	return q
}

func (q *listQuery) cond(col, op string, value any) *listQuery {
	if q.err != nil {
		return q
	}
	if !q.pg.allowsFilter(col) {
		q.err = fmt.Errorf("paginate: %q is not a configured filter column", col)
		return q
	}
	q.conds = append(q.conds, col+" "+op+" ?")
	q.args = append(q.args, value)
	return q
}

// build assembles the page + count plan for the paginator's own table (SELECT <columns>
// FROM <table> for the page, SELECT 1 FROM <table> as the count source).
func (q *listQuery) build() (built, error) {
	return q.buildSource(
		"SELECT "+q.pg.columns+" FROM "+q.pg.table,
		"SELECT 1 FROM "+q.pg.table,
		nil,
	)
}

// buildSource is build for callers needing a custom static prefix (e.g. a recursive
// CTE). pagePrefix ("[CTE] SELECT <cols> FROM <source>") and countInner ("[CTE] SELECT 1
// FROM <source>") are TRUSTED constants; prefixArgs bind any ? in the CTE (shared by the
// page and both count subqueries) and are placed first.
func (q *listQuery) buildSource(pagePrefix, countInner string, prefixArgs []any) (built, error) {
	if q.err != nil {
		return built{}, q.err
	}
	pg := q.pg

	key := q.req.Sort
	if key == "" {
		key = pg.defSort
	}
	mode, ok := pg.sorts[key]
	if !ok {
		return built{}, fmt.Errorf("unknown sort %q", q.req.Sort)
	}
	desc := pg.defDesc
	if q.req.Desc != nil {
		desc = *q.req.Desc
	}
	limit := q.req.Limit
	if limit <= 0 {
		limit = pg.defLimit
	}
	if limit > pg.maxLimit {
		limit = pg.maxLimit
	}

	backward := q.req.Before != ""
	tok := q.req.After
	if backward {
		tok = q.req.Before
	}

	// Scan direction: forward follows the display order, backward reverses it (then
	// orient flips the slice back). cmp/dir derive from the scan direction.
	scanAsc := !desc
	if backward {
		scanAsc = !scanAsc
	}
	cmp, dir := ">", "ASC"
	if !scanAsc {
		cmp, dir = "<", "DESC"
	}

	// WHERE: baseWhere + filters (shared by the count subqueries), then the cursor
	// predicate (page only — the count's own keyset is added per direction).
	conds := make([]string, 0, len(q.conds)+2)
	if pg.baseWhere != "" {
		conds = append(conds, pg.baseWhere)
	}
	conds = append(conds, q.conds...)
	countConds := append([]string(nil), conds...)

	var predArgs []any
	if tok != "" {
		vals, err := decodeCursor(tok, key, desc, mode)
		if err != nil {
			return built{}, err
		}
		pred, pArgs := keysetPredicate(mode, cmp, vals)
		conds = append(conds, pred)
		predArgs = pArgs
	}

	orderCols := make([]string, len(mode))
	for i, kc := range mode {
		orderCols[i] = kc.col + " " + dir
	}
	orderBy := strings.Join(orderCols, ", ")

	pageSQL := pagePrefix + whereClause(conds) + " ORDER BY " + orderBy + " LIMIT ?"
	pageArgs := make([]any, 0, len(prefixArgs)+len(q.args)+len(predArgs)+1)
	pageArgs = append(pageArgs, prefixArgs...)
	pageArgs = append(pageArgs, q.args...)
	pageArgs = append(pageArgs, predArgs...)
	pageArgs = append(pageArgs, int64(limit))

	return built{
		pageSQL:         pageSQL,
		pageArgs:        pageArgs,
		countInner:      countInner,
		countConds:      countConds,
		countPrefixArgs: prefixArgs,
		countFilterArgs: q.args,
		mode:            mode,
		sort:            key,
		desc:            desc,
		limit:           limit,
		backward:        backward,
	}, nil
}

// countQuery assembles the before/after counts as two bounded subqueries (rows strictly
// before first / after last in display order, each capped at pageCountCap+1 by LIMIT),
// as one statement yielding (before, after). A nil boundary (empty page) makes that side
// a literal 0.
func (b built) countQuery(first, last []any) (string, []any) {
	// "before the first row" / "after the last row" in display order.
	beforeCmp, afterCmp := "<", ">"
	if b.desc {
		beforeCmp, afterCmp = ">", "<"
	}
	cap1 := strconv.Itoa(pageCountCap + 1)
	side := func(vals []any, cmp string) (string, []any) {
		if len(vals) == 0 {
			return "0", nil
		}
		pred, predArgs := keysetPredicate(b.mode, cmp, vals)
		conds := append(append([]string(nil), b.countConds...), pred)
		inner := b.countInner + whereClause(conds) + " LIMIT " + cap1
		args := make([]any, 0, len(b.countPrefixArgs)+len(b.countFilterArgs)+len(predArgs))
		args = append(args, b.countPrefixArgs...)
		args = append(args, b.countFilterArgs...)
		args = append(args, predArgs...)
		return "(SELECT COUNT(*) FROM (" + inner + ") c)", args
	}
	beforeExpr, beforeArgs := side(first, beforeCmp)
	afterExpr, afterArgs := side(last, afterCmp)

	sql := "SELECT " + beforeExpr + ", " + afterExpr
	args := append(beforeArgs, afterArgs...)
	return sql, args
}

func whereClause(conds []string) string {
	if len(conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(conds, " AND ")
}

// orient restores display order for a backward page (scanned in reverse) and returns the
// page plus its boundary key values — the first/last rows' sort-key values that the count
// query and cursors are built from (both nil for an empty page).
func orient[T any](b built, rows []T, valsOf func(sort string, row T) []any) (items []T, first, last []any) {
	if b.backward {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	if len(rows) == 0 {
		return rows, nil, nil
	}
	return rows, valsOf(b.sort, rows[0]), valsOf(b.sort, rows[len(rows)-1])
}

// rowScanner is the read side of *sql.Rows / *sql.Row. Per-row scan helpers accept
// it so a single helper works for both the list-page loop and single-row reads.
type rowScanner = interface{ Scan(dest ...any) error }

// runPage executes a built page query, scans each row via scanRow, restores display
// order, and returns the page with its navigation metadata — the query → scan → orient →
// pageInfo tail shared by every list endpoint. Callers supply scanRow and cursorVals.
func runPage[T any](db *DB, b built, scanRow func(rowScanner) (T, error), cursorVals func(sort string, row T) []any) ([]T, PageInfo, error) {
	rows, err := db.exec.QueryContext(context.Background(), b.pageSQL, b.pageArgs...)
	if err != nil {
		return nil, PageInfo{}, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		item, err := scanRow(rows)
		if err != nil {
			return nil, PageInfo{}, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, PageInfo{}, err
	}
	items, first, last := orient(b, out, cursorVals)
	info, err := db.pageInfo(b, first, last)
	if err != nil {
		return nil, PageInfo{}, err
	}
	return items, info, nil
}

// keysetPredicate builds the lexicographic OR-chain selecting rows strictly after the
// cursor under cmp (> ascending, < descending) — e.g. for (a,b,c): (a cmp ?) OR (a = ?
// AND b cmp ?) OR (a = ? AND b = ? AND c cmp ?). Spelled out (not row-value syntax) so
// it runs identically on SQLite and Postgres.
func keysetPredicate(mode sortMode, cmp string, vals []any) (string, []any) {
	ors := make([]string, 0, len(mode))
	args := make([]any, 0, len(mode)*(len(mode)+1)/2)
	for i := range mode {
		terms := make([]string, 0, i+1)
		for j := 0; j < i; j++ {
			terms = append(terms, mode[j].col+" = ?")
			args = append(args, vals[j])
		}
		terms = append(terms, mode[i].col+" "+cmp+" ?")
		args = append(args, vals[i])
		ors = append(ors, "("+strings.Join(terms, " AND ")+")")
	}
	return "(" + strings.Join(ors, " OR ") + ")", args
}

// cursorToken is the opaque, URL-safe-base64-encoded JSON payload of a cursor. It
// carries the sort key and direction it was minted under, so a token is rejected
// if reused under a different sort. It does NOT encode the filters — like all
// keyset pagination it assumes stable filters across pages.
type cursorToken struct {
	K string `json:"k"` // sort-mode key
	D bool   `json:"d"` // desc
	V []any  `json:"v"` // key-column values, in mode order
}

func encodeCursor(sort string, desc bool, mode sortMode, vals []any) (string, error) {
	if len(vals) != len(mode) {
		return "", fmt.Errorf("cursor: got %d values, want %d", len(vals), len(mode))
	}
	b, err := json.Marshal(cursorToken{K: sort, D: desc, V: vals})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeCursor parses tok into its key-column values, coerced to each column's Go type.
// It rejects a malformed token or one minted under a different sort/direction.
func decodeCursor(tok, sort string, desc bool, mode sortMode) ([]any, error) {
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep kindInt values exact (avoid float64 round-trip)
	var c cursorToken
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	if c.K != sort || c.D != desc {
		return nil, fmt.Errorf("cursor does not match the requested sort")
	}
	if len(c.V) != len(mode) {
		return nil, fmt.Errorf("invalid cursor")
	}
	out := make([]any, len(mode))
	for i, kc := range mode {
		switch kc.kind {
		case kindInt:
			n, ok := c.V[i].(json.Number)
			if !ok {
				return nil, fmt.Errorf("invalid cursor")
			}
			iv, err := n.Int64()
			if err != nil {
				return nil, fmt.Errorf("invalid cursor")
			}
			out[i] = iv
		default:
			s, ok := c.V[i].(string)
			if !ok {
				return nil, fmt.Errorf("invalid cursor")
			}
			out[i] = s
		}
	}
	return out, nil
}
