# Custom tasks: child processes as the extension mechanism (no plugins)

Status: **NORTH-STAR / goals doc, 2026-07-21. Not implemented.** This records an
intended direction so future changes can be checked against it — it is a goal, not
a spec. Related: [the `unknown` type](unknown-type.md), [typed values](typed-values.md).

## Thesis

genroc should be extensible **without plugins**. No user code is ever dynamically
loaded into the engine. Instead:

- **Custom tasks are child processes** (YAML) — the flexible parent↔child
  relationship *is* the extension point.
- **Arbitrary logic lives in an HTTP sidecar** — a separate service (any language)
  that a child process calls via `fetch`/`external`. Complexity that can't be
  expressed in YAML goes there, out-of-process.

The extension boundary is the **network + auth**, never a dynamic-link boundary
into the core. This is the same choice Temporal (activities), Tekton/Argo (steps),
and GitHub Actions (containers) made, and for the same reasons.

## Why no plugins

Two payoffs, one obvious and one not:

1. **Stability.** In-process user code can crash the engine, leak memory, block the
   loop, or reach into internals. Keeping it out means the orchestrator's blast
   radius is bounded to orchestration.
2. **It is what makes multi-tenancy *tractable*.** Plugins put the trust boundary
   *inside* the engine — an in-process sandbox, a nearly unsolvable problem. Moving
   it to the network turns "secure an in-process sandbox" into "multi-tenant
   orchestration governance + normal API auth" — hard, but standard. The no-plugins
   choice is the security enabler, not a limitation.

## The three tiers

```
Engine            fixed, typed orchestration kernel — never runs user code
  │
Child process     the custom-task INTERFACE: typing, retry, cancel, polling (YAML)
  │
HTTP sidecar      arbitrary compute, out-of-process, any language
```

A child process's whole job is to wrap a raw endpoint in a **typed, cancellable,
retryable, observable** contract. That wrapper — not the raw HTTP call — is the
"custom task". The engine only ever moves typed data between wrappers.

Caveat vs. a native plugin: you lose in-process latency (a network hop) and
transactional coupling with the engine's DB. For orchestration-shaped tasks —
where the work dominates the latency — this does not matter.

## Use cases

- **Poller library** — the motivating case. An advanced `fetch` with polling, error
  handling, and cancellation, shipped as a YAML process a user loads and calls.
  Highly customizable: kickoff/poll/**cancel** endpoints, headers, error-code
  mapping, backoff.
- **Kubernetes / lambda-style handler** — a process pins a *specific version* of a
  container. The child subprocess does `ensure-running → poll-until-ready → call`.
  Like AWS Lambda: call any version any time and the system wakes it up. Payoff:
  **your API need not be backward-compatible, and long-running old processes can
  finish against the version they were pinned to.**
- **Third-party API access** — a vendor ships a YAML process wrapping their API;
  the user loads it and calls it as a typed function.
- **Provider built-ins (BPMN-style service tasks)** — a platform exposes built-in
  child processes that call its internal APIs, and lets its users compose them in a
  controlled, well-typed environment.

### Two trust models (do not conflate)

- **Model A — same trust domain.** User loads a library into *their own* genroc.
  npm-for-processes. The near-term target; achievable incrementally.
- **Model B — crossing trust.** A provider lets *untrusted users* author processes
  that call provider built-ins. A genuine multi-tenant-security undertaking
  (resource quotas, secret isolation, egress policy, log visibility). The
  no-plugins architecture makes it *feasible*, but it is out of scope for now — a
  potential future, named so we do not drift into it one feature at a time.

## The child-as-activity contract (what is necessary)

To make a child process a real custom-task boundary, the parent↔child (and
child↔sidecar) contract needs:

- **A typed interface.** `input_schema` is the signature; the result-typing modes
  (see [unknown-type.md](unknown-type.md) — pin / infer / unknown) are the return
  type; custom error codes are the `throws` clause. *(A process should be able to
  declare its output + error surface as a stable interface, so editing internals
  can't silently change consumers.)*
- **Idempotency under at-least-once.** Lease + crash recovery means a task can run
  twice (worker calls the sidecar, dies before recording completion, the lease
  expires, someone re-runs). "Kickoff a job" / "start a container" must not
  double-fire. The contract needs an **idempotency key** — ideally a stable task
  token the engine passes to the sidecar to dedup on. This is the single most
  important reliability detail, and it applies to *every* sidecar call.
- **Cancellation propagation, async and best-effort.** The parent cancelling a
  subtree must reach the sidecar to tear down real work — that is what the poller's
  **cancel** endpoint is for. Cancelling the engine's *view* does not stop a running
  container. Pin the semantics: fire-and-forget vs. confirmed, and what happens if
  cancel itself fails or lags.
- **Progress / heartbeat.** Long-running custom tasks should report progress upward
  (`UpdateInstanceProgress` exists; confirm it is reachable by a running child) —
  both for observability and to distinguish "slow" from "dead".
- **Timeouts & retries** — already present (`timeout`, `on_error` retries); they
  become the load-bearing reliability layer once every custom task is a network call.

## Coordination & lifecycle (the K8s handler's hard parts)

The K8s handler is the poller generalized, and it exposes two problems the engine
does *not* natively solve:

- **Cross-tree coordination.** "If not running, start it" across many concurrent
  callers is a thundering herd — genroc's model is a per-tree hierarchy with **no
  cross-tree mutex or singleton**, so "start exactly once, everyone else waits"
  isn't expressible in YAML. **Resolution that keeps the engine plugin-free: push
  dedup into the controller sidecar** — make `ensure-running` idempotent
  server-side. Do *not* add a distributed lock to the engine. (Name this as a
  deliberate choice; it means the sidecar owns coordination.)
- **Lifecycle ownership / GC.** Who tears the woken service down, and when
  (refcount? idle timeout?). Also the sidecar's job — the part AWS hides that you'd
  now own.

## Distribution tiers

- **Pure-YAML task** — fully portable. "Load the YAML and go." This is the
  frictionless story, and it holds *only* for tasks with no sidecar.
- **YAML + sidecar task** — needs infra deployed: "deploy the service, then load the
  YAML." The K8s handler is the lovely bootstrap that makes *other* sidecars
  deployable on demand — but the K8s controller sidecar itself must be deployed the
  old-fashioned way.

## Versioning

- Library processes are dependencies: a published version should be **immutable**
  and **pinnable**, with a compatibility story for updates (adding an optional input
  with a default = safe; removing one or narrowing output = breaking).
- The K8s pattern has **two independent version axes**: the wrapper process's
  version and the target service's version. The "finish old processes against old
  versions" payoff requires the registry to retain old service versions
  indefinitely.

## What is necessary — checklist

- [ ] Declared process interface: output type + error surface as a stable contract
      (see [unknown-type.md](unknown-type.md) result-typing modes).
- [ ] Idempotency token in the child↔sidecar contract.
- [ ] Cancellation-propagation semantics (async/best-effort) to sidecars.
- [ ] Progress/heartbeat reachable by a running child.
- [ ] Version immutability + pinning + a compatibility check on update.
- [ ] Process namespacing + dependency bundling for distribution.
- [ ] (Model B, later) resource quotas, secret isolation, egress policy, log
      visibility gating.

## Out of scope (for now)

Model B multi-tenant security. Recorded as a goal; not a near-term commitment.
