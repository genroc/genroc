# `fetch`: the missing HTTP surface

Three independent additions sharing one motivation. **All three are built** (2026-08-19); what
follows is the design record, not a proposal — the shipped behaviour is documented in
`docs/reference/tasks.mdx`.

- **§1 `query`** — structured query parameters. Drafted 2026-07-31.
- **§2 `responses`** — status-keyed schemas, retiring `result_schema` on a fetch. Drafted
  2026-08-10.
- **§3 response metadata** — `self.status`, `self.headers`. Drafted 2026-07-31.

Two decisions §1 left open were settled in the building. **Values are scalars, not
strings-only**: the null-omit is the point of the slot and does not compose with `${ }`, since
interpolating a nullable is refused at registration — so a strings-only target would have made
an optional NUMBER parameter unwritable. And **parameter order is by key**, so the same
definition and input produce a byte-identical url on every attempt — a url that varied would
break request caches and make an audit trail incomparable with itself. That ordering comes
from `url.Values.Encode`, which sorts; an explicit sort beside it was written first and was
pure ceremony, which a mutation test caught by not failing when it was removed. The third — repeated parameters via array values — was deferred until it
turned out to have no workaround, and is built; §1 records which serialisation was chosen.

All three came out of building the polling example. Building a query string means
interpolation, which performs **no escaping** — a term carrying `&`, `=`, `#` or a space
corrupts the URL or injects a parameter, a bug class reachable from untrusted input rather
than an ergonomic complaint. One `result_schema` types the body of every accepted status
alike, so an endpoint answering `200` with a job and `202` with nothing cannot be described:
the 202 is accepted and then fails to decode. And a fetch discards the status one line before
it could be used and never captures response headers, so `Location`, `Retry-After`, `Link`
pagination and the status itself are unreachable.

§2 and §3 compose — `responses` types the body per status, `self.status` lets a switch branch
on which one arrived — but neither waits on the other.

# §1 — `query`

An optional fetch field mirroring `headers`: a Shape evaluating to a map, URL-encoded and
appended. Three fixed semantics: **null omits the parameter** (the ergonomic win — optional
params without conditional gymnastics; deliberately different from headers, where null
errors); **appended, not exclusive** (a `url` may already carry `?a=1`); and **a value is a
scalar or an array of scalars**, the latter repeating the parameter (see below — the draft
deferred arrays, and the deferral did not survive contact with the missing workaround).

**Compatibility.** `Action` decodes with plain `encoding/json`, so the new `omitempty` field
is nil in every stored definition — byte-identical requests, no migration. **The real hazard
is silent version skew**: a definition using `query` on an older binary decodes cleanly and
drops the field, and the request goes out without its parameters. Pre-existing for any new
action field; genroc has no `min_engine`.

**Build notes.** Twin of `checkHeadersShape` with a null-permitting target; resolve beside
headers; append via `url.Values`; add to the fetch variant of the editor schema (each variant
is `additionalProperties: false`). `accepted_status` is the recent worked example of this
exact path.

**Space is `%20`, not `+`.** `url.Values.Encode` is form-urlencoded and renders a space as
`+`. Every mainstream decoder reads that back as a space, but RFC 3986 says a query is just a
string and `+` is a literal plus — a server reading it that way takes the wrong value in
silence, which is the failure class this slot exists to prevent. `%20` is a space under both
readings, so it is safe where `+` is only usually safe, and the rewrite is exact: `QueryEscape`
emits `+` for a space and nothing else, a literal plus already being `%2B`.

**An array repeats the parameter** — `?t=a&t=b`, OpenAPI's default (`form`/`explode: true`)
and what most services read. The deferral ended when it turned out there was no workaround at
all: `map` is the only builtin, so an array could not even be joined into one value, and the
only route left was writing values literally into the `url` — the unescaped path this slot
exists to replace. Elements are escaped individually, order is the array's, duplicates
survive, and an empty array behaves like `null`. A null ELEMENT is skipped rather than sent as
the text "null" — the same omission one level down — and elements may be declared nullable
because there is no filter builtin, so refusing them would leave an author holding an array
they cannot send.

The two forms NOT chosen would each need more than a target widening: `?t=a,b`
(`explode: false`) and `?t[]=a` are per-parameter serialisation choices, so they want either a
`join` builtin or an option beside the value. Neither has been asked for; OpenAPI's `style`
vocabulary is the obvious model if one ever is.

# §2 — `responses`

One map from status to schema describing **the whole endpoint**, success and failure alike.
The status class does the splitting: a declared 2xx types `self.result`, a declared 4xx/5xx
types `error.data` and still routes through `on_error`.

Today `accepted_status` defaults to any 2xx and `sendHTTP` decodes the body unconditionally,
so a `202` carrying no body is accepted and then fails to parse. Measured, not inferred —
`204`, an empty `200` and a `text/plain` `200` all return `output.parse`, and
[transport.go:130](../internal/transport/transport.go#L130) sets no `ErrorMessage` on that
path, so the trail says `output.parse` and nothing else. `DELETE` → 204, an async kickoff →
202, a webhook ACK: none are expressible. On the other side an error payload is unreachable at
any type — `error` is `{task, message, code}`
([error.go:71](../internal/engine/error.go#L71),
[infer.go:424](../internal/validation/infer.go#L424)) and a `problem+json` body survives only
as the 512 bytes [transport.go:109](../internal/transport/transport.go#L109) trims into
`message`.

```yaml
responses:
  200:        { type: object, properties: { state: { type: string } } }
  202:        null
  "400, 401": { $ref: "#/$defs/problem" }
  "5xx":      { type: object, properties: { trace_id: { type: string } } }
```

1. **Acceptance** — `accepted_status` when present; otherwise the **2xx** patterns of
   `responses`; otherwise every 2xx. Only 2xx keys influence the automatic set, so declaring
   `"404"` types it **without** accepting it: it still raises `http.404` and still routes
   through `on_error`. The three cases that follow, for a task declaring no
   `accepted_status`:

   | declared | accepted | `self.result` |
   |---|---|---|
   | nothing | every 2xx | untyped — an undeclared body is neither readable nor exportable |
   | any 2xx | exactly those | their union; every other 2xx becomes `http.NNN` |
   | error statuses only | every 2xx | untyped, exactly as if nothing were declared |

   The third row is the one that bites. A `404` declaration says nothing about what a
   success carries, so the 2xx default still accepts the response — and typing `self.result`
   as `null` there would be a claim the runtime contradicts with a real body. Both halves of
   this rule are read by the engine AND by inference, through
   `Action.EffectiveAcceptedStatus`; they were once separate and disagreed, so an undeclared
   2xx was accepted, matched no declaration, skipped validation, and landed in `self.result`
   typed as something it had never been checked against.
2. **Typing** — a declared schema types the body of its status, into `self.result` if that
   status is accepted and into `error.data` if it is not. `null` declares "no body", `{}` one
   of unknown shape. An accepted status matched by no pattern contributes `null` to
   `self.result`; an unaccepted one leaves `error.data` absent.
3. **Enforcement** — a declared schema is a contract on both channels. An empty body decodes
   to `null` rather than failing to parse, and the declared schema then validates it, so a
   declared `"200"` or `"400"` that arrives empty, unparseable, oversized or non-conforming
   raises the body-validation code (`output.invalid` / `output.parse` / `output.too_large`)
   **instead of** the status code the response would otherwise have produced.

So `{"200": T}` types `self.result` as exactly `T`: non-nullable, and enforced rather than
asserted. That is the point of the success half; everything below defends it.

**Keys** are a comma-separated list of the patterns `accepted_status` already takes, each
resolved independently — a key is a set, not a unit.

    key     := pattern ("," pattern)*      # surrounding whitespace ignored
    pattern := [1-5][0-9][0-9] | [1-5] "xx"

The status space is finite, so what the grammar raises is settled by enumeration rather than
by a syntactic rule. **Exact beats range**, per pattern: `{"404": A, "4xx": B}` gives a 404
the `A`. **Equal-specificity overlap is a registration error** naming both keys (`"400, 401"`
beside `"401, 402"`), not a precedence puzzle. **No declaration may straddle the
success/failure split** — rule 1 reads a declaration as deciding acceptance, so a straddling
key narrows acceptance from a line the author may have written for the error side only; say
`"4xx, 5xx"`. Coverage is then a pattern-subset test rather than set membership, which is
what makes a range worth having on the success side too: `{"2xx": T}` covers the whole default
accepted set, so `self.result` is exactly `T` without enumerating 200, 201 and 204.

## Decisions

**The success/failure split is the status class, not the slot.** One map describes the
endpoint the way an OpenAPI `responses` object does, and genroc reads 2xx as success — a rule
no author has to be taught. Rejected: *`keys(responses) ∪ accepted_status` as the accepted
set*, under which declaring a 404 to get its payload would silently **accept** it, deleting
the definition's error handling — a declaration made for typing must not change routing; and
*an `error_schema` on the `on_error` rule*, which types the same body but splits one
endpoint's description across two slots and puts a data declaration on a control-flow rule.

**Enforcement is uniform: a declared schema is enforced on both channels.** A body that does
not conform has its own error — `output.invalid`, joined by `output.parse` and
`output.too_large` — and on the error channel that code **replaces** the `http.NNN` the status
would have raised. So a malformed 400 against `{"400": A}` arrives as `output.invalid`, not
`http.400`, exactly as a malformed 200 against `{"200": T}` already does.

The rejected alternative was leniency: route `http.400` anyway and leave `error.data` null.
It reads as the safer choice — the error path *is* the recovery path — but it makes every
declared error schema nullable at the point of use, so a handler must write `?? {}` even where
it declared the shape, and a schema you must null-check anyway has bought almost nothing. The
escalation is also less drastic than it looks: `output.invalid` is catchable, so an author who
wants one handler for both writes `code: [http.400, output.invalid]`, and anyone who wants the
old behaviour outright declares `"4xx": {}` — the top type conforms to everything and therefore
never escalates.

**The consequence to state in docs:** declaring a schema for a status means that status's code
can be replaced by a body-validation code, so `code: [http.4%]` no longer catches a 400 whose
body is malformed. Symmetric with the success side, and the fix is to name both codes.

**`error.data` is present exactly where a pattern is declared.** An unaccepted status with no
declared pattern leaves it absent rather than untyped — the rule `self.result` already obeys,
where no `result_schema` means no `self.result` at all because *undeclared data is never
accessible* ([infer.go:440](../internal/validation/infer.go#L440)). The escape hatch is the
top type: `"4xx": {}` says "a body arrives, shape unknown", carried and exportable without
being navigable ([unknown-type.md](unknown-type.md)) until a consumer restates it. Reading an
undeclared body would be the one place in the language where data reaches an expression with
no declaration behind it, and on the path where the payload is least trustworthy.

**This is the only source of `null`,** which is why enforcement matters: `error.data` at a
handler is nullable exactly when some code reaching it has no declared schema — `pre.*`,
`http.timeout`, `http.disconnected`, a child raise, an undeclared status, or a body-validation code, since those
carry no conforming body either. `code: [http.400]` against `{"400": A}` is therefore exactly
`A`, and the null returns precisely when an author asks for it by widening the patterns. Absent from
the context is not absent from the record — `action_failed` still carries the body in its
`data` ([action.go:94](../internal/engine/action.go#L94)) — but behind two gates this spec
does not move: the entry is `LogDebug`, and `snippetRaw` blanks it unless payload logging is
on. "It is in the logs" is true of an operator who turned both on, not of a default install.

**A `null` entry ignores a body that arrives anyway.** Enforcement binds where a type is
claimed, and `null` claims none — the body is never read, so nothing can fail to conform.
Rejecting would be the symmetric-looking choice and is wrong in practice: it breaks a working
definition the first time a server adds a debug field to its 204.

**`null` is "no body", `{}` is "a body of unknown type".** `{}` already means the top type
everywhere else ([unknown-type.md](unknown-type.md)) and must not be locally redefined; `null`
is free because it is not a valid JSON Schema, and the boolean form that might have competed
is already refused ([schema.go:155](../internal/schema/schema.go#L155)). Rejected: nesting the
schema under a `schema:` key so a bare `{}` could mean "no body" — it buys a place to hang
per-status metadata that nothing needs (§3's `self.headers` is a runtime map of what arrived,
not a per-status declaration) and costs the correspondence with `result_schema`, a bare schema.

**Declaring a 2xx narrows acceptance.** A POST that starts returning 201 against `{"200": T}`
raises `http.201` — loud, catchable, and honest, since nothing proves the 201 body is a `T`.
The narrowing is confined to the success class by rule 1, and the message must name the way
out ("201 is not among the declared responses — add it, or set `accepted_status`"). The
quieter alternative — keep 2xx accepted always and derive the null from the set difference —
makes every typed fetch `T | null`, with the nullability read off the **absence** of a slot
rather than the presence of one. Defaulting the accepted set to 200 only was rejected for the
opposite reason: it makes 201 and 204 faulty for a definition that declared nothing at all.

**The status vocabulary stays `"2xx"`; `code`'s `%` was considered and rejected.** The pull is
real — a task writes `"5xx"` here and `code: [http.5%]` in an `on_error` rule a few lines
below, two spellings for one set — and `%` would have made `"40%"` sayable, which a
hundred-range cannot express at any length. It loses on the boundary that matters more: `2xx`
is what RFC 9110 and OpenAPI write, and these keys are copied out of API documentation far
more often than they are read beside an `on_error` rule. Keeping it also leaves
`ValidStatusPattern` and `matchAcceptedStatus` standing, so this is a splitter rather than a
migration across every definition that already names a status.

**A range is capability; a comma list is ergonomics.** A class cannot be enumerated — `"4xx"`
is the only way to say "every client error returns this envelope", without which the RFC 7807
case, the commonest reason to type an error body at all, is unwritable. A comma list adds no
expressiveness (`$defs` + `$ref` already shares a schema); it is in because the `$ref` spelling
makes an author name a definition to say "these two are the same", and the naming is the part
that gets skipped. Rejected: OpenAPI's `default` key — fine as an error catch-all, but on the
success side a pattern matching everything either accepts everything or accepts nothing, and
both readings are wrong.

**`accepted_status` stays, is authoritative when present, and stays a Shape.** The accepted set
can be runtime data: [poller.genroc.yaml:59](../examples/polling-task/poller.genroc.yaml#L59)
takes it from the caller's input, which is what makes that example a *generic* poller. Schemas
must be static for inference, so one map cannot carry both. Authoritative rather than widening
is what lets the poller declare `"202"` for its type while its caller decides whether 202 is a
success. **The edge that follows:** under a dynamic `accepted_status` no declared status is
statically known to be accepted, so its schema appears on **both** channels, nullable on each.

**A leftover `result_schema` on a fetch is refused at registration.** The field is removed
outright — a stored definition still carrying it is a prototype's problem, not a compatibility
one — but the refusal stays, because `Action` decodes with plain `encoding/json` and a field
nothing reads is silently ignored: an author who writes `result_schema` on a fetch would lose
their schema without being told. `validateActionRequiredFields` already switches on action type
and is the cheap place for it. An `Action.UnmarshalJSON` would also work, but it has to keep
`DelaySpec`'s fields flat on the wire — a trap [internal/model/CLAUDE.md](../internal/model/CLAUDE.md)
documents for precisely this shape.

**The union is `anyOf`, not `oneOf`.** Status bodies overlap in practice — two object
schemas whose properties are all optional both admit `{}` — and `oneOf` means *exactly one*
arm matches, so an overlapping union rejects a body that conforms to two of its arms. This
codebase has already paid for that once: the enum-aware canonicalization in
[literal-types.md](literal-types.md) exists because `?? false` inferred an overlapping `oneOf`
that rejected `false`. Runtime conform is per status and never touches the union, so the
damage would land where it is hardest to see — in the generated `<taskID>_output` schema a
consumer reads, and in `IsSubset` during a compat check.

**`last_error` is scoped to the task its rule routes to.** It used to persist on the instance
until another failure overwrote it, so a task three hops past a handler could still read a
failure it was never written for. Now the engine drops it on every ordinary transition, and
inference types it only on tasks an error edge enters. A handler that wants the failure to
travel projects it into its own `output` — the mechanism every other value already uses, and
an explicit one, where the old behaviour was an implicit second channel nothing declared.

Two things fall out. The static side collapses: `mustErr`/`mayErr` and the rules reaching a
handler stop being fixpoints over the graph and become local questions about one task's
incoming edges. And the rule applies to the whole error model, not just fetch — a child
failure routed by `on_error` is scoped the same way, because both write the same slot. All
three shipped examples already read `error` in the immediate `goto` target, so the
generality being removed had no user.

**Neither channel needs narrowing.** `{"200": T, "202": U}` is an `anyOf`; refining it by
`self.status` needs literal types, discriminated unions and guard narrowing, all deferred. The
design does not depend on them. The common success shape is one body-carrying status plus N
empty ones — `T | null`, which the type system already has. The error side discriminates for
free: an `on_error` rule already selects by code, so `error.data` at a handler is the union
over the rules reaching it — the treatment `contextSchemaAbsent` gives `outputs` — and exactly
one type where one rule reaches one handler.

## Build notes

- **`Responses map[string]*schema.Schema` — the pointer is load-bearing.** `encoding/json`
  calls `UnmarshalJSON` on a value type *including for a JSON null*, and `Schema.UnmarshalJSON`
  decodes `null` into a zero node indistinguishable from `{}`. Only a pointer is set to nil
  without the unmarshaler running. Key present + nil = declared with no body; key absent =
  undeclared. Tidying this to a value type silently turns every "no body" into "untyped body"
  and dissolves the non-nullable guarantee. Round-trip is otherwise free: `SaveDefinition`
  stores `json.Marshal` of the decoded struct, and a nil entry marshals back to `null`.
- `sendHTTP` changes on both exits: an empty body yields `Body: nil` instead of running
  `DecodeReader` to `io.EOF`, and the error exit decodes the body under `MaxResponseBytes`
  instead of trimming 512 bytes — **keeping the trimmed string as `ErrorMessage`**, which is
  what an operator reads. Both exits now run the same three checks, and on the error exit a
  failure replaces `errcode.HTTP(status)` with the body-validation code while the message
  still names the status, so the trail does not lose what actually came back. The `N+1`
  overflow detection must survive untouched; see
  [internal/transport/CLAUDE.md](../internal/transport/CLAUDE.md), where the ordering of the
  size check against the parse error is the invariant.
- `error` gains `data`, written beside `task`/`message`/`code` at
  [error.go:71](../internal/engine/error.go#L71) and typed as an optional nullable property on
  `errSchema` at [infer.go:424](../internal/validation/infer.go#L424). Absent for every error
  carrying no response at all (`pre.*`, `http.timeout`, `http.disconnected`,
  `only_once.interrupted`, a child
  raise), which is most of the set — nullable is not a formality.
- **`error.data` persists like a task output, which is a change: `error` was not a value-slot
  when this was written.** *Built, and by a simpler answer than the one below.* `error` is now
  an ORDINARY cut slot — `encodeState` puts it through the same `cut` as every other value
  ([db_instances.go:207](../internal/db/db_instances.go#L207)) — with no per-field envelope at
  all. The worry the envelope existed for (every `error.code` read in an `on_error` handler
  paying for a body it never asked for) is answered on the READ side instead: `model.Context`
  walks to the path and loads only what that path needs (lazy-context.md), so cheapness is the
  accessor's job rather than the column's. The plan it replaces is kept below because the
  reasoning about *where* the cost lands is what chose the accessor.

  > Outputs are enveloped **per task id**, which is what keeps one big output from dragging the
  > rest through an object load, so the faithful translation envelopes `error.data` **alone**
  > and leaves `task`/`message`/`code` inline. Pinning, dereference and GC then apply unchanged.
  > The stored shape of `error_internal` changes with it — in-flight instances in an existing
  > database do not survive that, which a prototype can accept and a release could not.

- Keys need a splitter in front of `ValidStatusPattern`, which validates one pattern: split on
  commas, trim, validate each, reject an empty element or a repeat within a key. Specificity,
  overlap, straddling and coverage are then one pass over the patterns. `matchAcceptedStatus`
  is untouched, and the editor schema's `patternProperties` admits the list form
  (`^\s*[1-5](\d\d|xx)(\s*,\s*[1-5](\d\d|xx))*\s*$`).
- **Precedence is a typing concern only.** Acceptance asks whether *any* pattern matches and
  needs no order; exact-beats-range applies where a schema is selected — in inference, and
  again at runtime when the response lands. Two call sites, one resolver, or they drift.
- **A plain code is written unquoted; a range or a list is not.** `200:` is an `!!int` node
  in YAML, but `yamlToAny` decodes every mapping key into a Go string and yaml.v3 obliges, so
  it reaches genroc as `"200"` — measured, not assumed. `"4xx"` and `"400, 401"` have no bare
  form, and JSON has no integer keys at all, so anything written or stored as JSON quotes
  everything regardless. The rule holds for authored YAML only, and the quotes that remain
  there are the ones that carry information.
- Add a `ruleFieldHints`-style hint for `schema` ([wire.go:206](../internal/model/wire.go#L206)):
  anyone arriving from OpenAPI writes `{"200": {schema: ...}}` and gets `unsupported schema
  keyword "schema"` from the strict allowlist — correct, but it should name the fix.

## Blast radius

The polling example's `check` drops `result_schema` and keeps its dynamic `accepted_status`;
it may now also declare `"202"`, typing the body its `$backoff` handler routes on, both
channels nullable there. `result_schema` stays on `child`, `child_list` and `external`, where
there is no status to key on. Every fetch fixture declaring a schema becomes narrower in what
it accepts — mechanical, and small. Nothing that already names a status is rewritten.

The compat command compares a fetch's result as **one merged union under the same
`task:fetch.result` address every other action type uses** — not per status. `child_map` looks
like the precedent and is not: its keys are separately readable outputs (`outputs.<task>.<key>`),
so each is genuinely its own contract, whereas a fetch's statuses all feed the single
`self.result` and no consumer can read one of them apart from the others. Comparing them one
at a time judges something nobody can observe, and it is wrong in both directions — dropping a
bodyless status reads as a removed declaration and goes unreported, though as a union it
narrows what the remote may answer and breaks a producer; and restating the same union over a
range (`{"200": T}` → `{"2xx": T}`) reads as one key removed and another added, reporting a
break for an edit that changed no type at all. Both are pinned by
`TestCompat_FetchResultIsComparedAsTheMergedUnion`.

The direction is the one every result schema runs, `old ⊆ new`: the schema is a demand on the
party that PRODUCES the value, so a wider union turns nobody away while a narrower one breaks
the producer that satisfied the old. What a downstream reader of `self.result` sees is a
different question, answered by the task's own output comparison — a result becomes visible
outside its task only through a projection.

`changedslots.go` follows the same address: `responses` is an ordinary slot whose leaf name is
`result`, exactly as `result_schema`'s is, because §6b's suppression drops a slot row only
where a break carries the SAME address.

# §3 — Response metadata

Two new siblings of `self.result`, **fetch tasks only**: `self.status` (integer) and
`self.headers` (`object<string>`, every access `string | null`). `self.result` keeps meaning
the decoded body — nothing is re-wrapped, which is the whole compatibility story. Decisions:

- **Lowercase header keys** (Go canonicalizes to `Retry-After`; a canonicalized map makes
  `self.headers['retry-after']` silently null — predictability beats fidelity; browsers
  lowercase too). Rejected: snake_casing header names — lossy, and the definition stops
  matching the wire.
- **Comma-join repeated headers** so the type stays the flat `object<string>` the request slot
  already uses. `Set-Cookie` is the accepted casualty.
- **Fetch only** — both the runtime `self` maps and the inference schema need an action-type
  gate so `delay`/`child` don't grow an always-null `self.status`.

**The blocker, found here and since built:** the parser accepted only integer literals in
`[...]`, so `self.headers['retry-after']` was a parse error and `.retry-after` a subtraction —
dot access fails for most of HTTP. Chosen fix: a string-literal index desugars to `MemberNode`
(identical semantics to `.foo`, non-breaking since it was a parse error before), unlocking
arbitrary-key access on every open map. What the sketch missed: inference carried access paths
as dot-joined strings, so `x['a.b']` and `x.a.b` rendered identically — a secret on a dotted key
could escape redaction — hence paths became steps (`nodeSteps`/`pathStep`), the non-free part.
Follow-up also built: **computed keys** on homogeneous bases (arrays, additionalProperties-only
maps), type-checkable because every key has the same type; rejected on objects with named
properties.

**What it buys, and has not yet been spent:** the poller's 202 loop can become an ordinary
switch (`accepted_status: ["200","202"]`, `case: self.status == 202`) instead of routing
through `on_error` — no error-path loop, no 19 `action_failed` entries per healthy run, the
attempt counter back on the task that polls. The example still uses the old trick, because
switching it means the poller must accept 202 itself and its caller therefore stops choosing
`accepted_status` — a change to that example's input contract, not to this feature. **`accepted_status` quietly shifts meaning** — from "which statuses are
successes" to "which statuses I handle myself" — worth stating in docs; behaviour is unchanged.

**Compatibility:** no stored definition can reference the new names (navigation would have
rejected them), objects are open so wholesale `output: "$: self"` exports widen safely, nothing
new persists, no migration, no root needed for lazy loading (never externalized). One remote
risk worth a directed test: `output: "$: self"` inside a recursive task grows the type per
unrolling level against the solver's widening cap. **Trap:** do not re-wrap `self.result` as
`{body, headers, status}` — tidier, and breaks every definition in existence.

# What none of this does

- **No media types.** A `text/plain` 200 still fails: the decode is JSON-only. `responses`
  describes shape, not content type.
- **No per-status response headers.** `self.headers` is one runtime map of what arrived.
- **No claim that a declared error status can occur.** Unlike the success side, where
  acceptance and declaration are one statement, declaring `"404"` asserts only what a 404
  would contain — nothing checks the endpoint can return one, and nothing requires an
  `on_error` rule to catch it.

# Open questions

- Should a declared error status whose body fails to conform leave a `warn` audit entry, or is
  that noise on a path already logging `action_failed`?
- Does `accepted_status` still earn a slot once `responses` keys carry the same vocabulary? It
  survives for the dynamic case alone — one example's requirement holding a slot open for all.
- Number/boolean `query` values: stringify (convenient, matches url templates) vs strings-only
  (trivial target)? Repeated parameters via array values — defer until asked.
- Should `headers` gain `query`'s null-omits for consistency? A behaviour change to an existing
  slot; needs its own argument.
- Version skew on new action fields generally — `min_engine`, or rejecting unknown action
  fields; both breaking, both bigger than these features.
- `Retry-After` is still not usable end to end: it is in seconds, `ms` wants milliseconds, and
  the expression language has no numeric conversion builtin — separate proposal.
- `Set-Cookie` lost to comma-joining; reopen if a session-carrying flow needs it.
