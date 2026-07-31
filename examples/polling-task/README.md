# Polling task example

A parent process that spawns a **child process** whose whole job is to kick off a
long-running task on a remote server and then **poll its status until it's done**, or until
an attempt budget runs out. On success the child hands back the job's payload **without ever
looking inside it** (an `unknown`, which the parent narrows); running out of attempts is
**raised** as an error the parent catches.

```
polling-example (parent)
  └─ run: child     ──spawn──▶ poll-until-done (child)
       result_schema:            kickstart  POST {url}/jobs    ─▶ { job_id }
         narrows result          check      POST {url}/status  ─▶ { status, result: unknown }
       on_error:                   ├─ status == "done"     ─▶ end ─▶ output { result, attempts }
         poll_timeout ─▶ report    ├─ attempts exhausted   ─▶ raise poll_timeout
                                   └─ else                 ─▶ backoff
                                  backoff    delay ({{ poll_interval_ms }}) ─▶ back to check
```

This is the child → parent error-handling contract (see
[`docs/child-error-handling.md`](../../docs/child-error-handling.md)): success is an
**output**, but running out of attempts is an anticipated error the child **raises** by a
named code, and the parent's `on_error` on the child task **catches** it — routing to a
`report` task that reads the raised error from `$error`. A result the caller inspects would
stay in output; control-flow conditions the caller reacts to are raises. Note the raise sits
directly on a `switch` case: an arm either routes (`goto`) or terminates (`raise`/`panic`),
so no separate task is needed to fail.

## Files

- [`poller.genroc.yaml`](./poller.genroc.yaml) — the child. Three tasks: starts the remote
  job, then loops `check → backoff → check` until it's `done` or out of attempts. Returns
  the job's payload untouched, as an `unknown`.
- [`parent.genroc.yaml`](./parent.genroc.yaml) — spawns the poller as a child, threads the
  connection details and the (optional) knobs down, **narrows** the returned payload with a
  `result_schema` on the child task, and catches `poll_timeout` via `on_error` on the same
  task.

## The polling pattern

genroc has no `while`/`until` keyword. A poll loop is expressed structurally: the `check`
task's `switch` routes to a `backoff` delay, which routes back to `$check`, until the status
becomes `done`. Each request is a `fetch` action (an HTTP call like `fetch(url, {method,
headers, body})`, where every field is an expression) that persists and reclaims, so the loop
is crash-safe and holds no worker while it's parked.

## Returning a payload the poller never reads (`unknown`)

A poller shouldn't have to know what it's polling for. The job's answer is the *caller's*
concern — but a result the child exports normally has to be typed where it is produced, so
declaring `{ answer: number }` inside `poll-until-done` would pin this generic loop to one
job type.

`unknown` is the way out: **the empty schema `{}`** — JSON Schema's top type — for a value
a process **carries but never inspects**. There is no keyword; `{}` already means "any
value", and genroc treats it as opaque. The child's `check` task splits its response along
exactly that line:

```yaml
result_schema:
  type: object
  properties:
    status: { type: string }   # the poller reads this to drive its loop → typed
    result:                    # the poller only forwards this → unknown
      description: "opaque job payload — the caller narrows this"
  required: [status]
```

The `description` is optional and carries no meaning to the type system — `{}` alone is
identical. It is there because the bare `{}` cannot say whether it was deliberate or an
unfinished stub, and unlike a YAML comment a description survives into the stored
definition and the editor. Any *shape* keyword you add (`type`, `properties`, `enum`, …)
stops it being the top type, which is the whole rule.

The split is **per field, not per process**: data a process acts on must stay typed, and
only data it forwards untouched can be `unknown`. Most real processes are this mix — the
poller's `output` returns `result` (opaque) alongside `attempts` (an ordinary typed number
it does know).

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
same relationship a `fetch` already has with its `result_schema`: an opaque source, typed at
the boundary. `unknown` just lets a **child process** be that source too.

The trade-off is **when** the check happens. The poller no longer validates the payload as
it arrives, so a malformed one is caught at the parent's boundary instead — later, further
from the cause, and outside the child's own `on_error`/retry scope. (Two other consequences
follow: an `unknown` nobody narrows is never validated at all, which is harmless since it
can't be read; and two callers may narrow the same value to *different* schemas, each
checked independently.) None of that hurts a stable poller, but it is the thing to weigh
before reaching for `unknown` on data whose shape you don't trust.

Note that **omitting** `result_schema` is a different thing from declaring it `{}`: an
omitted schema leaves the result untyped and *unusable* — not readable, and not exportable
either — so "I meant this to be opaque" stays distinguishable from "I forgot to type it".

## Configuring the poll interval and timeout (parent → child)

Both are **optional input parameters with schema defaults** — declare `default:` on the
`input_schema` property and validation fills it in when the caller omits it, so
`poll-until-done` reads like a function with default arguments. (A defaulted optional is also
inferred as non-nullable, so it's usable directly in expressions — `input.max_attempts` needs
no `?? ` guard.) The parent declares the same defaults and threads the values down to the child:

- **`poll_interval_ms`** (default 500) — the back-off between polls. It lives on the `backoff`
  **`delay`** task, because a delay's `ms` is a templated expression. (A task's `timeout_ms` is
  a static int and can't be templated, so it could not carry a caller-supplied interval.)
- **`max_attempts`** (default 20) — the overall timeout, expressed as a **maximum number of
  status checks**. genroc expressions have no wall clock, so a poll budget is the honest primitive;
  the wall-time budget is roughly `max_attempts × poll_interval_ms`. `check` counts its own
  runs via `self.previous`, and once the budget is spent its `switch` **raises `poll_timeout`**.

```sh
genctl run polling-example \
  --input '{ "url": "http://localhost:9000",
             "headers": { "Authorization": "Bearer s3cr3t" },
             "poll_interval_ms": 2000, "max_attempts": 30 }'
```

## Configuring headers (parent → child)

The whole request is caller-driven, so the **caller supplies the entire headers map** and
the child splats it onto every `fetch` with `headers: "{{ input.headers }}"`. The headers
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

The service base URL and a headers map are passed as input, so point it at any server exposing
`POST /jobs` (returns `{ "job_id": ... }`), `POST /status` (returns
`{ "status": "pending" | "done", "result": ... }`):

```sh
genctl apply -f poller.genroc.yaml -f parent.genroc.yaml
genctl run polling-example --input '{
  "url": "http://localhost:9000",
  "headers": { "Authorization": "Bearer s3cr3t" }
}'
```

## Automated test

[`tests/integration/examples_polling_test.ts`](../../tests/integration/examples_polling_test.ts)
loads these YAML files verbatim, applies them against a throwaway mock job service, and
asserts all three outcomes — polling through to `done` (the payload narrowed, a surplus key
dropped), a payload that fails the parent's narrowing (child completes, parent fails on the
collect conform), and running out of `max_attempts` (`poll_timeout` raised and caught) — so
this example is also an executable test. Run it with `make test-int`.
