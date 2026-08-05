# Error handling: design and record

Status: **implemented 2026-07-31.** Began as an audit; kept as the record of what
replaced the old paths and why. The finding that frames it: **the workflow error model
was good and the Go plumbing under it was not** — two systems sharing the word "error",
only the first designed. The work gave the second a design too, without merging them.

## What was already right — do not "fix" these

- **`errcode`**: single source of engine codes, zero genroc deps (importable
  everywhere), namespaces carrying semantic guarantees — `pre.` *means* "request never
  left", which is what makes `only_once` retries safe. The taxonomy encodes the
  property; it is not a naming convention.
- **`advanceOutcome`** is a sum type — failures are values in the normal flow.
- **`failInstance(inst, code, reason)`** takes the code positionally: no failure path
  can leave `error_code` empty. Policy by signature.
- **`ClassifyGoError`** separates dial from response timeouts via `errors.As`.
- **The expression parser** re-panics on any recovered value that is not a
  `parseError` — it never swallows unrelated bugs.

# Part 1 — the REST API

**The classification lives on `Reply`, not the HTTP response**: all three transports
share `Reply` (TCP/UDS have no status line); HTTP status is the *rendering* of the
code, mapped in one table. The set is small because it is what a **client** can act on:
`invalid` 400, `not_found` 404, `conflict` 409 (same call may succeed later),
`unsupported` 501, `internal` 500. Engine-level detail belongs in `errcode` on the
instance. **Unclassified defaults to 500, not 400** — that made the migration
self-driving: an unclassified error is a server fault until shown otherwise, so every
remaining 500 was an unexamined path. (Before: two status writes total; outage, missing
id, bad payload and unknown action were indistinguishable.)

**Classification is inherited, not repeated** — `codeOf` walks: explicit `*api.Error` →
db sentinel → validation failure → `internal`; a forwarding handler gets the right
status deciding nothing. The db sentinels' load-bearing split is `ErrConflict` ("may
work later") vs `ErrInvalid` ("never will"). Two deliberate overrides, each commented:
a submitted parent naming an off-channel child is `invalid` despite the underlying
not-found (the fault is in the document), and the batch-apply validation block is
`invalid` — except `ResolveConfig`, which reports the *server's* environment and stays
unclassified on purpose.

**Per-field validation errors**: `*model.ValidationError` carries `[]FieldError`
(field/rule/param/message, validator namespace with the root struct stripped), surfaced
as `fields` on the response. The index is what earns it — three tasks missing `id`
produce three identical messages but distinct `tasks[N].id` paths. `Error()` still
renders the joined form, and `genctl` keys on the `input validation: ` /
`result validation: ` prefixes — **both load-bearing**, commented at both sites.
`fieldsOf` unwraps rather than type-asserts, which is what survives `applyBatch`'s
per-process prefix wrapping.

**Decoders**: `okReply` no longer discards marshal errors (a 200 with an empty body
was the failure mode). Optional bodies decode strictly (`DecodeStrict` = Decode +
DisallowUnknownFields) — optional means *presence*, never "unparseable is fine";
`{"advance_ms": "12000"}` used to leave the clock unmoved and answer 200, silently
un-advancing time under whole tests. `DecodeStrict` is a separate function, not a flag:
`Decode` also reads stored rows and already-accepted payloads, where an unknown field
is history — strictness belongs only at the entry boundary. Layering bounds what this
catches: syntactically bad JSON is rejected a layer up; a JSON `null` into a struct
stays a no-op; over HTTP only `/tick` passes a client body to `decodeOptionalBody`
(hence the coverage lives in `tests/tick/optional_body_test.ts`).

# Part 2 — Go-level plumbing

- **Wrapping is now walked.** The audit found 157 `%w` wraps against five
  `errors.Is/As` sites and zero `Unwrap` methods — a chain built and never traversed.
  Fixed by introducing *values only where a caller branches* (db sentinels,
  `*api.Error` with `Unwrap`, `*model.ValidationError`), not by converting 395 sites.
- **`sql.ErrNoRows`**: all comparisons moved to `errors.Is` (never a live bug, but the
  sentinels' wrappers would have made it one). The real work was deciding which empty
  scans mean `ErrNotFound` — **only some do**; an absent parent in FinishChild, an
  empty signal queue, and no-identical-version in `applyBatch` are control flow, and
  promoting them would have turned normal operation into 404s.
- **`net.ErrClosed`** matched by `errors.Is`, not string text — a mismatch turns clean
  shutdown into a logged-error hot loop.
- **`errcode.Code` is a type**, not a bare string; the three places a non-code string
  legitimately becomes a code (authored panic, authored raise, a child's persisted
  code) are explicit conversions, each commented.

## The panic barrier

`advanceGuarded` converts a panic under `advance` into a terminal `EnginePanic` failure
— the blast radius is one definition, and killing the worker dropped every healthy
in-flight advance without even isolating the culprit (it gets re-claimed and panics
again). Three details `panic_barrier_test.go` pins: the barrier covers `advance()`
only, never `persist()` (a write-path panic is not definition-attributable, and nothing
is left to write a failure with); recording the panic can itself panic (audit resolves
the same malformed definition to redact secrets), so the outcome is pre-set, the
console is written first via the one path that cannot fail, and the durable recording
runs under a second barrier — `failInstance` assigns terminal fields *before* it
audits; one accepted residual — a panic after a committed spawn fails the parent while
children run on, which the failing/collect logic already tolerates.

**Open, low priority:** background loops log-and-continue with no escalation — a
renewer failing for ten minutes is indistinguishable from one that failed once. Noted
so it is not mistaken for an oversight.

## Resolved decisions

- **D1 — the error code is API contract**: `Code` publishes an `Enum()`, every
  operation documents its error statuses, each action declares extra codes in the
  registry (`invalid`/`internal` implicit everywhere). Shipping it undocumented is the
  worst of both — clients key on it anyway and nobody owes them stability.
- **D2 — recover in advance, fail the instance** (above).
- **D3 — malformed optional bodies rejected, unknown fields with them** — the typo
  case (`advnace_ms`) is the likelier mistake.

Compatibility: `genctl` treats ≥400 uniformly and needed nothing; the suite asserted
400 in two places (both still 400); the visible break is `/tick` on a polling server,
400 → 501. Coverage: `api_errors_test.ts` end to end.
