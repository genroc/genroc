# internal/api

## The API namespace is one constant, and one action opts out

Every route is served under `apiPrefix` (`/api`), applied by `actionDef.mountPath` at mount time.
The registry keeps the LOGICAL path, so the prefix is in one place rather than in 28 literals,
and the OpenAPI spec declares it once in `servers` — which is where a base path belongs, and what
lets a generated client prepend it while the documented paths stay the ones the registry routes.

`actionDef.Root` mounts an action at the server root instead. `/healthz` is its only user and the
bar for a second is high: it exists so a **probe does not move when the API namespace does**, and
so a deployment splitting humans from machines by path on one hostname can leave it alone
(specs/api-auth.md §1). A Root action's path item overrides the spec's `servers`, or it would be
documented at a path it is not served on.

**A new endpoint's prefix is a security decision, not a filing decision.** A deployment splits
humans from machines by path (specs/api-auth.md §1): `/api/external-tasks/*` is the low-trust
inbound zone a worker or a webhook is given, and everything else under `/api/` is control plane.
So an endpoint goes where its LOWEST-trust caller needs it, not next to the endpoints it reads
like. Filing a control-plane action under the inbound prefix hands it to every worker; filing an
inbound one under the control plane forces users to punch a hole in the rule protecting
`PUT /definitions`. Both mistakes have already been made once each and are recorded in §1.

## One gate, every transport, and a permission that cannot be forgotten

`authorize` (`auth.go`) is the only authorization check, and BOTH dispatch paths call it —
`ListenHTTP`'s route wrapper and `Handlers.Handle`, which serves TCP and UDS. Putting it in HTTP
middleware alone would leave two transports open, which is the shape this nearly had.

**An action with an empty `Allow` is admin-only.** That is the fail-closed default: a new
endpoint is closed until someone decides otherwise, and `TestEveryActionDeclaresAPermission`
makes the decision explicit by requiring admin-only actions to be named there with a reason.
`Open: true` skips the gate entirely; `/healthz` is its only user, pinned by
`TestOnlyTheProbeIsOpen`, because a probe must answer before an identity exists.

`Envelope.principal` is **unexported on purpose**. The envelope is decoded straight off a socket
for TCP/UDS, so a serialisable field there would let a client assert its own grants. The
transport attaches it after establishing identity; `Handle` refuses an envelope without one
rather than defaulting to open, which is why in-process callers (tests) must supply their own.

`none` is still the DEFAULT, and then the transports attach `anonymousAdmin()` and nothing is
refused — but `token` and `header` both ship, and a deployment runs them together
(specs/api-auth.md §2, §3). `httpPrincipal` tries the forwarded identity first and falls back to
the bearer token; they cannot collide, because a browser behind the proxy carries no token and a
machine bypassing the proxy carries no forwarded identity.

**Everything under `apiPrefix` requires a credential; `publicPrefix` (`/public`) is what does
not.** The split exists so the zone is legible from the path — a deployment writes ingress rules
from prefixes, and a route that reads as gated while answering without one is the mismatch
specs/api-auth.md §1 is about. It was briefly real: the API docs sat at `/api/docs`.

A hand-written route under `/api` that cannot be a registry action (it answers with HTML or raw
JSON rather than a `Reply`) must call `Server.guard` — the per-process docs are the only two,
and `TestEveryApiPathIsGated` names them so a third cannot be added silently.

`/public/process-schema.json` is served outside the registry because it answers with raw bytes
rather than a `Reply`, like the OpenAPI routes beside it. The docs site publishes the same schema
at `genroc.org/process-schema.json`; that is a released artifact and this is the unreleased-build
convenience, so the paths differ deliberately and getting-started.mdx says which to use.

Routes, request/response shapes and the OpenAPI spec are all generated from the action
registry in `actions.go` — **add an endpoint there, not in `server.go`**. `ListenHTTP`
iterates the registry; the handwritten `mux.HandleFunc` blocks below it are the docs and
spec routes only.

Connection limits and readiness: [specs/resource-limits.md](../../specs/resource-limits.md).
The rest of this file is the part that breaks silently.

## The actor is `source:subject`, and only operator-initiated rows carry one

`Principal.Actor()` is the ONE place an audit identity is spelled (specs/api-auth.md §7). The
source is inside the string rather than in a second column so that `ada@example.com` can never be
read without knowing whether genroc authenticated it (`token:`, `header:`) or merely wrote down
what a proxy claimed (`asserted:`, from `Server.attribute` in `none` mode, which grants nothing).

**Only what an operator asked for is attributed.** The engine advances instances on its own
behalf, so `AuditCreated` takes an actor for a ROOT instance and `""` for a spawned child, and no
engine event carries one. Crediting the operator who started a run for every row the engine then
writes would put an identity on work nobody requested.

## A successful assertion carries its status too

`Reply.Code` maps to a failure status through `statusOf`; `Reply.Outcome` is its
success-side twin and maps through `statusOfOutcome` (200 applied / 202 accepted / 204
unchanged). Both live on `Reply` rather than on the HTTP response for the same reason —
**TCP and UDS clients encode `Reply` directly and have no status line to read**, so an
outcome expressed only as a status code would be invisible to two of the three transports.

204 must not carry a body, which is why `outcomeReply` leaves `Data` empty for exactly
that outcome and `writeReply` returns before encoding. A body added there is silently
dropped by well-behaved clients and rejected by strict ones. `actionDef.AltSuccess` is what
puts the extra statuses in the spec — `Resp` alone documents 200, so without it a client
generated from `openapi.json` sees one of the three answers. specs/id-list-commands.md.

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

## `applyBatch` plans, then commits — a write in the planning loop reintroduces partial applies

An apply is one logical change. `applyBatch` therefore runs in two passes: the loop decides
and validates every definition, appending `db.DefinitionWrite` entries and nothing else,
and `db.ApplyDefinitions` then writes the lot in a single transaction.

The passes exist because validation is interleaved by nature — a definition validates
against the versions its batch siblings resolved to, held in `batchVersions`. The original
loop saved each definition as it validated it, so the first rejection had already committed
everything before it: one `apply` landed partially, leaving parents pointing at children
that were never stored.

Two things follow, and both are silent when broken:

- **Nothing in the planning loop may write.** Anything added there — a channel pointer, a
  dependency row, a "just this one" save — restores exactly the partial-apply bug the two
  passes exist to remove, and only for batches that fail after that point.
- **Channel pointers are decided during planning** (`channelsFor`), not derived mid-commit.
  Whether the default channel needs setting is a question about state *before* the batch,
  and asking it inside the transaction would read rows that transaction is writing.
- **`DefinitionWrite.Actor` is needed on the `Def == nil` entry too.** `ApplyDefinitions` upserts
  the channel pointer OUTSIDE its `Def != nil` block, so the "content already exists, only the
  pointer moves" entry still writes a row — stamping `updated_at` and `actor` together. Omitting
  the actor there blanks whoever set the pointer while claiming it moved just now.

`db.ApplyDefinitions` judges nothing — it writes what it is given. Validation belongs to the
planning pass alone.

There is deliberately **no cascade**: applying a child does not re-register its parents.
A parent keeps the child version baked into it until it is applied again, and `status`
reports the resulting drift as a stale ref. Re-introducing an auto-update would mean
planning parents too, and a parent revalidates against its children's *new* versions —
versions that exist only in the plan, so its getter would need to resolve from there rather
than from the DB.

## Compat resolution: two tables, and what a missing counterpart means

`handlers_compat.go` turns two selectors into two `name → version` tables and reconciles
them. The comparison itself judges; this decides what is answerable.
[specs/version-compatibility.md](../../specs/version-compatibility.md) is the design.

- **An entry is explicit or implicit, and that is the whole rule.** Explicit = the caller
  named it (a `versions` entry, or a submitted document). Implicit = it arrived via a
  channel listing or as some other version's pinned child. An explicit entry with no
  counterpart is an **error**; an implicit one is **carried over** at its current version.
  Naming a process and getting silence is a mistake worth catching; a process that came
  along for the ride is simply not moving.
- **Submitted documents are validated first**, through the same pass `POST
  /definitions/validate` runs (`validateSubmitted`). A document that does not analyse has
  nothing to compare, and "unanalysable" is a worse answer than the one validate gives.
  Stored versions are not re-validated — see
  [internal/validation/CLAUDE.md](../validation/CLAUDE.md).
- **`closeOverDependencies` makes a named parent compare the graph it runs**, not the parent
  alone. An entry already in the table always wins, or the result would depend on map
  iteration order.
- **`-f` and an explicit `--to` are refused together.** Both define the target side, and
  silently preferring one compares something the caller did not ask for.

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

## A list row must be complete; a single-instance response may be incomplete

A value past the inline cutoff leaves the row and is listed under `objects` instead, at the
path it belongs to. That is fine on a single-instance response: the caller asked about one
thing and can see what was cut. It is not fine in a LISTING — a row whose slot came back
silently empty is a row a caller computes on and gets wrong, and there is no natural moment
to notice. So a list row carries no value that can be externalized, and therefore carries no
`objects`.

Two endpoints are exempt because the externalized value IS what the caller came for, and
omitting it would not make the response complete, only useless:

| endpoint | why |
|---|---|
| `/external-tasks` | a worker claims a task to get its `input` |
| `/instances/{id}/logs` | a log entry exists to carry its payload |

Their hazard is real and worth knowing: a worker that ignores `objects` receives `input: {}`
and runs the task against an empty payload — a wrong answer rather than a failure. The shipped
worker splices (`eval-node/worker.ts`); a third-party one has to be told.

`listshape_test.go` walks the action registry and enforces this, so a new listing that carries
`objects` fails rather than being noticed later. Adding a third exception means naming it there
with its reason.

## Adding a `Code` means three edits

`errors.go` holds the API classification. A new `Code` needs its constant, an entry in
`statusByCode` (which `statusOf` falls back to 500 for, so an omission is silent), and an
entry in `Enum()` — that last one is what publishes it to the OpenAPI spec, so a code
missing from it is undocumented while still being returned.

## Pointers

- Connection limits are `Server` fields rather than constants **only** so tests can drive
  them to durations worth waiting on (`server_test.go`). `NewServer` sets every one;
  nothing in production overrides them.
