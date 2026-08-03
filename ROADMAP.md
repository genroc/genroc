# Planned improvements

## CLI
- [x] commands mirroring the API capabilities
- [x] yaml support
- [x] config file with the url to the server
- [] test with different auth types + add helpers for genctl

## server
- [x] versioning channels
- [x] child process compatibility check and versioning made convenient
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
- [] fetch response metadata - expose `self.status` and `self.headers` (see specs/fetch-http-surface.md part 1; retires the `http.202`-via-`on_error` trick in examples/polling-task/ and unblocks Location / Retry-After / Link)
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
- [x] survive a frozen worker - a suspended host or a throttled container used to make the engine re-claim its own in-flight work and exit as "overwhelmed"; it now checks how long ago a lease renewal last succeeded, repairs its own leases before claiming, and declines lease takeovers for one lease period (see specs/lease-fencing.md). Still open in that doc: fencing every write on a per-grant `lease_epoch` so a stale advance's write is refused rather than clobbering — the multi-worker half, which the gate cannot cover
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
- [] pause as a debugging tool: start an instance paused, then step it with tick

# docs

- [] write docs, plan and motivation


