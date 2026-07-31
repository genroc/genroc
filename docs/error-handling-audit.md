# Error handling: design and record

Status: **implemented (2026-07-31).** Closes `ROADMAP.md` → "Go + REST API error
handling". This began as an audit (2026-07-23) of the error paths as they stood; it
is now the record of what replaced them, keeping the findings because they are the
reason the current shape is the shape it is.

The finding in one line, which still frames everything below: **the workflow error
model was good and the Go plumbing under it was not.** Those are two different systems
that happen to share the word "error", and only the first had been designed. The work
was to give the second one a design too — not to merge them.

## What was already right — do not "fix" these

The domain model is deliberate. Listed first so a later cleanup pass does not flatten
it in the name of consistency:

- **[errcode](../internal/errcode/errcode.go)** is the single source of truth for
  engine-produced codes, with no genroc dependencies so every layer can import it
  without a cycle. Codes are namespaced, and the namespace carries a semantic
  guarantee: `errcode.NotReached` (`"pre."`) means the request never left, which is
  exactly what makes a retry safe for an `only_once` task
  ([isRetryAllowed](../internal/engine/error.go#L21)). That is a property the taxonomy
  *encodes*, not a naming convention.
- **[advanceOutcome](../internal/engine/advance.go#L22)** is a sum type. The failure
  path (`failInstance`) returns the same type as the success path, so errors are values
  in the normal flow rather than a second control channel.
- **`failInstance(inst, code, reason)`** takes the code positionally, so no failure path
  can leave `error_code` empty. Policy enforced by signature.
- **[ClassifyGoError](../internal/transport/transport.go#L157)** uses `errors.As` into
  `*net.OpError` to separate a dial timeout from a response timeout.
- **[the expression parser](../internal/expression/syntax/parser.go#L39)** uses
  panic/recover internally and **re-panics** on any value that is not a `parseError`,
  instead of swallowing unrelated bugs.

None of that changed. What changed is that this care now extends past the edge of the
engine.

---

# Part 1 — the REST API

## The classification, and why it lives on `Reply`

[internal/api/errors.go](../internal/api/errors.go) defines a small `Code` set:

| `Code` | HTTP | meaning |
|---|---|---|
| `invalid` | 400 | malformed or unacceptable request; retrying it unchanged never helps |
| `not_found` | 404 | the definition / instance / channel / task named does not exist |
| `conflict` | 409 | well-formed, target exists, but its state forbids this now; the same call may succeed later |
| `unsupported` | 501 | the endpoint is routed but this server is not configured to serve it |
| `internal` | 500 | unclassified — a server fault until shown otherwise |

The code sits on `Reply`, not on the HTTP response, because all three transports share
`Reply`: TCP and UDS encode it directly ([handleConn](../internal/api/server.go#L137))
and have no status line to read. **The status is HTTP's rendering of the code, not the
other way round.** `writeReply` maps one to the other through a single table.

The set is deliberately small. These are distinctions a *client* can act on — fix the
request, look elsewhere, wait and retry, give up. Detail about what actually broke
belongs in `errcode`, on the instance, where the engine already puts it.

**Unclassified defaults to 500, not 400.** That is what made the migration
self-driving: an error nobody had classified is a server problem until someone shows
otherwise, so any path still answering 500 was a path nobody had looked at. Before
this, the whole HTTP surface had exactly two status writes — a 404 for an unknown spec
route and a blanket 400 for everything else — so across all 28 actions a database
outage, a missing instance id, a malformed payload and an unknown action name were the
same response. A client could not tell "retry this" from "never retry this", and a 4xx
rate meant nothing because it contained both.

## Classification is inherited, not repeated

The thing that keeps this from being 28 hand-maintained mappings: `codeOf` resolves an
error in precedence order — an explicit `*api.Error`, then a db-layer sentinel, then a
definition-validation failure, then `internal`. So a handler that merely forwards a db
error gets the right status without deciding anything:

```go
inst, err := h.db.GetInstance(id)
if err != nil {
    return errReply(err)   // db.ErrNotFound → not_found → 404
}
```

Handlers only construct a code where they are the ones making the judgement —
`invalid("id is required")`, `conflict(...)`, `unsupported(...)`.

The db layer's three sentinels are in [internal/db/errors.go](../internal/db/errors.go)
(`ErrNotFound`, `ErrConflict`, `ErrInvalid`), wrapped with `%w` and the human wording
kept in the prefix. The split that matters is `ErrConflict` vs `ErrInvalid`: "retrying
this may work later" vs "retrying this is pointless". Pause/resume/retry state
rejections are conflicts; naming a descendant where a tree root is required is invalid.

Two places deliberately override an inherited classification, and both say why in a
comment: a parent naming a child that is not on the channel is `invalid` even though a
`db.ErrNotFound` is underneath it (the fault is in the submitted document, not in a
resource the caller asked to read), and everything in the batch-apply validation block
is `invalid` rather than `internal` — except `ResolveConfig`, which reports the
server's own environment and stays unclassified on purpose.

## Per-field validation errors

`fmtValidationErr` receives `validator.ValidationErrors` — one entry per failed field,
each with the field, the failed tag and its parameter — and used to join them into a
sentence. For an API whose main job is accepting user-authored process definitions,
that threw away the most useful part. It now returns a
[`*model.ValidationError`](../internal/model/validate.go#L488) carrying
`[]model.FieldError`, which `errReply` surfaces as `fields` on the response:

```json
{"error": "tasks must have at least 1 item(s)", "code": "invalid",
 "fields": [{"field": "tasks", "rule": "min", "param": "1",
             "message": "tasks must have at least 1 item(s)"}]}
```

`Field` is the validator namespace with the root struct name stripped, so it reads as
the client wrote it: `tasks[0].id`, not `ProcessDefinition.tasks[0].id`.

The index is the part that earns its keep, and it is why the joined message alone was
not enough: for a nested failure the message names only the leaf. Three tasks missing
an `id` produce three entries whose messages are all `"id is required"` and are
therefore indistinguishable — `tasks[0].id` / `tasks[2].id` are not. Both halves are
pinned, the path construction in
[validation_error_test.go](../internal/model/validation_error_test.go) and its survival
to the wire in [api_errors_test.ts](../tests/integration/api_errors_test.ts), including
through the process-name prefix `applyBatch` wraps each definition's failure with —
which only works because `fieldsOf` unwraps rather than type-asserting.

`Error()` still renders the joined human form, so every caller that only prints continues to work
— including `genctl`, which keys on the `input validation: ` / `result validation: `
message prefixes ([commands.go](../cmd/genctl/commands.go#L670)). **Those two prefixes
are load-bearing**; both sites carry a comment saying so.

## `okReply` and the decoders

`okReply` used to discard the marshal error, yielding `Reply{OK: true, Data: nil}` — a
200 with an empty body, so the failure mode was a client believing it had received an
empty result. It now reports the failure.

`decodeOptionalBody` used to drop decode errors entirely, so a malformed optional body
was indistinguishable from an absent one and silently became the zero value. Optional
is about *presence*; it never meant "unparseable is fine". Both decoders now use
`numeric.DecodeStrict`, which is `Decode` plus `DisallowUnknownFields`.

`DecodeStrict` is a separate function rather than a flag, and the reason is in its
doc comment: `Decode` also reads rows already written to the database and payloads
already accepted from the network, where an unrecognised field is history and rejecting
it would make stored data undecodable. Strictness belongs only at the *entry* boundary,
where the sender is still there to be told.

Worth knowing about the layering, because it bounds what this catches:

- Syntactically invalid JSON never reaches the decoders. `actionDef.envelope` decodes
  the HTTP body into a `json.RawMessage` first, and TCP/UDS decode the whole envelope,
  so `{oops` is already rejected as `invalid`. What the decoders catch is well-formed
  JSON of the wrong *shape* — plus, now, a misspelled field.
- A JSON `null` unmarshals into a struct as a no-op with no error, so a client sending
  `null` keeps working.
- Over HTTP, only `POST /tick` passes a client body to `decodeOptionalBody`; the other
  five callers have a `fromHTTP` that rebuilds the payload from query parameters and
  discards whatever was sent. TCP/UDS clients reach all six. That is why the
  optional-body coverage lives in
  [tests/tick/optional_body_test.ts](../tests/tick/optional_body_test.ts), against a
  manual-tick server — on a polling server `/tick` answers 501 before it looks at the
  body at all.

The concrete bug this closed: `POST /tick` with `{"advance_ms": "12000"}` left the
clock unmoved and answered `200 {"count": N}`. Since `/tick`'s whole purpose is
shifting the server clock so timers expire without real waits, a test written that way
silently never advanced time, then asserted against timers that never fired.

---

# Part 2 — Go-level plumbing

## Wrapping that is now walked

The audit counted 395 `fmt.Errorf` calls, 157 of them wrapping with `%w`, against
**5** `errors.Is`/`errors.As` sites and **0** `Unwrap` methods: the chain was built and
never walked, paying the cost of Go 1.13 wrapping and getting the benefit of neither.

This was **not** fixed by converting all 395. Most are fine as messages. Values were
introduced exactly where a caller branches: the db sentinels above, `*api.Error`
(which has `Unwrap`, and whose `apiErrf` keeps the `fmt.Errorf` result as the cause so
a `%w` in the format stays reachable), and `*model.ValidationError`. `codeOf` and
`fieldsOf` are the walkers.

## `sql.ErrNoRows`

Ten sites compared with `==`, one used `errors.Is`. This was never a live bug —
`sql.Row.Scan` returns the sentinel unwrapped, from `database/sql` rather than the
driver — but it breaks the moment a wrapper appears between the query and the caller,
which is exactly what the db sentinels introduced. All sites now use `errors.Is`.

The more interesting half was deciding which of them meant `ErrNotFound`. **Only some
empty scans mean "you asked for something that isn't there."** The rest are ordinary
control flow and are commented as such where they sit:

- an absent parent in `FinishChild` / `FailInstanceAndAncestors` — a root child, or a
  parent already gone;
- an empty signal queue in `ArmExternalOrConsumeSignal` — that is what sends the call
  down the park branch;
- `FindVersionByHash` finding no identical version — that is what sends `applyBatch`
  down the save-a-new-version path.

Promoting any of those to `ErrNotFound` would have turned normal operation into 404s.

## `net.ErrClosed`

The accept loop matched the text of a stdlib error string to recognise its own shutdown
path. It uses `errors.Is(err, net.ErrClosed)` now; a mismatch there would have turned a
clean shutdown into a logged error plus a hot `continue` loop.

## `errcode.Code`

Codes were bare strings, so an arbitrary string could flow into `failInstance` where a
code was expected, and `IsNotReached` was a free function doing a prefix test rather
than a method. `type Code string` cost nothing — every existing literal stayed valid,
since an untyped string constant converts implicitly — and `code.IsNotReached()` is now
a method.

Strings remain correct at the boundaries: the value is persisted to `error_code` and
matched against `on_error` patterns written in YAML. The conversions that had to be
written out are precisely the three places a non-code string becomes a code, and each
is commented: an authored `panic` clause in `panicInstance`, an authored `raise` code in
the audit entry, and a child's persisted `error_code` in `resolveRaisedBatch`.

## The panic barrier

`dispatch` spawned the advance with no `recover()`, so a nil-map write or an
index-out-of-range anywhere under `advance` — expression evaluation, JSON handling,
collect — took down the whole worker process with instances leased.

That is now [`advanceGuarded`](../internal/engine/advance.go), which converts a panic
into an ordinary terminal failure carrying `errcode.EnginePanic`. The reasoning is
recorded on the function itself; the short version is that fail-fast is right when the
blast radius is unknown, and here it is known and narrow. An `OverwhelmError` is a
statement about the whole worker (lease renewal cannot keep up, so everything it holds
is suspect); a panic under advance is almost always attributable to the one definition
being advanced, and killing the process drops dozens of healthy in-flight advances to
punish one bad definition — without even isolating it, since the panicking instance is
re-claimed and panics again.

Three details that are easy to get wrong, and that the tests in
[panic_barrier_test.go](../internal/engine/panic_barrier_test.go) pin down:

1. **The barrier covers `advance()` only, not `persist()`.** A panic in the write path
   is not definition-attributable, and there would be nothing left to write the failure
   with. That one still takes the process down, and `dispatch` carries a comment saying
   so rather than looking like an omission.
2. **Recording the panic can itself panic.** This is not hypothetical: `audit` resolves
   the instance's definition in order to redact secrets from the entry it writes, so a
   definition malformed enough to panic advance is a good bet to panic the recording
   too. The recovery path therefore pre-sets the outcome, logs to the console first via
   `logOnly` (which touches neither the database nor the definition and so cannot
   fail), and runs the durable recording under a second barrier. `failInstance` assigns
   the terminal fields *before* it audits, so an instance that dies in the audit is
   still correctly marked failed.
3. **One residual, accepted:** a panic landing after a spawn transaction committed fails
   the parent while its children keep running. That is not new — it is true of every
   `failInstance` reached after a spawn — and the children settle into a tree whose
   parent is already failed, which the failing/collect logic tolerates.

## Log-and-continue (open, low priority)

The engine's background loops (`leaseRenewer`, log pruning, object expiry) log and
continue, which is right for a poller — the next tick retries. There is still no
escalation: a lease renewer that has failed every attempt for ten minutes is
indistinguishable from one that failed once, and the worker keeps claiming work it can
no longer hold. Noted so it is not mistaken for an oversight.

---

# Resolved decisions

- **D1 — the error code is part of the API contract.** `Code` publishes an `Enum()`,
  so the generated spec carries `ApiCode` with the full enumeration and every operation
  documents its error statuses via `errorBody`. Each action declares its extra codes in
  the registry (`Errors []Code`); `invalid` and `internal` are implicit on all of them,
  since any body can be rejected and any action can hit an unclassified failure. The
  alternative — ship the code undocumented — is the worst of both: clients key on it
  anyway, and nobody owes them stability.

- **D2 — recover in advance, fail the instance.** See the panic barrier above.

- **D3 — malformed optional bodies are rejected, and unknown fields with them.**
  Rejecting the shape mismatch alone would have left the typo case (`{"advnace_ms":
  12000}` silently becoming a default) untouched, which is the more likely mistake of
  the two.

# Compatibility

- `genctl` treats any status `>= 400` uniformly ([http.go](../cmd/genctl/http.go#L26))
  and needed no change. It keeps classifying two cases by message prefix; see the note
  above.
- The suite asserted on a 400 in exactly two places, both in
  [map_expression_test.ts](../tests/integration/map_expression_test.ts) — both are
  definition-validation failures, which are still 400.
- The visible break is `/tick` on a polling server: 400 → 501.
- Coverage for the new surface lives in
  [api_errors_test.ts](../tests/integration/api_errors_test.ts) (statuses and codes end
  to end, plus the `fields` detail) and the two files named above.
