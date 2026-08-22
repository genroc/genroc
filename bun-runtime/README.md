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
  "timeout_ms": 5000                              // optional — default 5000
}
```

`code` is the **body of an async function**, so `await` works and the value reaches genroc
through `return`. It is compiled with `input` and `require` as parameters, under
`"use strict"` — `require` is what a bundled `import` of a node builtin lands on.

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

`422` covers compile errors, throws, timeouts, unserialisable returns and a script that ends
its own realm, told apart by `kind` (`compile_error` | `threw` | `timeout` |
`nonserializable` | `exited`). They share a status
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

## `${` must be escaped as `$${` — when the code is inline

A fetch `body` is a Shape, so `${…}` is genroc's interpolation marker and a JS template
literal inside `code` is read by genroc rather than passed through. Write `` `<$${x}>` ``.
A leading `$:` on the code string needs `$$:` for the same reason. See
[specs/typed-values.md](../specs/typed-values.md). Moving the code into a `.ts` file
removes this entirely — see the next section.

## `import.ts` — the author-time half

`import.ts` is the **code-phase resolver** genctl runs before a definition is applied. It
never serves HTTP and the server never runs it; the two halves share this package only
because they share a calling convention, which is exactly the coupling that breaks silently
if they version apart.

Register it in the project's `genroc.yaml`:

```yaml
resolvers:
  import: { phase: code, ext: .ts, command: [bun, run, ../bun-runtime/import.ts] }
```

then write the script as a module and name it from the definition:

```yaml
body:
  code: "$import: ./fee.ts"
  input: "$: input"
```

```ts
import type { Input, Output } from "./fee.genroc";

export default async function (input: Input): Promise<Output> {
  return { fee: input.amount * 0.1 };
}
```

`genctl types -f process.yaml` writes `fee.genroc.d.ts` beside the script — named for the
**script's path**, not the task, so renaming a task cannot break the import line. `Input` is
the inferred type of what the definition passes; `Output` is what it declares
(`responses.200`, or `result_schema` on a child). `genctl apply` regenerates them, runs
`tsc --noEmit`, and bundles — so **a type error is a failed apply**, and a stored definition
cannot hold code that failed to typecheck.

The bundle is emitted as CJS and wrapped as a function body, so the evaluator needs to know
nothing about modules. Imports are resolved at build time and inlined, so the string a
definition version stores is self-contained forever — with one exception: **node builtins
stay as `require` calls**, which the realm satisfies. A package is frozen into the
definition; `node:fs` is resolved by whatever runner executes it.

### Your tsconfig, your types

The generated project config `extends` **the nearest `tsconfig.json` above the script** —
the one your editor already reads, so the two cannot disagree. Two scripts under two
different configs are two `tsc` runs.

Of that config, three keys are the toolchain's and the rest are yours:

| key | owner | why |
|---|---|---|
| `lib` | the toolchain | Describes the realm. A Bun worker has no `document`, whatever a config claims. |
| `include` | the toolchain | Forced to `[]`. A base `include` survives beside our `files` and would drag your whole tree in to be checked as scripts. |
| `types` | **you** | How a script opts into node and Bun globals. The realm has them, so refusing the declarations would only lie. With no tsconfig at all the default is `[]`. |

```jsonc
// tsconfig.json beside your scripts
{ "compilerOptions": { "types": ["bun"] } }   // now `import { appendFile } from "node:fs/promises"` typechecks
```

**This is what removes the `$${` escaping above** — a template literal in a `.ts` file is
never read by genroc, because genctl doubles every `$` on splice.

## The realm — one Worker per execution

`evaluate()` starts a Worker, posts the code into it, and races the reply against the budget;
`terminate()` runs on every path. That thread is what the contract rests on, and it buys
exactly three things the previous in-process evaluator could not:

- **The budget is enforced, not merely reported.** A synchronous `while(true){}` never
  yields, so no in-process timer can interrupt it — the old evaluator hung forever on that
  input, and said so in this file. Killing the thread is the only bound. Measured on Bun
  1.3.14: `terminate()` stops a spinning worker, and the CPU it was burning goes with it.
- **A fresh global object per execution.** One script cannot configure the next. It is also
  why there is no compile cache any more: a cache inside a discarded realm can never be hit.
- **The script's mistakes stay the script's.** An uncaught throw, and `process.exit()`, end
  the realm and come back as a `422` — neither reaches the runner.

It costs about **1.7ms per execution** for the realm, plus recompiling the body (~40ms for a
201 KiB bundle, which is a large one). A subprocess per execution was the alternative at
~16.6ms, and it is the upgrade path for the two things a thread does not contain.

## What this is not

- **Not deterministic.** A script reads the real clock and the real RNG, and a retry
  re-executes — so attempt two can differ from attempt one. An earlier version injected a
  pinned `Date` and a seeded `Math`; nothing could supply a stable `now` (the expression
  environment has no clock), so the pin was the wall clock under another name, and the `ctx`
  it needed was surface with nothing behind it. A value that must survive a retry belongs in
  the definition, passed through `input`.
- **Not a sandbox.** The realm isolates *execution*, not *authority*: a script gets the
  runner's filesystem, network and environment, and `require` of any node builtin. That is
  deliberate — a script task is meant to do real work — but the trust boundary stays the
  same-trust-domain one (your genroc, your machine). It is not the multi-tenant story, and
  nothing here should be mistaken for one.
- **A thread does not contain memory or a native crash.** A worker shares the process
  address space, so a script that exhausts memory takes the runner with it. Containing that
  is what the subprocess strategy is for — `eval.ts` keeps HTTP out precisely so it can be
  swapped underneath.
- **Nothing caps concurrency.** Each request is a thread, so N concurrent script tasks are N
  threads; genroc's own concurrency limits are what bound this today.
- **Not where imports and type checking happen.** The evaluator still takes one
  self-contained function body and knows nothing about TypeScript; `import.ts` is what turns
  a module into that body, at author time. See above.
