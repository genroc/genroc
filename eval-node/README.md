# @genroc/eval-node

Script tasks for Node. Two halves with different jobs:

* **the bundler** (`genroc-import`) — an author-time resolver genctl runs on every apply and
  every edit, turning `$import: ./x.ts` into a bundled, typechecked string
* **the worker** (`genroc-eval-node`) — claims parked `external` script tasks off genroc's queue
  and evaluates each in its own realm

## Setting up a project

    npm i -D @genroc/eval-node

Then register the resolver in a `.genroc` beside your definitions — a dotfile, so it does not
read as another process definition. Discovery walks up from the source file, so nothing depends
on the cwd:

    resolvers:
      import:
        phase: code
        ext: .ts
        command: [npx, genroc-import]

`genctl apply` and `genctl types` now resolve `$import` directives. Typechecking is the
resolver's exit code, so a stored definition cannot hold code that failed to typecheck.

## Building

    npm run build       # tsc -p tsconfig.build.json -> dist/

The published package is JavaScript, not TypeScript. **Node refuses to strip types for files
under `node_modules`**, so shipping `.ts` works from a checkout and fails for every consumer —
which is why `prepublishOnly` builds and CI installs the packed tarball rather than only running
from source. The source keeps its `.ts` import extensions; `rewriteRelativeImportExtensions`
turns them into `.js` on the way out.

## Running the worker

    docker run -e GENROC_SERVER=http://host:8448 ghcr.io/genroc/eval-node:preview

or locally, if you already have Node:

    GENROC_SERVER=http://localhost:8448 npx genroc-eval-node

Add `GENROC_TOKEN` once the server has auth on; `worker` is the only permission it needs.

**The bundler cannot be the image.** It runs per edit and must resolve your project's own
`node_modules`, so a container start per invocation would break the editor loop.

Design record: [specs/external-task-queue.md](../specs/external-task-queue.md) for the queue,
[specs/script-tasks.md](../specs/script-tasks.md) for the runtime.

    GENROC_SERVER=http://localhost:8448 node eval-node/worker.ts

Against a server started with `--auth token`, mint a scoped credential first:

    TOKEN=$(genctl token create --perms worker --label evaluator -q)
    GENROC_SERVER=http://localhost:8448 GENROC_TOKEN=$TOKEN node eval-node/worker.ts

`worker` reaches the four queue verbs and `GET /api/objects/{ref}` — enough to claim, fetch an
externalized input and answer, and nothing else. This is the credential most likely to end up on
a machine you trust least, so it is worth scoping rather than reusing an admin token: a leaked
one cannot read a definition, list an instance, or mint another token.

A credential problem **exits the worker** rather than being retried. Polling through a 401 looks
like a healthy worker that never picks anything up, which is the worst shape to debug.

It needs **Node 24 or newer**: the sources are TypeScript and are run as-is, by Node's own
type stripping.

| env | default | |
|---|---|---|
| `GENROC_SERVER` | `http://localhost:8448` | where to claim from |
| `GENROC_TOKEN` | *(none)* | credential, when the server runs with `--auth token`. Needs the **`worker`** permission and nothing more |
| `WORKER_ID` | `evaluator-<pid>` | the claim holder; renewals are scoped to it |
| `CONCURRENCY` | `4` | how many scripts run at once — **this worker's** decision |
| `LEASE_MS` | `30000` | visibility timeout; renewed at a third of it while working |
| `POLL_MS` | `250` | idle poll interval; a non-empty claim polls again immediately |
| `PROCESS` / `TASK` | unset | claim only this process / task id |

**The worker calls genroc, not the other way round.** That is the whole reason this is a
queue worker rather than the HTTP sidecar it used to be:

- **Concurrency is the worker's to set.** Under the old `fetch` shape genroc decided how many
  scripts ran at once (`--max-concurrent`, default 200) and the evaluator accepted every one.
  A backlog is now a queue, not 200 threads fighting over a core.
- **No engine worker is held.** A `fetch` occupies one of genroc's advance slots for its whole
  duration, so a slow evaluator starved unrelated tasks. An `external` task parks.
- **The connection direction inverts.** `/eval` was unauthenticated arbitrary code execution
  that genroc had to be able to reach. A worker only needs outbound access, so it can live
  anywhere — behind NAT, in another trust zone.

## The task input

The `input` of the external task IS the evaluation request:

```jsonc
{
  "code": "export default (input) => ({ fee: input.amount * 0.1 });",  // required — a module
  "input": { "amount": 250 },                                         // optional — its argument
  "timeout_ms": 5000                                                  // optional — default 5000
}
```

`code` is an **ES module**, and the evaluator imports it and calls its **default export** with
`input`. The value it returns reaches genroc; the function may be async, and so may the module's
top level. A module exporting anything else — or nothing — is a `compile_error`: there is
nothing to call, and so is one that will not parse; a throw from its top level is a `threw`
like any other. `import` of a node builtin resolves as it does anywhere else, and that is what
a bundled builtin lands on.

## The answer — the failure kind IS the error code

genroc's `on_error` matches error codes, and an external task's `raises` declares the closed
set a worker may send. So the classification lives in the **code**, where a rule can match it,
rather than in a body a `switch` has to read.

| outcome | the worker submits | |
|---|---|---|
| the script returned | `result: <the return value>` | `return;` sends nothing, which genroc reads as `null` |
| the script faulted | `error: {code, message, data}` | `code` is the kind, below |
| the **evaluator** faulted | *nothing* — it releases the claim | the task returns to the queue for another worker |

The five kinds, every one of them **permanent** — a retry re-runs the same code on the same
input and fails identically:

`compile_error` · `threw` · `timeout` · `nonserializable` · `exited`

`data` carries `{name, stack?}`: `name` is what a script sets to tell one refusal from another
(`e.name = 'LimitExceeded'`), and `stack` is renumbered to the lines the author wrote and
trimmed to 2 KiB.

**The retryable class has no code on purpose.** A runner that faults releases its claim, which
is how a queue spells "try this somewhere else" — it puts the task in front of a different
worker instead of spending the definition's `on_error` budget on this one's bad day. There is
no retry policy to write.

## The genroc side

```yaml
- id: price
  action:
    type: external
    input:
      code: |
        export default function (input) {
          if (input.amount > 100) {
            const e = new Error('amount over the limit');
            e.name = 'LimitExceeded';
            throw e;
          }
          return { fee: input.amount * 0.1 };
        }
      input: "$: input"
    result_schema: { type: object, properties: { fee: { type: number } }, required: [fee] }
    raises:
      threw:           { $ref: "#/$defs/script_error" }
      timeout:         { $ref: "#/$defs/script_error" }
      compile_error:   { $ref: "#/$defs/script_error" }
      nonserializable: { $ref: "#/$defs/script_error" }
      exited:          { $ref: "#/$defs/script_error" }
  timeout: 30s
  on_error:
    - code: [threw]
      goto: $script_failed
    - code: [compile_error, nonserializable, exited]
      panic: { code: script_broken, message: "the script did not run: ${error.code}" }
  switch: [{ goto: end }]

- id: script_failed
  switch:
    - case: 'last_error.data.name == "LimitExceeded"'
      raise: { code: limit_exceeded, message: "the script rejected the amount" }
    - raise: { code: script_failed, message: "the script failed" }
```

**A script cannot name a genroc error code.** `raise`/`panic` codes are literals, never
expressions, so the mapping from a thrown error to an authored code is this `switch` — one
task the definition owns, reading `error.data`. That is the whole error protocol: the worker
classifies into a code, the definition names the outcome.

Set the task `timeout` **above** `timeout_ms`. If the task's deadline fires first the code is
`external.timeout`, which is in `errcode.Unknowable()` — permanently unretryable on an
`only_once` task, and indistinguishable from no evaluator running at all.

`only_once` is worth considering if your scripts have side effects: without it a worker that
dies mid-script has its task re-claimed and the script runs again. With it the task is never
handed out twice, and the instance gets a catchable `external.lost` instead.

## `${` must be escaped as `$${` — when the code is inline

An external task's `input` is a Shape, so `${…}` is genroc's interpolation marker and a JS
template literal inside `code` is read by genroc rather than passed through. Write `` `<$${x}>` ``.
A leading `$:` on the code string needs `$$:` for the same reason. See
[specs/typed-values.md](../specs/typed-values.md). Moving the code into a `.ts` file
removes this entirely — see the next section.

## `import.ts` — the author-time half

`import.ts` is the **code-phase resolver** genctl runs before a definition is applied. It
never touches the queue and the worker never runs it; the two halves share this package only
because they share a calling convention, which is exactly the coupling that breaks silently
if they version apart.

Register it in the project's `.genroc`:

```yaml
resolvers:
  import: { phase: code, ext: .ts, command: [node, ../eval-node/import.ts] }
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

The bundle is one self-contained ES module whose default export is the author's own — nothing
wraps or rewrites it, so what the editor checks and what the realm runs are the same module, and
**a missing default export is a failed apply** rather than a fault at run time. Imports resolve
through TypeScript under the same config the check ran with, so a `paths` alias that typechecks
also bundles. They are inlined at build time, so the string a definition version stores is
self-contained forever — with one exception: **node builtins stay as imports**, which the realm
resolves. A package is frozen into the definition; `node:fs` is resolved by whatever runner
executes it.

### Your tsconfig, your types

The generated project config `extends` **the nearest `tsconfig.json` above the script** —
the one your editor already reads, so the two cannot disagree. Two scripts under two
different configs are two `tsc` runs.

Of that config, three keys are the toolchain's and the rest are yours:

| key | owner | why |
|---|---|---|
| `lib` | the toolchain | Describes the realm. A worker thread has no `document`, whatever a config claims. |
| `include` | the toolchain | Forced to `[]`. A base `include` survives beside our `files` and would drag your whole tree in to be checked as scripts. |
| `types` | **you** | How a script opts into the node globals. The realm has them, so refusing the declarations would only lie. With no tsconfig at all the default is `[]`. |

```jsonc
// tsconfig.json beside your scripts
{ "compilerOptions": { "types": ["node"] } }  // now `import { appendFile } from "node:fs/promises"` typechecks
```

**This is what removes the `$${` escaping above** — a template literal in a `.ts` file is
never read by genroc, because genctl doubles every `$` on splice.

## The realm — one Worker per execution

`evaluate()` starts a Worker (`realm.ts`), posts the code into it — where a module loader hook
serves it to `import()`, so the engine numbers the script's frames from the author's own source —
and races the reply against the budget; `terminate()` runs on every path. That thread is what the contract rests on, and it buys
exactly three things the previous in-process evaluator could not:

- **The budget is enforced, not merely reported.** A synchronous `while(true){}` never
  yields, so no in-process timer can interrupt it — the old evaluator hung forever on that
  input, and said so in this file. Killing the thread is the only bound. Measured on Node
  24: `terminate()` stops a spinning worker, and the CPU it was burning goes with it.
- **A fresh global object per execution.** One script cannot configure the next. It is also
  why there is no compile cache any more: a cache inside a discarded realm can never be hit.
- **The script's mistakes stay the script's.** An uncaught throw, and `process.exit()`, end
  the realm and come back as a `422` — neither reaches the runner.

It costs about **27ms per execution** end to end for a trivial script (Node 24, M-series
laptop), a 200 KiB body about 30ms. Roughly 11ms of that is Node re-stripping `realm.ts`'s
types on every realm — precompiling it to JavaScript measures 17ms and would buy that back, and
is deliberately not done: a build artefact that goes stale against its source fails silently,
and this file is the one where a wrong line number is invisible.

A subprocess per execution measures ~55ms here, twice the thread rather than the ten times it
cost on the previous runtime. It contains the two things a thread cannot (below), so the upgrade
path is a live one at a price worth naming rather than a theoretical one.

## What this is not

- **Not deterministic.** A script reads the real clock and the real RNG, and a retry
  re-executes — so attempt two can differ from attempt one. An earlier version injected a
  pinned `Date` and a seeded `Math`; nothing could supply a stable `now` (the expression
  environment has no clock), so the pin was the wall clock under another name, and the `ctx`
  it needed was surface with nothing behind it. A value that must survive a retry belongs in
  the definition, passed through `input`.
- **Not a sandbox.** The realm isolates *execution*, not *authority*: a script gets the
  worker's filesystem, network and environment, and any node builtin it imports. That is
  deliberate — a script task is meant to do real work — but the trust boundary stays the
  same-trust-domain one (your genroc, your worker host). It is not the multi-tenant story, and
  nothing here should be mistaken for one. Pulling does move the boundary in one useful way:
  the worker needs no inbound reachability, so nothing but the worker can ask it to run code.
- **A thread does not contain memory or a native crash.** A worker shares the process
  address space, so a script that exhausts memory takes the runner with it — and so does a
  fault in the runtime itself, which is not hypothetical: the previous one segfaulted on
  resume from laptop sleep, taking the in-flight evaluation with it. Containing that is what
  the subprocess strategy is for — `eval.ts` keeps HTTP out precisely so it can be swapped
  underneath.
- **Concurrency is capped by this worker, not by genroc.** Each evaluation is a thread, and
  `CONCURRENCY` is how many it will claim at once. Raising it past what the host can run turns
  a queue back into threads fighting over a core.
- **Not where imports and type checking happen.** The evaluator takes one self-contained
  module and knows nothing about TypeScript; `import.ts` is what bundles an author's script and
  its dependencies into that module, at author time. See above.
- **`eval.ts` and `realm.ts` know nothing about genroc.** `worker.ts` is the entire
  queue-facing half, which is what keeps the containment strategy swappable — and what lets
  the realm's own properties be tested by calling `evaluate()` directly.
