# Polling task example

A **generic HTTP poller**, written as a child process. The caller hands it two whole
requests — a kickoff and a check — and it loops the check until the job is done, or until an
attempt budget runs out. It reads **nothing** from either response body: both are `unknown`,
progress is signalled by the HTTP status, and the final payload goes back to the parent to
narrow. Running out of attempts is **raised** as an error the parent catches.

```
polling-example (parent)
  └─ run: child     ──spawn──▶ poll-until-done (child)
       composes both requests    kickstart  caller's request  ─▶ response ignored
       result_schema:            check      caller's request
         narrows result            ├─ accepted status  ─▶ end ─▶ output { result, attempts }
       on_error:                   └─ 202 (on_error)   ─▶ backoff
         poll_timeout ─▶ report  backoff    delay ({{ poll_interval_ms }})
                                   ├─ attempts exhausted ─▶ raise poll_timeout
                                   └─ else               ─▶ back to check
```

This is the child → parent error-handling contract (see
[`specs/child-error-handling.md`](../../specs/child-error-handling.md)): success is an
**output**, but running out of attempts is an anticipated error the child **raises** by a
named code, and the parent's `on_error` on the child task **catches** it — routing to a
`report` task that reads the raised error from `error`. A result the caller inspects would
stay in output; control-flow conditions the caller reacts to are raises. Note the raise sits
directly on a `switch` case: an arm either routes (`goto`) or terminates (`raise`/`panic`),
so no separate task is needed to fail.

## Files

- [`poller.genroc.yaml`](./poller.genroc.yaml) — the child. Three tasks: fire the caller's
  kickoff request, then loop `check → backoff → check` until the check returns an accepted
  status or the budget runs out. Returns the check's payload untouched, as an `unknown`.
- [`parent.genroc.yaml`](./parent.genroc.yaml) — composes both requests, correlates them
  with its own `ref`, **narrows** the returned payload with a `result_schema` on the child
  task, and catches `poll_timeout` via `on_error` on the same task.

## The polling pattern

genroc has no `while`/`until` keyword. A poll loop is expressed structurally: `check` routes
to a `backoff` delay, which routes back to `$check`. Each request is a `fetch` action (an
HTTP call like `fetch(url, {method, headers, body})`, where every field is an expression)
that persists and reclaims, so the loop is crash-safe and holds no worker while it's parked.

## Both requests are the caller's (`kickoff` / `check`)

The poller owns no URLs, bodies or status conventions. Its input carries two request
descriptions plus one shared `headers` map:

```yaml
headers: { Authorization: "Bearer …" }    # applied to both requests
kickoff:
  url: "https://api.example.com/jobs"
  body: { command: compute-answer, ref: "abc-123" }
check:
  url: "https://api.example.com/status"
  body: { ref: "abc-123" }
  accepted_status: ["200"]                # which statuses mean "done"
```

`method` (default `POST`) and `accepted_status` are optional with schema defaults, and
`body` is `unknown` — the poller passes it straight through to the request without ever
looking at it. Because a fetch `body` is inferred against no required shape, an opaque value
is accepted there; a *typed* slot would have rejected it.

### Progress is an HTTP status, not a body field

`self.result` is only the response **body** — a fetch's status code is not visible to
expressions. What *is* visible is that a status outside `accepted_status` becomes a
**catchable error code** `http.<N>`. So the loop branches through `on_error`, not `switch`:

```yaml
accepted_status: "$: input.check.accepted_status"   # e.g. ["200"] → done
on_error:
  - code: [http.202]                                # → still running
    goto: $backoff
```

**202 Accepted** is the convention, and it is fixed rather than caller-supplied: `on_error`
codes are static patterns, not expressions. That is a fair contract — 202 is precisely
HTTP's "request accepted, processing not complete." The caller still controls the *done*
side through `accepted_status`; anything that is neither fails the poll.

Two consequences worth knowing:

- **The attempt counter lives on `backoff`.** A task's `output` is only computed when its
  action *succeeds*, and on the polling path `check` always errors — so `check.output`
  never runs while polling. `backoff` is a `delay`, which always succeeds, so it counts the
  polls and its `switch` enforces the budget off `self.output.attempt`.
- **Every poll logs as an error.** A healthy 20-poll run leaves 19 `action_failed` /
  `error_route` entries in the instance log. Accurate, but noisy.

### What this costs: no server-assigned id

Because the poller never reads the kickoff response, the check request **cannot depend on
it** — a job id the server invents is unreachable. The caller correlates the two requests
itself instead, here by generating a `ref` and putting it in both bodies. Polling a
server-assigned resource would need the kickoff response typed, which is exactly the
coupling this design trades away.

## Returning a payload the poller never reads (`unknown`)

A poller shouldn't have to know what it's polling for. The job's answer is the *caller's*
concern — but a result the child exports normally has to be typed where it is produced, so
declaring `{ answer: number }` inside `poll-until-done` would pin this generic loop to one
job type.

`unknown` is the way out: **the empty schema `{}`** — JSON Schema's top type — for a value
a process **carries but never inspects**. There is no keyword; `{}` already means "any
value", and genroc treats it as opaque. Because progress is signalled by the status code,
the check's whole response body can be exactly that:

```yaml
responses:
  200: { description: "opaque job result — the caller narrows this" }
```

The `description` is optional and carries no meaning to the type system — `{}` alone is
identical. It is there because the bare `{}` cannot say whether it was deliberate or an
unfinished stub, and unlike a YAML comment a description survives into the stored
definition and the editor. Any *shape* keyword you add (`type`, `properties`, `enum`, …)
stops it being the top type, which is the whole rule.

Opacity is **per value, not per process**: data a process acts on must stay typed, only
data it forwards untouched can be `unknown`, and most processes are a mix. This poller is —
its output pairs the opaque `result` with `attempts`, an ordinary typed number it does know
because it counted the polls itself. The parent's `result_schema` narrows the first and
simply restates the second.

An `unknown` is not an escape hatch — it is emphatically not `any`. It has exactly two
legal moves:

- **Forward it.** Export it, or nest it in a known structure. Anything is assignable *into*
  an unknown, so passing it up costs nothing.
- **Narrow it.** Reading it, or passing it to a typed input, is refused
  (`cannot access .answer: the value is unknown`) until someone declares its shape.

The parent is the one who knows, because it chose the job — so it narrows, on the
`result_schema` of the child task:

```yaml
result_schema:
  type: object
  properties:
    result:                          # was unknown; this declaration is the narrowing
      type: object
      properties:
        answer: { type: number }
      required: [answer]
    attempts: { type: integer }      # already typed by the child; nothing to narrow
  required: [result, attempts]
```

Now `self.result.result.answer` is readable in the parent — and **checked**, not assumed:
when the engine collects the child it conforms the whole output against this schema, so a
payload of the wrong shape fails the task rather than flowing on. Undeclared keys are
dropped by the same conform, so a job may return more than the parent declares. This is the
same relationship a `fetch` already has with its `responses`: an opaque source, typed at
the boundary. `unknown` just lets a **child process** be that source too.

The trade-off is **when** the check happens. The poller no longer validates the payload as
it arrives, so a malformed one is caught at the parent's boundary instead — later, further
from the cause, and outside the child's own `on_error`/retry scope. (Two other consequences
follow: an `unknown` nobody narrows is never validated at all, which is harmless since it
can't be read; and two callers may narrow the same value to *different* schemas, each
checked independently.) None of that hurts a stable poller, but it is the thing to weigh
before reaching for `unknown` on data whose shape you don't trust.

Note that **declaring no schema at all** is a different thing from declaring `{}`: an
undeclared result is untyped and *unusable* — not readable, and not exportable either — so
"I meant this to be opaque" stays distinguishable from "I forgot to type it".

## Configuring the poll interval and timeout (parent → child)

Both are **optional input parameters with schema defaults** — declare `default:` on the
`input_schema` property and validation fills it in when the caller omits it, so
`poll-until-done` reads like a function with default arguments. (A defaulted optional is also
inferred as non-nullable, so it's usable directly in expressions — `input.max_attempts` needs
no `?? ` guard.) The parent declares the same defaults and threads the values down to the child:

- **`poll_interval_ms`** (default 500) — the back-off between polls. It lives on the `backoff`
  **`delay`** task, because the back-off is a wait between polls, not a bound on any one of
  them — a task's `timeout` would cut a single status check short instead.
- **`max_attempts`** (default 20) — the overall timeout, expressed as a **maximum number of
  status checks**. genroc expressions have no wall clock, so a poll budget is the honest primitive;
  the wall-time budget is roughly `max_attempts × poll_interval_ms`. `backoff` counts its own
  runs via `self.previous`, and once the budget is spent its `switch` **raises `poll_timeout`**.

```sh
genctl run polling-example \
  --input '{ "url": "http://localhost:9000", "ref": "job-1",
             "headers": { "Authorization": "Bearer s3cr3t" },
             "poll_interval_ms": 2000, "max_attempts": 30 }'
```

## Configuring headers (parent → child)

The whole request is caller-driven, so the **caller supplies the entire headers map** and
the child splats the one map onto both `fetch`es with `headers: "$: input.headers"`. The headers
input is typed as an **open string map** — `{ type: object, additionalProperties: { type:
string } }` — so arbitrary keys (auth, trace ids, …) flow from the parent down without the
child declaring each one. This is `additionalProperties` in action: without it, undeclared
header keys would be stripped by normalization; with it they survive as typed string values.
A `fetch` action's `headers` is a shape, so it accepts either this whole-map expression or a
literal map of templated values.

genroc also **auto-stamps** `X-Genroc-Instance-Id` and `X-Genroc-Task-Id` on every request
(set authoritatively, so a caller header can't spoof them), so the receiving service can
correlate a call back to the instance/task that made it — the run context the raw body no
longer carries.

## Running it

The parent builds both requests from a base URL, a correlation `ref` and a headers map, so
point it at any server exposing `POST /jobs` and `POST /status`, where `/status` answers
**202** while the job runs and **200** with the result when it's done:

```sh
genctl apply -f poller.genroc.yaml -f parent.genroc.yaml
genctl run polling-example --input '{
  "url": "http://localhost:9000",
  "ref": "job-1",
  "headers": { "Authorization": "Bearer s3cr3t" }
}'
```

The poller itself has no opinion about those paths — swap the `kickoff` / `check` blocks in
[`parent.genroc.yaml`](./parent.genroc.yaml) and it polls any other API unchanged.

## Automated test

[`tests/integration/examples_polling_test.ts`](../../tests/integration/examples_polling_test.ts)
loads these YAML files verbatim, applies them against a throwaway mock job service, and
asserts all three outcomes — polling through to a 200 (the payload narrowed, a surplus key
dropped), a payload that fails the parent's narrowing (child completes, parent fails on the
collect conform), and running out of `max_attempts` (`poll_timeout` raised and caught) — so
this example is also an executable test. Run it with `make test-int`.
