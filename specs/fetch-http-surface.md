# `fetch`: the missing HTTP surface

Status: **Built.** The user-facing account is
`docs/src/content/docs/guides/process-definition/error-handling.mdx`.

Three additions. Building a query string by interpolation performs **no escaping** — a term
carrying `&`, `=`, `#` or a space corrupts the URL or injects a parameter, reachable from
untrusted input. One `result_schema` could not describe an endpoint answering `200` with a job
and `202` with nothing. And the status and response headers were unreachable. §2 and §3 compose —
`responses` types the body per status, `self.status` lets a switch branch on which one arrived.

# §1 — `query`

An optional fetch field mirroring `headers`: a Shape evaluating to a map, URL-encoded and
appended (`appendQuery`).

- **Null omits the parameter** — optional params without conditional gymnastics; deliberately
  unlike headers, where null errors.
- **Appended, not exclusive** — a `url` may already carry `?a=1`.
- **Values are scalars or arrays of scalars, not strings only.** The null-omit does not compose
  with `${ }` (interpolating a nullable is refused at registration), so a strings-only target
  would make an optional number parameter unwritable.
- **An array repeats the parameter** — `?t=a&t=b`, OpenAPI's default (`form`/`explode: true`).
  Without it there is no workaround: `map` is the only builtin, so an array cannot be joined,
  leaving only the unescaped `url`. Elements are escaped individually, order kept, duplicates
  survive; an empty array behaves like `null`, and a null element is skipped (there is no filter
  builtin, so refusing nullable elements would leave an array the author cannot send).
- **Parameter order is by key**, so the same input yields a byte-identical url on every attempt
  (request caches, an audit trail comparable with itself). The order comes from
  `url.Values.Encode`, which sorts — keep it.
- **Space is `%20`, not `+`.** `Encode` is form-urlencoded, but RFC 3986 reads `+` as a literal
  plus, so a server reading it that way takes the wrong value in silence. `%20` is a space under
  both readings, and the rewrite is exact: a literal plus is already `%2B`.

Rejected for now: `?t=a,b` and `?t[]=a` — per-parameter choices needing a `join` builtin or an
option beside the value; OpenAPI's `style` is the model once a server demands one.

`Action` decodes with plain `encoding/json`, so a definition using `query` on an older binary
decodes cleanly and drops it — silent version skew, true of any new action field (Open).

# §2 — `responses`

One map from status to schema describing **the whole endpoint**, success and failure alike. The
status class does the splitting: a declared 2xx types `self.result`, a declared 4xx/5xx types
`error.data` and still routes through `on_error`.

```yaml
responses:
  200:        { type: object, properties: { state: { type: string } } }
  202:        null
  "400, 401": { $ref: "#/$defs/problem" }
  "5xx":      { type: object, properties: { trace_id: { type: string } } }
```

1. **Acceptance** — `accepted_status` when present; otherwise the **2xx** patterns of
   `responses`; otherwise every 2xx. Only 2xx keys influence the automatic set, so declaring
   `"404"` types it **without** accepting it. For a task with no `accepted_status`:

   | declared | accepted | `self.result` |
   |---|---|---|
   | nothing | every 2xx | untyped — an undeclared body is neither readable nor exportable |
   | any 2xx | exactly those | their union; every other 2xx becomes `http.NNN` |
   | error statuses only | every 2xx | untyped, exactly as if nothing were declared |

   The third row is the one that bites: a `404` declaration says nothing about success, so typing
   `self.result` as `null` there would be contradicted by a real body. The engine and inference
   both resolve acceptance through `Action.EffectiveAcceptedStatus` — two copies drift, and an
   undeclared 2xx then lands in `self.result` typed as something nothing checked.
2. **Typing** — a declared schema types the body of its status, into `self.result` if that status
   is accepted and into `error.data` if not. `null` declares "no body", `{}` one of unknown shape.
   An accepted status matched by no pattern contributes `null` to `self.result`; an unaccepted
   one leaves `error.data` absent.
3. **Enforcement** — a declared schema is a contract on both channels. An empty body decodes to
   `null` and is then validated, so a declared status that arrives empty, unparseable, oversized
   or non-conforming raises `result.invalid` / `result.parse` / `result.too_large` **instead of**
   the status code it would otherwise have produced. A `null` entry is the exception: its body is
   ignored, so none of the three can arise.

So `{"200": T}` types `self.result` as exactly `T`: non-nullable, and enforced rather than
asserted.

**Keys** are a comma-separated list of `accepted_status` patterns (`[1-5][0-9][0-9]` or
`[1-5]xx`), each resolved independently (`ParseResponseKey`). **Exact beats range**, per pattern.
**A pattern declared twice is a registration error** naming both keys — not a precedence puzzle.
**No key may mix success and failure statuses**: a declaration decides acceptance, so a mixed key
would narrow acceptance from a line written for the error side. Coverage is a pattern-subset
test, so `{"2xx": T}` covers the whole default accepted set without enumerating 200, 201 and 204.

## Decisions

- **The split is the status class, not the slot.** Rejected: *`keys(responses) ∪
  accepted_status` as the accepted set* — declaring a 404 to type it would silently accept it,
  deleting the error handling; and *an `error_schema` on the `on_error` rule* — one endpoint split
  across two slots, and a data declaration on a control-flow rule.
- **Enforcement is uniform**, and on the error channel the body-validation code **replaces**
  `http.NNN`. Rejected: leniency (route `http.400` anyway, `error.data` null) — every declared
  error schema becomes nullable at the point of use. `code: [http.400, result.invalid]` handles
  both; `"4xx": {}` never escalates, the top type conforming to everything.

- **`error.data` is present exactly where a pattern is declared** — undeclared data is never
  accessible, the rule `self.result` obeys; `"4xx": {}` is the escape hatch (carried and
  exportable, not navigable — [unknown-type.md](unknown-type.md)). So `last_error.data` at a
  handler is nullable exactly when some code reaching it has no declared schema (`pre.*`,
  `http.timeout`, `http.disconnected`, `only_once.interrupted`, a child raise or status with no
  declaration, a body-validation code). The body is also in `action_failed`'s `data`, but
  `snippetRaw` blanks it unless payload logging is on.
- **A `null` entry ignores whatever body arrives anyway**, JSON or not: rejecting would break a
  working definition the first time a server adds a debug field to its 204, or a proxy answers a
  declared 404 with HTML. The engine clears the transport's `BodyCode` for it on both channels.
- **`null` is "no body", `{}` "a body of unknown type".** `{}` is the top type everywhere and must
  not be locally redefined; `null` is free (not a valid JSON Schema, and the boolean form is
  refused). Rejected: nesting under a `schema:` key so a bare `{}` could mean "no body" — it buys
  per-status metadata nothing needs and loses the correspondence with `result_schema`.
- **Declaring a 2xx narrows acceptance.** A POST that starts returning 201 against `{"200": T}`
  raises `http.201` — nothing proves the body is a `T`. Rejected: keeping every 2xx accepted and
  deriving the null (every typed fetch becomes `T | null`, nullability read off an absent slot);
  defaulting acceptance to 200 only (201 and 204 fail where nothing was declared).
- **`"2xx"`, not `code`'s `%`** — these keys are copied out of API documentation (RFC 9110 and
  OpenAPI write `2xx`) far more often than read beside an `on_error` rule.
- **A range is capability, a comma list ergonomics.** `"4xx"` is the only way to type every
  client error (RFC 7807); a list saves naming a `$defs` entry. Rejected: OpenAPI's `default` —
  on the success side it accepts everything or nothing.
- **`accepted_status` stays, is authoritative when present, and stays a Shape.** It can be
  runtime data (`examples/polling-task/poller.genroc.yaml` takes it from the caller's input),
  while schemas must be static. Under a dynamic `accepted_status` no status is statically known to
  be accepted, so a declared schema appears on **both** channels, nullable on each.
- **A leftover `result_schema` on a fetch is refused** (`validateResponses`): `Action` decodes
  with plain `encoding/json`, so it would otherwise be dropped silently. An `Action.UnmarshalJSON`
  would have to keep `DelaySpec` flat — the trap [internal/model/CLAUDE.md](../internal/model/CLAUDE.md)
  documents.
- **The union is `anyOf`, not `oneOf`.** Status bodies overlap (two all-optional objects both
  admit `{}`), and an overlapping `oneOf` rejects a body matching two arms
  ([path-sensitive-output.md](path-sensitive-output.md) §3). Runtime conform is per status, so the
  damage would land in the generated `<taskID>_output` schema and in compat's `IsSubset`.
- **`last_error` is scoped to the task its rule routes to.** The engine drops it on every ordinary
  transition and inference types it only where an error edge enters; a handler that wants the
  failure to travel projects it into its own `output`. That keeps the static side local (one
  task's incoming edges, not a graph fixpoint), and it applies to child failures too — both write
  the same slot.
- **Neither channel needs narrowing.** Refining `{"200": T, "202": U}` by `self.status` needs
  literal types; the common shape is one body plus empty statuses, `T | null`. The error side
  discriminates for free: `last_error.data` is the union over the rules reaching a handler.

## Implementation traps

- **`Responses map[string]*schema.Schema` — the pointer is load-bearing.** `encoding/json` calls
  `UnmarshalJSON` on a value type *even for a JSON null*, and `Schema.UnmarshalJSON` decodes
  `null` into a zero node indistinguishable from `{}`. Only a pointer is set to nil without the
  unmarshaler running. Key present + nil = declared with no body; key absent = undeclared. A value
  type silently turns every "no body" into "untyped body".
- `sendHTTP`'s error exit decodes the body under `MaxResponseBytes` and keeps the trimmed text as
  `ErrorMessage`, which is what an operator reads; when a body-validation code replaces
  `errcode.HTTP(status)`, the message still names the status. The `N+1` overflow ordering is the
  invariant in [internal/transport/CLAUDE.md](../internal/transport/CLAUDE.md).
- `error.data` is an ordinary cut slot; reading `error.code` stays cheap because `model.Context`
  loads only the path it needs ([lazy-context.md](lazy-context.md)).
- **Precedence is a typing concern only**: acceptance asks whether any pattern matches;
  exact-beats-range applies where a schema is selected — inference, and `ResponseFor` at runtime.
  One resolver, or they drift.
- A plain code may be written unquoted in YAML (`200:` reaches genroc as `"200"`); a range or a
  list must be quoted, and JSON quotes everything.

## Compat

A fetch's result is compared as **one merged union under `task:fetch.result`**, the address every
action type uses — not per status. `child_map` is not the precedent: its keys are separately
readable outputs, while a fetch's statuses all feed one `self.result`. Per status would be wrong
both ways — dropping a bodyless status would go unreported though it narrows what the remote may
answer, and `{"200": T}` → `{"2xx": T}` would report a break for no type change (pinned by
`tests/cli/testdata/compat/shapes/a-bodyless-status-dropped.yaml` and `a-status-set-changed.yaml`).
The direction is `old ⊆ new`, as for every result schema: it is a demand on the producer.
`changedslots.go` gives `responses` the leaf `result`, as `result_schema` has, because
compat-command.md §6b suppresses a slot row only where a break carries the same address.

# §3 — Response metadata

`self.status` (integer) and `self.headers` (`object<string>`, every access `string | null`),
**fetch tasks only** (`withFetchMeta`, and the runtime `self` maps), so `delay`/`child` grow no
always-null `self.status`. `self.result` keeps meaning the decoded body.

- **Lowercase header keys** — a canonicalized map (`Retry-After`) makes
  `self.headers['retry-after']` silently null. Rejected: snake_casing names — lossy.
- **Comma-join repeated headers**, so the type stays the flat `object<string>` the request slot
  uses. `Set-Cookie` is the accepted casualty.

**A string-literal index desugars to `MemberNode`** (identical to `.foo`): dot access fails for
most of HTTP, since `.retry-after` is a subtraction. Inference carries access paths as steps
(`nodeSteps`/`pathStep`), never dot-joined strings, so `x['a.b']` and `x.a.b` cannot collide.
**Computed keys** are typed only on homogeneous bases (arrays, `additionalProperties`-only
maps), where every key has the same type; refused on objects with named properties.

Objects are open, so a wholesale `output: "$: self"` export widens safely. **Trap:** do not
re-wrap `self.result` as `{body, headers, status}` — tidier, and breaks every definition in
existence.

# What none of this does

- **No media types.** A `text/plain` 200 still fails: the decode is JSON-only. `responses`
  describes shape, not content type.
- **No per-status response headers.** `self.headers` is one runtime map of what arrived.
- **No claim that a declared error status can occur.** Unlike the success side, where
  acceptance and declaration are one statement, declaring `"404"` asserts only what a 404
  would contain — nothing checks the endpoint can return one, and nothing requires an
  `on_error` rule to catch it.

# Open

- Version skew on new action fields — `min_engine`, or rejecting unknown action fields; both
  breaking. Trigger: a definition silently losing a field on an older binary.
- `Retry-After` end to end: seconds, while `for` wants milliseconds and there is no numeric
  conversion builtin. Trigger: a retry that must honour it.
- A `ruleFieldHints`-style hint ([wire.go](../internal/model/wire.go)) for `{"200": {schema: ...}}`,
  which OpenAPI habit produces and the allowlist refuses without naming the fix. Trigger: the
  first user it trips.
- The poller example's 202 loop as a switch (`accepted_status: ["200","202"]`, `case: self.status
  == 202`) instead of an `on_error` loop. Trigger: changing that example's input contract.
- A directed test for `output: "$: self"` in a recursive task, which grows the type per unrolling
  level against the solver's widening cap. Trigger: the next change to that cap.
