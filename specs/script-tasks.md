# Script tasks: a scaffolded runtime, not an engine feature

Status: **PROPOSAL, 2026-08-07. Partly built 2026-08-19. Superseded in part 2026-08-20.**
`bun-runtime/` is a first evaluator, and the error tiering §"What the template owns" argues
for is live — but as a plain **`fetch`** at a sidecar, not the `external`-plus-worker-fleet
shape below, which spends even less engine capability than this doc claims to need. The
import directive, the type generator and the bundler are unbuilt, so code is inlined in the
definition (and therefore subject to `${` escaping) and nothing typechecks. What shipped is
documented in [bun-runtime/README.md](../bun-runtime/README.md).

**§"What genroc adds" is superseded by
[source-resolution.md](source-resolution.md)**, which owns the resolution model: this doc
argued a single pass, and generating types from inferred schemas needs two. Everything else
here stands.

The sidecar tier of
[custom-tasks.md](custom-tasks.md), made turnkey by a scaffolder rather than by the
engine. "Plugin" here means an optional external component — the toolchain and the
worker fleet — never dynamically loaded code, which that doc rules out and this keeps out.

## Thesis

Running user TypeScript needs **no new engine capability**. A script task is an
`external` task whose input carries a code string; a worker pulls it off the queue,
evaluates it, and resolves it with JSON. Lease, retry, `on_error` and timeout are the
ones already there.

So the feature is a **setup experience**, not a subsystem. `create-genroc-app` scaffolds
a project and optionally installs the TypeScript runtime: the type generator, the
bundler, the tsconfig, the worker. All of it versions independently of genroc and can be
replaced wholesale by a Python or WASM equivalent without the engine noticing.

`genctl` gains one generic thing to make it ergonomic, below. Everything else stays out.

## What genroc adds: the import directive

**Superseded by [source-resolution.md](source-resolution.md).** What this section got right
stands there: a directive in a source file resolves a path into a string through a binary
the project names, type checking lives in that binary's exit code rather than in a step
anyone adds, and resolution is `genctl`-side so **the server having no resolver is the
security answer**.

What it got wrong is the pass count. Resolution cannot run wholly *before* validation,
because the `Input` declarations a script typechecks against are the *product* of
validation's inference. So resolution splits at the line where a resolver's output stops
being visible to the type checker — structural resolvers before validation, code resolvers
after it, with the second constrained to produce strings so it cannot invalidate the first.

## What the template owns

Recorded so it does not drift back into the engine:

- **Types from schemas.** The generator emits `Input`/`Output` declarations and the author
  fills a typed body; the importer runs `tsc` before it bundles. Nothing is lost by checking
  on the author's machine — schemas already validate both ends at runtime, so static types
  are editor support, not the safety mechanism. That is what keeps the *server* free of any
  evaluator dependency, which was always the claim worth making: the schemas it validates
  against are its own. (An earlier draft said this kept validation *offline*. It does not —
  the types are inferred by validation, so the generator asks for them. See
  [source-resolution.md](source-resolution.md) §Open questions for the one change that
  would.)
- **The tsconfig is part of the sandbox.** No DOM lib and no Node types means `fetch`,
  `process` and `setTimeout` do not typecheck, so the authoring layer refuses what the
  runtime would refuse.
- **The worker.** Fresh evaluation context per execution — module-level state would
  otherwise leak between unrelated instances. Cache the compiled artifact by content hash
  (immutable, so no invalidation) to get the warm start without the leak.
- **A pinned clock and seeded RNG, not deleted ones.** Retries re-execute; a script
  reading the wall clock differs on attempt two. Injecting a fixed timestamp keeps
  `Date.now()` working *and* reproducible, where deleting `Date` leaves the generated types
  asserting what the runtime contradicts.
- **Error codes with honest retryability.** A type error and a thrown exception are
  permanent; only an evaluator fault is worth retrying. The worker picks distinct codes and
  the scaffolded `on_error` rules act on them — folding all three into one failure makes the
  retry budget worse than useless. Engine-side this is just ordinary error routing.

## Deferred: `process_objects` ownership

Small bundles inline fine, so nothing here blocks a first version. It becomes real when
bundles carry libraries.

Content addressing already exists (`ObjectRef.Ref` is a sha256 prefix), so a worker could
cache by ref with no invalidation problem. Two things stand in the way, and they are one
change: definition-embedded values are never externalized (only what a running instance
produces becomes an object), and ownership is `(instance_id, hash)` with instance-scoped
GC — so code would outlive its object and a fetch after a cache eviction would 404 against
a runnable definition. Routing definition-embedded values through the store puts the object
under the definition version, which *is* the retention rule code needs; the correct lifetime
falls out rather than needing a special case.

Constrained by migration 018: unredacted context-only objects are never served, only
log-referenced (pre-redacted) rows. Anything letting a worker fetch by ref must not become
a general read primitive over context.

## Open questions

- **Secrets reaching the worker.** Deferred. Cheap non-foreclosure: have the worker carry
  its lease credential on any object fetch from the start, even unchecked, so authorization
  is a later tightening rather than a protocol break.
- **Runtime**, template's choice: goja is the simplest embedding but has no hard memory
  ceiling, so a runaway allocation takes the worker with it; QuickJS-on-wazero and a Deno
  subprocess both contain it. Deciding late costs nothing — the resolution contract is
  indifferent.
- Whether a script task is a distinct action type or plain `external` with a reserved
  input shape. The latter adds nothing to the engine, which is the argument for it; the
  former is what would let the editor schema and `genctl` say anything useful about it.
- Scripts are **leaf computations**. Logic that migrates from the definition into a script
  stops being self-describing state, and a script that orchestrates is the feature failing.
  Nothing enforces this — DSL expressiveness is what holds the line, which makes it a
  reliability concern rather than an ergonomic one.
