# `fetch`: responses

Draft 2026-08-10, not implemented. Replaces `result_schema` on a fetch with one map from
status to schema describing **the whole endpoint**, success and failure alike. The status
class does the splitting: a declared 2xx types `self.result`, a declared 4xx/5xx types
`error.data` and still routes through `on_error`.

Independent of both parts of [fetch-http-surface.md](fetch-http-surface.md), and worth
reading against Part 1: once `self.status` exists, a definition can branch on the status
this spec types.

## The bug it starts from

`accepted_status` defaults to any 2xx and `sendHTTP` decodes the body unconditionally, so a
`202 Accepted` carrying no body is accepted and then fails to parse. Measured, not inferred
— `204`, an empty `200`, and a `text/plain` `200` all return `output.parse`, and
[transport.go:130](../internal/transport/transport.go#L130) sets no `ErrorMessage` on that
path, so the audit trail says `output.parse` and nothing else. `DELETE` → 204, an async
kickoff → 202, a webhook ACK: none of them are expressible.

`result_schema` compounds it rather than fixing it. One schema covers whatever status
arrived, so a task accepting 200 and 202 must declare a schema true of both — nullable,
hand-written, and asserting a union nothing checks. On the other side, an error payload is
unreachable at any type: `error` is `{task, message, code}`
([error.go:71](../internal/engine/error.go#L71),
[infer.go:424](../internal/validation/infer.go#L424)), and a `problem+json` body survives
only as the 512 bytes [transport.go:109](../internal/transport/transport.go#L109) trims into
`message`.

## The design

```yaml
action:
  type: fetch
  url: "${ config.api_url }/jobs/${ input.id }"
  method: GET
  responses:
    "200": { type: object, properties: { state: { type: string } } }
    "202": null
    "404": { type: object, properties: { detail: { type: string } } }
```

Three rules:

1. **Acceptance** — `accepted_status` when present; otherwise the 2xx patterns of
   `responses`; otherwise every 2xx. Declaring `"404"` therefore types it **without** accepting
   it: it still raises `http.404` and still routes through `on_error`.
2. **Typing** — a declared schema types the body of its status, into `self.result` if that
   status is accepted and into `error.data` if it is not. A `null` entry declares "no
   body". An accepted status matched by no declared pattern contributes `null` to
   `self.result`; an unaccepted one leaves `error.data` absent.
3. **Enforcement, asymmetrically** — see below.

### Keys

A key is a comma-separated list of the status patterns `accepted_status` already takes — an
exact code or a hundred-range — so one schema can cover a class or an enumerated set:

```yaml
responses:
  "200":      { type: object, properties: { state: { type: string } } }
  "202":      null
  "400, 401": { $ref: "#/$defs/problem" }
  "5xx":      { type: object, properties: { trace_id: { type: string } } }
```

    key     := pattern ("," pattern)*      # surrounding whitespace ignored
    pattern := [1-5][0-9][0-9] | [1-5] "xx"

Each pattern in a key resolves independently; the key is a set, not a unit. The status space
is finite, so the questions the grammar raises are settled by enumeration rather than by a
syntactic rule:

- **Exact beats range**, per pattern rather than per key — `{"404": A, "4xx": B}` gives a
  404 the `A`.
- **Equal-specificity overlap is a registration error**, naming both keys (`"400, 401": A`
  beside `"401, 402": B`). Not a precedence puzzle.
- **No declaration may straddle the success/failure split** — neither a pattern spanning it
  nor a key whose patterns collectively do (`"200, 404"`). Rule 1 reads a declaration as
  deciding acceptance, so a straddling key narrows acceptance from a line the author may
  have written for the error side only. Say `"4xx, 5xx"`.

Coverage is then a pattern-subset test rather than set membership, which is what makes a
range key worth having on the success side too: `{"2xx": T}` covers the whole default
accepted set, so `self.result` is exactly `T` — no `null` arm, without enumerating 200, 201
and 204.

So `{"200": T}` alone types `self.result` as exactly `T`: non-nullable, and enforced rather
than asserted. That is the point of the success half; everything below defends it.

## Decisions

**The success/failure split is the status class, not the slot.** One map describes the
endpoint the way an OpenAPI `responses` object does, and genroc reads 2xx as success —
a rule no author has to be taught. Two alternatives were rejected:

- *`keys(responses) ∪ accepted_status` as the accepted set.* Declaring a 404 to get its
  payload would silently **accept** the 404 — the task succeeds, `on_error` never runs, and
  the definition's error handling quietly stops existing. A declaration made for typing must
  not change routing.
- *An `error_schema` on the `on_error` rule instead.* It types the same body and needs no
  status-class rule, but it splits one endpoint's description across two slots and adds a
  field to a rule that is about control flow. The endpoint is one thing; describe it once.

**The status vocabulary stays `"2xx"`; `code`'s `%` was considered and rejected.** The pull
is real — a task writes `"5xx"` in `responses` and `code: [http.5%]` in an `on_error` rule a
few lines below, two spellings for one set of statuses — and `%` would have made `"40%"`
sayable, which a hundred-range cannot express at any length. It loses on the boundary that
matters more: `2xx` is what RFC 9110 and OpenAPI write, and these keys are copied out of API
documentation far more often than they are read beside an `on_error` rule. Keeping it also
means `ValidStatusPattern` and `matchAcceptedStatus` stand as they are, so this spec adds a
splitter rather than a migration across every definition that already names a status. Reopen
if a status pattern ever needs to say something a hundred-range cannot.

**A range is capability; a comma list is ergonomics.** The two halves of the key grammar
earn their place differently and it is worth not confusing them. A class cannot be
enumerated — `"4xx"` is the only way to say "every client error returns this envelope", and
without it the RFC 7807 case, which is the single most common reason to type an error body
at all, is unwritable. A comma list adds no expressiveness: `{"400": {$ref: "#/$defs/problem"},
"401": {$ref: "#/$defs/problem"}}` already works, since the schema package has `$defs` and
`$ref`. It is in because the `$ref` spelling makes an author name a definition to say
"these two are the same", and the naming is the part that gets skipped. Rejected: OpenAPI's
`default` key. On the error side it would be a fine catch-all; on the success side it cannot
answer rule 1 — a pattern matching everything either accepts everything or accepts nothing,
and both readings are wrong. `"4xx, 5xx"` says it without the ambiguity, and the straddling
rule refuses the alternative outright.

**Enforcement is asymmetric, and deliberately so.** On the success side the schema is a
contract: an empty body decodes to `null` rather than failing to parse, and the declared
schema then validates it, so a declared `"200"` arriving empty is `output.invalid` naming
the empty body. On the error side it is best-effort: a body that is not JSON, overruns
`MaxResponseBytes`, or does not conform leaves `error.data` null and logs, while `http.404`
routes exactly as it would have. The reason is that there is no failure left to escalate
into — the error path *is* the recovery path, and making it fail on a malformed payload
converts a handled error into an unhandled one. The cost is that `error.data` is always
nullable, even where declared; that is the honest type, and `??` already reads it.

**`error.data` is present exactly where a pattern is declared.** An unaccepted status with
no declared pattern leaves it absent rather than untyped — the rule `self.result` already
obeys, where no `result_schema` means no `self.result` at all because *undeclared data is
never accessible* ([infer.go:440](../internal/validation/infer.go#L440)). The escape hatch is
the top type: `"4xx": {}` says "a body arrives, shape unknown", which is carried and
exportable without being navigable ([unknown-type.md](unknown-type.md)) until a consumer
restates it. Reading an undeclared body would be the one place in the language where data
reaches an expression with no declaration behind it, and it would do so on the path where the
payload is least trustworthy — a body from a service that has just failed.

Absent from the expression context is not absent from the record: `action_failed` already
carries the body in its `data` ([action.go:94](../internal/engine/action.go#L94)), so an
undeclared payload stays diagnosable while staying unreadable by the definition. Two gates
sit on that, and neither is this spec's to move — the entry is `LogDebug`, like every other
action payload, and `snippetRaw` blanks it unless payload logging is enabled. "It is in the
logs" is true of an operator who turned both on, not of a default deployment.

**`null` is "no body", `{}` is "a body of unknown type".** `{}` already means the top type
everywhere else ([unknown-type.md](unknown-type.md)) and must not be locally redefined;
`null` is free because it is not a valid JSON Schema, and the boolean form that might have
competed for the slot is already refused
([schema.go:155](../internal/schema/schema.go#L155)). Rejected alternative: nesting the
schema under a `schema:` key so a bare `{}` could mean "no body" — it buys a place to hang
per-status metadata that nothing needs (Part 1's `self.headers` is a runtime map of what
arrived, not a per-status declaration) and costs the correspondence with `result_schema`,
which is a bare schema.

**Declaring a 2xx narrows acceptance.** A POST that starts returning 201 against
`{"200": T}` raises `http.201` — loud, catchable, and honest, since nothing proves the 201
body is a `T`. The narrowing is confined to the success class by rule 1, and the message
must name the way out ("201 is not among the declared responses — add it, or set
`accepted_status`"). The quieter alternative — keep 2xx accepted always and derive the null
from the set difference — makes every typed fetch `T | null`, with the nullability read off
the **absence** of a slot rather than the presence of one. Defaulting the accepted set to
200 only was rejected for the opposite reason: it makes 201 and 204 faulty for a definition
that declared nothing at all, the one case where the author has expressed no expectation.

**`accepted_status` stays, is authoritative when present, and stays a Shape.** The accepted
set can be runtime data: [poller.genroc.yaml:59](../examples/polling-task/poller.genroc.yaml#L59)
takes it from the caller's input, which is what makes that example a *generic* poller.
Schemas must be static for inference, so one map cannot carry both. Authoritative rather
than widening is what lets the poller declare `"202"` for its type while its caller decides
whether 202 is a success — the two slots then answer two different questions instead of
competing over one. **The edge that follows:** under a dynamic `accepted_status` no declared
status is statically known to be accepted, so its schema appears on **both** channels,
nullable on each. Dynamic acceptance costs static precision; it does not get a special case.

**A leftover `result_schema` on a fetch must be a decode error.** `Action` decodes with
plain `encoding/json` and has no `UnmarshalJSON`, so a retired field silently decodes into
the struct and is ignored — the definition loses its schema and nothing says so. Same
family as the version-skew hazard [fetch-http-surface.md](fetch-http-surface.md) names for
`query`, but this one is reachable today by anyone editing an existing definition, so it
gets a real rejection naming `responses`.

**Neither channel needs narrowing.** `{"200": T, "202": U}` is a `oneOf`; refining it by
`self.status` needs literal types, discriminated unions and guard narrowing, all deferred.
The design does not depend on them. The common success shape is one body-carrying status
plus N empty ones — `T | null`, which the type system already has. And the error side
discriminates for free: an `on_error` rule already selects by code, so `error.data` at a
handler is the union over the rules that reach it — the treatment `contextSchemaAbsent`
gives `outputs` — and is exactly one type where one rule reaches one handler.

## Build notes

- **`Responses map[string]*schema.Schema` — the pointer is load-bearing.** `encoding/json`
  calls `UnmarshalJSON` on a value type *including for a JSON null*, and
  `Schema.UnmarshalJSON` decodes `null` into a zero node indistinguishable from `{}`. Only
  a pointer is set to nil without the unmarshaler running. Key present + nil = declared with
  no body; key absent = undeclared. Tidying this to a value type silently turns every "no
  body" into "untyped body" and dissolves the non-nullable guarantee.
- Round-trip is free: `SaveDefinition` stores `json.Marshal` of the decoded struct, and a
  nil entry marshals back to `null`.
- `sendHTTP` changes on both exits: an empty body yields `Body: nil` instead of running
  `DecodeReader` to `io.EOF`, and the error exit decodes the body under `MaxResponseBytes`
  instead of trimming 512 bytes — **keeping the trimmed string as `ErrorMessage`**, which is
  what an operator reads, alongside the parsed value. The `N+1` overflow detection must
  survive untouched; see [internal/transport/CLAUDE.md](../internal/transport/CLAUDE.md),
  where the ordering of the size check against the parse error is the invariant.
- `error` gains `data`: written beside `task`/`message`/`code` at
  [error.go:71](../internal/engine/error.go#L71), typed as an optional nullable property on
  `errSchema` at [infer.go:424](../internal/validation/infer.go#L424). It is absent for every
  error that carries no response at all (`pre.*`, `http.timeout`, `only_once.interrupted`, a
  child raise), which is most of the set — nullable is not a formality here.
- Keys need a splitter in front of `ValidStatusPattern`, which validates one pattern: split
  on commas, trim, validate each, and reject an empty element or a repeat within a key.
  Specificity, overlap, straddling and coverage are then one pass over the patterns — exact,
  and cheap enough to run at registration. `matchAcceptedStatus` is untouched.
- The editor schema's `patternProperties` regex admits the list form:
  `^\s*[1-5](\d\d|xx)(\s*,\s*[1-5](\d\d|xx))*\s*$`, so a typo'd key fails in the editor
  rather than never matching at runtime.
- **Quote the keys in every doc and example.** Unquoted `200:` is an *integer* key in YAML,
  and whether it survives to genroc as `"200"` depends on the YAML→JSON conversion rather
  than on anything this spec controls. Only all-digit keys are actually at risk, which is
  precisely why the rule is "always quote" — an exception is a thing to remember wrong.
- **Precedence is a typing concern only.** Acceptance asks whether *any* pattern matches and
  needs no order; smallest-match-wins applies where a schema is selected — in inference,
  building the two unions and the coverage test, and again at runtime when the response
  lands. Two call sites, one resolver, or they drift.
- Add a `ruleFieldHints`-style hint for `schema` ([wire.go:206](../internal/model/wire.go#L206)).
  Anyone arriving from OpenAPI writes `{"200": {schema: ...}}` and gets `unsupported schema
  keyword "schema"` from the strict allowlist — correct, but it should name the fix.

## Blast radius

- The polling example's `check` drops `result_schema` and keeps its dynamic
  `accepted_status`. It may now also declare `"202"`, typing the body its `$backoff` handler
  routes on; both channels are nullable there, which is what a generic poller honestly
  knows. Its `http.202` comment stands until Part 1 lands.
- `result_schema` stays on `child`, `child_list` and `external`, where there is no status to
  key on.
- Every fetch fixture declaring a schema becomes narrower in what it accepts; the rewrite is
  mechanical and small.
- `accepted_status` is untouched — same vocabulary, same matcher, same Shape. Nothing that
  already names a status has to be rewritten.

## What it does not do

- **No media types.** A `text/plain` 200 still fails: the decode is JSON-only. `responses`
  describes shape, not content type.
- **No per-status response headers.** `self.headers` (Part 1) is one runtime map.
- **No claim that a declared error status can occur.** Unlike the success side, where
  acceptance and declaration are the same statement, declaring `"404"` asserts only what a
  404 would contain — nothing checks that the endpoint can return one, and nothing requires
  an `on_error` rule to catch it.

## Open questions

- Does a `null` entry **reject** a body that arrives anyway, or ignore it? Ignoring matches
  "acknowledgment"; rejecting is symmetric with the success half of rule 3.
- Does `accepted_status` still earn a slot once `responses` keys carry the same vocabulary?
  It survives here for the dynamic case alone (a Shape resolved per attempt), which is one
  example's requirement holding a slot open for everyone.
- Should a declared error status whose body fails to conform leave a `warn` audit entry, or
  is that noise on a path that is already logging `action_failed`?
