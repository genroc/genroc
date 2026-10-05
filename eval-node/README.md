# @genroc/eval-node

TypeScript script tasks for [genroc](https://genroc.org). The package provides two binaries:

* **`genroc-import`**: the bundler. `genctl` runs it on every `apply` and `generate` to turn
  `$import: ./script.ts` into one typechecked, self-contained module string.
* **`genroc-eval-node`**: the worker. It claims script tasks from genroc's queue and runs each
  one in a fresh worker thread.

Requires **Node 24+**. Full guide:
[TypeScript evaluation](https://genroc.org/guides/process-definition/typescript-evaluation/).

## Quick start

    genctl init --eval-node

This scaffolds a project with everything set up: the package, a `.genroc` that registers the
resolver, a `tsconfig.json`, a reusable `script-node` child process, and a `compose.yaml` that
runs the worker.

## Adding it to an existing project

    npm i -D @genroc/eval-node

Register the resolver in the `.genroc` beside your definitions:

```yaml
resolvers:
  - name: import
    phase: typed
    ext: [.ts]
    command: [npx, genroc-import]
    types:
      Input: task.action.input.input
      Output: task.action.result
```

Then reference a script from a task. The simplest way is to spread the `script-node` child that
`genctl init --eval-node` generates:

```yaml
- id: greet
  action:
    type: child
    <<: "$process: ./script-node.genroc.yaml"
    input:
      code: "$import: ./greet.ts"
      input: { who: "$: input.who" }
    result_schema:
      type: object
      properties: { greeting: { type: string } }
      required: [greeting]
```

## Writing a script

A script is an ES module. Its default export receives `input`, and its return value becomes the
task result. The function may be async.

```ts
import type { Input, Output } from "./greet.genroc";

export default function (input: Input): Output {
  return { greeting: `hello, ${input.who}` };
}
```

* `genctl generate` writes `greet.genroc.d.ts` beside the script. `Input` and `Output` come from
  the definition, so a mismatch fails the typecheck.
* `genctl apply` typechecks and bundles. **A type error or a missing default export fails the
  apply.**
* Imported files and npm packages are inlined into the bundle, so a stored definition never
  depends on what is installed later. Node builtins (`node:fs`, …) stay as imports and resolve
  on the worker.
* The generated config extends your nearest `tsconfig.json`. To use Node globals and
  builtins, add `"types": ["node"]` to it.

## Running the worker

```sh
docker run -e GENROC_SERVER=http://host:8448 -e TASK=eval_node ghcr.io/genroc/eval-node:preview
# or
GENROC_SERVER=http://localhost:8448 TASK=eval_node npx genroc-eval-node
```

The worker connects to genroc, not the other way round, so it only needs outbound access.

| env | default | |
|---|---|---|
| `GENROC_SERVER` | `http://localhost:8448` | the genroc server |
| `GENROC_TOKEN` / `GENROC_TOKEN_FILE` | *(none)* | token, or a file holding it, when the server uses `--auth token` |
| `TASK` / `PROCESS` | *(none)* | claim only this task id / process. **Without one, the worker claims every external task.** |
| `CONCURRENCY` | `4` | scripts run at once |
| `LEASE_MS` | `30000` | claim lease, renewed every third of it |
| `POLL_MS` | `250` | idle poll interval |
| `WORKER_ID` | `evaluator-<pid>` | the claim holder's name |

With auth enabled, create a token that has only the `worker` permission:

    genctl token create --perms worker --label evaluator -q

If the token is rejected, the worker exits instead of retrying.

## Errors

When a script fails, the worker reports one of five error codes for your `on_error` rules to
match. Retrying does not help with any of them, because the same code fails the same way:

| code | when |
|---|---|
| `threw` | the script threw or rejected |
| `timeout` | it ran past `timeout_ms` (default 5000) |
| `compile_error` | the module does not parse, or has no default-export function |
| `nonserializable` | the return value cannot be serialized to JSON (a cycle, a `BigInt`) |
| `exited` | the script called `process.exit()` |

`error.data` is `{name, stack?}`. Set `e.name = "LimitExceeded"` in the script to tell your
own failures apart. The stack is mapped back to your source lines.

If the worker itself faults, it releases the task for another worker and reports no error.

Keep the task's `timeout` above `timeout_ms`. Otherwise a slow script ends with
`external.timeout`, and you cannot tell it from having no worker at all. The `script-node`
template already does this.

## Limits

* **Not a sandbox.** Scripts have the worker host's filesystem, network and environment.
  Run only code you trust.
* **Not deterministic.** On a retry the script runs again with the real clock and random
  numbers. Pass any value that must stay stable through `input`.
* **Memory is not isolated.** A script that runs out of memory takes down the whole worker.
* **Run-once is opt-in.** If a worker dies mid-script, the task is claimed again and the script
  runs again. Mark the task `only_once` if your script has side effects.
* **Inline code needs escaping.** When you write `code` directly in YAML rather than
  `$import`, write `${` as `$${`, because genroc reads `${` as interpolation. Scripts in `.ts`
  files need no escaping.

## Links

* [TypeScript evaluation guide](https://genroc.org/guides/process-definition/typescript-evaluation/)
* [External tasks and the queue](https://genroc.org/guides/process-definition/external-tasks/)
* [Error handling](https://genroc.org/guides/process-definition/error-handling/)
* [Source on GitHub](https://github.com/genroc/genroc/tree/main/eval-node)
