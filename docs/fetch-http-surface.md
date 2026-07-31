# `fetch`: the missing HTTP surface

Two independent additions to the `fetch` action, written up together because they
share one motivation and one compatibility argument. Each carries its own status so
they can be built — and marked done — separately.

- **Part 1 — response metadata** (`self.status`, `self.headers`): **DRAFT / proposal,
  2026-07-31. Not implemented.**
- **Part 2 — `query`** (structured query parameters): **DRAFT / proposal,
  2026-07-31. Not implemented.**

Engine facts are cited to `file:line` so the draft stays honest about what exists vs.
what is new. Line numbers are as of 2026-07-31.

## The idea in one line

`fetch` models an HTTP call as `fetch(url, {method, headers, body})`, but a process
can only see the response **body** and can only build a URL by **string
concatenation**. Both halves of that are one small change away from being fixed.

## Motivation

The polling example (`examples/polling-task/`) is the case study for both gaps,
because building it ran into each one.

**Response metadata.** A fetch's HTTP status is discarded one line before it could be
used: `transport.Response` carries `Status`
(`internal/transport/transport.go:34,103`), and `executeAction` returns
`resp.Body` alone (`internal/engine/action.go:105`). Response headers are not
captured at all. So a process cannot see:

- **`Location`** — 202 + Location is *the* async HTTP pattern. The poller cannot
  follow a server-assigned job id, which is why its two requests are independent and
  the caller has to correlate them with its own `ref`.
- **`Retry-After`** — the server saying when to poll again. The poller ignores it and
  uses a fixed interval.
- **`Link`** — header-based pagination (GitHub-style) is inexpressible.
- **the status itself** — see below; this is the expensive one.

**Query parameters.** The only way to build one is interpolation:

```yaml
url: "${ input.base }/search?q=${ input.term }"
```

This performs **no escaping**. A term containing a space, `&`, `=` or `#` either
corrupts the URL or injects an additional parameter. That is a bug class, not an
ergonomic complaint, and it is reachable from untrusted input.

---

# Part 1 — Response metadata

## What is exposed

Two new siblings of `self.result`, on **fetch tasks only**:

| Expression | Type | Notes |
|---|---|---|
| `self.status` | `integer` | the HTTP status code |
| `self.headers` | `object` + `additionalProperties: {type: string}` | lowercase keys; every access is `string \| null`. **See the blocker below — hyphenated names are unreadable today.** |

`self.result` keeps meaning exactly what it means today — the decoded body. Nothing
is re-wrapped. That is the whole reason this is not a breaking change.

### Three decisions worth making up front

**Lowercase the header keys.** HTTP header names are case-insensitive and Go
canonicalizes them to `Retry-After`. Exposing them canonicalized would mean
`self.headers['retry-after']` silently yields null, which is the sort of thing an
author debugs for twenty minutes. `fetch()` in the browser lowercases; so should we.
Predictability beats fidelity here.

**Comma-join repeated headers** into a single string, so the type is a flat
`object<string>` — the same type the *request* headers slot already uses
(`internal/model/definition.go:78`). A `map[string][]string` would be more faithful
and much worse to consume in an expression language with one builtin. `Set-Cookie` is
the notable casualty; it is also the header you are least likely to want in a
workflow.

**Fetch only.** The runtime `self` map is built generically for every action type
(`internal/engine/advance.go:249,314`), and the inference schema likewise
(`internal/validation/infer.go:349,418`). Both need an action-type guard, so a
`delay` or `child` task does not grow a `self.status` that is always null. An honest
type is worth the extra condition.

### Blocker found while writing this: you cannot read a hyphenated header

`self.headers` as an open string map is **unusable as-is**, and this is the one thing
that must be settled before Part 1 is built. The parser accepts only an integer
literal inside `[...]` (`internal/expression/syntax/parser.go:179-183`), so:

```
self.headers.location            # works — a valid identifier
self.headers['retry-after']      # PARSE ERROR: "an index must be a literal integer"
self.headers.retry-after         # parses as subtraction: retry MINUS after
```

Dot access works for `location` and `link`, and fails for `retry-after`,
`content-type`, `x-request-id` — that is, most of HTTP. Two ways out:

**(a) Normalize header names to snake_case identifiers** — `Retry-After` →
`retry_after`, read as `self.headers.retry_after`. Needs no expression change and can
ship with Part 1. The costs: the name in a definition no longer matches the name on
the wire, and the transform is lossy (`X-Foo-Bar` and `X-Foo_Bar` collide — rare
enough to accept, since underscores in header names are unusual and frequently
stripped by proxies).

**(b) Accept a string literal as an index** — `self.headers['retry-after']`, matching
the wire name exactly. This is the better general fix, and smaller than it sounds:
`x['foo']` is semantically identical to `x.foo`, so the parser can **desugar it to a
`MemberNode`** and inference, navigation and evaluation are untouched. It is also
non-breaking by construction, since it only accepts input that is a parse error
today. It would additionally unlock arbitrary-key access on *every* open map — the
`config` namespace, a caller-supplied object — which is a gap independent of this
proposal.

The parser's stated rationale is that "a computed index cannot be type-checked",
which is true and does not apply to a literal. **Recommendation: (b)**, as a small
separate change that Part 1 depends on; fall back to (a) only if (b) turns out to
disturb the inference of index access on arrays.

## What this buys, concretely

The poller's loop currently branches through `on_error`, because a status is only
reachable as the catchable code `http.<N>` when it falls *outside* `accepted_status`:

```yaml
accepted_status: "$: input.check.accepted_status"   # ["200"] = done
on_error:
  - code: [http.202]                                # = still running
    goto: $backoff
```

With `self.status` it becomes an ordinary switch:

```yaml
accepted_status: ["200", "202"]
switch:
  - case: "self.status == 202"
    goto: $backoff
  - goto: end
```

That retires three compromises documented in that example's README:

1. the loop no longer runs through the error path;
2. a healthy 20-poll run stops leaving 19 `action_failed` / `error_route` entries in
   the instance log;
3. the attempt counter moves back onto `check`. It lives on `backoff` today only
   because a task's `output` is computed *on success*, and on the polling path
   `check` always errors.

**`accepted_status` quietly changes meaning** — worth stating in its documentation.
Today it reads as "which statuses are successes". Once a process can branch on
`self.status`, it reads as "which statuses I will handle myself"; anything outside it
is still an error, and a status you intend to inspect must first be accepted. The
*behaviour* is unchanged, only the way authors will think about the slot.

## Compatibility

**No breaking change.** Surface by surface:

- **Expressions.** `self` currently offers `result`, `output`, `previous`. A stored
  definition cannot already reference `self.status` or `self.headers` — those
  navigate against the inferred `self` schema and would be a hard registration error
  today ("field not found in schema"). So no existing definition can be depending on
  either name. Adding a property only *widens* what resolves.
- **Subset checks.** The one place a widened `self` could propagate is a task that
  exports it wholesale (`output: "$: self"`), growing that task's output type by two
  properties. That is safe in both directions: objects are open, so extra properties
  in a *sub* are accepted and stripped by the parent's conform
  (`checkChildOutputType`), and a wider type never fails a check a narrower one
  passed.
- **Storage.** `self.result` is transient — "available to this task's own
  output/switch" (`internal/engine/action.go:91`) — and so are these. Nothing new is
  persisted unless a definition explicitly exports it, which is ordinary new data.
  **No migration.**
- **Wire/API.** No request or response type changes. `openapi.json` is unaffected.
- **Lazy loading.** `Roots` drives slot-level resolution of externalized values
  (`internal/expression/refs.go:26-35`). It tracks `SelfResult`/`SelfPrevious`
  because those can be object-store refs; status and headers are small and never
  externalized, so they need no root and a bare `self.status` correctly records
  nothing.

**The one risk to watch:** a recursive output type that exports `self` wholesale
would grow by two properties per unrolling level, against the solver's 64KB widening
cap. This is remote — it needs `output: "$: self"` *inside* a recursive task — but it
is the only path by which this change can alter a type that already type-checks.
Worth a directed test rather than a shrug.

## Ledger

**The build:**
- Capture `resp.Header` in `sendHTTP` (`internal/transport/transport.go:103`),
  lowercased and comma-joined, onto the existing `Response`.
- Widen `executeAction`'s return (`internal/engine/action.go:23`) so the body no
  longer travels alone.
- Add the two keys to the runtime `self` maps
  (`internal/engine/advance.go:249,314`), gated on `ActionTypeFetch`.
- Add the two properties to the inference `self` schema
  (`internal/validation/infer.go:349,418`), same gate.

**Reused as-is:** `Response.Status` is already populated on both the success and
failure paths. The header map type is the one request headers already use.

**Trap to avoid:** do **not** re-wrap `self.result` as `{body, headers, status}`.
That is the obvious "tidier" design and it breaks every definition in existence.

---

# Part 2 — `query`

## Design

A new optional `fetch` field, mirroring `headers` almost exactly: a `Shape` that
evaluates to a string map, URL-encoded and appended to the URL.

```yaml
- id: search
  action:
    type: fetch
    url: "${ input.base }/search"
    query:
      q: "$: input.term"
      cursor: "$: outputs.page.next"     # null -> omitted entirely
      limit: "50"
```

Three semantics to fix:

**A null value omits the parameter.** This is the main ergonomic win beyond escaping:
an optional parameter is just an expression that may be null, with no conditional
gymnastics. Note this differs from `headers`, whose values must be non-null. Making
headers match is a separate, later question — worth not bundling.

**Appended, not exclusive.** A `url` may legitimately already carry `?a=1`; `query`
adds to it rather than replacing or erroring. Documented explicitly, because the
alternative (reject a url containing `?`) is defensible and would be a surprise.

**Values are scalars.** `string`, and — open question below — possibly number and
boolean stringified. Repeated parameters (`?tag=a&tag=b`) would need array values;
`url.Values` supports it for free, but it complicates the shape target, so it is
listed as an open question rather than assumed.

## Compatibility

**No breaking change**, and a weaker claim than Part 1's because nothing existing
changes shape at all:

- **Definitions.** `Action` decodes with plain `encoding/json` — there is no custom
  `UnmarshalJSON` and no `DisallowUnknownFields` — so a new `omitempty` field is
  absent in every stored definition, decodes to nil, and appends nothing. Behaviour
  identical.
- **URLs.** No existing URL is rewritten. A definition without `query` produces a
  byte-identical request.
- **Storage.** A new optional field on a stored JSON document. No migration.

**The one real hazard is version skew, and it is silent.** A definition using `query`
applied to an *older* genroc binary decodes cleanly and drops the field — the request
goes out without its parameters, and nothing reports a problem. Note that this hazard
is not specific to `query`; it applies to any new action field, and genroc has no
"minimum engine version" on a definition today. See the open questions.

## Ledger

**The build:**
- `Action.Query *Shape` (`internal/model/definition.go:74-88`).
- `checkQueryShape`, a twin of `checkHeadersShape`
  (`internal/validation/infer.go:230`), with a target that permits null values.
- Resolve it next to headers in `executeAction`
  (`internal/engine/action.go:42`), the way `resolveAcceptedStatus`
  (`internal/engine/action.go:312`) already resolves a shape to a concrete value.
- Append via `url.Values` in `sendHTTP`.
- Add `query` to the fetch variant of `actionSchemaTemplate`
  (`internal/model/definition.go:145-161`) — each variant sets
  `"additionalProperties": false`, so an unlisted field is rejected by the editor
  schema even though the server accepts it. `shape.RelaxedSchema` generates the
  relaxed node, as it already does for `headers` and `accepted_status`.

**Reused as-is:** the whole shape-slot pattern. `accepted_status` was added by exactly
this path, so there is a recent worked example to follow.

---

## Open questions

- **Number/boolean query values.** Stringifying them is convenient and matches how
  `url` templates already behave; requiring strings is stricter and makes the target
  schema trivial. *(open)*
- **Repeated parameters.** Array-valued `query` entries give `?tag=a&tag=b` free from
  `url.Values`, at the cost of a more complex shape target. Defer until asked for.
  *(open)*
- **Should `headers` also gain null-omits?** It would make the two slots consistent,
  but it is a behaviour change to an existing slot (today a null header is a
  validation error), so it needs its own compatibility argument. *(open)*
- **Version skew on new action fields.** A definition using a field the running
  engine does not know is silently degraded rather than rejected. A `min_engine`
  assertion on a definition, or rejecting unknown action fields at registration,
  would close it — both are larger than either feature here and would themselves be
  breaking. *(open, and pre-existing)*
- **`Retry-After` is not usable end to end even with headers.** A `delay`'s `ms`
  accepts a string and parses it at runtime, but `Retry-After` is in **seconds** and
  `ms` wants milliseconds, so it needs a multiply — and the expression language has
  exactly one builtin, `map` (`internal/expression/syntax/ast.go:121-123`). There is
  no string-to-number conversion. Exposing headers is necessary but not sufficient
  for that use case; a numeric builtin is a separate proposal. *(open)*
- **String-literal indexing** (the blocker above) is written up here because it was
  found here, but it is really its own small proposal — it affects every open map,
  not just headers. Worth splitting out if it grows. *(open — blocks Part 1)*
- **`Set-Cookie` is lost** to comma-joining. Acceptable, and the alternative is a
  much worse type; reopen if a session-carrying flow ever needs it. *(open)*
