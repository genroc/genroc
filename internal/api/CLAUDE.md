# internal/api

Routes, request/response shapes and the OpenAPI spec are all generated from the action
registry in `actions.go` — **add an endpoint there, not in `server.go`**. `ListenHTTP`
iterates the registry; the handwritten `mux.HandleFunc` blocks below it are the docs and
spec routes only.

Connection limits and readiness: [docs/resource-limits.md](../../docs/resource-limits.md).
The rest of this file is the part that breaks silently.

## `ListenHTTP` must wait for the drain, and must not wait after a bind failure

`ListenAndServe` returns `http.ErrServerClosed` the moment `Shutdown` closes the listener —
**before** in-flight requests finish. So returning as soon as it does completes the caller's
`WaitGroup` while requests are still being served, and process exit severs them: a graceful
shutdown that is graceful in name only. Bounding `Shutdown`'s context without also awaiting
it guards a goroutine nobody waits for, which is the shape this code had first.

The `errors.Is(err, http.ErrServerClosed)` guard before `<-drained` is not defensive
tidying. A bind failure returns before the context is ever cancelled, so the shutdown
goroutine is still parked on `ctx.Done()`; waiting on it there hangs the process on a port
that is already in use — the one case `main` treats as fatal precisely so it fails loudly.

## There is no `WriteTimeout`, and adding one breaks `/tick`

`POST /tick` blocks until every instance it claimed has finished advancing. That is
unbounded by design — it is the manual-tick mode (`-poll 0`) the integration suite runs on.
Any `WriteTimeout` short enough to defend against a slow reader also severs a legitimate
long tick, and the failure appears as flaky, unexplained truncation in tests far from this
file. `ReadHeaderTimeout` is what bounds a connection that opens and sends nothing, which is
the exposure that actually matters on an unauthenticated listener.

## `MaxBytesReader` belongs in the route wrapper

It is applied once in the registry loop, before `a.envelope(r)`, so it covers every action
including those with a custom `fromHTTP` that reads the body itself. Moving it into
`actionDef.envelope` would silently exempt every custom extractor.

The resulting error surfaces through `envelope` as a 400 `invalid`, carrying the stdlib's
`"request body too large"`. That text is the only thing distinguishing a body that was
*refused* from one that was buffered whole and then rejected on its merits — an oversized
definition is a 400 either way, which is what made the first version of the regression test
pass with the cap removed.

## The health endpoint must not consult the engine

`health()` reaches its verdict from `db.Ping` alone and returns before touching
`h.engine`, so a worker whose database is gone still answers the probe instead of faulting
inside it. `TestHealth_ReportsUnavailableWhenTheDatabaseIsGone` passes a **nil** engine to
hold that in place — a new `h.engine.X()` call above the ping turns that test into a panic
rather than a failure.

`lease_age_ms` is reported, never judged. It grows during exactly the conditions
`Engine.leaseGate` recovers from by itself (see
[internal/engine/CLAUDE.md](../engine/CLAUDE.md)), so failing the probe on it restarts
workers that were seconds from recovering — and restart discards the in-flight advances the
gate was protecting.

## Adding a `Code` means three edits

`errors.go` holds the API classification. A new `Code` needs its constant, an entry in
`statusByCode` (which `statusOf` falls back to 500 for, so an omission is silent), and an
entry in `Enum()` — that last one is what publishes it to the OpenAPI spec, so a code
missing from it is undocumented while still being returned.

## Pointers

- Connection limits are `Server` fields rather than constants **only** so tests can drive
  them to durations worth waiting on (`server_test.go`). `NewServer` sets every one;
  nothing in production overrides them.
