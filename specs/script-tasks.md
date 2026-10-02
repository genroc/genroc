# Script tasks: a scaffolded runtime, not an engine feature

Status: **Built.** Shipped behaviour is [eval-node/README.md](../eval-node/README.md); the queue
is [external-task-queue.md](external-task-queue.md) and resolution
[source-resolution.md](source-resolution.md).

## Thesis

Running user TypeScript needs **no new engine capability**. A script task is a plain `external`
task whose input carries a code string (`{code, input, timeout_ms}`); a worker (`eval-node`)
claims it off the queue, evaluates it, and resolves it with JSON. Lease, retry, `on_error` and
timeout are the ones already there. The engine gained only `external.lost`; the larger half was
the queue under it — claim, visibility timeout, error channel.

- **Not a distinct action type**: plain `external` adds nothing to the engine, at the cost of the
  editor schema and `genctl` saying nothing script-specific.
- **Not a `fetch` to an HTTP evaluator**: that holds one of `--max-concurrent` advance slots per
  call and needs genroc to reach an unauthenticated code-execution endpoint. A worker needs only
  outbound access and sets its own concurrency.

So the feature is a **setup experience**: `genctl init --eval-node` scaffolds the type generator,
bundler, tsconfig and worker. All of it versions independently of genroc and could be replaced by
a Python or WASM equivalent without the engine noticing.

## What genroc adds: the import directive

Owned by [source-resolution.md](source-resolution.md). What carries over: a directive resolves a
path through a binary the project names, type checking is that binary's exit code, and resolution
is `genctl`-side, so **the server having no resolver is the security answer**.

## What the template owns

Recorded so it does not drift into the engine:

- **Types from schemas, checked on the author's machine.** The generator emits `Input`/`Output`
  declarations from the inferred schemas and the importer runs `tsc` before it bundles. Schemas
  validate both ends at runtime, so static types are editor support, not the safety mechanism —
  which keeps the server free of any evaluator dependency.
- **The tsconfig describes the realm; the Worker enforces it.** `lib: [esnext, webworker]`, since
  a worker has no `document`, and `types` left to the author, since the realm has node's. Not a
  tsconfig fence refusing `process` and `require`: it stops nothing at runtime, and it makes
  "import a library" and "reach the host" the same refusal.
- **The base config is the nearest `tsconfig.json` above the script** — the one the author's
  editor reads, so editor and apply agree. Scripts under different bases are separate `tsc` runs,
  because `extends` takes one base. `lib` and `include: []` are written after the `extends`: a
  base must not widen the realm's description, and a base `include` would drag a tree in to be
  checked as scripts.
- **One `worker_threads` realm per execution** (`eval-node/realm.ts`). A synchronous
  `while(true){}` never yields, so only killing a thread bounds it; a fresh global per execution,
  and `process.exit` ending a realm rather than the runner, follow from the same choice. It costs a
  realm (~1.7ms) and a recompile per call — a compile cache in a discarded realm can never hit.
  Not goja: it has no hard memory ceiling. A subprocess (~16.6ms) also contains memory and native
  crashes, and is the upgrade path if scripts must be distrusted rather than merely bounded.
- **No pinned clock or seeded RNG.** The expression environment exposes no clock, so a pin would
  be the wall clock renamed. Reopening this means giving the engine a per-attempt timestamp
  *first*; the value would then travel through `input` like any other.
- **Error codes with honest retryability.** The five script failures (`compile_error`, `threw`,
  `timeout`, `nonserializable`, `exited`) are permanent and arrive as authored codes an `on_error`
  rule matches directly. An evaluator fault has no code: the runner releases its claim, which puts
  the task in front of another worker without spending the definition's retry budget.

## Bundles and the object store

A bundle is a definition-embedded string, stored inline in the definition. Once it reaches an
instance's `external_input` it is cut into the content-addressed `objects` store under that
instance's claim, so instances running the same code share one object and a worker can cache by
ref. Nothing claims objects under a definition: `model.ObjectOwnerDefinition` is declared and
unused.

## Open questions

- **Secrets reaching the worker.** Deferred. Cheap non-foreclosure: have the worker carry
  its lease credential on any object fetch from the start, even unchecked, so authorization
  is a later tightening rather than a protocol break.
- Scripts are **leaf computations**. Logic that migrates from the definition into a script
  stops being self-describing state, and a script that orchestrates is the feature failing.
  Nothing enforces this — DSL expressiveness is what holds the line, which makes it a
  reliability concern rather than an ergonomic one.
