# Error handling: the Go plumbing under the error model

Status: **Built.** Two systems share the word "error": the workflow error model (codes an
instance reports and a definition routes) and the Go plumbing under it. This records the
plumbing's design; they stay separate.

## Already right — do not "fix" these

- **`errcode`** is the single source of engine codes, with no genroc dependency, and its
  namespaces are guarantees, not naming: `pre.` *means* the request never left, which is what
  makes an `only_once` retry safe.
- **`advanceOutcome`** is a sum type: failures are values in the normal flow.
- **`failInstance(inst, code, reason)`** takes the code positionally, so no failure path can
  leave `error_code` empty.
- **`ClassifyGoError`** separates dial from response timeouts with `errors.As`.
- **The expression parser re-panics** on any recovered value that is not its `parseError`, so it
  never swallows an unrelated bug.

# Part 1 — the REST API

**The classification lives on `Reply`, not the HTTP response**: the body carries `code`, and
the HTTP status renders it through one table. The set is what a **client** can act on — `invalid` 400, `not_found` 404, `conflict` 409
(may succeed later), `unsupported` 501, `internal` 500, `unauthenticated` 401, `forbidden` 403,
`unavailable` 503 — and engine detail belongs in `errcode` on the instance. **Unclassified is
500, not 400**: an unclassified error is a server fault until shown otherwise.

**The code is API contract** (`Code.Enum()`; every action documents its error statuses and
declares extra codes, `invalid`/`internal` implicit). Clients key on it anyway, so shipping it
undocumented would owe them nothing while they depend on it.

**Classification is inherited, not repeated**: `codeOf` walks explicit `*api.Error` → db
sentinel → validation failure → `internal`, so a forwarding handler gets the right status
deciding nothing. The sentinels' load-bearing split is `ErrConflict` ("may work later") vs
`ErrInvalid` ("never will"). Two deliberate overrides: a submitted parent naming a child not on
the channel is `invalid` despite the underlying not-found (the fault is in the document), and an
apply's validation failures are `invalid` except `ResolveConfig`, which reports the *server's*
environment and stays unclassified.

**Per-field errors**: `*model.ValidationError` carries `[]FieldError`, surfaced as `fields` —
the path is what earns it (three tasks missing `id` give three identical messages but distinct
`tasks[N].id`). `fieldsOf` unwraps rather than type-asserts, surviving `applyBatch`'s
per-process wrapping. `genctl` keys on the `input validation: ` / `result validation: `
prefixes, **both load-bearing**.

**Decoders.** `okReply` reports a marshal failure rather than a 200 with an empty body. A
present optional body decodes strictly — optional means *presence*, never "unparseable is fine";
otherwise a typo'd field answers 200 having done nothing. `DecodeStrict` is a separate function,
not a flag: `Decode` also reads stored rows, where an unknown field is history.

# Part 2 — Go-level plumbing

- **Wrapping is walked**: values exist where a caller branches (db sentinels, `*api.Error` with
  `Unwrap`, `*model.ValidationError`), and nowhere else.
- **`sql.ErrNoRows` is compared with `errors.Is`, and only some empty scans mean
  `ErrNotFound`**: an absent parent in `FinishChild`, an empty signal queue, and no identical
  version in `applyBatch` are control flow — promoting them turns normal operation into 404s.
- **`net.ErrClosed` is matched with `errors.Is`**, not text: a mismatch turns clean shutdown into
  a logged-error hot loop.
- **`errcode.Code` is a type**, so a plain string (an authored code, a child's persisted code, a
  worker's submitted code) becomes one only by explicit conversion.

## The panic barrier

`advanceGuarded` converts a panic under `advance` into a terminal `engine.panic` failure: the
blast radius is one definition, where killing the worker dropped every healthy in-flight advance
and the culprit, re-claimed, panicked again (`panic_barrier_test.go`). Three details:

- **It covers `advance()` only, never `persist()`**: a write-path panic is not
  definition-attributable, and nothing would be left to record a failure with.
- **Recording the panic can panic** (audit reads the same malformed definition), so the outcome
  is pre-set, the console is written first by the one path that cannot fail, and the durable
  recording runs under a second barrier.
- **`failInstance` assigns the terminal fields before it audits**, so `failed` persists even if
  the audit panics.

## Open

- **Background loops never escalate** — they log and continue, so a renewer failing for ten
  minutes reads like one that failed once. Trigger: a persistent loop failure that went unnoticed.
