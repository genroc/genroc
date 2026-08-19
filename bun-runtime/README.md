# bun-runtime — the script-task evaluator

An HTTP sidecar that evaluates a TypeScript/JavaScript function body and answers with its
return value. A **script task is a plain `fetch`** at this server: no new action type, no
engine change, no worker fleet. `timeout`, `on_error`, `retry` and `only_once` apply exactly
as they do to any other fetch.

Design record: [specs/script-tasks.md](../specs/script-tasks.md). It proposes the `external`
variant (engine parks, a worker fleet pulls from the queue); this is the `fetch` variant,
which spends no engine capability at all. Moving between them is a definition-level change.

    PORT=3010 bun run bun-runtime/server.ts

## Request

`POST /eval`

```jsonc
{
  "code": "return { fee: input.amount * 0.1 };",  // required — an async function body
  "input": { "amount": 250 },                     // optional — bound as `input`
  "now": 1755600000000,                           // optional — pins Date.now()
  "seed": "abc",                                  // optional — seeds Math.random()
  "timeout_ms": 5000                              // optional — default 5000
}
```

`code` is the **body of an async function**, so `await` works and the value reaches genroc
through `return`. It is compiled with `input`, `ctx`, `Date` and `Math` as parameters, under
`"use strict"` — which is what stops an undeclared assignment from creating a global that
outlives the request. `ctx` is `{ now, seed }`.

`GET /health` answers `{"ok": true}`.

## Response — the status is the retryability class

genroc's `on_error` matches error codes and nothing finer, and the only codes a wire can
produce are `http.NNN`. So the status carries **exactly one bit of meaning: may a retry
help?** Everything diagnostic goes in the body, where a `switch` can read it.

| status | meaning | body | retry |
|---|---|---|---|
| `200` | the script returned | **the return value itself** | — |
| `400` | the request envelope was malformed | `{kind: "bad_request", …}` | no |
| `422` | the script faulted | `{kind, name, message, stack?}` | **no** |
| `500` | the evaluator faulted | `{kind: "internal", …}` | **yes** |

`422` covers compile errors, throws, timeouts and unserialisable returns alike, told apart
by `kind` (`compile_error` | `threw` | `timeout` | `nonserializable`). They share a status
because they share the only property the status encodes — a retry re-runs the same code on
the same input and fails identically. Splitting them across statuses would invite an
`on_error` rule that retries one of them, which is the failure the split exists to prevent.

A `200` body is the return value **bare**, not wrapped: `responses: {200: T}` then types
`self.result` as exactly `T`, and a script task reads like a typed function call. `return;`
sends an empty body, which genroc reads as `null`.

`stack` is renumbered to the lines the author wrote and trimmed to 2 KiB.

## The genroc side

```yaml
- id: price
  action:
    type: fetch
    url: "${config.script_runner}/eval"
    body:
      code: |
        if (input.amount > 100) {
          const e = new Error('amount over the limit');
          e.name = 'LimitExceeded';
          throw e;
        }
        return { fee: input.amount * 0.1 };
      input: "$: input"
    responses:
      200: { type: object, properties: { fee: { type: number } }, required: [fee] }
      "422": { $ref: "#/$defs/script_error" }
  timeout: 10000
  on_error:
    - code: [http.422]
      goto: $script_failed
  switch: [{ goto: end }]

- id: script_failed
  switch:
    - case: 'error.data.name == "LimitExceeded"'
      raise: { code: limit_exceeded, message: "the script rejected the amount" }
    - case: 'error.data.kind == "compile_error"'
      panic: { code: script_broken, message: "the script did not compile" }
    - raise: { code: script_failed, message: "the script failed" }
```

**A script cannot name a genroc error code.** `raise`/`panic` codes are literals, never
expressions, so the mapping from a thrown error to an authored code is this `switch` — one
task the definition owns, reading `error.data`. That is the whole error protocol: the
runner classifies into a status, the definition names the outcome.

Set the task `timeout` **above** the runner's `timeout_ms`. If the engine's deadline fires
first the code is `http.timeout`, which is in `errcode.Unknowable()` — permanently
unretryable on an `only_once` task, and indistinguishable from the runner being unreachable.

## `${` must be escaped as `$${`

A fetch `body` is a Shape, so `${…}` is genroc's interpolation marker and a JS template
literal inside `code` is read by genroc rather than passed through. Write `` `<$${x}>` ``.
A leading `$:` on the code string needs `$$:` for the same reason. See
[specs/typed-values.md](../specs/typed-values.md); the spec's import directive — code in a
`.ts` file that `genctl` resolves to a string — is what eventually removes this.

## Determinism

Retries re-execute, so the clock is **pinned rather than deleted**: pass `now` and
`Date.now()`, `new Date()` and `ctx.now` all read it, while `new Date(x)`, `Date.parse`,
`Date.UTC` and `instanceof` keep working. Deleting `Date` would leave a generated `Input`
type asserting what the runtime contradicts.

`Math.random()` is a seeded PRNG. With no `seed` the runner derives one from the
`X-Genroc-Instance-Id` and `X-Genroc-Task-Id` headers genroc stamps on every fetch — stable
across attempts of the same task, different between tasks, and free to the author.

**Omitting `now` falls back to the wall clock**, so a script that reads the time is
reproducible only when the definition passes one.

## What this is not

- **Not a sandbox.** The script runs in the server's own realm and reaches `fetch`, `Bun`,
  `process` and `require`. Fine for the same-trust-domain case (your genroc, your machine);
  it is not the multi-tenant story, and nothing here should be mistaken for one.
- **The timeout does not cover a synchronous busy loop.** `while(true){}` never yields, so
  the timer never fires and the whole runner hangs. Only an unsettled `await` is bounded.
  Containing the synchronous case needs a `Worker` or a subprocess per evaluation —
  `eval.ts` keeps HTTP out precisely so that strategy can be swapped underneath.
- **No imports, no bundling, no type checking.** One self-contained function body. The
  generator/bundler/tsconfig the spec describes are not built.
