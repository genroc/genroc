# Planned improvements

## CLI
- [x] commands mirroring the API capabilities
- [x] yaml support
- [x] config file with the url to the server
- [] test with different auth types + add helpers for genctl

## server
- [x] versioning channels
- [x] version compatibility — compare two versions of a process (`genctl compat`,
  `POST /definitions/compat`) and report what changed, so a change that would strand running
  work is visible before it is deployed. Two verdicts, never folded: instance continuation
  and the output contract run in opposite directions. It is a **shape** check — it catches
  the accidental break (a required input appearing, an output whose type changed) and names
  which slots differ, but dollars → cents is `number` either way and comes back compatible.
  One check spans two documents: when only one of a waiting parent / running child moves,
  whichever moved must still fit the one that did not. See specs/version-compatibility.md
- [] instance upgrade — move a live instance from one version to another, gated on the
  comparison above (specs/version-compatibility.md §1–§4, §6). The design is settled: the
  gate refines the comparison with presence taken from the row, the write is one column
  (which buys reversibility and idempotency), and the unit is the non-terminal tree closure,
  because a running child may not move without the parent whose definition names its version
- [x] external tasks (outgoing request to start, incoming request to complete), human or long running
- [x] non-idempotent tasks - steps which can't be safely repeated (`only_once`; an interrupted one raises the catchable `only_once.interrupted` so a definition can verify against the system of record and then continue or deliberately re-run, and retries are refused for the errors nothing came back from — see specs/only-once-interrupted.md)
- [x] logs for each process
- [x] let user to repeat the task manually (how will it interact with parents?)
- [x] pagination
- [x] filtering
- [x] env variables
- [x] map function (lambdas + object/array literals; own parser)
- [x] think about error handling child -> parent (raise/panic, the `raised` status, error_code, child→parent catch with batch resolution — see specs/child-error-handling.md)
- [] think about action extensivity/passability from parent
- [x] resource limits and readiness - the five bounds a worker was missing, grouped because
  they share a premise: a worker holds leases on everything it claimed, so a fault costs
  every instance it was advancing, not one request. A fetch response cap (8 MiB, raising the
  catchable `output.too_large` — an unbounded read OOM'd the worker into a crash loop that
  also stalled up to `--max-concurrent` unrelated processes), a shared HTTP client (the
  stdlib pools 2 connections per host, so a worker re-handshook TLS on nearly every call to
  the same endpoint), jittered retry backoff (which also fixed an overflow: `time.Duration`
  is int64 *nanoseconds*, so the old `2^attempt` broke at attempt 34 and returned a flat
  `0s` — no backoff at all — from 62 up), HTTP listener timeouts + a request body cap, and
  `GET /healthz`. See specs/resource-limits.md. **Still open** in that doc: no per-message
  limit on the TCP/UDS envelope stream (needs framing, not a limit), no upper bound on
  `retries` at registration, and no metrics — `/healthz` answers "is this worker serving",
  not "how deep is the backlog"
- [x] unknown type - a way how child can pass data to parent, without looking at it (the empty schema `{}`, narrowed by the parent's `result_schema`; no new syntax — see specs/unknown-type.md. Still open: the `infer` mode from that doc)
- [x] fetch response metadata - `self.status` (integer) and `self.headers` (string map),
  siblings of `self.result` rather than a wrapper around it, so nothing that reads a body
  changed. Header keys are lowercased (Go canonicalises to `Retry-After`, which would make
  `self.headers['retry-after']` silently null) and repeated headers comma-joined, with
  `Set-Cookie` the accepted casualty. Gated to fetch at BOTH ends — one helper types it
  (`withFetchMeta`), one builds it (`engine.taskSelf`) — because a slot present in the schema
  and absent at runtime reads null where the type promised a value, and the reverse is
  unreadable. See specs/fetch-http-surface.md §3. **Still open:** the poller still routes its
  202 through `on_error`; switching it to `case: self.status == 202` means the caller stops
  choosing `accepted_status`, which is a change to that example's contract
- [] fetch query params - a structured `query` slot (see specs/fetch-http-surface.md part 2; interpolating into the url string does no escaping, so a value with `&`/`=`/space injects a parameter)
- [x] string-literal indexing in expressions - `x['some-key']` (desugars to a MemberNode; unblocks the headers map above. Access paths are now carried as steps and rendered as accessors everywhere, so a key that isn't an identifier stays distinct from a nested one — including in validation errors and `SecretAt` — see internal/schema/path.go)
- [x] computed keys - `m[k]`, `xs[i]` (allowed only where every key shares one type: an array, or a map declaring only `additionalProperties`. An object with named properties is rejected — a computed key there could land on a declared property whose type differs from `additionalProperties`. Narrows like a static path when the key is itself a path, and the guard dies when a lambda rebinds the key)
- [x] Go + REST API error handling (an error `code` on `Reply` that HTTP renders as a status, db sentinels handlers inherit, per-field `fields`, and a panic barrier around `advance` — see specs/error-handling-audit.md. Still open: escalation for the log-and-continue background loops)
- [x] look at naming conventions - cancel -> pause, then resume. Retry only for failed processes.
- [x] delay syntax - `for` (human durations: `2h30m`, `1d 12h`) and `until` (calendar
  deadlines: `+2d 08:00`, `*-*-01 08:00`, RFC 3339) with `tz`; the raw `ms` slot was
  **removed** rather than deprecated, which was only possible pre-release (see
  specs/delay-syntax.md). Grammars in internal/delayspec. Still open: a ceiling on the
  resolved delay, `tz` from an expression, a definition-level default timezone
- [x] survive a frozen worker - a suspended host or a throttled container used to make the engine re-claim its own in-flight work and exit as "overwhelmed"; it now checks how long ago a lease renewal last succeeded, repairs its own leases before claiming, and declines lease takeovers for one lease period (see specs/lease-fencing.md)
- [x] fence every write on a per-grant `lease_epoch` - the multi-worker half the gate cannot cover: a claim bumps the epoch, every lease-holding write carries it, and a stale advance's write is refused (a `lease_lost` audit entry) rather than clobbering the new owner's state. The fatal "overwhelmed" exit is retired with it - lease pressure repairs or refuses, it never kills the worker (see specs/lease-fencing.md)
- [x] path-sensitive process output - coalescing across branches that between them cover every
  way the process can end (`outputs.a.v ?? outputs.b.v`) now types non-null. The output
  expression is checked once per terminal and joined, instead of once against a context that
  has already intersected the terminals' must-sets and lost the correlation. Fell out of it:
  `??` canonicalizes its union (an un-merged `oneOf[{T},{T|null}]` overlaps, and oneOf means
  exactly one, so it rejected every value it described), reading a property through a null
  yields null (matching the evaluator), and `StripNull` regained its `!HasNull` contract. See
  specs/path-sensitive-output.md. **Still open:** mid-process task contexts are still collapsed
  — the same coalesce read from a task reachable from two branches stays nullable, because
  that fixpoint's lattice element is one must-set and path sensitivity there needs a DNF
  lattice with widening (§5 of that doc). Workaround is a trailing `?? default`
- [] guard narrowing - a `switch` case is currently the one expression whose meaning the type
  system discards: a task can route on `case: "self.output.v != null"` and the routed task
  still cannot use `v` without a `?? default` that can never be evaluated. Proposal is to
  carry a per-reference refinement along the edge the case selects (see
  specs/guard-narrowing.md). Tractable where the mid-process case in specs/path-sensitive-output.md
  is not, because it refines ONE reference rather than correlating two — the same line
  TypeScript draws. Two soundness traps recorded there: `config` is re-resolved every tick,
  and a re-entered task overwrites its own output, so loops need a dataflow kill
- [] enum-aware canonicalization - `mergeSimpleVariants` refuses to fold arms carrying an
  `enum`, so a hand-written `oneOf[{string, enum:[a]}, {type:string}]` stays an overlapping
  union that (oneOf being exactly-one) rejects "a". Latent today, mandatory before literal
  types. Stands alone with no churn; see specs/literal-types.md §4
- [] more precise typing: literal (singleton) types - a string literal infers as
  `{type: string}`, losing which string it was; a declared `enum` survives navigation but
  inference never produces one. Producing them is 4 lines; the work is the canonicalization
  above plus ~51 Go tests of shape churn and regenerated published schemas (measured with a
  spike). `IsSubset`/validation/arithmetic need no change. See specs/literal-types.md.
  **Unblocks discriminated unions below**, and catches provably-false comparisons like
  `kind == "sucess"` against a declared enum
- [] discriminated unions (deferred, blocked on literal types) - narrow a `oneOf` by a tag
  check (`case: 'self.output.r.kind == "success"'`) so the matched arm's fields are readable
  and the other arm's are not. Today reading through such a union yields a nullable type (the
  missing arm contributes null) and the tag check is ignored. The mechanism is specified in
  specs/discriminated-unions.md and builds on guard narrowing — but a definition cannot
  currently *build* a narrowable union: `kind: sent` types as plain `string`, so coalesced
  branches are discriminated by shape, not by tag. Only a hand-written result_schema would be
  narrowable, which is not worth the feature. Note `const` is not in the supported keyword
  subset: a tag is `enum: [value]`
- [] revisit how externalized values are served - `GET /instances/{id}` returns every slot the
  object store holds as a `{ref, size}` marker, and `?resolve=true` materialises **all** of
  them inline (`HydrateContext`) before redacting; `genctl get --resolve` and
  `genctl logs --resolve` set it, and `GET /instances/{id}/objects/{ref}` fetches one object.
  The flag is all-or-nothing, so a caller wanting one output pays for every object on the
  instance, while the per-object route leaves the client to parse markers out of the context
  and issue N follow-ups. Two things to weigh alongside it. A marker is **indistinguishable
  from data** without the schema — `{ref, size}` is an ordinary-looking object, and a test
  written during the `error.data` work asserted a field on one and silently read the marker's
  own `size` instead of failing. And hydrate-then-redact materialises secret values before
  scrubbing them, on the one path whose scrub is schema-driven (`RedactContext`) rather than
  value-based like the audit log's — the two differ on a secret that reached a string through
  `${ }` interpolation, which carries no `secret` marking of its own
- [] pause as a debugging tool: start an instance paused, then step it with tick

# docs

- [] write docs, plan and motivation


