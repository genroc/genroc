# `fetch`: the missing HTTP surface

Two independent additions sharing one motivation; each ships separately.

- **Part 1 — response metadata** (`self.status`, `self.headers`): draft 2026-07-31,
  not implemented. Its parser blocker (string-literal indexing) **is** implemented.
- **Part 2 — `query`** (structured query parameters): draft 2026-07-31, not implemented.

A third addition lives in its own doc: [fetch-responses.md](fetch-responses.md) retires
`result_schema` for a status-keyed `responses` map. It is independent of both parts here,
but it settles the `accepted_status` question Part 1 raises below — the slot keeps its
meaning and stops being the primary control.

Motivation, both from building the polling example: a fetch discards the HTTP status
one line before it could be used and never captures response headers, so `Location`
(202-async), `Retry-After`, `Link` pagination and the status itself are unreachable;
and the only way to build a query string is interpolation, which performs **no
escaping** — a term containing `&`, `=`, `#` or a space corrupts the URL or injects a
parameter. A bug class reachable from untrusted input, not an ergonomic complaint.

# Part 1 — Response metadata

Two new siblings of `self.result`, **fetch tasks only**: `self.status` (integer) and
`self.headers` (`object<string>`, every access `string | null`). `self.result` keeps
meaning the decoded body — nothing is re-wrapped, which is the whole compatibility
story. Decisions:

- **Lowercase header keys** (Go canonicalizes to `Retry-After`; a canonicalized map
  makes `self.headers['retry-after']` silently null — predictability beats fidelity;
  browsers lowercase too).
- **Comma-join repeated headers** so the type stays the flat `object<string>` the
  request slot already uses. `Set-Cookie` is the accepted casualty.
- **Fetch only** — both the runtime `self` maps and the inference schema need an
  action-type gate so `delay`/`child` don't grow an always-null `self.status`.

**The blocker, found here and since built:** the parser accepted only integer literals
in `[...]`, so `self.headers['retry-after']` was a parse error and `.retry-after` a
subtraction — dot access fails for most of HTTP. Chosen fix (b): a string-literal index
desugars to `MemberNode` (identical semantics to `.foo`, non-breaking since it was a
parse error before), unlocking arbitrary-key access on every open map. What the sketch
missed: inference carried access paths as dot-joined strings, so `x['a.b']` and `x.a.b`
rendered identically — a secret on a dotted key could escape redaction — hence paths
became steps (`nodeSteps`/`pathStep`), the non-free part. Follow-up also built:
**computed keys** on homogeneous bases (arrays, additionalProperties-only maps) —
type-checkable because every key has the same type; rejected on objects with named
properties. Rejected alternative (a): snake_casing header names — lossy and the
definition stops matching the wire.

**What it buys:** the poller's 202 loop becomes an ordinary switch
(`accepted_status: ["200","202"]`, `case: self.status == 202`) instead of routing
through `on_error` — no more error-path loop, no 19 `action_failed` log entries per
healthy run, the attempt counter moves back onto the task that polls.
**`accepted_status` quietly shifts meaning** — from "which statuses are successes" to
"which statuses I handle myself" — worth stating in docs; behaviour is unchanged.

**Compatibility:** no stored definition can reference the new names (navigation would
have rejected them), objects are open so wholesale `output: "$: self"` exports widen
safely, nothing new persists, no migration, no root needed for lazy loading (never
externalized). One remote risk worth a directed test: `output: "$: self"` inside a
recursive task grows the type per unrolling level against the solver's widening cap.
**Trap:** do not re-wrap `self.result` as `{body, headers, status}` — tidier and breaks
every definition in existence.

# Part 2 — `query`

An optional fetch field mirroring `headers`: a Shape evaluating to a string map,
URL-encoded and appended. Three fixed semantics: **null omits the parameter** (the
ergonomic win — optional params without conditional gymnastics; deliberately different
from headers, where null errors); **appended, not exclusive** (a `url` may already
carry `?a=1`); **values are scalars** (repeated params via arrays deferred).

**Compatibility:** `Action` decodes with plain `encoding/json`, so the new `omitempty`
field is nil in every stored definition — byte-identical requests, no migration. **The
real hazard is silent version skew**: a definition using `query` on an older binary
decodes cleanly and drops the field — the request goes out without its parameters.
Pre-existing for any new action field; genroc has no `min_engine`. Build notes: twin of
`checkHeadersShape` with a null-permitting target; resolve beside headers; append via
`url.Values`; add to the fetch variant of the editor schema (each variant is
`additionalProperties: false`). `accepted_status` is the recent worked example of this
exact path.

## Open questions

- Number/boolean query values: stringify (convenient, matches url templates) vs
  strings-only (trivial target)?
- Repeated parameters via array values — defer until asked.
- Should `headers` gain null-omits for consistency? A behaviour change to an existing
  slot; needs its own argument.
- Version skew on new action fields generally — `min_engine` or rejecting unknown
  action fields; both breaking, both bigger than these features.
- `Retry-After` is still not usable end to end: it is in seconds, `ms` wants
  milliseconds, and the expression language has no numeric conversion builtin —
  separate proposal.
- `Set-Cookie` lost to comma-joining; reopen if a session-carrying flow needs it.
