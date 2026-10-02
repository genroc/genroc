# Custom tasks: child processes, no plugins

Status: **a principle in force, not a feature.** The typed interface ([unknown-type.md](unknown-type.md))
and version pinning ([version-compatibility.md](version-compatibility.md)) shipped on their own; the
sidecar tier exists as [script-tasks.md](script-tasks.md).

No user code is loaded into the engine. A custom task is a child process; arbitrary logic is an
out-of-process service it calls via `fetch`/`external`. The extension boundary is network + auth.

```
Engine            fixed, typed orchestration kernel — never runs user code
  │
Child process     the custom-task interface: typing, retry, cancel, polling (YAML)
  │
Sidecar           arbitrary compute, out-of-process, any language
```

- **Bounded blast radius**: in-process code could crash, leak or block the engine.
- **The trust boundary moves out of the engine**, so multi-tenancy is governance plus API auth,
  not an in-process sandbox. Untrusted authors calling provider built-ins (Model B) is out of scope.

**Decided:** no cross-tree lock or singleton in the engine. "Start exactly once, everyone else
waits" is the sidecar's job — its `ensure-running` dedups server-side.

## Open

- An idempotency key for sidecar calls: a re-run after a lost lease calls twice, and nothing
  instance-scoped reaches the expression scope today to dedup on.
- Cancel reaching a child-process wrapper's sidecar: only `external` workers learn of a cancel
  (renew's `cancelled` list); a `fetch` sidecar never does.
